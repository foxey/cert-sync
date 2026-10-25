package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/redis/go-redis/v9"
)

const (
	defaultRedisHost    = "redis"
	defaultRedisPort    = "6379"
	defaultRedisKey     = "traefik:acme.json"
	defaultAcmeFile     = "/acme/acme.json"
	defaultDockerSocket = "/var/run/docker.sock"
	defaultSSLDir       = "/ssl"
	defaultPollInterval = time.Hour
	redisPubSubChannel  = "traefik:acme:updated"
	maxRetries          = 5
	initialRetryDelay   = time.Second
)

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("[cert-sync] ERROR: %s environment variable is required", key)
	}
	return v
}

// debugEnabled reports whether CERT_SYNC_DEBUG is set, checked live (rather
// than cached) since loadHAOptions can set it after process start.
func debugEnabled() bool {
	v, _ := strconv.ParseBool(os.Getenv("CERT_SYNC_DEBUG"))
	return v
}

func debugf(label, format string, args ...interface{}) {
	if debugEnabled() {
		log.Printf("[cert-sync:%s:debug] %s", label, fmt.Sprintf(format, args...))
	}
}

func newRedisClient() *redis.Client {
	host := getEnv("REDIS_HOST", defaultRedisHost)
	port := getEnv("REDIS_PORT", defaultRedisPort)
	password := os.Getenv("REDIS_PASSWORD")

	return redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%s", host, port),
		Password: password,
	})
}

// newDockerHTTPClient creates an HTTP client that talks to the Docker daemon
// via the Unix socket. This bypasses the moby SDK's minimum API version
// enforcement, allowing us to work with any Docker daemon version.
func newDockerHTTPClient() *http.Client {
	socketPath := getEnv("DOCKER_HOST", defaultDockerSocket)
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		Timeout: 60 * time.Second,
	}
}

// detectAPIVersion pings the Docker daemon and returns the API version
// it reports. Falls back to "1.24" if the ping fails.
func detectAPIVersion(client *http.Client) string {
	resp, err := client.Get("http://localhost/version")
	if err != nil {
		log.Printf("[cert-sync:client] WARNING: could not detect Docker API version: %v, falling back to 1.24", err)
		return "1.24"
	}
	defer resp.Body.Close()

	var info struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || info.APIVersion == "" {
		log.Printf("[cert-sync:client] WARNING: could not parse Docker version response, falling back to 1.24")
		return "1.24"
	}
	return info.APIVersion
}

func retryWithBackoff(ctx context.Context, operation func() error, operationName string) error {
	var err error
	delay := initialRetryDelay

	for attempt := 1; attempt <= maxRetries; attempt++ {
		err = operation()
		if err == nil {
			return nil
		}

		if attempt < maxRetries {
			log.Printf("[cert-sync] %s failed (attempt %d/%d): %v, retrying in %v...",
				operationName, attempt, maxRetries, err, delay)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				delay *= 2 // Exponential backoff
			}
		}
	}

	return fmt.Errorf("%s failed after %d attempts: %w", operationName, maxRetries, err)
}

// fetchFromRedis reads redisKey with retry, treating a missing key as "no
// update" (nil, nil) rather than an error.
func fetchFromRedis(ctx context.Context, rdb *redis.Client, redisKey, label string) ([]byte, error) {
	var value []byte
	err := retryWithBackoff(ctx, func() error {
		var err error
		value, err = rdb.Get(ctx, redisKey).Bytes()
		if err == redis.Nil {
			log.Printf("[cert-sync:%s] WARNING: no data found in Redis for key %s", label, redisKey)
			return nil
		}
		return err
	}, "Redis GET")
	return value, err
}

