# Changelog

## 1.0.2
- Fix a race condition in `homeassistant` mode where concurrent restart triggers (hourly poll, pubsub notification, deferred-restart window) could call the Supervisor restart API twice in parallel, surfacing as `context deadline exceeded` / `unexpected status 400` errors.

## 1.0.1
- No functional changes. Version bump used to test the automated cert-sync → home-assistant-apps version-sync workflow end to end.

## 1.0.0
- Initial release: syncs an ACME certificate from Redis directly into Home Assistant Core, with an optional daily restart window (`restart_window` option).
