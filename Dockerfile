# syntax=docker/dockerfile:1

ARG VERSION=dev

# Builder stage
FROM golang:1.23-alpine AS builder

ARG VERSION=dev

# Install git for go mod download
RUN apk add --no-cache git

WORKDIR /build

# Copy go mod files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build with CGO disabled for static binary
# GOGC=50 reduces memory usage during compilation
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s -X main.version=${VERSION}" -o opencode-router .

# Runner stage
FROM gcr.io/distroless/static-debian12:latest

WORKDIR /

# Copy binary from builder
COPY --from=builder /build/opencode-router /opencode-router

# Use nonroot user (UID 65534) which already exists in distroless
USER nonroot:nonroot

LABEL org.opencontainers.image.title="opencode-smart-router" \
      org.opencontainers.image.description="Lightweight HTTP proxy for rotating OpenCode Go API keys" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/puzzithinker/opencode-smart-router" \
      org.opencontainers.image.vendor="puzzithinker"

# Expose the default port
EXPOSE 8080

# GOGC=50 is recommended for memory tuning in constrained environments
# Example: docker run -e GOGC=50 opencode-router

ENTRYPOINT ["/opencode-router"]