// runSyncDaemon runs the hourly fallback poll and pub/sub subscription loops
// shared by client and homeassistant modes, performing an initial sync on
// startup, then blocks until a shutdown signal is received.
func runSyncDaemon(ctx context.Context, cancel context.CancelFunc, rdb *redis.Client, label string, pollInterval time.Duration, sync func(forceRestart bool)) {
	startedAt := time.Now()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	forceRestart, _ := strconv.ParseBool(os.Getenv("CERT_SYNC_FORCE_RESTART"))
	debugf(label, "Starting sync daemon (pollInterval=%v, forceRestart=%v)", pollInterval, forceRestart)
	sync(forceRestart)

	// Hourly fallback poll
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				log.Printf("[cert-sync:%s] Polling Redis...", label)
				sync(false)
			}
		}
	}()

	// Subscribe for immediate notifications with reconnection logic
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("[cert-sync:%s] Subscribing to %s...", label, redisPubSubChannel)
				sub := rdb.Subscribe(ctx, redisPubSubChannel)

				func() {
					defer sub.Close()
					for {
						select {
						case <-ctx.Done():
							return
						case msg, ok := <-sub.Channel():
							if !ok {
								log.Printf("[cert-sync:%s] Subscription closed, reconnecting...", label)
								return
							}
							log.Printf("[cert-sync:%s] Received update notification (msg: %s), syncing...", label, msg.Payload)
							sync(false)
						}
					}
				}()

				// Wait before reconnecting
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}
	}()

	// Wait for shutdown signal
	sig := <-sigChan
	log.Printf("[cert-sync:%s] Received shutdown signal (%v) after %v uptime, exiting gracefully...", label, sig, time.Since(startedAt).Round(time.Second))
	cancel()
	time.Sleep(time.Second) // Give goroutines time to clean up
}

// --- Server ---

func runServer() {
	acmeFile := getEnv("TRAEFIK_ACME_FILE", defaultAcmeFile)
	redisKey := getEnv("REDIS_KEY", defaultRedisKey)

	rdb := newRedisClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	push := func() {
		// Small delay to handle atomic writes (write to temp, then rename)
		time.Sleep(100 * time.Millisecond)

		data, err := os.ReadFile(acmeFile)
		if err != nil {
			log.Printf("[cert-sync:server] WARNING: could not read %s: %v", acmeFile, err)
			return
		}

		// Retry Redis operations with backoff
		err = retryWithBackoff(ctx, func() error {
			if err := rdb.Set(ctx, redisKey, data, 0).Err(); err != nil {
				return err
			}
			return rdb.Publish(ctx, redisPubSubChannel, "1").Err()
		}, "Redis push")

		if err != nil {
			log.Printf("[cert-sync:server] ERROR: %v", err)
			return
		}

		log.Printf("[cert-sync:server] Pushed %s to Redis (%d bytes)", acmeFile, len(data))
	}

	// Push current state on startup
	push()

	// Watch for file changes
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("[cert-sync:server] ERROR: could not create watcher: %v", err)
	}
	defer watcher.Close()

	// Watch both the file and its directory to catch atomic writes
	if err := watcher.Add(acmeFile); err != nil {
		// If watching the file directly fails, watch the directory
		if err := watcher.Add(filepath.Dir(acmeFile)); err != nil {
			log.Fatalf("[cert-sync:server] ERROR: could not watch %s: %v", acmeFile, err)
		}
	}

	log.Printf("[cert-sync:server] Watching %s for changes...", acmeFile)

	for {
		select {
		case <-sigChan:
			log.Printf("[cert-sync:server] Received shutdown signal, exiting gracefully...")
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			// Handle both direct file events and directory events for the target file
			if event.Name == acmeFile || filepath.Base(event.Name) == filepath.Base(acmeFile) {
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
					log.Printf("[cert-sync:server] Change detected (%s), syncing...", event.Op)
					push()
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[cert-sync:server] WARNING: watcher error: %v", err)
		}
	}
}

// --- Client ---

