# Prometheus Monitoring Guide

This guide covers setting up Prometheus and Grafana to monitor opencode-smart-router.

## Prerequisites

- opencode-smart-router with `"enable_prometheus": true` in config
- Prometheus installed (or Docker)
- Grafana installed (optional, for dashboards)

## Enable Metrics

In your `config.json`:

```json
{
  "enable_prometheus": true
}
```

Or via Docker Compose:

```yaml
services:
  opencode-router:
    build: .
    ports:
      - "127.0.0.1:8080:8080"
    environment:
      - OPENCODE_KEYS=${OPENCODE_KEYS}
    restart: unless-stopped
```

Verify metrics are being served:

```bash
curl http://127.0.0.1:8080/metrics
```

You should see lines like:

```
opencode_router_requests_total{key="sk-ab1...xyz",status_group="2xx"} 42
opencode_router_key_usage_total{key="sk-ab1...xyz"} 42
opencode_router_key_healthy{key="sk-ab1...xyz"} 1
opencode_router_request_duration_seconds_bucket{key="sk-ab1...xyz",le="0.1"} 30
```

## Available Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `opencode_router_requests_total` | Counter | `key`, `status_group` | Total upstream requests per key, grouped by HTTP status class (2xx, 4xx, 5xx) |
| `opencode_router_key_usage_total` | Counter | `key` | Times each key was selected by the rotator |
| `opencode_router_key_healthy` | Gauge | `key` | Current health state: 1 if healthy, 0 if in cooldown or disabled |
| `opencode_router_request_duration_seconds` | Histogram | `key` | Request latency distribution. Default Prometheus buckets: 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10 |

### Status Groups

The `status_group` label collapses HTTP status codes to keep cardinality low:

| Code Range | Label |
|-----------|-------|
| 200-299 | `2xx` |
| 300-399 | `3xx` |
| 400-499 | `4xx` |
| 500+ | `5xx` |
| Everything else | `other` |

### Key Labels

All metric labels use masked keys (e.g., `sk-ab1...xyz`), never raw API keys. The masking rules:

- Keys longer than 8 chars: first 5 + `...` + last 3
- Keys 4-8 chars: first 3 + `***`
- Keys 3 chars or fewer: key + `***`

## Prometheus Setup

### Docker Compose

Add Prometheus to your `docker-compose.yml`:

```yaml
services:
  opencode-router:
    build: .
    ports:
      - "127.0.0.1:8080:8080"
    environment:
      - OPENCODE_KEYS=${OPENCODE_KEYS}
    restart: unless-stopped
    deploy:
      resources:
        limits:
          memory: 256M
          cpus: "0.5"

  prometheus:
    image: prom/prometheus:latest
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml
    ports:
      - "127.0.0.1:9090:9090"
    restart: unless-stopped
```

### Prometheus Configuration

Create `prometheus.yml` in the project root:

```yaml
global:
  scrape_interval: 15s
  evaluation_interval: 15s

scrape_configs:
  - job_name: 'opencode-router'
    static_configs:
      - targets: ['opencode-router:8080']
```

If Prometheus runs outside Docker, use `127.0.0.1:8080` as the target instead.

Start the stack:

```bash
docker compose up -d
```

Prometheus is now available at `http://127.0.0.1:9090`.

### Useful PromQL Queries

**Total requests per minute:**
```promql
rate(opencode_router_requests_total[5m]) * 60
```

**Request success rate (2xx / total):**
```promql
sum(rate(opencode_router_requests_total{status_group="2xx"}[5m]))
/
sum(rate(opencode_router_requests_total[5m]))
```

**Error rate by key:**
```promql
sum by (key) (rate(opencode_router_requests_total{status_group=~"4xx|5xx"}[5m]))
```

**P95 request latency:**
```promql
histogram_quantile(0.95, sum(rate(opencode_router_request_duration_seconds_bucket[5m])) by (le, key))
```

**Keys currently healthy:**
```promql
opencode_router_key_healthy
```

**Keys in cooldown or disabled:**
```promql
opencode_router_key_healthy == 0
```

**Key usage distribution:**
```promql
topk(5, sum by (key) (rate(opencode_router_key_usage_total[5m])))
```

## Grafana Dashboard

### Docker Compose Setup

Add Grafana to your `docker-compose.yml`:

```yaml
  grafana:
    image: grafana/grafana:latest
    ports:
      - "127.0.0.1:3000:3000"
    environment:
      - GF_SECURITY_ADMIN_PASSWORD=admin
      - GF_USERS_ALLOW_SIGN_UP=false
    volumes:
      - grafana-storage:/var/lib/grafana
    restart: unless-stopped

volumes:
  grafana-storage:
```

### Configure Data Source

1. Open Grafana at `http://127.0.0.1:3000`
2. Login with admin/admin (change the password on first login)
3. Go to Configuration > Data Sources > Add data source
4. Select Prometheus
5. Set URL to `http://prometheus:9090` (Docker) or `http://127.0.0.1:9090` (host)
6. Click Save & Test

### Import Dashboard

Create a dashboard with these panels:

**Panel 1: Request Rate**
- Type: Time series
- Query: `sum(rate(opencode_router_requests_total[5m]))`
- Unit: requests/sec

**Panel 2: Success Rate**
- Type: Stat
- Query: `sum(rate(opencode_router_requests_total{status_group="2xx"}[5m])) / sum(rate(opencode_router_requests_total[5m]))`
- Unit: Percent (0-100)
- Thresholds: Red < 90, Yellow 90-95, Green > 95

