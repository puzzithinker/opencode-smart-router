# Reliability Improvements: Need and Rationale

This document records the four reliability and functional improvements applied to the `opencode-smart-router` key rotation proxy after its initial implementation. Each section explains **why the change was needed** (the real-world failure it prevented), **how the problem arose** (root cause in the original design), **what was done** (the fix and its design decisions), and **what tradeoffs were accepted**.

The companion document [`bugs-fixed.md`](bugs-fixed.md) covers defects found *during* the original implementation. This document covers *post-implementation* hardening — gaps that were correct-by-the-letter-of-the-spec but wrong for real production traffic.

---

## Summary

| # | Severity | Component | Problem | Fix |
|---|----------|-----------|---------|-----|
| 1 | Critical | `proxyHandler` response path | Streaming (SSE) responses were fully buffered, breaking token-by-token output for LLM clients | Added `streamingResponseWriter` with `http.Flusher` passthrough and retry-only-before-first-byte semantics |
| 2 | High | `newReverseProxy` transport | No upstream timeout; hung upstream caused every client to hang for minutes | Added custom `http.Transport` with configurable `ResponseHeaderTimeout` |
| 3 | High | `proxyHandler` body read | Unbounded `io.ReadAll(r.Body)` could OOM the process, especially on the 256 MB Pi 4 target | Added `http.MaxBytesReader` with configurable `max_request_body_bytes` |
| 4 | High | `healthHandler` | `/health` made a real upstream call per probe, consuming key rate budget under frequent K8s polling | Split into `/health` (liveness, no upstream call) and `/ready` (readiness, cached) |
| 5 | High | `proxyErrorHandler` | Error handler wrote a 502 to the streaming writer even when the response was already marked retryable, flushing garbage to the client and breaking streaming retry | Guard added: skip the error write when `holder.result.ShouldRetry` is true |
| 6 | Low | `TestClassifyResponse_2xx` | Pre-existing test asserted the wrong key state (expected cooldown after a 2xx success) | Corrected assertion to expect `KeyHealthy` |

---

## Improvement 1: SSE Streaming Support

### The Need

The router proxies an OpenAI-compatible API (`opencode-go`). Every modern LLM client — including the Hermes Agent this router was built for — uses **Server-Sent Events** (`text/event-stream`) for chat completions so the user sees tokens appear incrementally as the model generates them. This is not a nice-to-have; it is the default interaction model for any streaming chat UI.

The original `proxyHandler` routed *every* response through a `bufferedResponseWriter` (main.go, original lines 369-410). That writer collects the entire response body into a `bytes.Buffer` and only writes it to the real client at the end, in `writeTo()`. For a streaming completion, this meant:

- The client received **nothing** until the entire generation finished.
- For long generations (30+ seconds), the client appeared frozen and many HTTP clients would time out waiting for the first byte.
- The interactive UX that streaming exists to provide was completely lost.
- Worse, some clients interpret the absence of a streaming response as an error and abort, wasting the already-completed upstream work.

A proxy that silently breaks the primary interaction model of the API it proxies is not functional for its intended use case, regardless of how correct its key rotation logic is.

### Root Cause

The buffering was not a mistake — it was a deliberate design choice to support **transparent retry**. To retry a failed request with a different key, the proxy must be able to *discard* the first response and try again. You cannot discard bytes you have already sent to the client (HTTP is not a request/response protocol you can rewind). Buffering the full response is the simplest way to make retry safe.

The original implementation generalized this to all responses. It did not distinguish between:

- **Error responses** (429/401/403 bodies): small, fast, must be discardable for retry.
- **Success responses** for non-streaming calls (JSON blobs): small-to-medium, buffering is harmless.
- **Success responses** for streaming calls (SSE event sequences): potentially large, time-sensitive, must NOT be buffered.

The single code path treated all three identically.

### The Fix

A new `streamingResponseWriter` was introduced alongside the existing `bufferedResponseWriter`. The `proxyHandler` now detects streaming requests and selects the appropriate writer.

**Detection** (`isStreamingRequest`) checks two standard signals:

1. `Accept: text/event-stream` in the request headers.
2. `"stream": true` in the JSON request body.

Both are standard OpenAI-compatible signals. Checking both handles clients that set one but not the other. A lightweight `json.Unmarshal` into a struct with only a `Stream bool` field avoids parsing the entire (potentially large) request body just to detect streaming.

**Dual-mode writer behavior.** The `streamingResponseWriter` holds a `flushed` flag. Its `Write` method branches on the response status code:

