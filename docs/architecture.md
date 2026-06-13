# Architecture: opencode-smart-router

## Project Overview

opencode-smart-router is a lightweight, deterministic HTTP proxy written in Go. It sits between AI coding agents (like Hermes Agent) and the OpenCode Go API, rotating multiple API keys automatically so the agent only needs to talk to a single local endpoint.

When a key fails, hits a rate limit, or returns an authentication error, the router transparently retries with the next available key. The caller never sees the retry, only the final success or a clean error response.

The project is built as a single static binary with one external dependency (Prometheus client library). It uses Go 1.22+ features including `log/slog` for structured logging and `httputil.ReverseProxy` with the `Rewrite` API. It is designed to run on resource-constrained hardware like a Raspberry Pi 4.

---

## Architecture Overview

### Single-Binary Design

All logic lives in `main.go`. The file is organized into clearly marked sections:

1. **Config** — JSON parsing, defaults, environment overrides, validation
2. **Key State Machine** — `KeyEntry`, `KeyRotator`, state transitions
3. **Proxy** — `httputil.ReverseProxy`, buffered response writer, transparent retry loop
4. **Error Classification** — `classifyResponse`, status code mapping to retry decisions
5. **Middleware** — Basic auth for admin endpoints
6. **Handlers** — `/health`, `/admin/stats`
7. **Metrics** — Prometheus counters, gauges, and histograms
8. **Logging** — Structured logging with `log/slog`, JSON-compatible output to stdout or file
9. **Custom Errors** — `KeyError`, `UpstreamError`, `ConfigError` for typed error handling
9. **OpenAI Error Format** — Standard error response shape
10. **Main** — Wiring, signal handling, graceful shutdown

This single-file approach keeps the project easy to understand, build, and deploy. There are no internal packages or complex abstractions.

### Request Flow

```
Client (Hermes Agent)
    |
    v
/v1/*  -->  proxyHandler
                |
                v
        Pick key from KeyRotator
                |
                v
        httputil.ReverseProxy
                |
                v
        classifyResponse (ModifyResponse)
                |
                +-- ShouldRetry? --> Pick next key, loop
                |
                +-- Success? --> Write buffered response to client
```

---

## Key Rotation

### KeyState Machine

Each API key is wrapped in a `KeyEntry` with three possible states:

| State | Meaning | Transition Trigger |
|-------|---------|-------------------|
| `HEALTHY` | Key is available for use | Default state; entered after cooldown expires or on success |
| `COOLDOWN` | Key is temporarily paused | Entered on 429 rate limit or transport timeout |
| `DISABLED` | Key is permanently removed from rotation | Entered on 401, 403, or 429 with `insufficient_quota` |

Transitions:

- `HEALTHY --> COOLDOWN`: Rate limit (429) or timeout
- `COOLDOWN --> HEALTHY`: Cooldown period expires (checked at pick time)
- `HEALTHY --> DISABLED`: Authentication failure (401/403) or quota exhaustion
- `COOLDOWN --> DISABLED`: Never happens directly; only via HEALTHY

### Selection Strategies

Two strategies are implemented in `PickKey()`:

**round_robin**

An atomic counter increments on every call. The rotator starts at the counter position and scans forward, skipping `DISABLED` keys and keys still in `COOLDOWN`. The first available key is selected, its usage count increments, and the counter advances. This gives fair distribution across all healthy keys.

**least_used**

The rotator scans all keys and picks the one with the lowest `UsageCount` that is not `DISABLED` or in active `COOLDOWN`. This naturally balances load toward underutilized keys, which is useful when keys have different rate limits.

If no key is available, `PickKey()` returns an error and the handler returns a 429 or auth error to the client.

---

## Reverse Proxy

The router uses Go's standard `httputil.ReverseProxy` with the modern `Rewrite` API (not the deprecated `Director`).

### Rewrite Function

For every outgoing request, the rewrite handler:

1. Sets the upstream URL to the configured OpenCode Go endpoint
2. Sets `X-Forwarded-*` headers
3. Strips hop-by-hop headers (`Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `Transfer-Encoding`, `Upgrade`)
4. Injects the selected API key as `Authorization: Bearer <raw_key>` from request context

### ModifyResponse

`classifyResponse` runs after the upstream responds. It inspects the status code, updates the key state, records metrics, and writes a `ClassificationResult` into the request context. This result tells the outer handler whether to retry or forward the response.

### ErrorHandler

`proxyErrorHandler` catches transport-level failures (connection refused, DNS errors, timeouts). If the error contains "timeout" or "deadline exceeded", the key is moved to `COOLDOWN` for 10 seconds. Transport errors are not retried, the client receives a 502 Bad Gateway with an OpenAI-formatted error body.

---

## Transparent Retry Mechanism

The retry logic lives entirely inside `proxyHandler`. It is transparent to the client, meaning the caller sees only one request and one response, even if multiple keys were tried internally.

### How It Works

1. **Buffer the request body**: The handler reads the entire request body into memory so it can be replayed for each attempt.

2. **Determine max retries**: `maxRetries` equals the total number of configured keys. In the worst case, every key is tried once.

3. **Attempt loop**: For each attempt:
   - Pick the next available key
   - Clone the request and attach the key to context
   - Create a `bufferedResponseWriter` to capture the upstream response
   - Call `rp.ServeHTTP(buf, newReq)`
   - Check the `ClassificationResult` from `ModifyResponse`
   - If `ShouldRetry` is true, discard the buffer and continue to the next key
   - If `ShouldRetry` is false, copy the buffered response to the real `ResponseWriter` and return

4. **Exhaustion**: If all keys are tried and all trigger retries, the handler returns a 429 (or 401/403 if the last failures were auth errors) with an OpenAI-formatted error.

### Buffered Response Writer

`bufferedResponseWriter` implements `http.ResponseWriter`. It captures headers, status code, and body in memory instead of writing to the client. If the attempt succeeds or produces a non-retryable error, `writeTo()` copies everything to the real response writer. If the attempt should be retried, the buffer is simply discarded.

---

## Error Classification

The `classifyResponse` function maps upstream status codes to actions:

| Status Code | Action | Retry? | Key State Change |
|-------------|--------|--------|-----------------|
| 2xx | Forward to client | No | Mark `HEALTHY` |
| 401 / 403 | Auth failure | Yes (next key) | Mark `DISABLED` |
| 429 + `insufficient_quota` | Quota exhausted | Yes (next key) | Mark `DISABLED` |
| 429 (other) | Rate limited | Yes (next key) | Mark `COOLDOWN` with `Retry-After` or default duration |
| 5xx | Upstream error | No | None (forward error to client) |
| Timeout / transport error | Network issue | No | Mark `COOLDOWN` for 10 seconds |
| Other 4xx | Client error | No | None (forward error to client) |

### Retry-After Parsing

For 429 responses, the router parses the `Retry-After` header. It supports both integer seconds and HTTP-date formats. If the header is missing or invalid, it falls back to `cooldown_seconds` from config.

### OpenAI-Compatible Errors

When the router itself returns an error (all keys exhausted, request body read failure), it writes a JSON body matching the OpenAI error format:

```json
{
  "error": {
    "message": "...",
    "type": "server_error",
    "code": "all_keys_exhausted"
  }
}
```

---

## Admin and Health Endpoints

### `/health`

Returns the router's own health and upstream connectivity status.

- Finds the first `HEALTHY` or expired `COOLDOWN` key
- Makes a real GET request to `<upstream_url>/v1/models` using that key
- Returns JSON with `status`, `upstream`, `healthy_keys`, `total_keys`, `disabled_keys`
- HTTP 200 if upstream is reachable, HTTP 503 if not

This is not a simple connection test. It validates that at least one key can successfully talk to the OpenCode Go API.

### `/admin/stats`

Protected by basic auth. Returns a JSON snapshot of all keys:

- `masked_key`: The masked key identifier
- `state`: `healthy`, `cooldown`, or `disabled`
- `usage_count`: Total times the key was selected
- `last_used`: RFC3339 timestamp or `null`

Also returns `total_requests` (sum of all usage counts) and `strategy`.

### Basic Auth Middleware

`basicAuthMiddleware` guards `/admin/stats`. If `admin_pass` is empty in config, admin endpoints return 403 with the message "admin endpoints disabled". Otherwise, it requires a valid `Authorization: Basic ...` header matching `admin_user` and `admin_pass`.

---

## Prometheus Metrics

Metrics are optional and controlled by `enable_prometheus` in config. When enabled, four metrics are registered at startup and exposed at `/metrics` via `promhttp.Handler()`.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `opencode_router_requests_total` | Counter | `key`, `status_group` | Total requests per key, grouped by status |
| `opencode_router_key_usage_total` | Counter | `key` | Times each key was selected by the rotator |
| `opencode_router_key_healthy` | Gauge | `key` | 1 if healthy, 0 if cooldown or disabled |
| `opencode_router_request_duration_seconds` | Histogram | `key` | Request latency distribution using default Prometheus buckets |

### Status Grouping

Status codes are collapsed into buckets for the counter label:

- `2xx` for 200-299
- `3xx` for 300-399
- `4xx` for 400-499
- `5xx` for 500+
- `other` for everything else

This keeps cardinality low while still distinguishing success from failure classes.

### Runtime Enable/Disable

If `enable_prometheus` is false, no metrics are registered and `/metrics` is not mounted. The Prometheus client library is still compiled into the binary, but it consumes no resources at runtime. See [docs/prometheus-monitoring.md](prometheus-monitoring.md) for complete setup instructions including Grafana dashboards and alerting rules.

### Structured Logging

The router uses Go's `log/slog` package for structured logging. All log entries use key-value pairs for machine-parseable output:

```
time=2026-06-13T12:00:00.000Z level=INFO msg=key_selected key=sk-ab1...xyz strategy=round_robin attempt=1
time=2026-06-13T12:00:01.000Z level=INFO msg=key_cooldown key=sk-ab1...xyz duration=30s
time=2026-06-13T12:00:02.000Z level=INFO msg=key_disabled key=sk-ab1...xyz
time=2026-06-13T12:00:03.000Z level=INFO msg=transparent_retry key=sk-ab1...xyz status=429 attempt=2
time=2026-06-13T12:00:04.000Z level=INFO msg=request_forwarded key=sk-cd2...lmn status=200
```

When `enable_logging` is true and `log_file` is set, output goes to the specified file. Otherwise, output goes to stdout with the `slog.TextHandler`.

Log events: `key_selected`, `key_cooldown`, `key_disabled`, `key_recovered`, `request_forwarded`, `transparent_retry`, `startup`, `listening`, `shutdown`.

### Version Injection

The binary includes a `version` variable injected at build time via `-ldflags`:

```bash
make build  # uses git describe --tags --always or "dev"
VERSION=v1.0.0 make build  # explicit version
```

The version is logged at startup:

```
level=INFO msg=startup keys=3 strategy=round_robin listen=127.0.0.1:8080 upstream=https://opencode.ai/zen/go
level=INFO msg=startup version=v1.0.0
```

### Custom Error Types

The router defines typed errors for common failure modes:

- `KeyError` — key rotation failures (unavailable keys, disabled state)
- `UpstreamError` — upstream connection failures (wrapped original error)
- `ConfigError` — configuration parsing and validation failures

---

## Configuration

Configuration is loaded from a JSON file with environment variable overrides.

### Config File

Default path is `config.json`. Override with `OPENCODE_CONFIG` environment variable.

### Config Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `listen_addr` | string | `0.0.0.0:8080` | HTTP bind address. Use `127.0.0.1` for localhost-only, `0.0.0.0` for Docker |
| `upstream_url` | string | `https://opencode.ai/zen/go` | OpenCode Go API base URL |
| `keys` | []string | (required) | API keys to rotate |
| `strategy` | string | `round_robin` | `round_robin` or `least_used` |
| `cooldown_seconds` | int | `60` | Default cooldown duration |
| `health_check_timeout_seconds` | int | `10` | Timeout for upstream health probe |
| `admin_user` | string | `admin` | Basic auth username for admin endpoints |
| `admin_pass` | string | `""` | Basic auth password. Empty string disables admin endpoints |
| `enable_prometheus` | bool | `false` | Enable `/metrics` endpoint |
| `enable_logging` | bool | `false` | Enable file logging |
| `log_file` | string | `""` | Log file path (stdout if empty) |

### Environment Overrides

- `OPENCODE_KEYS`: Comma-separated list of keys. Overrides `keys` from config file. This is the preferred way to inject secrets in Docker or systemd environments.
- `OPENCODE_CONFIG`: Path to config file. Overrides the default `config.json`.

### Validation

`LoadConfig` validates that:
- At least one key is provided (via file or environment)
- `strategy` is either `round_robin` or `least_used`

If validation fails, the process exits with a fatal error before starting the server.

---

## Deployment

### Docker

The `Dockerfile` uses a multi-stage build:

1. **Builder**: `golang:1.23-alpine` compiles a static binary with `CGO_ENABLED=0` and `-ldflags="-w -s -X main.version=${VERSION}"`
2. **Runner**: `gcr.io/distroless/static-debian12:latest` runs the binary with no shell, no package manager, and minimal attack surface

The container runs as the `nonroot` user (UID 65534), which is built into the distroless image. Port 8080 is exposed.

`docker-compose.yml` adds:
- `127.0.0.1:8080:8080` binding (localhost only)
- `restart: unless-stopped`
- Resource limits: 256 MB memory, 0.5 CPU
- Optional Prometheus service (commented out)

### Systemd

`deploy/systemd/opencode-router.service` registers the Docker Compose project as a systemd unit:

- `Type=oneshot` with `RemainAfterExit=yes`
- `ExecStart` runs `docker compose up -d`
- `ExecStop` runs `docker compose down`
- Depends on `docker.service`

Install by copying the unit file to `/etc/systemd/system/` and running `systemctl enable --now opencode-router`.

### Resource Limits

The project is tuned for low-resource environments:

- Static binary with no CGO
- Distroless runtime image (no OS bloat)
- Docker Compose memory cap at 256 MB
- Optional `GOGC=50` for reduced GC pressure
- Optional features (Prometheus, file logging) disabled by default

---

## File Structure

```
opencode-smart-router/
├── main.go                          # All business logic (~970 lines)
├── main_test.go                     # Unit and integration tests (49 tests)
├── go.mod                           # Go module definition (Go 1.22, Prometheus client)
├── go.sum                           # Dependency checksums
├── Makefile                         # Build, cross-compile (arm64), Docker, test targets
├── Dockerfile                       # Multi-stage build, distroless, non-root
├── docker-compose.yml               # Compose with resource limits
├── .dockerignore                    # Build context exclusions
├── config.example.json              # Minimal config template
├── plan.md                          # Original requirements and design notes
├── architecture.md                  # Original architecture overview
├── deploy/
│   └── systemd/
│       └── opencode-router.service  # Systemd unit for Docker Compose
├── examples/
│   └── config.json                  # Realistic example with 3 keys
└── docs/
    ├── architecture.md              # This document
    ├── bugs-fixed.md                # All bugs found and fixed during development
    ├── prometheus-monitoring.md     # Prometheus + Grafana setup guide
    └── raspberry-pi-deployment.md   # RPi 4 deployment guide
```

---

## Key Masking

API keys are never logged or returned in API responses in their raw form. The `MaskKey` function produces a safe identifier:

- Keys longer than 8 characters: first 5 + `...` + last 3 (e.g., `sk-ab1...xyz`)
- Keys 4-8 characters: first 3 + `***`
- Keys 3 or fewer: key + `***`

Masked keys are used:
- In log events (`key_selected`, `key_cooldown`, `key_disabled`, `key_recovered`)
- In Prometheus metric labels
- In the `/admin/stats` JSON response

The raw key only appears in two places:
1. The `Authorization: Bearer <raw_key>` header sent to the upstream
2. The health check probe sent to `/v1/models`

---

## Security Considerations

### Key Handling

- Keys are stored in memory as `RawKey` inside `KeyEntry` structs
- Keys are injected via environment variable (`OPENCODE_KEYS`) in production to avoid committing secrets to disk
- Keys are masked in all logs and admin responses
- There is no key rotation or encryption at rest, the project assumes the host is trusted

### Admin Authentication

- Admin endpoints (`/admin/stats`) require HTTP Basic Auth
- If `admin_pass` is empty, admin endpoints are completely disabled (return 403)
- There is no session management, CSRF protection, or rate limiting on admin endpoints
- Admin endpoints should not be exposed to the public internet

### Container Security

- Docker image runs as non-root (`nonroot:nonroot`, UID 65534)
- Distroless base image has no shell, no package manager, and minimal system libraries
- Only port 8080 is exposed
- The binary is statically linked with `CGO_ENABLED=0`

### Network Security

- Default `listen_addr` is `127.0.0.1:8080`, not `0.0.0.0`
- Docker Compose binds to `127.0.0.1:8080` by default
- No TLS termination is built in, use a reverse proxy (nginx, traefik) for HTTPS

---

## Graceful Shutdown

The main goroutine blocks on `os.Interrupt` or `syscall.SIGTERM`. When a signal is received:

1. A 10-second context timeout is created
2. `http.Server.Shutdown()` drains active connections
3. The log file is closed if file logging was enabled
4. The process exits

In-flight requests are allowed to complete within the timeout window. New requests are rejected with "server closed".