func runClient() {
	acmeFile := getEnv("TRAEFIK_ACME_FILE", defaultAcmeFile)
	redisKey := getEnv("REDIS_KEY", defaultRedisKey)
	traefikContainer := requireEnv("TRAEFIK_CONTAINER")
	pollInterval := defaultPollInterval

	rdb := newRedisClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dockerHTTP := newDockerHTTPClient()

	// Detect the Docker daemon's API version via ping.
	dockerAPIVersion := detectAPIVersion(dockerHTTP)
	log.Printf("[cert-sync:client] Using Docker API version %s", dockerAPIVersion)

	restartTraefik := func() {
		err := retryWithBackoff(ctx, func() error {
			// POST to the Docker Engine API directly via the Unix socket.
			url := fmt.Sprintf("http://localhost/v%s/containers/%s/restart?t=30", dockerAPIVersion, traefikContainer)
			resp, err := dockerHTTP.Post(url, "", nil)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				return fmt.Errorf("unexpected status %d", resp.StatusCode)
			}
			return nil
		}, "Traefik restart")

		if err != nil {
			log.Printf("[cert-sync:client] ERROR: %v", err)
			return
		}
		log.Printf("[cert-sync:client] Restarted %s", traefikContainer)
	}

	sync := func(forceRestart bool) {
		value, err := fetchFromRedis(ctx, rdb, redisKey, "client")
		if err != nil {
			log.Printf("[cert-sync:client] ERROR: %v", err)
			return
		}
		if value == nil {
			return // No data in Redis
		}

		// Only update and restart if content changed
		current, err := os.ReadFile(acmeFile)
		if err == nil && string(current) == string(value) {
			if forceRestart {
				log.Printf("[cert-sync:client] acme.json unchanged, but force restart requested.")
				restartTraefik()
			} else {
				log.Printf("[cert-sync:client] acme.json unchanged, skipping restart.")
			}
			return
		}

		// Ensure directory exists
		if err := os.MkdirAll(filepath.Dir(acmeFile), 0755); err != nil {
			log.Printf("[cert-sync:client] ERROR: could not create directory: %v", err)
			return
		}

		if err := os.WriteFile(acmeFile, value, 0600); err != nil {
			log.Printf("[cert-sync:client] ERROR: could not write %s: %v", acmeFile, err)
			return
		}

		log.Printf("[cert-sync:client] Updated %s (%d bytes)", acmeFile, len(value))
		restartTraefik()
	}

	runSyncDaemon(ctx, cancel, rdb, "client", pollInterval, sync)
}

// --- Home Assistant ---

// acmeCertEntry is one entry in a Traefik ACME resolver's Certificates list.
type acmeCertEntry struct {
	Domain struct {
		Main string   `json:"main"`
		SANs []string `json:"sans,omitempty"`
	} `json:"domain"`
	Certificate string `json:"certificate"`
	Key         string `json:"key"`
}

type acmeResolverStore struct {
	Certificates []acmeCertEntry `json:"Certificates"`
}

// extractCertificate pulls a certificate/key pair out of a Traefik acme.json
// payload. If domain is empty, the first certificate found across all
// resolvers is used (matches the single-domain assumption the rest of
// cert-sync already makes).
func extractCertificate(data []byte, domain string) (cert, key []byte, err error) {
	var store map[string]acmeResolverStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, nil, fmt.Errorf("parse acme store: %w", err)
	}

	for _, resolver := range store {
		for _, entry := range resolver.Certificates {
			if domain != "" && entry.Domain.Main != domain {
				continue
			}

			certBytes, err := base64.StdEncoding.DecodeString(entry.Certificate)
			if err != nil {
				return nil, nil, fmt.Errorf("decode certificate: %w", err)
			}
			keyBytes, err := base64.StdEncoding.DecodeString(entry.Key)
			if err != nil {
				return nil, nil, fmt.Errorf("decode key: %w", err)
			}
			return certBytes, keyBytes, nil
		}
	}

	if domain != "" {
		return nil, nil, fmt.Errorf("no certificate found for domain %s", domain)
	}
	return nil, nil, fmt.Errorf("no certificate found in acme store")
}

func writeHACertificate(sslDir string, cert, key []byte) error {
	if err := os.MkdirAll(sslDir, 0755); err != nil {
		return fmt.Errorf("create ssl dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(sslDir, "fullchain.pem"), cert, 0644); err != nil {
		return fmt.Errorf("write fullchain.pem: %w", err)
	}
	if err := os.WriteFile(filepath.Join(sslDir, "privkey.pem"), key, 0600); err != nil {
		return fmt.Errorf("write privkey.pem: %w", err)
	}
	return nil
}

// restartWindow is a daily HH:MM-HH:MM range (host-local time) during which
// a pending Home Assistant Core restart is allowed to fire. It may wrap past
// midnight (e.g. 22:00-04:00).
type restartWindow struct {
	startMin, endMin int
}

func parseRestartWindow(s string) (*restartWindow, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("expected format HH:MM-HH:MM, got %q", s)
	}
	start, err := parseHHMM(parts[0])
	if err != nil {
		return nil, fmt.Errorf("invalid start time: %w", err)
	}
	end, err := parseHHMM(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid end time: %w", err)
	}
	return &restartWindow{startMin: start, endMin: end}, nil
}