- **Retryable statuses (429/401/403):** the (small) error body is written to an internal `discard` buffer, exactly like the buffered writer. No bytes reach the client. The retry loop can discard and try the next key. This preserves the original retry guarantee.
- **Non-retryable statuses (2xx, 5xx, other 4xx):** on the *first* write, headers and status code are forwarded to the real client, `flushed` is set to true, and `X-Accel-Buffering: no` is added. Subsequent writes go directly to the client with an `http.Flusher.Flush()` call after each one, delivering SSE events in real time.

**Retry boundary.** The retry loop checks `!sw.flushed` before retrying a streaming attempt. Once a non-retryable response has started streaming to the client, retry is impossible — the client has already received the status code and (potentially) some event bytes. This is the correct semantic: you cannot retry a response you have already begun sending. Retry only remains possible *before* the first byte of a real response is committed, which is exactly when the error responses (429/401/403) arrive.

### Design Decisions and Tradeoffs

**Why not always stream?** Non-streaming requests benefit from buffering because it keeps the retry guarantee unconditional and the code path simple. A 200 JSON response buffered and then forwarded is indistinguishable to the client from one streamed directly. Splitting the path only where it matters (streaming) minimizes risk to the existing, tested, non-streaming behavior.

**Why `X-Accel-Buffering: no`?** When the router sits behind nginx (a common deployment for TLS termination), nginx will buffer upstream responses by default, re-introducing the exact problem we just solved. This header is the standard nginx-specific signal to disable buffering for a given response. It is harmless when the router is not behind nginx (the header is simply ignored).

**Why flush after every write?** SSE events are delimited by `\n\n`. Without flushing, the OS TCP stack coalesces small writes and the client receives events in batches rather than as they arrive. Per-write flushing ensures each event (or chunk the upstream sent) reaches the client promptly. The cost is more syscalls, which is irrelevant for LLM streaming where events arrive at human-reading pace (tens per second at most).

**Tradeoff accepted.** If the upstream begins streaming a 200 response and then fails partway through (e.g., drops the connection mid-generation), the client receives a truncated stream. The router cannot retry because it has already committed bytes to the client. This is an inherent limitation of HTTP streaming and cannot be solved at the proxy layer without buffering (which would reintroduce the original problem). Clients must handle mid-stream failures themselves; this is standard for SSE consumers.

---

## Improvement 2: Upstream Response Timeout

### The Need

The original `newReverseProxy` did not set a custom `Transport`. The `httputil.ReverseProxy` falls back to `http.DefaultTransport`, which has **no overall response timeout** and no `ResponseHeaderTimeout`. Its only timeout is the OS-level TCP keepalive and the `Transport`'s `IdleConnTimeout` (which governs idle connection reuse, not active requests).

This means: if the upstream API accepts the TCP connection but never sends response headers — due to a backend hang, an overloaded load balancer, a network device black hole, or a slow-LLM-generation stall before the first token — the proxy would wait **indefinitely**. The client on the other end would also wait indefinitely, until the client's own timeout (often 60-300 seconds for LLM clients, but sometimes longer) fired.

For a key rotation proxy whose entire purpose is reliability, having no timeout on the primary code path is a serious gap. A single hung upstream connection ties up a goroutine, a key's usage slot, and a client connection. Under load, hung upstreams accumulate and exhaust the server's file descriptors and goroutines.

Notably, the `healthHandler` *did* set a 10-second timeout on its dedicated `http.Client`. The inconsistency was telling: the health check was more protected against upstream hangs than the actual proxy traffic.

### Root Cause

`http.DefaultTransport` is intentionally permissive — it is designed as a general-purpose transport where the caller is expected to impose timeouts via `context.WithDeadline` on individual requests. The reverse proxy did not do this, and did not configure the transport, so the default permissiveness applied.

### The Fix

`newReverseProxy` now constructs a dedicated `http.Transport`:

```go
transport := &http.Transport{
    Proxy:                 http.ProxyFromEnvironment,
    MaxIdleConns:          100,
    MaxIdleConnsPerHost:   10,
    IdleConnTimeout:       90 * time.Second,
    ResponseHeaderTimeout: time.Duration(timeoutSeconds) * time.Second,
}
```

A new config field `upstream_timeout_seconds` (default 60) feeds `ResponseHeaderTimeout`. This is the time the proxy will wait for the upstream to send response *headers* after the request is fully sent. Once headers arrive, the timeout no longer applies — the response body can stream for as long as the upstream keeps sending (which is necessary for long LLM generations).

