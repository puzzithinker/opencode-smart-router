# Raspberry Pi 4 Deployment Guide

This guide covers deploying opencode-smart-router on a Raspberry Pi 4 using Docker.

## Prerequisites

- Raspberry Pi 4 (any RAM variant: 2GB, 4GB, or 8GB)
- Raspberry Pi OS Lite (64-bit) or Ubuntu Server 22.04+ for ARM64
- Docker Engine and Docker Compose installed
- At least one OpenCode Go API key

## Step 1: Install Docker

Connect to your Pi via SSH and install Docker:

```bash
curl -fsSL https://get.docker.com | sh
sudo usermod -aG docker $USER
```

Log out and back in for the group change to take effect. Install Docker Compose:

```bash
sudo apt-get update && sudo apt-get install -y docker-compose-plugin
```

Verify:

```bash
docker compose version
```

## Step 2: Clone and Build

```bash
git clone https://github.com/puzzithinker/opencode-smart-router.git
cd opencode-smart-router
```

Build the Docker image directly on the Pi. The Dockerfile uses a multi-stage build with `GOARCH=arm64` for native ARM64 compilation:

```bash
docker compose build
```

This takes a few minutes on first build. Subsequent builds are faster due to layer caching.

Alternatively, use the Makefile to cross-compile the binary without Docker:

```bash
make build-arm64
```

This produces `bin/opencode-router-linux-arm64` which you can run directly.

## Step 3: Configure

Create a `config.json` from the example:

```bash
cp config.example.json config.json
```

Edit it with your API keys:

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
  "health_check_timeout_seconds": 10,
  "admin_user": "admin",
  "admin_pass": "change-me-in-production",
  "enable_prometheus": true,
  "enable_logging": true,
  "log_file": ""
}
```

Or set keys via environment variable (preferred for Docker):

```bash
export OPENCODE_KEYS="sk-key1,sk-key2,sk-key3"
```

## Step 4: Run with Docker Compose

```bash
docker compose up -d
```

Check the logs:

```bash
docker compose logs -f
```

Expected output (structured slog format):

```
time=2026-06-13T12:00:00.000Z level=INFO msg=startup keys=3 strategy=round_robin listen=127.0.0.1:8080 upstream=https://opencode.ai/zen/go
time=2026-06-13T12:00:00.000Z level=INFO msg=startup version=dev
time=2026-06-13T12:00:00.000Z level=INFO msg=listening addr=127.0.0.1:8080
```

## Step 5: Verify

Check health:

```bash
curl http://127.0.0.1:8080/health
```

Check stats (replace with your admin password):

```bash
curl -u admin:change-me-in-production http://127.0.0.1:8080/admin/stats
```

## Step 6: Run as a systemd Service

The project includes a systemd unit file for Docker Compose:

```bash
sudo cp deploy/systemd/opencode-router.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now opencode-router
```

Check status:

```bash
sudo systemctl status opencode-router
```

View logs:

```bash
sudo journalctl -u opencode-router -f
```

## Resource Tuning for Pi 4

The default `docker-compose.yml` limits the container to 256MB RAM and 0.5 CPU. For a Pi 4 with 4GB+ RAM, you can increase these:

```yaml
deploy:
  resources:
    limits:
      memory: 512M
      cpus: "1.0"
```

For the Go runtime, set `GOGC=50` to reduce memory usage (trades more CPU for less RAM):

```yaml
environment:
  - OPENCODE_KEYS=${OPENCODE_KEYS}
  - GOGC=50
```

## Running Without Docker

If you prefer to run the binary directly:

```bash
make build-arm64
OPENCODE_KEYS="sk-key1,sk-key2" ./bin/opencode-router-linux-arm64
```

For a systemd service running the binary directly (no Docker):

```bash
sudo cp bin/opencode-router-linux-arm64 /usr/local/bin/
sudo useradd -r -s /bin/false opencode-router
```

Create `/etc/systemd/system/opencode-router.service`:

```ini
[Unit]
Description=OpenCode Smart Router
After=network.target

[Service]
Type=simple
User=opencode-router
Group=opencode-router
Environment=OPENCODE_KEYS=sk-key1,sk-key2,sk-key3
ExecStart=/usr/local/bin/opencode-router-linux-arm64
Restart=always
RestartSec=5
TimeoutStopSec=10

[Install]
WantedBy=multi-user.target
```

Then:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now opencode-router
```

## Monitoring

If you enabled Prometheus in your config, add a scrape target to your `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: 'opencode-router'
    static_configs:
      - targets: ['127.0.0.1:8080']
```

## Troubleshooting

| Problem | Check |
|---------|-------|
| Container exits immediately | `docker compose logs` for startup errors |
| Connection refused | Verify `listen_addr` matches your port mapping |
| High memory usage | Set `GOGC=50` environment variable |
| Keys not rotating | Check `/admin/stats` for key states |
| Upstream errors | Check `/health` endpoint for upstream reachability |