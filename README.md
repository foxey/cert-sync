# cert-sync

ACME certificate synchronization over Redis for distributed Traefik deployments.

## Overview

cert-sync enables automatic certificate synchronization across multiple Traefik instances using Redis as a message broker. When Traefik generates or renews ACME certificates, the server mode watches for changes and pushes them to Redis. Client instances subscribe to updates and automatically restart their Traefik containers with the new certificates.

Communicates with the Docker daemon directly via the Unix socket, auto-detecting the API version at startup. This makes it compatible with a wide range of Docker versions without requiring the Docker CLI or SDK.

## Modes

### Server Mode
Watches the ACME certificate file for changes and pushes updates to Redis with pub/sub notifications.

### Client Mode
Subscribes to Redis for certificate updates, syncs the local ACME file, and restarts the Traefik container. Includes hourly polling as a fallback mechanism.

## Configuration

### Environment Variables

**Required:**
- `CERT_SYNC_MODE` - Operation mode: `server` or `client`
- `TRAEFIK_CONTAINER` - Docker container name to restart (client mode only)

**Optional:**
- `REDIS_HOST` - Redis hostname (default: `redis`)
- `REDIS_PORT` - Redis port (default: `6379`)
- `REDIS_PASSWORD` - Redis password (default: none)
- `REDIS_KEY` - Redis key for certificate storage (default: `traefik:acme.json`)
- `TRAEFIK_ACME_FILE` - Path to ACME certificate file (default: `/acme/acme.json`)
- `DOCKER_HOST` - Path to Docker socket (default: `/var/run/docker.sock`)
- `CERT_SYNC_FORCE_RESTART` - Force a Traefik restart on startup even if the certificate file is unchanged. Accepts `1`, `true`, `yes` (and uppercase variants). Useful after deploying a new cert-sync version or when Traefik needs a restart for other reasons.

## Usage

### Docker Compose Example

```yaml
services:
  # Server instance (where Traefik generates certificates)
  cert-sync-server:
    image: cert-sync:latest
    environment:
      CERT_SYNC_MODE: server
      REDIS_HOST: redis
      TRAEFIK_ACME_FILE: /acme/acme.json
    volumes:
      - traefik-acme:/acme:ro
    depends_on:
      - redis

  # Client instance (receives certificate updates)
  cert-sync-client:
    image: cert-sync:latest
    environment:
      CERT_SYNC_MODE: client
      REDIS_HOST: redis
      TRAEFIK_CONTAINER: traefik
      TRAEFIK_ACME_FILE: /acme/acme.json
    volumes:
      - traefik-acme:/acme
      - /var/run/docker.sock:/var/run/docker.sock
    depends_on:
      - redis

  redis:
    image: redis:alpine
```

## Building

```bash
docker build -t cert-sync:latest .
```

## License

See LICENSE file for details.