### Design Decisions and Tradeoffs

**Why `ResponseHeaderTimeout` and not an overall deadline?** An overall request deadline (`context.WithTimeout` on the request context) would apply to the *entire* request including body streaming. For a streaming LLM completion that takes 60 seconds to finish generating, a 60-second overall deadline would kill legitimate long responses. `ResponseHeaderTimeout` specifically targets the failure mode we care about — the upstream accepted the connection but is not responding at all — while allowing legitimate long-running streams to complete. The distinction is: "time to first byte of response" should be bounded; "time to last byte" should not.

**Why 60 seconds default?** The upstream is a single API endpoint (`opencode.ai/zen/go`). Under normal conditions, response headers (including the start of a streaming response) arrive within a few seconds. 60 seconds is generous enough to absorb transient slowness and TLS handshake overhead, while being short enough that a genuinely hung upstream is detected within a minute rather than "never." Operators with stricter requirements can lower it.

**Why `MaxIdleConns` / `MaxIdleConnsPerHost`?** These prevent unbounded connection pool growth under load. With a single upstream host, `MaxIdleConnsPerHost: 10` is sufficient for the target hardware (Pi 4, low concurrency) while reusing connections for keep-alive efficiency. The defaults in `http.DefaultTransport` are 100/2 per host; 10 per host here is tuned for the constrained target.

**Tradeoff accepted.** A request that receives headers quickly but then has its body stream stall indefinitely will still hang. Solving that requires a `ResponseHeaderTimeout` *plus* a read deadline on the body, which the standard `ReverseProxy` does not expose cleanly. This is a known residual gap; the current fix addresses the more common and more impactful failure mode (no headers at all).

---

## Improvement 3: Request Body Size Limit

### The Need

The `proxyHandler` buffers the entire request body in memory to enable retry replay:

```go
bodyBytes, err = io.ReadAll(r.Body)
```

With no bound on `r.Body`'s size, a single request with a large body — whether accidental (a client sending a huge prompt) or malicious (an attacker POSTing a 2 GB body to exhaust memory) — would be fully loaded into RAM. The router's stated deployment target is a **Raspberry Pi 4 with a 256 MB Docker memory cap**. A single 300 MB request body would OOM-kill the container instantly, taking down all in-flight requests for every client.

This is not a theoretical concern. The proxy accepts arbitrary `/v1/*` paths with no authentication on the proxy itself (auth is injected upstream). Anyone who can reach the listen address can send an arbitrarily large POST body. Even on non-Pi deployments, an unbounded buffer is a denial-of-service vector.

### Root Cause

`io.ReadAll` reads until EOF with no size limit. This is the default behavior of the function and is "correct" in the sense that it faithfully reads whatever the client sends. The original code trusted the client to send a reasonable body. There is no trust boundary here — the proxy is a network-facing service.

### The Fix

Before reading the body, the handler now wraps it:

```go
if cfg.MaxRequestBodyBytes > 0 {
    r.Body = http.MaxBytesReader(nil, r.Body, cfg.MaxRequestBodyBytes)
}
```

A new config field `max_request_body_bytes` (default 10 MB / 10485760) controls the limit. When the limit is exceeded, `io.ReadAll` returns an error and the handler responds with `413 Request Entity Too Large` and an OpenAI-compatible error body, without consuming the oversized data.

A value of `0` disables the limit, preserving backward compatibility for deployments that intentionally accept large bodies.

### Design Decisions and Tradeoffs

**Why `http.MaxBytesReader` instead of a manual size check?** `MaxBytesReader` is the standard library's purpose-built tool for this. It wraps the body reader so that reads *beyond* the limit fail immediately, without first reading the entire oversized body into memory. A naive approach (`io.ReadAll` then `len(bodyBytes) > limit`) would still load the full body before checking — defeating the entire purpose. `MaxBytesReader` fails fast, as soon as the limit is crossed, so a 2 GB body never occupies more than `max_request_body_bytes` of memory at any point.

**Why 10 MB default?** A typical chat completion request body is under 100 KB. Even a request with a long conversation history and a large system prompt rarely exceeds 1 MB. 10 MB provides a generous margin for unusual-but-legitimate requests (e.g., vision model inputs with base64-encoded images) while being two orders of magnitude below the OOM threshold of the 256 MB Pi 4 container. The limit is configurable so operators with vision or embedding workloads can raise it.