**Panel 3: Key Health**
- Type: Stat
- Query: `opencode_router_key_healthy`
- Display: Value mapping: 1 = Healthy, 0 = Down

**Panel 4: Latency P50/P95/P99**
- Type: Time series
- Queries:
  - `histogram_quantile(0.5, sum(rate(opencode_router_request_duration_seconds_bucket[5m])) by (le))`
  - `histogram_quantile(0.95, sum(rate(opencode_router_request_duration_seconds_bucket[5m])) by (le))`
  - `histogram_quantile(0.99, sum(rate(opencode_router_request_duration_seconds_bucket[5m])) by (le))`
- Unit: seconds

**Panel 5: Requests by Status Group**
- Type: Time series (stacked)
- Query: `sum by (status_group) (rate(opencode_router_requests_total[5m]))`
- Legend: `{{status_group}}`

**Panel 6: Key Usage Over Time**
- Type: Time series
- Query: `sum by (key) (rate(opencode_router_key_usage_total[5m]))`
- Legend: `{{key}}`

### Dashboard JSON

To import directly, save this as `opencode-router-dashboard.json` and import it in Grafana:

```json
{
  "dashboard": {
    "title": "OpenCode Smart Router",
    "tags": ["opencode", "proxy"],
    "timezone": "browser",
    "panels": [
      {
        "title": "Request Rate",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 0, "y": 0},
        "targets": [{"expr": "sum(rate(opencode_router_requests_total[5m]))", "legendFormat": "requests/sec"}]
      },
      {
        "title": "Success Rate",
        "type": "stat",
 "gridPos": {"h": 8, "w": 6, "x": 12, "y": 0},
        "targets": [{"expr": "sum(rate(opencode_router_requests_total{status_group=\"2xx\"}[5m])) / sum(rate(opencode_router_requests_total[5m]))", "legendFormat": "success rate"}],
        "fieldConfig": {"defaults": {"unit": "percentunit", "thresholds": {"steps": [{"value": null, "color": "red"}, {"value": 0.9, "color": "yellow"}, {"value": 0.95, "color": "green"}]}}}
      },
      {
        "title": "Key Health",
        "type": "stat",
        "gridPos": {"h": 8, "w": 6, "x": 18, "y": 0},
        "targets": [{"expr": "opencode_router_key_healthy", "legendFormat": "{{key}}"}]
      },
      {
        "title": "Latency P50/P95/P99",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 0, "y": 8},
        "targets": [
          {"expr": "histogram_quantile(0.5, sum(rate(opencode_router_request_duration_seconds_bucket[5m])) by (le))", "legendFormat": "p50"},
          {"expr": "histogram_quantile(0.95, sum(rate(opencode_router_request_duration_seconds_bucket[5m])) by (le))", "legendFormat": "p95"},
          {"expr": "histogram_quantile(0.99, sum(rate(opencode_router_request_duration_seconds_bucket[5m])) by (le))", "legendFormat": "p99"}
        ],
        "fieldConfig": {"defaults": {"unit": "s"}}
      },
      {
        "title": "Requests by Status",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 12, "y": 8},
        "targets": [{"expr": "sum by (status_group) (rate(opencode_router_requests_total[5m]))", "legendFormat": "{{status_group}}"}]
      },
      {
        "title": "Key Usage",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 0, "y": 16},
        "targets": [{"expr": "sum by (key) (rate(opencode_router_key_usage_total[5m]))", "legendFormat": "{{key}}"}]
      }
    ],
    "refresh": "10s"
  }
}
```

## Alerting Rules

Add these to your Prometheus alert rules file:

```yaml
groups:
  - name: opencode-router
    rules:
      - alert: OpenCodeRouterAllKeysDown
        expr: sum(opencode_router_key_healthy) == 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "All API keys are down"
          description: "No healthy keys available. The router cannot forward requests."

      - alert: OpenCodeRouterHighErrorRate
        expr: |
          sum(rate(opencode_router_requests_total{status_group=~"4xx|5xx"}[5m]))
          /
          sum(rate(opencode_router_requests_total[5m]))
          > 0.1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Error rate above 10%"
          description: "More than 10% of requests are returning 4xx or 5xx errors."

      - alert: OpenCodeRouterKeyDisabled
        expr: opencode_router_key_healthy == 0
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Key {{ $labels.key }} is disabled or in cooldown"
          description: "The key has been unhealthy for more than 5 minutes."

      - alert: OpenCodeRouterHighLatency
        expr: |
          histogram_quantile(0.95, sum(rate(opencode_router_request_duration_seconds_bucket[5m])) by (le))
          > 5
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "P95 latency above 5 seconds"
          description: "95th percentile request latency is above 5 seconds."
```

## Resource Impact

Prometheus metrics add minimal overhead:

- Memory: ~2 MB for 10 keys with full request tracking
- CPU: Negligible (counter increments on each request)
- Network: ~5 KB per scrape at 15s intervals

On a Raspberry Pi 4, the metrics endpoint adds less than 1% CPU overhead under normal load.

## Troubleshooting

| Problem | Check |
|---------|-------|
| `/metrics` returns 404 | Ensure `enable_prometheus: true` in config |
| No data in Grafana | Verify Prometheus data source URL and that targets are up in `/targets` |
| Stale metrics | Check `scrape_interval` and router connectivity |
| Missing key labels | Metrics only appear after a key has been used at least once |