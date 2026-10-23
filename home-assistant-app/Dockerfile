# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o cert-sync .

# Final stage
FROM ghcr.io/home-assistant/base:3.24

COPY --from=builder /build/cert-sync /cert-sync
# Needed if Redis is configured with TLS
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

ENV CERT_SYNC_MODE=homeassistant

HEALTHCHECK CMD ["/cert-sync", "--healthcheck"]
ENTRYPOINT ["/cert-sync"]

ARG BUILD_VERSION
ARG BUILD_ARCH
LABEL \
  io.hass.version=${BUILD_VERSION} \
  io.hass.type="app" \
  io.hass.arch="${BUILD_ARCH}"