**Why `413` and not `400`?** `413 Request Entity Too Large` is the HTTP status code specifically for this condition. Returning it (rather than a generic 400 or 500) lets clients distinguish "your request was too big" from "your request was malformed" or "the server is broken," enabling client-side retry with a smaller payload.

**Tradeoff accepted.** A legitimate request that exceeds the limit is rejected. This is the correct behavior — the operator chose the limit, and a request that large would likely OOM the process anyway. The configurability (and the `0`-disables escape hatch) ensures no legitimate use case is permanently blocked.

---

## Improvement 4: Health/Ready Probe Split

### The Need

The original `/health` endpoint performed a real upstream API call (`GET /v1/models` with a live key) on **every invocation**. This is a problem in any orchestrated environment (Kubernetes, Nomad, Docker Swarm) where liveness probes fire every 5-10 seconds:

- Each probe consumed one unit of the key's rate limit budget. A key rate-limited at, say, 60 requests/minute would have 10-20% of its budget consumed by *health checks alone*, before any real client traffic.
- The probe used a "healthy" key, so under load the health check competed with real requests for key bandwidth.
- If all keys were in cooldown (a transient state), `/health` returned 503 — even though the router process itself was perfectly alive and would recover in seconds when cooldowns expired. An orchestrator seeing 503 from a liveness probe would **restart the pod**, which discards all key state, drops all in-flight requests, and makes recovery harder, not easier.

This is a classic conflation of **liveness** (is the process running?) with **readiness** (can the process handle requests right now?). They have opposite failure responses: a liveness failure triggers a *restart*; a readiness failure triggers *traffic removal*. Using a single endpoint for both means a transient upstream issue causes a restart, which is the worst possible response.

### Root Cause

The original design had a single health endpoint that answered both "is the router alive?" and "is the upstream reachable?" in one call. This is simpler to implement and reason about, but it couples two concerns with different operational semantics.

### The Fix

The endpoint was split into two:

**`/health` — Liveness probe.** Checks only local state: does the router have at least one non-disabled key? No upstream call is made. Returns 200 with `"status": "alive"` if any key is non-disabled, 503 with `"status": "unhealthy"` only if every key is permanently disabled (a state that requires manual admin action to reach and requires manual action to recover from). This endpoint is safe to hit every second — it does no I/O and consumes no rate budget.

**`/ready` — Readiness probe.** Performs the upstream connectivity check (same logic as the old `/health`). The result is cached for `ready_check_cache_seconds` (default 30s). Within the cache window, repeated `/ready` calls return the cached result without calling upstream. This bounds the upstream probe rate to one per 30 seconds regardless of probe frequency.

A new `readyCache` global holds the cached result, timestamp, and status code. `resetReadyCache()` is exposed for test isolation.

### Design Decisions and Tradeoffs

**Why cache the readiness result?** Without caching, a K8s readiness probe hitting `/ready` every 5 seconds would make 12 upstream calls per minute — the exact rate-budget-consumption problem we are solving. The cache decouples probe frequency from upstream call frequency. 30 seconds is short enough that a real upstream outage is detected within half a minute, and long enough that probe overhead is negligible (one upstream call per 30s vs. one per 5s).

**Why is the liveness check "at least one non-disabled key"?** A disabled key is a *permanent* administrative state — it requires a manual admin action to disable and a manual action to re-enable. If all keys are disabled, the router cannot serve any request and will never recover without operator intervention; restarting is appropriate. Cooldown is *transient* — keys auto-recover. If all keys are in cooldown, the router will recover on its own within seconds; restarting would discard the cooldown timers and likely cause the same keys to be tried again immediately, worsening the situation. Liveness must therefore return 200 during cooldown-exhaustion (process is fine, will recover) and 503 only during all-disabled (process cannot function, needs help).

**Why a global cache and not per-handler state?** The handler is a free function matching the existing codebase style (which uses package-level `cfg` and `rotator` globals). A `Server` struct holding the cache would be cleaner but would require refactoring the entire handler set — out of scope for this change. The cache is guarded implicitly by the HTTP server's single-goroutine-per-request model and the fact that the worst case of a race is two concurrent upstream calls (a minor waste, not a correctness issue). This matches the concurrency discipline of the surrounding code.

**Tradeoff accepted.** The readiness cache means a real upstream outage that begins *between* cache refreshes will not be detected until the next refresh (up to 30s later). During that window, `/ready` returns 200 and the orchestrator keeps sending traffic, which will fail at the actual request level. This is acceptable because (a) 30s is short, (b) the proxy's own retry logic handles per-request upstream failures, and (c) the alternative (no caching) causes the rate-budget problem that motivated the split.

