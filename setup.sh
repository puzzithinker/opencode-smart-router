#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

echo ""
echo "  ___                    _   _           _   "
echo " / _ \\ _ __   ___ _ __ | | | |_ __   __| | "
echo "| | | | '_ \\ / _ \\ '_ \\| |_| | '_ \\ / _\` | "
echo "| |_| | |_) |  __/ | | |  _  | | | | (_| | "
echo " \\___/| .__/ \\___|_| |_|_| |_|_| |_|\\__,_| "
echo "      |_|          Smart Router for OpenCode Go"
echo ""
echo "============================================="
echo " One-time setup for Raspberry Pi 4"
echo "============================================="
echo ""
echo "  Project directory: ${SCRIPT_DIR}"
echo ""
echo ""

# --- Checks ---
echo "[1/8] Checking prerequisites..."

for cmd in docker git curl; do
    if ! command -v "$cmd" &> /dev/null; then
        echo "  ERROR: $cmd is not installed."
        echo "  Install it with: sudo apt-get install -y $cmd"
        exit 1
    fi
done

if ! docker compose version &> /dev/null; then
    echo "  ERROR: docker compose v2 is not available."
    echo "  Install it with: sudo apt-get install -y docker-compose-plugin"
    exit 1
fi

if groups | grep -q docker; then
    echo "  Docker:     OK"
else
    echo "  WARNING: User is not in the 'docker' group."
    echo "  Run: sudo usermod -aG docker $USER && newgrp docker"
    echo "  Then re-run this script."
    exit 1
fi

echo "  Git:        OK"
echo "  Docker:     OK"
echo "  Compose:    OK"
echo "  curl:       OK"

# --- Config ---
echo ""
echo "[2/8] Setting up configuration..."

CONFIG_FILE="${SCRIPT_DIR}/config.json"

if [ ! -f "$CONFIG_FILE" ]; then
    echo "  Creating config.json from config.example.json..."
    cp config.example.json config.json

    if [ -z "${OPENCODE_KEYS:-}" ]; then
        echo ""
        echo "  You need at least one OpenCode Go API key."
        echo "  Enter your key(s), comma-separated (or set OPENCODE_KEYS env var):"
        echo -n "  Keys: "
        read -r KEYS_INPUT
        if [ -z "$KEYS_INPUT" ]; then
            echo "  ERROR: No keys provided. Set OPENCODE_KEYS and re-run."
            exit 1
        fi
        # Convert comma-separated keys to JSON array entries
        JSON_KEYS=$(echo "$KEYS_INPUT" | sed 's/,/","/g' | sed 's/^/"/;s/$/"/' | sed 's/ //g')
        sed -i "s/\[\"sk-opencode-go-your-key-here\"\]/[$JSON_KEYS]/" "$CONFIG_FILE"
    else
        # OPENCODE_KEYS is set, use env var (docker-compose passes it through)
        echo "  Using OPENCODE_KEYS from environment (${#OPENCODE_KEYS} chars)"
    fi

    # Enable Prometheus for monitoring
    sed -i 's/"enable_prometheus": false/"enable_prometheus": true/' "$CONFIG_FILE"
    echo "  Prometheus enabled."
else
    echo "  config.json already exists."
    # Ensure Prometheus is enabled
    if grep -q '"enable_prometheus": false' "$CONFIG_FILE"; then
        sed -i 's/"enable_prometheus": false/"enable_prometheus": true/' "$CONFIG_FILE"
        echo "  Prometheus enabled."
    fi
fi

# --- Prometheus config ---
echo ""
echo "[3/8] Creating Prometheus configuration..."

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

  - job_name: 'tavily-router'
    static_configs:
      - targets: ['tavily-router:8082']
        labels:
          instance: 'rpi4'
EOF

echo "  Written: prometheus/prometheus.yml"

# --- Grafana provisioning ---
echo ""
echo "[4/8] Creating Grafana provisioning..."

mkdir -p "${SCRIPT_DIR}/grafana/provisioning/datasources"
mkdir -p "${SCRIPT_DIR}/grafana/provisioning/dashboards/json"

