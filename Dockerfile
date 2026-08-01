# Coves AppView - Multi-stage Dockerfile
# Builds a minimal production image for the Go server

# Stage 1: Build
FROM golang:1.25-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Set working directory
WORKDIR /build

# Copy go mod files first (better caching)
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
# CGO_ENABLED=0 for static binary (no libc dependency)
# -ldflags="-s -w" strips debug info for smaller binary
#
# GOARCH and BUILD_TAGS default to the production values, so an unparameterised
# `docker build` still cross-compiles for the amd64 deploy target exactly as
# before. docker-compose.ci.yml overrides both: CI needs the host's native
# architecture (an emulated amd64 binary on an arm64 dev machine makes every
# test run drastically slower) and -tags dev, which is what `make run` uses and
# what the localhost OAuth resolvers live behind.
ARG GOARCH=amd64
ARG BUILD_TAGS=""
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${GOARCH} go build \
    -tags "${BUILD_TAGS}" \
    -ldflags="-s -w" \
    -o /build/coves-server \
    ./cmd/server

# Stage 2: Runtime
FROM alpine:3.19

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Create non-root user for security
RUN addgroup -g 1000 coves && \
    adduser -u 1000 -G coves -s /bin/sh -D coves

# Set working directory
WORKDIR /app

# Copy binary from builder
# Migrations are embedded in the binary (see internal/db/migrations/embed.go),
# so there is no migrations directory to copy. Note the static assets below are
# still resolved relative to WORKDIR.
COPY --from=builder /build/coves-server /app/coves-server

# Copy static assets (images, etc. for the web interface)
COPY --from=builder /build/static /app/static

# Set ownership
RUN chown -R coves:coves /app

# Switch to non-root user
USER coves

# Expose port
EXPOSE 8080

# Health check
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget --spider -q http://localhost:8080/xrpc/_health || exit 1

# Run the server
ENTRYPOINT ["/app/coves-server"]
