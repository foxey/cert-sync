# Home Assistant

Installs an ACME certificate, synced from a `cert-sync` server (running
alongside Traefik elsewhere) over Redis, directly for Home Assistant Core.

## How it works

1. A `cert-sync` server instance watches Traefik's `acme.json` and pushes it
   to Redis whenever the certificate changes.
2. This app subscribes to the same Redis instance, extracts the certificate
   and private key, and writes them to `/ssl/fullchain.pem` and
   `/ssl/privkey.pem`.
3. Home Assistant Core needs to be told to use that certificate — add to
   `configuration.yaml`:

   ```yaml
   http:
     ssl_certificate: /ssl/fullchain.pem
     ssl_key: /ssl/privkey.pem
   ```

4. There is no certificate hot-reload in Home Assistant Core, so after
   installing a changed certificate this app restarts Core via the
   Supervisor API. Set `restart_window` to defer that restart to a quiet
   time (e.g. overnight) instead of restarting the moment a renewed
   certificate arrives.

## Options

- `redis_host` / `redis_port` / `redis_password` — how to reach the Redis
  instance the `cert-sync` server publishes to.
- `redis_key` — the Redis key the server stores `acme.json` under. Must
  match the server's `REDIS_KEY` (default `traefik:acme.json`).
- `domain` — which domain's certificate to use, if the ACME store ever
  contains more than one. Leave blank to use whichever certificate is
  found first (fine as long as there is only one).
- `restart_window` — a daily local-time range, e.g. `02:00-04:00`, during
  which a pending Core restart is allowed to fire. May wrap past midnight
  (e.g. `22:00-04:00`). Leave blank to restart immediately when the
  certificate changes.

## Requirements

- The domain your Home Assistant instance is reached on must match the
  certificate Traefik issued (e.g. a wildcard like `*.app.example.com`
  covers `homeassistant.app.example.com`).