GRAFANA_PASSWORD="${GRAFANA_ADMIN_PASSWORD:-admin}"

cat > "${SCRIPT_DIR}/grafana/provisioning/datasources/datasource.yml" << 'EOF'
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
    editable: true
EOF

cat > "${SCRIPT_DIR}/grafana/provisioning/dashboards/dashboard.yml" << 'EOF'
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

# --- Tavily Router dashboard (if tavily-smart-router is running) ---
cat > "${SCRIPT_DIR}/grafana/provisioning/dashboards/json/tavily-router.json" << 'DASHBOARD'
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
      "targets": [{"expr": "sum(rate(tavily_router_requests_total[5m]))", "legendFormat": "req/s"}],
      "fieldConfig": {"defaults": {"unit": "reqps"}}
    },
    {
      "title": "Success Rate",
      "type": "stat",
      "gridPos": {"h": 8, "w": 6, "x": 12, "y": 0},
      "targets": [{"expr": "sum(rate(tavily_router_requests_total{status_group=\"2xx\"}[5m])) / sum(rate(tavily_router_requests_total[5m]))", "legendFormat": "success rate"}],
      "fieldConfig": {"defaults": {"unit": "percentunit", "thresholds": {"steps": [{"value": null, "color": "red"}, {"value": 0.9, "color": "yellow"}, {"value": 0.95, "color": "green"}]}}}
    },
    {
      "title": "Key Health",
      "type": "stat",
      "gridPos": {"h": 8, "w": 6, "x": 18, "y": 0},
      "targets": [{"expr": "tavily_router_key_healthy", "legendFormat": "{{key}}"}]
    },
    {
      "title": "Latency P50 / P95 / P99",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 0, "y": 8},
      "targets": [
        {"expr": "histogram_quantile(0.5, sum(rate(tavily_router_request_duration_seconds_bucket[5m])) by (le))", "legendFormat": "p50"},
        {"expr": "histogram_quantile(0.95, sum(rate(tavily_router_request_duration_seconds_bucket[5m])) by (le))", "legendFormat": "p95"},
        {"expr": "histogram_quantile(0.99, sum(rate(tavily_router_request_duration_seconds_bucket[5m])) by (le))", "legendFormat": "p99"}
      ],
      "fieldConfig": {"defaults": {"unit": "s"}}
    },
    {
      "title": "Requests by Status",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 12, "y": 8},
      "targets": [{"expr": "sum by (status_group) (rate(tavily_router_requests_total[5m]))", "legendFormat": "{{status_group}}"}]
    },
    {
      "title": "Key Usage",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 0, "y": 16},
      "targets": [{"expr": "sum by (key) (rate(tavily_router_key_usage_total[5m]))", "legendFormat": "{{key}}"}]
    },
    {
      "title": "Cooldown Events",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 12, "y": 16},
      "targets": [{"expr": "sum by (key) (rate(tavily_router_key_cooldown_total[5m]))", "legendFormat": "{{key}}"}]
    },
    {
      "title": "Upstream Errors",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 0, "y": 24},
      "targets": [{"expr": "sum by (error_type) (rate(tavily_router_upstream_errors_total[5m]))", "legendFormat": "{{error_type}}"}]
    },
    {
      "title": "Key Errors",
      "type": "timeseries",
      "gridPos": {"h": 8, "w": 12, "x": 12, "y": 24},
      "targets": [{"expr": "sum by (key) (rate(tavily_router_requests_total{status_group=~\"4xx|5xx\"}[5m]))", "legendFormat": "{{key}}"}]
    }
  ],
  "refresh": "10s",
  "schemaVersion": 39,
  "tags": ["tavily", "proxy"],
  "templating": {"list": []},
  "time": {"from": "now-1h", "to": "now"},
  "title": "Tavily Smart Router",
  "uid": "tavily-router"
}
DASHBOARD

echo "  Written: Tavily Router dashboard (shows data when tavily-smart-router is running)"

