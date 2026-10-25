# Changelog

## 1.0.4
- Fix the actual root cause of the recurring "stuck in starting" / repeated
  restart issue: Docker's `HEALTHCHECK` runs `--healthcheck` as a separate
  process from the main daemon, so it never inherited the Redis connection
  settings the daemon derives from `/data/options.json` (only set via
  `os.Setenv` on the daemon's own process). The healthcheck fell back to
  the default Redis host, failed to connect, and Supervisor's watchdog
  killed/restarted the container in a loop — independent of any actual
  certificate or restart activity. Fixed by having `--healthcheck` read
  `/data/options.json` itself before running its checks.

## 1.0.3
- Add a `debug` option (config form toggle) that enables verbose logging:
  sync() decision tracing, subscribe/healthcheck tracing, and the full
  request/response (including body) for Core restart calls.
- Unconditionally (no flag needed) log the shutdown signal name and process
  uptime on every shutdown — useful on its own for spotting whether
  something external is killing the container on a fixed cadence.

## 1.0.2
- Fix a race condition in `homeassistant` mode where concurrent restart triggers (hourly poll, pubsub notification, deferred-restart window) could call the Supervisor restart API twice in parallel, surfacing as `context deadline exceeded` / `unexpected status 400` errors.

## 1.0.1
- No functional changes. Version bump used to test the automated cert-sync → home-assistant-apps version-sync workflow end to end.

## 1.0.0
- Initial release: syncs an ACME certificate from Redis directly into Home Assistant Core, with an optional daily restart window (`restart_window` option).