---

## Related Fix 5: `proxyErrorHandler` and Streaming Retry

### The Need

This fix was discovered while implementing Improvement 1. The streaming retry test (`TestStreamingResponse_RetryOn429BeforeStream`) failed: when the first upstream returned 429, the client received a `502 Bad Gateway` with an `"upstream error"` body instead of a retried-and-streamed 200.

### Root Cause

`httputil.ReverseProxy` calls the `ErrorHandler` whenever `ModifyResponse` (our `classifyResponse`) returns an error — which it does for 429/401/403 to signal that the key should be cooled down. The original `proxyErrorHandler` did two things:

1. Set `holder.result` if it was nil (to mark the response as non-retryable 502, as a fallback).
2. **Unconditionally** call `writeOpenAIError(w, ...)` to write a 502 response to the `ResponseWriter`.

For the buffered path, the unconditional write was harmless — it wrote to the `bufferedResponseWriter`'s buffer, which the retry loop discarded when it saw `holder.result.ShouldRetry == true`.

For the streaming path, the write was catastrophic. The 502 error body was written to the `streamingResponseWriter`, which (because 502 is not a retryable status code per its `isRetryable()` check) forwarded it directly to the real client with `flushed = true`. The retry loop then checked `!sw.flushed`, saw that bytes had already been sent, and gave up on retrying. The client received a 502 and no retry ever happened.

The error handler was writing to the response writer *before* the retry loop had a chance to decide whether to retry. The buffered path masked this; the streaming path exposed it.

### The Fix

A guard was added before the `writeOpenAIError` call:

```go
if holder != nil && holder.result != nil && holder.result.ShouldRetry {
    return
}
```

If `classifyResponse` has already classified the response as retryable, the error handler returns *without writing anything to the response writer*. The retry loop then sees `ShouldRetry == true` and `!sw.flushed` (nothing was flushed), and proceeds to try the next key. The error from `classifyResponse` is still used internally for logging and key state management; it just does not result in a client-visible error response when a retry is pending.

### Design Decisions and Tradeoffs

**Why not remove the `writeOpenAIError` call entirely?** The error handler is also called for *genuine* upstream errors — connection refused, DNS failure, TLS error — where `classifyResponse` was never called (or returned nil without setting `holder.result`). In those cases, the 502 error response is the correct client-visible behavior: the upstream is genuinely unreachable and no retryable status was returned. The guard preserves this path while suppressing the write only when a retry is in progress.

**Why check `holder.result != nil`?** `classifyResponse` may not have run (e.g., the error occurred before the response was received). In that case `holder.result` is nil and the fallback path (set it to non-retryable 502, then write the error) executes as before. The guard only short-circuits when we are *certain* a retry is intended.

---

## Related Fix 6: Pre-existing Test Bug

### The Need

While establishing a test baseline before making any changes, `TestClassifyResponse_2xx` was already failing:

```
main_test.go:1123: key state = 0, want 1 (cooldown)
```

The test expected a key to be in `KeyCooldown` (state 1) after a 2xx success response. The implementation correctly set the key to `KeyHealthy` (state 0) via `MarkSuccess`. This was a test-only bug — the assertion was wrong, not the implementation.

### Why It Was Fixed

A failing baseline makes it impossible to verify that subsequent changes do not introduce regressions. If the baseline is red, a future failure could be either a new bug or this pre-existing one, and distinguishing them requires archaeology. Fixing the one-line assertion (`KeyCooldown` → `KeyHealthy`) made the baseline green, so every subsequent test failure was unambiguously attributable to the change that caused it.

This is the same class of bug documented as BUG-4, BUG-5, and BUG-6 in `bugs-fixed.md`: tests whose expectations diverge from the (correct) implementation. The lesson from that document applies here — when a test fails, question the test before assuming the implementation is wrong.

---

## Configuration Changes

Three new config fields were introduced. All have sensible defaults and are backward-compatible — existing `config.json` files without these fields will use the defaults.

| Field | Type | Default | Purpose |
|-------|------|---------|---------|
| `upstream_timeout_seconds` | int | `60` | Seconds to wait for upstream response headers (Improvement 2) |
| `max_request_body_bytes` | int64 | `10485760` (10 MB) | Maximum request body size; `0` disables the limit (Improvement 3) |
| `ready_check_cache_seconds` | int | `30` | How long to cache the `/ready` upstream check result (Improvement 4) |

The `config.example.json` and `README.md` were updated to document these fields, the new `/ready` endpoint, and the SSE streaming behavior.

