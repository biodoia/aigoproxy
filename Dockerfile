# syntax=docker/dockerfile:1.7
# Multi-stage build for aigoproxy: tiny final image (~15MB).

# === Stage 1: build ===
FROM golang:1.25-alpine AS build
RUN apk add --no-cache git ca-certificates
WORKDIR /src

# Cache deps separately
COPY go.mod go.sum ./
RUN go mod download

# Copy sources
COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" \
    -o /out/aigoproxy \
    ./cmd/aigoproxy

# === Stage 2: runtime ===
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/aigoproxy /usr/local/bin/aigoproxy
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# aigoproxy binds :80 (and optionally :443) as non-root via AmbientCapabilities
# Distroless runs as uid 65532, so we need a different approach: run with
# setcap to grant CAP_NET_BIND_SERVICE, or just run as root in the container.
# We default to root and let the operator secure it.

USER root
EXPOSE 80 443
VOLUME ["/data"]
ENV AIGOPROXY_DATA=/data

ENTRYPOINT ["/usr/local/bin/aigoproxy"]
CMD ["-addr", ":80", "-data", "/data"]
