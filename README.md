# opencode-smart-router

A lightweight, deterministic HTTP proxy for rotating multiple OpenCode Go API keys.

![Go Version](https://img.shields.io/badge/go-1.22+-blue)
![License](https://img.shields.io/badge/license-MIT-green)

## Overview

opencode-smart-router sits between AI coding agents (like Hermes Agent) and the OpenCode Go API. It rotates multiple API keys automatically so the agent only needs to talk to a single local endpoint.

When a key fails, hits a rate limit, or returns an authentication error, the router transparently retries with the next available key. The caller never sees the retry, only the final success or a clean error response.

The project ships as a single static binary with one external dependency (Prometheus client library). It uses structured logging (`slog`) for JSON-compatible output and supports build-time version injection. It is designed to run on resource-constrained hardware like a Raspberry Pi 4.

## Features

- **Round robin and least-used key rotation** with automatic failover
- **Transparent retry** on 429, 401, and 403 responses, plus failover on 5xx and timeouts with escalating backoff cooldown
- **SSE streaming support** — `text/event-stream` responses pass through with flushing, no buffering
- **Key state machine** that tracks healthy, cooldown, and disabled keys
- **Real health check** that probes the upstream API with a live key
- **Split health/readiness probes** — liveness (`/health`) is free; readiness (`/ready`) is cached
- **Request body size limiting** to prevent memory exhaustion
- **Upstream timeout** with configurable header response timeout
- **Admin stats endpoint** with basic auth for key monitoring
- **Prometheus metrics** for requests, key usage, health, and latency
- **Graceful shutdown** with in-flight request draining
- **Structured logging** with `slog` (JSON-compatible, file or stdout)
- **Build-time version injection** via `-ldflags`
- **Docker and systemd** deployment ready

## Quick Start

### Local Development

Build the binary:

```bash
make build
```

To include a version string:

```bash
VERSION=v1.0.0 make build
./bin/opencode-router
# Output includes: level=INFO msg=startup version=v1.0.0
```

Without `VERSION`, the binary defaults to the git commit hash or `dev`.

Set your API keys and run:

```bash
export OPENCODE_KEYS="sk-opencode-go-key1,sk-opencode-go-key2,sk-opencode-go-key3"
./bin/opencode-router
```

The router listens on `127.0.0.1:8080` by default.

### Docker

```bash
docker compose up -d --build
```

This starts the router and Prometheus. Grafana runs on demand via its compose profile (saves ~170 MB RAM on a Pi 4):

```bash
docker compose --profile grafana up -d   # start Grafana
docker compose stop grafana              # stop it again
```

The container mounts `./config.json` as read-only inside the container. Make sure your `config.json` exists in the project root (copy from `config.example.json` and add your keys). Set `enable_prometheus: true` for the `/metrics` endpoint Prometheus scrapes.

Docker Compose binds port 8080 with a 512 MB memory limit and 0.5 CPU cap; `GOMEMLIMIT=450MiB` keeps the Go GC tuned to the container limit, and container logs rotate at 10 MB × 3 files per service.

### Using with OpenCode / Hermes Agent

Point your agent at the router instead of the upstream API directly:

```json
{
  "provider": "openai",
  "model": "opencode-go",
  "api_key": "unused",
  "api_base": "http://127.0.0.1:8080/v1"
}
```

The router injects the real API key into each upstream request automatically.

## Configuration

Configuration is loaded from `config.json` by default. Override the path with the `OPENCODE_CONFIG` environment variable.

### Config Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `listen_addr` | string | `0.0.0.0:8080` | HTTP bind address. Use `127.0.0.1` for localhost-only, `0.0.0.0` for Docker |
| `upstream_url` | string | `https://opencode.ai/zen/go` | OpenCode Go API base URL |
| `keys` | []string | (required) | API keys to rotate |
| `strategy` | string | `round_robin` | `round_robin` or `least_used` |
| `cooldown_seconds` | int | `60` | Cooldown duration for rate-limited keys (429) |
| `auth_cooldown_seconds` | int | `10` | Cooldown duration for auth failures (401/403) |
| `quota_cooldown_seconds` | int | `86400` | Cooldown duration for exhausted quota (429 insufficient_quota). Default 24h, matching typical monthly quota resets |
| `timeout_cooldown_seconds` | int | `10` | Base cooldown for timeouts, transport errors, and repeated 5xx. Escalates with consecutive failures (10s → 30s → 60s → 120s, capped at 5 min) |
| `health_check_timeout_seconds` | int | `10` | Timeout for upstream health probe |
| `upstream_timeout_seconds` | int | `60` | Timeout for upstream response headers. Prevents hanging when upstream is unresponsive |
| `max_request_body_bytes` | int | `10485760` | Maximum request body size in bytes (10 MB default). Protects against memory exhaustion. `0` disables the limit |
| `ready_check_cache_seconds` | int | `30` | How long to cache the `/ready` upstream check result. Prevents rate-limit consumption from frequent probes |
| `admin_user` | string | `admin` | Basic auth username for admin endpoints |
| `admin_pass` | string | `""` | Basic auth password. Empty string disables admin endpoints |
| `disabled_keys` | []int | `[]` | 0-based indices into `keys` that start in the `disabled` state at startup. Survives restarts. Runtime changes via the admin API are in-memory only and are lost on restart unless also added here. |
| `enable_prometheus` | bool | `false` | Enable `/metrics` endpoint |
| `enable_logging` | bool | `false` | Enable file logging |
| `log_file` | string | `""` | Log file path (stdout if empty) |

### Environment Variables

- `OPENCODE_KEYS`: Comma-separated list of keys. Overrides `keys` from config file. This is the preferred way to inject secrets in Docker or systemd environments.
- `OPENCODE_CONFIG`: Path to config file. Overrides the default `config.json`.

### Minimal Config

`config.example.json`:

```json
{
  "listen_addr": "127.0.0.1:8080",
  "upstream_url": "https://opencode.ai/zen/go",
  "keys": ["sk-opencode-go-your-key-here"],
  "strategy": "round_robin",
  "cooldown_seconds": 60,
  "auth_cooldown_seconds": 10,
  "timeout_cooldown_seconds": 10,
  "health_check_timeout_seconds": 10,
  "upstream_timeout_seconds": 60,
  "max_request_body_bytes": 10485760,
  "ready_check_cache_seconds": 30,
  "admin_user": "admin",
  "admin_pass": "",
  "enable_prometheus": false,
  "enable_logging": false,
  "log_file": ""
}
```

### Full Example Config

```json
{
  "listen_addr": "127.0.0.1:8080",
  "upstream_url": "https://opencode.ai/zen/go",
  "keys": [
    "sk-opencode-go-key1",
    "sk-opencode-go-key2",
    "sk-opencode-go-key3"
  ],
  "strategy": "round_robin",
  "cooldown_seconds": 60,
  "auth_cooldown_seconds": 10,
  "timeout_cooldown_seconds": 10,
  "health_check_timeout_seconds": 10,
  "upstream_timeout_seconds": 60,
  "max_request_body_bytes": 10485760,
  "ready_check_cache_seconds": 30,
  "admin_user": "admin",
  "admin_pass": "change-me-in-production",
  "enable_prometheus": true,
  "enable_logging": true,
  "log_file": "/var/log/opencode-router.log"
}
```

## SSE Streaming

The router detects streaming requests by checking for `"stream": true` in the request body or `Accept: text/event-stream` in the headers. When a streaming request is detected:

- Response bytes are forwarded to the client incrementally with `http.Flusher` — no buffering.
- The `X-Accel-Buffering: no` header is set to prevent nginx from buffering the stream.
- Retry only happens if the first upstream attempt returns a retryable status (429, 401, 403, 5xx) **before** any bytes are streamed to the client. Once streaming begins, retry is no longer possible (the client has already received partial output).

Non-streaming requests continue to use the buffered path for full retry transparency.

## Key Rotation Strategies

### round_robin

An atomic counter increments on every request. The rotator starts at the counter position and scans forward, skipping disabled keys and keys still in cooldown. The first available key is selected, its usage count increments, and the counter advances. This gives fair distribution across all healthy keys.

### least_used

The rotator scans all keys and picks the one with the lowest usage count that is not disabled or in active cooldown. This naturally balances load toward underutilized keys, which is useful when keys have different rate limits.

## Key State Machine

Each API key moves through three states:

```
                    +------------+
                    |  HEALTHY   |<----- admin enable (POST /admin/keys/{i}/enable)
                    +------------+<----- cooldown expires
                    /    |        \
        401/403/429 |    |         | insufficient_quota
          (cooldown)|    |         | (long cooldown, default 24h)
                    v    |         v
              +----------+  +----------+
              | COOLDOWN |  | DISABLED |  (admin disable / disabled_keys config)
              +----------+  +----------+
```

| State | Meaning |
|-------|---------|
| `HEALTHY` | Key is available for use. This is the default state. |
| `COOLDOWN` | Key is temporarily paused. Duration depends on the trigger: 401/403 → 10s, 429 rate limit → 60s (or `Retry-After`), 429 `insufficient_quota` → 24h, a manual admin cooldown, or timeout/transport error and 3+ consecutive 5xx → escalating backoff (10s → 30s → 60s → 120s, 5 min cap). Keys auto-recover when cooldown expires. |
| `DISABLED` | Key is permanently removed from rotation. Entered via the admin disable endpoint or the `disabled_keys` config field. Recovers only via the admin enable endpoint (`POST /admin/keys/{index}/enable`); never auto-recovers. |

## Managing Key State

The router is **reactive** to quota exhaustion — it cannot know a key is near its limit until the upstream returns `429 insufficient_quota`. When that happens the key is cooled down for 24h and the request is transparently retried on another key (see [Transparent Retry](#transparent-retry)), so the client never sees the failure.

To **proactively** stop using a key before it exhausts (for example, when you know from the OpenCode dashboard that a key is at 99% of its quota), use the admin endpoints to disable it. All traffic then shifts to the remaining healthy keys with zero errors.

### Disable a key at runtime

```bash
curl -u admin:your-admin-pass -X POST http://127.0.0.1:8080/admin/keys/1/disable
# {"index":1,"masked_key":"sk-ab1...xyz","state":"disabled"}
```

`{index}` is the 0-based position of the key in the `keys` config array, shown by `/admin/stats`. The key is immediately removed from rotation.

### Make the disable survive restarts

Runtime disables are in-memory. If the router restarts, the key returns to `healthy`. To make a disable durable, also add the index to `disabled_keys` in `config.json`:

```json
{
  "keys": ["sk-opencode-go-key1", "sk-opencode-go-key2"],
  "disabled_keys": [1]
}
```

On startup the router marks every index in `disabled_keys` as `disabled`. Validation fails fast if an index is out of range.

### Re-enable a key

When the key's quota resets (e.g. monthly), bring it back:

```bash
curl -u admin:your-admin-pass -X POST http://127.0.0.1:8080/admin/keys/1/enable
# {"index":1,"masked_key":"sk-ab1...xyz","state":"healthy"}
```

Remember to remove the index from `disabled_keys` in config as well, otherwise the next restart will disable it again.

### Temporary cooldown

If you want to pause a key for a fixed window instead of disabling it permanently (it auto-recovers when the window expires):

```bash
curl -u admin:your-admin-pass -X POST http://127.0.0.1:8080/admin/keys/1/cooldown \
  -d '{"seconds": 3600}'
# {"index":1,"masked_key":"sk-ab1...xyz","state":"cooldown","cooldown_seconds":3600}
```

## Transparent Retry and Failover

When the upstream returns a 429, 401, or 403 status code, the router automatically retries the same request with the next available key. The same applies to upstream 5xx responses and transport-level failures (timeouts, connection errors): the request fails over to the next key instead of surfacing a one-shot 502. The client sees only one request and one response.

The retry logic works like this:

1. The request body is buffered in memory so it can be replayed.
2. The router tries up to `N` keys, where `N` is the total number of configured keys.
3. For each attempt, it picks the next available key, forwards the request, and checks the response.
4. If the response triggers a retry, the buffer is discarded and the loop continues.
5. If the response succeeds or produces a non-retryable error, it is forwarded to the client.
6. If the client disconnects or its own timeout fires mid-attempt, the attempt is abandoned without penalizing the key — no cooldown, no error write to a dead client.

When all keys are exhausted, the router returns a 429 (or 401/403 if the last failures were auth errors, or 502 if the last failures were upstream 5xx/transport errors) with an OpenAI-compatible error body:

```json
{
  "error": {
    "message": "all API keys are unavailable",
    "type": "server_error",
    "code": "all_keys_exhausted"
  }
}
```

Repeated failures back off exponentially: each key tracks consecutive failures and its timeout/5xx cooldown grows 10s → 30s → 60s → 120s, capped at 5 minutes, resetting on the first success. After 3 consecutive 5xx responses a key is parked with this backoff (honoring `Retry-After` when present), so an overloaded upstream is not hammered by constant re-probing.

## API Endpoints

| Endpoint | Method | Auth | Description |
|----------|--------|------|-------------|
| `/v1/*` | Any | None | Proxied to the upstream OpenCode Go API. The router injects `Authorization: Bearer <key>` automatically. SSE streaming responses (`text/event-stream`) are forwarded with flushing — no buffering. |
| `/health` | GET | None | Liveness probe. Returns 200 if the router process is alive and has at least one non-disabled key. Does **not** call upstream — safe for high-frequency probes. |
| `/ready` | GET | None | Readiness probe. Checks upstream connectivity with a live key. Result is cached for `ready_check_cache_seconds` (default 30s) to avoid consuming rate limit budget. HTTP 200 if upstream reachable, 503 if not. |
| `/admin/stats` | GET | Basic Auth | JSON snapshot of all keys with masked identifiers, index, states, usage counts, last used, and cooldown expiry timestamps. |
| `/admin/keys/{index}/disable` | POST | Basic Auth | Permanently remove a key from rotation (sets state to `disabled`). `{index}` is the 0-based position from the `keys` config, visible in `/admin/stats`. In-memory only — add the index to `disabled_keys` in config for restart persistence. |
| `/admin/keys/{index}/enable` | POST | Basic Auth | Restore a disabled or cooled-down key to `healthy`, returning it to rotation. |
| `/admin/keys/{index}/cooldown` | POST | Basic Auth | Put a key into temporary cooldown. Request body: `{"seconds": 3600}`. The key auto-recovers to `healthy` when the cooldown expires. |
| `/metrics` | GET | None | Prometheus metrics. Only available when `enable_prometheus` is `true`. |

### Health Response (Liveness)

```json
{
  "status": "alive",
  "healthy_keys": 3,
  "total_keys": 3,
  "disabled_keys": 0
}
```

### Ready Response (Readiness)

```json
{
  "status": "healthy",
  "upstream": "reachable",
  "healthy_keys": 3,
  "total_keys": 3,
  "disabled_keys": 0
}
```

### Stats Response

```json
{
  "keys": [
    {
      "index": 0,
      "masked_key": "sk-ab1...xyz",
      "state": "healthy",
      "usage_count": 42,
      "last_used": "2024-01-15T10:30:00Z",
      "cooldown_until": null
    }
  ],
  "total_requests": 42,
  "strategy": "round_robin"
}
```

## Deployment

### Docker

The Dockerfile uses a multi-stage build with a distroless runtime image. The container runs as the `nonroot` user (UID 65534) with no shell and minimal attack surface.

```bash
docker compose up -d --build
```

### Systemd

Install the unit file (this replaces `__WORKINGDIR__` with your project path automatically):

```bash
sed "s|__WORKINGDIR__|$(pwd)|" deploy/systemd/opencode-router.service | sudo tee /etc/systemd/system/opencode-router.service
sudo systemctl daemon-reload
sudo systemctl enable --now opencode-router
```

Or simply run `./setup.sh` which handles this in step [9/9].

### Resource Limits

The project is tuned for low-resource environments:

- Static binary with no CGO
- Distroless runtime image with no OS bloat
- Docker Compose memory cap at 256 MB
- CPU limit at 0.5 cores
- Optional features (Prometheus, file logging) disabled by default

These defaults work well on a Raspberry Pi 4.

## Building

| Target | Command | Description |
|--------|---------|-------------|
| `build` | `make build` | Compile for the current platform (with version injection) |
| `build-arm64` | `make build-arm64` | Cross-compile for Linux ARM64 |
| `run` | `make run` | Build and run locally |
| `docker` | `make docker` | Build the Docker image |
| `test` | `make test` | Run tests with race detector |
| `lint` | `make lint` | Run golangci-lint |
| `tidy` | `make tidy` | Run `go mod tidy` and check for diffs |
| `ci` | `make ci` | Run tidy + lint + test + build + build-arm64 |
| `clean` | `make clean` | Remove the `bin/` directory |
| `version` | `make version` | Print the current version string |

## Prometheus Monitoring

Enable metrics in your config:

```json
{
  "enable_prometheus": true
}
```

This exposes a `/metrics` endpoint that Prometheus can scrape. See [docs/prometheus-monitoring.md](docs/prometheus-monitoring.md) for a complete setup guide including Grafana dashboards and alerting rules.

### Available Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `opencode_router_requests_total` | Counter | `key`, `status_group` | Total requests per key, grouped by status (2xx, 4xx, 5xx) |
| `opencode_router_key_usage_total` | Counter | `key` | Times each key was selected by the rotator |
| `opencode_router_key_healthy` | Gauge | `key` | 1 if healthy, 0 if in cooldown or disabled |
| `opencode_router_request_duration_seconds` | Histogram | `key` | Request latency distribution |

Labels use masked keys (e.g., `sk-ab1...xyz`) to avoid exposing raw secrets.

### Quick Prometheus Setup

Add this to your `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: 'opencode-router'
    static_configs:
      - targets: ['127.0.0.1:8080']
```

For full setup with Grafana dashboards and alerting, see the [monitoring guide](docs/prometheus-monitoring.md).

## Security

The `/admin/stats` endpoint exposes key usage data and states. When `admin_pass` is empty, admin endpoints are **disabled entirely** and return 403.

For production deployments:

- Set `admin_pass` to a strong, unique password in `config.json` or via environment
- Do not expose the router port to public networks without TLS (use a reverse proxy like nginx/Caddy for TLS termination)
- The router binds to `0.0.0.0:8080` by default (required for Docker; use `127.0.0.1:8080` for localhost-only)
- API keys are never logged or exposed in stats responses (only masked versions are shown)

## License

MIT License. See LICENSE for details.
