# Connecting Hermes Agent and OpenCode to opencode-smart-router

This guide covers how to configure Hermes Agent and OpenCode to route their API requests through opencode-smart-router.

## How It Works

Without the router, your agent talks directly to the OpenCode API:

```
Agent → https://opencode.ai/zen/go (single key)
```

With the router, the agent talks to the local proxy which rotates multiple keys transparently:

```
Agent → http://127.0.0.1:8080/v1 → router → https://opencode.ai/zen/go (rotating keys)
```

The agent only needs to know the router's address. The router handles key selection, retry on failures, and cooldown automatically.

## Prerequisites

- opencode-smart-router running and healthy (verify with `curl http://127.0.0.1:8080/health`)
- At least one API key configured in the router
- The `api_key` field in your agent config can be set to any non-empty string — the router replaces it

## Hermes Agent

Hermes Agent uses an OpenAI-compatible provider config. Point it at the router's `/v1` endpoint and set any non-empty `api_key` value.

### Config File

Create or edit your Hermes config (typically `~/.hermes/config.json` or the path set in `HERMES_CONFIG`):

```json
{
  "provider": "openai",
  "model": "opencode-go",
  "api_key": "unused",
  "api_base": "http://127.0.0.1:8080/v1"
}
```

### Environment Variables

Alternatively, set environment variables before launching Hermes:

```bash
export HERMES_PROVIDER=openai
export HERMES_MODEL=opencode-go
export HERMES_API_KEY=unused
export HERMES_API_BASE=http://127.0.0.1:8080/v1
```

### On Raspberry Pi (Remote Access)

If the router runs on a different machine (e.g., your Pi at `192.168.1.100`), use that IP instead:

```json
{
  "provider": "openai",
  "model": "opencode-go",
  "api_key": "unused",
  "api_base": "http://192.168.1.100:8080/v1"
}
```

### Why "unused" for api_key?

The router injects the real API key into every upstream request. The `api_key` field in your agent config is still required by the OpenAI SDK, but its value is ignored by the router. Use any non-empty string — `"unused"`, `"router"`, or `"sk-placeholder"` all work. An empty string may cause SDK validation errors in some clients.

## OpenCode CLI

The OpenCode command-line tool can also route through the proxy.

### Config File

Edit `~/.opencode/config.json` (or the config path for your installation):

```json
{
  "provider": "openai",
  "model": "opencode-go",
  "api_key": "unused",
  "api_base": "http://127.0.0.1:8080/v1"
}
```

### Environment Variables

```bash
export OPENCODE_PROVIDER=openai
export OPENCODE_MODEL=opencode-go
export OPENCODE_API_KEY=unused
export OPENCODE_API_BASE=http://127.0.0.1:8080/v1
```

## OpenAI-Compatible Clients (curl, Python, etc.)

Any client that supports the OpenAI API format can use the router.

### curl

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer unused" \
  -d '{
    "model": "opencode-go",
    "messages": [{"role": "user", "content": "Hello"}],
    "max_tokens": 100
  }'
```

### Python (openai SDK)

```python
from openai import OpenAI

client = OpenAI(
    api_key="unused",
    base_url="http://127.0.0.1:8080/v1"
)

response = client.chat.completions.create(
    model="opencode-go",
    messages=[{"role": "user", "content": "Hello"}],
    max_tokens=100
)

print(response.choices[0].message.content)
```

### Node.js (openai SDK)

```javascript
import OpenAI from 'openai';

const client = new OpenAI({
  apiKey: 'unused',
  baseURL: 'http://127.0.0.1:8080/v1',
});

const response = await client.chat.completions.create({
  model: 'opencode-go',
  messages: [{ role: 'user', content: 'Hello' }],
  max_tokens: 100,
});

console.log(response.choices[0].message.content);
```

## Verifying It Works

After configuring your agent, send a request and check the router stats:

```bash
# Check router health
curl http://127.0.0.1:8080/health

# Check key stats (replace with your admin password)
curl -u admin:your-password http://127.0.0.1:8080/admin/stats

# Check Prometheus metrics
curl http://127.0.0.1:8080/metrics | grep opencode_router_key_usage
```

### Expected Output

**Health check:**
```json
{
  "status": "healthy",
  "upstream": "reachable",
  "healthy_keys": 3,
  "total_keys": 3,
  "disabled_keys": 0
}
```

**Stats:**
```json
{
  "keys": [
    {
      "masked_key": "sk-ab1...xyz",
      "state": "healthy",
      "usage_count": 12,
      "last_used": "2026-06-13T10:30:00Z"
    }
  ],
  "total_requests": 12,
  "strategy": "round_robin"
}
```

If `healthy_keys` is 0 or `upstream` is `unreachable`, the router cannot reach the OpenCode API. Check your network and API keys.

## Troubleshooting

### Agent returns 401 or 403

The router's configured keys are invalid or expired. Check `/admin/stats` — keys with `state: "disabled"` have failed authentication:

```bash
curl -u admin:your-password http://127.0.0.1:8080/admin/stats | python3 -m json.tool
```

Replace disabled keys by updating `OPENCODE_KEYS` or `config.json` and restart the router.

### Agent returns 429 (Too Many Requests)

All keys are rate-limited. The router automatically retries with the next key, but if all keys are rate-limited simultaneously, it returns 429. Options:

- Add more keys to the rotation
- Increase `cooldown_seconds` in config (default: 60)
- Use `least_used` strategy for better distribution across uneven rate limits

### Agent returns connection refused

The router is not running or not listening on the expected address:

```bash
# Check if the router process is running
docker compose ps

# Check logs
docker compose logs opencode-router --tail 20
```

### Agent returns 502 (Bad Gateway)

The router could not reach the upstream API. Check:

- Network connectivity from the router host to `opencode.ai`
- DNS resolution: `nslookup opencode.ai`
- Firewall rules blocking outbound HTTPS

### Requests go through but no retry on failures

The agent may be sending requests to paths outside `/v1/`. The router only proxies `/v1/` paths. All other paths return 404. Make sure your agent config points to `/v1` at the end of `api_base`:

```
http://127.0.0.1:8080/v1   ← correct
http://127.0.0.1:8080       ← missing /v1, will 404
http://127.0.0.1:8080/v1/   ← correct (trailing slash OK)
```

### Multiple agents sharing the same router

The router handles concurrent requests from multiple agents by design. Each request gets its own key selection from the rotator. There is no conflict or state sharing between agents — just point all agents at the same `api_base`.

## Configuration Reference

| Agent Config Field | Value | Notes |
|---|---|---|
| `provider` | `openai` | The router implements the OpenAI API format |
| `model` | `opencode-go` | Passed through to the upstream API |
| `api_key` | Any non-empty string | Router replaces this with a real key |
| `api_base` | `http://HOST:8080/v1` | Router endpoint with `/v1` path |

For LAN access, replace `127.0.0.1` with the router host's IP address (e.g., `192.168.1.100`).