# Update docker-compose.yml with Grafana password
sed -i "s/GF_SECURITY_ADMIN_PASSWORD:.*/GF_SECURITY_ADMIN_PASSWORD: ${GRAFANA_PASSWORD}/" "${SCRIPT_DIR}/docker-compose.yml" 2>/dev/null || true

# --- Build ---
echo ""
echo "[5/8] Building Docker image (this takes a few minutes on first run)..."

docker compose build 2>&1 | tail -5

# --- Start ---
echo ""
echo "[6/8] Starting services..."

docker network create smart-routers 2>/dev/null || true
docker compose down 2>/dev/null || true
docker compose up -d

# --- Wait for services ---
echo ""
echo "[7/8] Waiting for services to start..."

MAX_WAIT=30
WAITED=0
ROUTER_UP=false

while [ $WAITED -lt $MAX_WAIT ]; do
    if curl -sf http://127.0.0.1:8080/health > /dev/null 2>&1; then
        ROUTER_UP=true
        break
    fi
    sleep 2
    WAITED=$((WAITED + 2))
    echo "  Waiting... ($WAITED/${MAX_WAIT}s)"
done

# --- Verify ---
echo ""
echo "[8/8] Verifying services..."
echo ""

print_status() {
    local name="$1"
    local url="$2"
    local label="$3"
    if curl -sf "$url" > /dev/null 2>&1; then
        echo "  ✅ $name:    $label"
    else
        echo "  ❌ $name:    Not responding ($label)"
    fi
}

print_status "Router"    "http://127.0.0.1:8080/health"       "http://$(hostname -I 2>/dev/null | awk '{print $1}'):8080/health"
print_status "Metrics"   "http://127.0.0.1:8080/metrics"      "http://$(hostname -I 2>/dev/null | awk '{print $1}'):8080/metrics"
print_status "Prometheus" "http://127.0.0.1:9090/-/healthy"   "http://$(hostname -I 2>/dev/null | awk '{print $1}'):9090"
print_status "Grafana"    "http://127.0.0.1:3000/api/health" "http://$(hostname -I 2>/dev/null | awk '{print $1}'):3000"

echo ""
echo "============================================="
echo " Setup Complete!"
echo "============================================="
echo ""
echo " Endpoints (LAN access from other hosts):"
echo ""
echo "   Router:        http://$(hostname -I 2>/dev/null | awk '{print $1}'):8080"
echo "   Health:        http://$(hostname -I 2>/dev/null | awk '{print $1}'):8080/health"
echo "   Admin Stats:   http://$(hostname -I 2>/dev/null | awk '{print $1}'):8080/admin/stats"
echo "   Prometheus:    http://$(hostname -I 2>/dev/null | awk '{print $1}'):9090"
echo "   Grafana:        http://$(hostname -I 2>/dev/null | awk '{print $1}'):3000"
echo ""
echo " Grafana login:"
echo "   Username: admin"
echo "   Password: ${GRAFANA_PASSWORD}"
echo ""
echo " The 'OpenCode Smart Router' dashboard is auto-provisioned."
echo " Find it in Grafana → Dashboards."
echo ""

# --- Systemd service ---
echo ""
echo "[9/9] Installing systemd service..."

SERVICE_FILE="${SCRIPT_DIR}/deploy/systemd/opencode-router.service"

if [ ! -f "$SERVICE_FILE" ]; then
    echo "  WARNING: systemd unit file not found at ${SERVICE_FILE}"
    echo "  Skipping systemd setup. You can start services with: docker compose up -d"
else
    sed "s|__WORKINGDIR__|${SCRIPT_DIR}|" "$SERVICE_FILE" | sudo tee /etc/systemd/system/opencode-router.service > /dev/null
    sudo systemctl daemon-reload
    sudo systemctl enable --now opencode-router
    echo "  systemd service installed and started."
fi

echo ""
echo " Manual commands:"
echo "   docker compose logs -f                    Follow all logs"
echo "   docker compose logs opencode-router       Router logs only"
echo "   docker compose restart opencode-router    Restart router"
echo "   docker compose down                       Stop all services"
echo "   docker compose up -d                      Start all services"
echo "   sudo systemctl status opencode-router      Service status"
echo ""