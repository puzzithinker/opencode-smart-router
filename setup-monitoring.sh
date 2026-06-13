#!/bin/bash
set -euo pipefail

# ============================================================
# OpenCode Smart Router — Prometheus + Grafana Setup for RPi 4
# Run this script on your Raspberry Pi 4
# ============================================================

echo "=== OpenCode Smart Router: Prometheus + Grafana Setup ==="
echo ""

# --- Configuration ---
ROUTER_PORT=8080
PROMETHEUS_PORT=9090
GRAFANA_PORT=3000
GRAFANA_ADMIN_PASSWORD="${GRAFANA_ADMIN_PASSWORD:-admin}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# --- Step 1: Enable Prometheus in router config ---
echo "[1/6] Checking router config..."

CONFIG_FILE="${SCRIPT_DIR}/config.json"

if [ ! -f "$CONFIG_FILE" ]; then
    echo "ERROR: config.json not found at ${CONFIG_FILE}"
    echo "Create it first: cp config.example.json config.json"
    exit 1
fi

if grep -q '"enable_prometheus": false' "$CONFIG_FILE"; then
    echo "  Enabling Prometheus in config.json..."
    sed -i 's/"enable_prometheus": false/"enable_prometheus": true/' "$CONFIG_FILE"
elif grep -q '"enable_prometheus": true' "$CONFIG_FILE"; then
    echo "  Prometheus already enabled."
else
    echo "  WARNING: Could not find enable_prometheus in config.json."
    echo "  Make sure to add \"enable_prometheus\": true manually."
fi

# --- Step 2: Create Prometheus config ---
echo ""
echo "[2/6] Creating Prometheus configuration..."

mkdir -p "${SCRIPT_DIR}/prometheus"

cat > "${SCRIPT_DIR}/prometheus/prometheus.yml" << 'EOF'
global:
  scrape_interval: 15s
  evaluation_interval: 15s

scrape_configs:
  - job_name: 'opencode-router'
    static_configs:
      - targets: ['opencode-router:8080']
        labels:
          instance: 'rpi4'
EOF

echo "  Written: ${SCRIPT_DIR}/prometheus/prometheus.yml"

# --- Step 3: Create Grafana provisioning ---
echo ""
echo "[3/6] Creating Grafana provisioning configs..."

mkdir -p "${SCRIPT_DIR}/grafana/provisioning/datasources"
mkdir -p "${SCRIPT_DIR}/grafana/provisioning/dashboards"

cat > "${SCRIPT_DIR}/grafana/provisioning/datasources/datasource.yml" << EOF
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
    editable: true
EOF

cat > "${SCRIPT_DIR}/grafana/provisioning/dashboards/dashboard.yml" << EOF
apiVersion: 1
providers:
  - name: 'OpenCode Router'
    orgId: 1
    folder: ''
    type: file
    disableDeletion: false
    editable: true
    options:
      path: /etc/grafana/provisioning/dashboards/json
      foldersFromFilesStructure: false
EOF

# Create dashboard JSON directory
mkdir -p "${SCRIPT_DIR}/grafana/provisioning/dashboards/json"

cat > "${SCRIPT_DIR}/grafana/provisioning/dashboards/json/opencode-router.json" << 'DASHBOARD'
{
  "annotations": {"list": []},
  "editable": true,
  "fiscalYearStartMonth": 0,
  "graphTooltip": 1,
  "id": null,
  "links": [],
  "panels": [
    {
      "title": "Request Rate",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 0, "y": 0},
      "targets": [{"expr": "sum(rate(opencode_router_requests_total[5m]))", "legendFormat": "req/s"}],
      "fieldConfig": {"defaults": {"unit": "reqps"}}
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
      "title": "Latency P50 / P95 / P99",
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
    },
    {
      "title": "Key Errors",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 12, "y": 16},
      "targets": [{"expr": "sum by (key) (rate(opencode_router_requests_total{status_group=~\"4xx|5xx\"}[5m]))", "legendFormat": "{{key}}"}]
    }
  ],
  "refresh": "10s",
  "schemaVersion": 39,
  "tags": ["opencode", "proxy"],
  "templating": {"list": []},
  "time": {"from": "now-1h", "to": "now"},
  "title": "OpenCode Smart Router",
  "uid": "opencode-router"
}
DASHBOARD

echo "  Written: datasource, dashboard provider, and dashboard JSON"

# --- Step 4: Update docker-compose.yml ---
echo ""
echo "[4/6] Updating docker-compose.yml with Prometheus and Grafana..."

# Check if an active (uncommented) prometheus service already exists
if grep -q "^  prometheus:" "${SCRIPT_DIR}/docker-compose.yml"; then
    echo "  Prometheus service already in docker-compose.yml, skipping."