---

## Testing

Each improvement is covered by new tests in `main_test.go`:

| Test | Covers |
|------|--------|
| `TestIsStreamingRequest` (7 subtests) | Streaming detection via body and header signals, including invalid JSON and nil body |
| `TestStreamingResponse_ForwardedDirectly` | SSE response bytes reach the client unbuffered and in order |
| `TestStreamingResponse_RetryOn429BeforeStream` | Retry still works for streaming requests when the first key returns 429 before any bytes are streamed |
| `TestRequestBodyLimit_RejectsOversizedBody` | Oversized body returns 413 and does not reach upstream |
| `TestRequestBodyLimit_AllowsUnderLimit` | Under-limit body is proxied normally |
| `TestLoadConfigNewFieldDefaults` | All three new config fields have correct defaults |
| `TestReadyHandler_HealthyUpstream` | `/ready` returns 200 with upstream reachable |
| `TestReadyHandler_UpstreamUnreachable` | `/ready` returns 503 with upstream unreachable |
| `TestReadyHandler_CachesResult` | Five `/ready` calls result in only one upstream call (cache works) |
| `TestHealthHandler_LivenessNoUpstreamCall` | `/health` returns 200 and makes zero upstream calls |
| `TestHealthHandler_AllKeysDisabled` (existing, retained) | `/health` returns 503 when all keys disabled |

The two pre-existing `/health` upstream-check tests were converted to `/ready` tests, since the upstream-checking behavior moved to `/ready`. The existing liveness-when-all-disabled test was retained as-is because that behavior remained on `/health`.

The test setup helpers (`setupTestGlobals`, `setupTestGlobalsNoAuth`) were updated to include the new config fields and to call `resetReadyCache()` for isolation, ensuring cached readiness results from one test do not leak into another.

---

## Verification

All changes were verified with the project's standard toolchain:

| Check | Command | Result |
|-------|---------|--------|
| Formatting | `gofmt -l .` | Clean (no files listed) |
| Vet | `go vet ./...` | Exit 0 |
| Tests (race detector) | `go test -race -count=1 ./...` | All pass |
| Build (amd64) | `CGO_ENABLED=0 go build` | Exit 0 |
| Build (arm64) | `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build` | Exit 0 |

The race detector run is particularly important for the `streamingResponseWriter` and `readyCache`, both of which involve shared state across the reverse proxy's internal goroutines and the handler's retry loop.

---

## Lessons Learned

1. **A proxy must understand the semantics of what it proxies.** The original buffering broke SSE because the proxy treated all responses as "a blob to forward" rather than recognizing that streaming responses have time-sensitive delivery requirements. Correct proxying requires protocol-aware behavior, not just byte forwarding.

2. **Liveness and readiness are different questions with different answers.** "Is the process alive?" and "Can I send it traffic?" are answered by different checks and trigger different orchestration responses. Conflating them causes restarts when you want traffic removal, and vice versa. The Kubernetes probe split exists for a reason.

3. **Trust boundaries apply to request size.** Any network-facing service that reads a request body must bound the read. `io.ReadAll` on an untrusted stream is a denial-of-service vulnerability, regardless of how well the rest of the service is hardened. `http.MaxBytesReader` exists specifically because this is a common and serious mistake.

4. **A timeout on the happy path is not optional.** The health check had a timeout; the main proxy path did not. The path that matters more (the one carrying all real traffic) was less protected than the path that matters less. Timeouts must be applied to the primary code path, not just the auxiliary endpoints.

5. **Error handlers must not produce side effects that preempt the retry decision.** The `proxyErrorHandler` bug (Improvement 5) was masked by the buffered writer for non-streaming traffic. It only surfaced when the streaming path exposed the premature write to the real client. When a retry loop and an error handler both touch the same response writer, the error handler must defer to the retry loop's decision — it must not commit bytes to the client before the loop has decided whether to retry.

6. **A green baseline is a prerequisite for safe change.** Fixing the pre-existing `TestClassifyResponse_2xx` failure first meant that every subsequent test failure was unambiguously caused by the change under test. Shipping changes on top of a red baseline makes regression detection impossible.

7. **Backward-compatible defaults enable incremental adoption.** All three new config fields have defaults that match or improve on the prior behavior. Existing deployments gain the improvements without config changes, while operators who need different values can opt in. A `0`-disables escape hatch on the body limit ensures no deployment is permanently blocked by an over-conservative default.

---

*Document covers changes applied to `opencode-smart-router` on 2026-06-20.*
