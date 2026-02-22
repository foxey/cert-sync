package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/moby/moby/client"
	"github.com/fsnotify/fsnotify"
	"github.com/redis/go-redis/v9"
)

const (
	defaultRedisHost      = "redis"
	defaultRedisPort      = "6379"
	defaultRedisKey       = "traefik:acme.json"
	defaultAcmeFile       = "/acme/acme.json"
	defaultPollInterval   = time.Hour
	redisPubSubChannel    = "traefik:acme:updated"
	maxRetries            = 5
	initialRetryDelay     = time.Second
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

func newRedisClient() *redis.Client {
	host := getEnv("REDIS_HOST", defaultRedisHost)
	port := getEnv("REDIS_PORT", defaultRedisPort)
	password := os.Getenv("REDIS_PASSWORD")

	return redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%s", host, port),
		Password: password,
	})
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

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("[cert-sync:client] ERROR: could not connect to Docker: %v", err)
	}
	defer dockerClient.Close()

	restartTraefik := func() {
		timeout := 30 // Increased timeout for larger instances
		err := retryWithBackoff(ctx, func() error {
			_, err := dockerClient.ContainerRestart(ctx, traefikContainer, client.ContainerRestartOptions{Timeout: &timeout})
			return err
		}, "Traefik restart")

		if err != nil {
			log.Printf("[cert-sync:client] ERROR: %v", err)
			return
		}
		log.Printf("[cert-sync:client] Restarted %s", traefikContainer)
	}

	sync := func() {
		var value []byte
		err := retryWithBackoff(ctx, func() error {
			var err error
			value, err = rdb.Get(ctx, redisKey).Bytes()
			if err == redis.Nil {
				log.Printf("[cert-sync:client] WARNING: no data found in Redis for key %s", redisKey)
				return nil // Don't retry for missing key
			}
			return err
		}, "Redis GET")

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
			log.Printf("[cert-sync:client] acme.json unchanged, skipping restart.")
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

	// Initial sync on startup
	sync()

	// Hourly fallback poll
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				log.Printf("[cert-sync:client] Polling Redis...")
				sync()
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
				log.Printf("[cert-sync:client] Subscribing to %s...", redisPubSubChannel)
				sub := rdb.Subscribe(ctx, redisPubSubChannel)
				
				func() {
					defer sub.Close()
					for {
						select {
						case <-ctx.Done():
							return
						case msg, ok := <-sub.Channel():
							if !ok {
								log.Printf("[cert-sync:client] Subscription closed, reconnecting...")
								return
							}
							log.Printf("[cert-sync:client] Received update notification (msg: %s), syncing...", msg.Payload)
							sync()
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
	<-sigChan
	log.Printf("[cert-sync:client] Received shutdown signal, exiting gracefully...")
	cancel()
	time.Sleep(time.Second) // Give goroutines time to clean up
}

// --- Main ---

func main() {
	mode := requireEnv("CERT_SYNC_MODE")

	switch mode {
	case "server":
		runServer()
	case "client":
		runClient()
	default:
		log.Fatalf("[cert-sync] ERROR: CERT_SYNC_MODE must be 'server' or 'client', got '%s'", mode)
	}
}