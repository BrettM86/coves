# Coves AppView - Multi-stage Dockerfile
# Builds a minimal production image for the Go server

# Stage 1: Build
FROM golang:1.24-alpine AS builder

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
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
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
COPY --from=builder /build/coves-server /app/coves-server

# Copy migrations (needed for goose)
# Must maintain path structure as app looks for internal/db/migrations
COPY --from=builder /build/internal/db/migrations /app/internal/db/migrations

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
