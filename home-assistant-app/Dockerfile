# Build stage -- runs on the build host's native platform (amd64 on GitHub
# Actions runners) and cross-compiles to the target platform. Go's toolchain
# cross-compiles natively (no emulation needed), unlike running the whole
# build under QEMU for a foreign TARGETPLATFORM.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o cert-sync .

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