else
    # We need to add the prometheus and grafana services
    # First uncomment the existing prometheus section if it's there
    if grep -q "# prometheus:" "${SCRIPT_DIR}/docker-compose.yml"; then
        echo "  Uncommenting existing Prometheus section..."
        # This is tricky with sed, let's just rewrite the file
        :
    fi

    cat > "${SCRIPT_DIR}/docker-compose.yml" << COMPOSE
services:
  opencode-router:
    build:
      context: .
      args:
        VERSION: \${VERSION:-dev}
    ports:
      - "127.0.0.1:${ROUTER_PORT}:8080"
    volumes:
      - ./config.json:/config.json:ro
    environment:
      - OPENCODE_KEYS=\${OPENCODE_KEYS}
    restart: unless-stopped
    deploy:
      resources:
        limits:
          memory: 256M
          cpus: "0.5"

  prometheus:
    image: prom/prometheus:latest
    volumes:
      - ./prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro
    ports:
      - "127.0.0.1:${PROMETHEUS_PORT}:9090"
    restart: unless-stopped
    deploy:
      resources:
        limits:
          memory: 128M
          cpus: "0.25"

  grafana:
    image: grafana/grafana:latest
    volumes:
      - ./grafana/provisioning:/etc/grafana/provisioning:ro
      - grafana-storage:/var/lib/grafana
    ports:
      - "127.0.0.1:${GRAFANA_PORT}:3000"
    environment:
      - GF_SECURITY_ADMIN_PASSWORD=${GRAFANA_ADMIN_PASSWORD}
      - GF_USERS_ALLOW_SIGN_UP=false
    restart: unless-stopped
    deploy:
      resources:
        limits:
          memory: 128M
          cpus: "0.25"

volumes:
  grafana-storage:
COMPOSE
    echo "  Written: ${SCRIPT_DIR}/docker-compose.yml"
fi

# --- Step 5: Rebuild and start ---
echo ""
echo "[5/6] Rebuilding and starting services..."

cd "$SCRIPT_DIR"
docker compose down 2>/dev/null || true
docker compose up -d --build

# --- Step 6: Verify ---
echo ""
echo "[6/6] Verifying services..."
echo ""

sleep 5

ROUTER_OK=false
PROM_OK=false
GRAF_OK=false

if curl -sf http://127.0.0.1:${ROUTER_PORT}/health > /dev/null 2>&1; then
    ROUTER_OK=true
    echo "  ✅ Router:    http://127.0.0.1:${ROUTER_PORT}/health"
else
    echo "  ❌ Router:    Not responding (may need a few more seconds to start)"
    echo "               Check: docker compose logs opencode-router"
fi

if curl -sf http://127.0.0.1:${ROUTER_PORT}/metrics > /dev/null 2>&1; then
    echo "  ✅ Metrics:   http://127.0.0.1:${ROUTER_PORT}/metrics"
else
    echo "  ❌ Metrics:   Not available (check enable_prometheus in config.json)"
fi

if curl -sf http://127.0.0.1:${PROMETHEUS_PORT}/-/healthy > /dev/null 2>&1; then
    PROM_OK=true
    echo "  ✅ Prometheus: http://127.0.0.1:${PROMETHEUS_PORT}"
else
    echo "  ❌ Prometheus: Not responding (may need longer to start)"
fi

if curl -sf http://127.0.0.1:${GRAFANA_PORT}/api/health > /dev/null 2>&1; then
    GRAF_OK=true
    echo "  ✅ Grafana:    http://127.0.0.1:${GRAFANA_PORT}"
else
    echo "  ❌ Grafana:    Not responding (may need longer to start)"
fi

echo ""
echo "=== Setup Complete ==="
echo ""
echo "Endpoints:"
echo "  Router:       http://127.0.0.1:${ROUTER_PORT}"
echo "  Router Stats: http://127.0.0.1:${ROUTER_PORT}/admin/stats"
echo "  Prometheus:    http://127.0.0.1:${PROMETHEUS_PORT}"
echo "  Grafana:       http://127.0.0.1:${GRAFANA_PORT}"
echo ""
echo "Grafana login:"
echo "  Username: admin"
echo "  Password: ${GRAFANA_ADMIN_PASSWORD}"
echo ""
if [ "$GRAF_OK" = true ]; then
    echo "The 'OpenCode Smart Router' dashboard should be auto-provisioned."
    echo "Find it in Grafana → Dashboards → OpenCode Smart Router."
fi
echo ""
echo "Useful commands:"
echo "  docker compose logs -f              # Follow all logs"
echo "  docker compose logs opencode-router # Router logs only"
echo "  docker compose restart opencode-router  # Restart router only"
echo "  docker compose down                  # Stop all services"