func parseHHMM(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

func (w *restartWindow) contains(now time.Time) bool {
	m := now.Hour()*60 + now.Minute()
	if w.startMin <= w.endMin {
		return m >= w.startMin && m < w.endMin
	}
	return m >= w.startMin || m < w.endMin // wraps past midnight
}

// restartCore asks the Supervisor to restart Home Assistant Core. There is
// no certificate-reload API, so a full restart is the only way to pick up a
// renewed cert.
func restartCore(ctx context.Context) error {
	token := requireEnv("SUPERVISOR_TOKEN")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://supervisor/core/restart", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	debugf("homeassistant", "POST %s", req.URL)
	start := time.Now()
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		debugf("homeassistant", "Core restart request failed after %v: %v", time.Since(start).Round(time.Millisecond), err)
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	debugf("homeassistant", "Core restart responded %d in %v: %s", resp.StatusCode, time.Since(start).Round(time.Millisecond), string(body))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// haOptions mirrors the `options`/`schema` in home-assistant-apps'
// cert_sync/config.yaml. The
// Supervisor writes the user's configured values here before starting the
// container; apps without bashio read it directly instead of getting
// options as environment variables.
type haOptions struct {
	RedisHost     string `json:"redis_host"`
	RedisPort     int    `json:"redis_port"`
	RedisPassword string `json:"redis_password"`
	RedisKey      string `json:"redis_key"`
	Domain        string `json:"domain"`
	RestartWindow string `json:"restart_window"`
	Debug         bool   `json:"debug"`
}

// loadHAOptions reads /data/options.json and applies it on top of the
// CERT_SYNC_MODE=homeassistant environment variable defaults, since the
// Supervisor does not expose app options as environment variables.
func loadHAOptions(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[cert-sync:homeassistant] WARNING: could not read %s: %v", path, err)
		return
	}

	var opts haOptions
	if err := json.Unmarshal(data, &opts); err != nil {
		log.Printf("[cert-sync:homeassistant] WARNING: could not parse %s: %v", path, err)
		return
	}

	if opts.RedisHost != "" {
		os.Setenv("REDIS_HOST", opts.RedisHost)
	}
	if opts.RedisPort != 0 {
		os.Setenv("REDIS_PORT", strconv.Itoa(opts.RedisPort))
	}
	os.Setenv("REDIS_PASSWORD", opts.RedisPassword)
	if opts.RedisKey != "" {
		os.Setenv("REDIS_KEY", opts.RedisKey)
	}
	os.Setenv("CERT_SYNC_DOMAIN", opts.Domain)
	os.Setenv("CERT_SYNC_RESTART_WINDOW", opts.RestartWindow)
	os.Setenv("CERT_SYNC_DEBUG", strconv.FormatBool(opts.Debug))

	if opts.Debug {
		log.Printf("[cert-sync:homeassistant] Debug logging enabled (redis_host=%s redis_port=%s redis_key=%s domain=%s restart_window=%q)",
			getEnv("REDIS_HOST", defaultRedisHost), getEnv("REDIS_PORT", defaultRedisPort), getEnv("REDIS_KEY", defaultRedisKey), opts.Domain, opts.RestartWindow)
	}
}

func runHomeAssistant() {
	loadHAOptions(getEnv("CERT_SYNC_OPTIONS_FILE", "/data/options.json"))

	redisKey := getEnv("REDIS_KEY", defaultRedisKey)
	sslDir := getEnv("CERT_SYNC_SSL_DIR", defaultSSLDir)
	domain := os.Getenv("CERT_SYNC_DOMAIN")

	windowRaw := getEnv("CERT_SYNC_RESTART_WINDOW", "")
	window, err := parseRestartWindow(windowRaw)
	if err != nil {
		log.Fatalf("[cert-sync:homeassistant] ERROR: invalid CERT_SYNC_RESTART_WINDOW: %v", err)
	}

	rdb := newRedisClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var pendingRestart atomic.Bool
	var restartInFlight atomic.Bool

	applyRestart := func() {
		if !restartInFlight.CompareAndSwap(false, true) {
			log.Printf("[cert-sync:homeassistant] Restart already in progress, skipping duplicate request.")
			return
		}
		defer restartInFlight.Store(false)

		if err := restartCore(ctx); err != nil {
			log.Printf("[cert-sync:homeassistant] ERROR: restarting Core: %v", err)
			return
		}
		pendingRestart.Store(false)
		log.Printf("[cert-sync:homeassistant] Restarted Home Assistant Core")
	}

	scheduleRestart := func() {
		if window == nil {
			debugf("homeassistant", "no restart window configured, restarting immediately")
			applyRestart()
			return
		}
		pendingRestart.Store(true)
		log.Printf("[cert-sync:homeassistant] Restart deferred to window %s", windowRaw)
	}

	sync := func(forceRestart bool) {
		debugf("homeassistant", "sync() invoked (forceRestart=%v)", forceRestart)
		value, err := fetchFromRedis(ctx, rdb, redisKey, "homeassistant")
		if err != nil {
			log.Printf("[cert-sync:homeassistant] ERROR: %v", err)
			return
		}
		if value == nil {
			debugf("homeassistant", "no certificate data in Redis, nothing to do")
			return
		}

		cert, key, err := extractCertificate(value, domain)
		if err != nil {
			log.Printf("[cert-sync:homeassistant] ERROR: %v", err)
			return
		}

		existingCert, _ := os.ReadFile(filepath.Join(sslDir, "fullchain.pem"))
		if string(existingCert) == string(cert) {
			if forceRestart {
				log.Printf("[cert-sync:homeassistant] certificate unchanged, but force restart requested.")
				scheduleRestart()
			} else {
				log.Printf("[cert-sync:homeassistant] certificate unchanged, skipping restart.")
			}
			return
		}

		debugf("homeassistant", "certificate changed (%d bytes -> %d bytes), writing to %s", len(existingCert), len(cert), sslDir)
		if err := writeHACertificate(sslDir, cert, key); err != nil {
			log.Printf("[cert-sync:homeassistant] ERROR: %v", err)
			return
		}

		log.Printf("[cert-sync:homeassistant] Updated certificate in %s", sslDir)
		scheduleRestart()
	}

	// Apply a deferred restart once the configured window opens
	if window != nil {
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if pendingRestart.Load() && window.contains(time.Now()) {
						applyRestart()
					}
				}
			}
		}()
	}

	runSyncDaemon(ctx, cancel, rdb, "homeassistant", defaultPollInterval, sync)
}

