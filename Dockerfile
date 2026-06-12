# syntax=docker/dockerfile:1

# Builder stage
FROM golang:1.22-alpine AS builder

# Install git for go mod download
RUN apk add --no-cache git

WORKDIR /build

# Copy go mod files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build with CGO disabled for static binary
# GOGC=50 reduces memory usage during compilation
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o opencode-router .

# Runner stage
FROM gcr.io/distroless/static-debian12:latest

WORKDIR /

# Copy binary from builder
COPY --from=builder /build/opencode-router /opencode-router

# Use nonroot user (UID 65534) which already exists in distroless
USER nonroot:nonroot

# Expose the default port
EXPOSE 8080

# GOGC=50 is recommended for memory tuning in constrained environments
# Example: docker run -e GOGC=50 opencode-router

ENTRYPOINT ["/opencode-router"]