// --- Healthcheck ---

func runHealthcheck(mode string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	debugf(mode, "Running --healthcheck")

	if mode == "homeassistant" {
		sslDir := getEnv("CERT_SYNC_SSL_DIR", defaultSSLDir)
		if _, err := os.Stat(filepath.Join(sslDir, "fullchain.pem")); err != nil {
			debugf(mode, "healthcheck failed: ssl cert: %v", err)
			fmt.Fprintf(os.Stderr, "ssl cert: %v\n", err)
			os.Exit(1)
		}
		if _, err := os.Stat(filepath.Join(sslDir, "privkey.pem")); err != nil {
			debugf(mode, "healthcheck failed: ssl key: %v", err)
			fmt.Fprintf(os.Stderr, "ssl key: %v\n", err)
			os.Exit(1)
		}
	} else {
		acmeFile := getEnv("TRAEFIK_ACME_FILE", defaultAcmeFile)
		if _, err := os.Stat(acmeFile); err != nil {
			debugf(mode, "healthcheck failed: acme file: %v", err)
			fmt.Fprintf(os.Stderr, "acme file: %v\n", err)
			os.Exit(1)
		}
	}

	rdb := newRedisClient()
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		debugf(mode, "healthcheck failed: redis: %v", err)
		fmt.Fprintf(os.Stderr, "redis: %v\n", err)
		os.Exit(1)
	}

	if mode == "client" {
		dockerHTTP := newDockerHTTPClient()
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://localhost/_ping", nil)
		resp, err := dockerHTTP.Do(req)
		if err != nil {
			debugf(mode, "healthcheck failed: docker: %v", err)
			fmt.Fprintf(os.Stderr, "docker: %v\n", err)
			os.Exit(1)
		}
		resp.Body.Close()
	}

	debugf(mode, "healthcheck passed")
	os.Exit(0)
}

// --- Main ---

func main() {
	mode := requireEnv("CERT_SYNC_MODE")

	if len(os.Args) > 1 && os.Args[1] == "--healthcheck" {
		runHealthcheck(mode)
	}

	switch mode {
	case "server":
		runServer()
	case "client":
		runClient()
	case "homeassistant":
		runHomeAssistant()
	default:
		log.Fatalf("[cert-sync] ERROR: CERT_SYNC_MODE must be 'server', 'client', or 'homeassistant', got '%s'", mode)
	}
}
