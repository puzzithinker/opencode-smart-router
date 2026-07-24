# Incident Analysis: Peak-Hour 502 Storm (2026-07-24)

This document records the root-cause analysis of a production incident in which the router returned a sustained burst of `502 Bad Gateway` errors during peak usage, and the fix plan that follows from it. The analysis is based on the production log timeline and a code-level audit of `main.go`.

The companion documents [`reliability-improvements.md`](reliability-improvements.md) and [`bugs-fixed.md`](bugs-fixed.md) cover earlier hardening. This document covers a failure mode those changes did not address: **how the router behaves when the upstream itself is overloaded**.

---

## Summary

| # | Severity | Component | Problem | Planned Fix |
|---|----------|-----------|---------|-------------|
| 1 | Critical | `classifyResponse` 5xx path | Upstream 5xx is never retried and never cools the key down — every failing request is a one-shot client-visible 502 | Retry 5xx on the next key; feed repeated 5xx into the key state machine |
| 2 | Critical | `proxyErrorHandler` | Client-side cancellation (`context.Canceled`) matches neither `"timeout"` nor `"deadline exceeded"` — no cooldown, and a 502 is written to a client that is already gone | Classify with `errors.Is`; treat cancel and deadline as distinct cases |
| 3 | High | `proxyErrorHandler` timeout cooldown | Hardcoded `10*time.Second`, not configurable, no escalation — keys flap in and out of rotation for the duration of an outage | Exponential backoff per key, reset on success |
| 4 | High | `PickKey` recovery | Cooldown expiry auto-marks a key healthy with zero verification — the first request after expiry is a sacrificial probe | Acceptable once backoff exists; optional active prober deferred |
| 5 | Medium | `newReverseProxy` transport | `ResponseHeaderTimeout` does not cover an SSE stream that stalls after headers — hangs until the client gives up | Deferred; documented known gap |

---

## Incident Timeline (UTC)

| Time | Observation | Interpretation |
|------|-------------|----------------|
| 17:00 | Burst of `key_selected` (8+ requests). **No `key_cooldown`, no recovery.** Requests purely timed out | Failure paths that produce zero feedback into the key state machine (root causes 1, 2, 5) |
| 18:00 | Second `key_selected` burst | Same silent failure, one hour later — nothing was learned from the first wave |
| 18:02 | First `key_cooldown` (`sk-ET`), `duration=10s` | Failures finally reached a path that triggers cooldown (header-wait timeout or 401/403 — both are 10s) |
| 18:03–18:14 | Repeated `key_cooldown` alternating `sk-ET` / `sk-0h`, all 10s | Deterministic flap loop: 10s cooldown → sibling key fails → 10s cooldown → first key's cooldown expired → re-selected unverified → fails again |
| 18:30 | `request_forwarded status=200`, traffic normal | The **upstream** recovered. The router played no active role in recovery |

Two properties of the timeline are diagnostic gold:

1. **The 17:00 silence.** An hour of failing requests produced not a single `key_cooldown`. In the current code there are exactly three failure paths that leave the state machine untouched, and all three are consistent with peak-hour upstream overload (see Root Causes 1, 2, 5).
2. **The 18:02–18:14 oscillation.** Two keys alternating 10-second cooldowns for twelve minutes is the precise signature of a fixed, un-escalating cooldown combined with unverified recovery (Root Causes 3, 4).

---

## Root Cause 1: 5xx Is Neither Retried Nor Fed to the State Machine

### The code

`classifyResponse` (main.go:732-738), running as the `ModifyResponse` hook:

```go
// 5xx — upstream problem, don't retry
if statusCode >= 500 {
    recordMetrics()
    if holder != nil {
        holder.result = &ClassificationResult{ShouldRetry: false, StatusCode: statusCode}
    }
    return nil
}
```

### Why it caused the storm

A 502 from the upstream (or its gateway) produces:

- **No retry.** `ShouldRetry: false` means the retry loop in `proxyHandler` (main.go:649) forwards the 502 to the client immediately. The whole point of the router — transparent failover across keys — does not apply to the single most common overload signal.
- **No cooldown.** The key stays `KeyHealthy`. The next request round-robins straight back onto a key that just 502'd.
- **No log event.** Nothing distinguishes "upstream 502 storm" from healthy traffic except the status code on `request_forwarded`.

The design assumption was "5xx is the upstream's problem, not the key's problem." That assumption is wrong in two ways during a peak-hour incident: gateways frequently rate-limit or shed load *per key* behind a 502/503 (making failover to a sibling key effective), and even when the failure is global, forwarding a bare 502 after a single attempt gives the client no benefit from the router at all.

---

## Root Cause 2: Client Cancellation Bypasses Cooldown Classification

### The code

`proxyErrorHandler` (main.go:788-807):

```go
if key != nil {
    if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline exceeded") {
        rotator.MarkCooldown(key, 10*time.Second)
    }
}
...
writeOpenAIError(w, fmt.Sprintf("upstream error: %s", err.Error()), ...)
```

### Why it caused the 17:00 silence

The filter is substring matching on the error text. It catches genuine transport timeouts but misses the most common peak-hour path: **the client gives up first**.

When the caller's own timeout fires (LLM clients commonly time out at 30–120s), the request context is cancelled. `httputil.ReverseProxy` invokes `ErrorHandler` with `context.Canceled`, whose message is `"context canceled"` — matching neither `"timeout"` nor `"deadline exceeded"`. The result:

1. **No cooldown.** The key remains `KeyHealthy` and is immediately re-selected by the next request. Eight consecutive requests can hang and die on the same keys with zero state-machine feedback — exactly what the 17:00 log shows.
2. **A 502 written to a dead client.** `writeOpenAIError` executes unconditionally, writing to a connection that no longer exists. Pure waste, and it pollutes the error picture.

String matching on errors is also inherently fragile — any rewording of Go's net error messages silently disables cooldown.

---

## Root Cause 3: Hardcoded 10-Second Timeout Cooldown, No Escalation

### The code

`proxyErrorHandler` (main.go:794):

```go
rotator.MarkCooldown(key, 10*time.Second) // hardcoded — not cfg.CooldownSeconds
```

### Why it caused the 12-minute flap

A fixed 10-second cooldown with two keys is a metronome:

```
t=0    key A times out      → A: cooldown 10s
t=0    request retried? no  → next request picks B
t=5    key B times out      → B: cooldown 10s
t=10   A's cooldown expired → PickKey auto-revives A → next request picks A
t=10   A times out again    → A: cooldown 10s
...repeat until upstream recovers
```

This is the alternating `sk-ET` / `sk-0h` pattern in the 18:02–18:14 log. A cooldown shorter than the time it takes the sibling key to fail guarantees the keys oscillate; nothing ever backs off far enough to ride out the outage, and every oscillation costs a real client request (a 60s header-wait timeout) before the 502.

The duration is also invisible to operators: it is a literal in the source, not a config field.

---

## Root Cause 4: Recovery Is Passive and Unverified

### The code

`PickKey` (main.go:208-211), on encountering an expired cooldown:

```go
if entry.State == KeyCooldown {
    entry.State = KeyHealthy        // auto-recovery, no probe
    entry.CooldownUntil = time.Time{}
}
```

`MarkSuccess` (main.go:270-284) — the only other recovery path — runs only when a **real request** gets a 2xx.

### Why it prolonged the incident

There is no health verification anywhere in the recovery path. A key "recovers" because a timer expired, and the first request to discover whether it actually works is a paying client request. During the incident this meant every cooldown expiry sacrificed one user request (60s of waiting, then a 502) to re-learn that the upstream was still down — over and over for 12 minutes.

The 18:30 recovery confirms the passivity: traffic normalized when the *upstream* healed, not because the router probed anything.

---

## Root Cause 5: No Timeout Coverage After Response Headers (SSE Stall)

`ResponseHeaderTimeout` (main.go:545) bounds only time-to-headers. A streaming response that delivers headers promptly and then stalls mid-body — the classic shape of an overloaded LLM upstream — hangs **indefinitely**, until the client's own timeout cancels the request (feeding Root Cause 2). This was already documented as a residual gap in `reliability-improvements.md` (Improvement 2, "Tradeoff accepted") and is the hardest to close cleanly with `httputil.ReverseProxy`.

---

## Fix Plan

### P0 — Failover (eliminates most client-visible 502s)

1. **Retry on 5xx.** In `classifyResponse`, set `ShouldRetry: true` for 502/503/504 (bounded by the existing `maxRetries = key count` loop in `proxyHandler`). A 502 on key A becomes a transparent retry on key B; the client sees a 502 only if *all* keys fail.
2. **Classify cancellation correctly.** In `proxyErrorHandler`, replace string matching with `errors.Is`:
   - `context.Canceled` → log `request_canceled`, **no cooldown** (the failure is the client's, not the key's), **no error write** (the client is gone).
   - `context.DeadlineExceeded` / `net.Error` timeout → cooldown **and** `ShouldRetry: true`, so the request fails over instead of dying.
3. **Retry on transport errors** generally (connection refused, reset, DNS): failover to the next key before surfacing a 502.

### P1 — Escalating Cooldown (eliminates the flap loop, makes P0 safe)

4. **Per-key exponential backoff.** Add a `consecutiveFailures` counter to `KeyEntry`. Cooldown duration grows 10s → 30s → 60s → 5min (cap), reset by `MarkSuccess`. New config knob `timeout_cooldown_seconds` for the base; honor `Retry-After` on 5xx when present.
5. **Feed 5xx into the state machine.** After N consecutive 5xx responses on a key, apply a short backoff cooldown — peak-hour gateways often key-rate-limit behind a 502/503, so the state machine should see it.
6. **Observability.** Add `request_canceled`, `upstream_5xx`, and `failover_retry` log events in the same functions being touched. This incident was diagnosable only by the *absence* of events; that should not be true twice.

### P2 — Deferred

7. **Active recovery prober.** A background goroutine probing cooled-down keys via `GET /v1/models` before re-admission. Deferred: with exponential backoff in place, unverified re-admission costs at most one request and then backs off harder — the prober's marginal value is small relative to a permanent background consumer of rate budget on a Pi 4. Revisit if incidents recur.
8. **SSE mid-stream stall watchdog** (read idle timeout after first byte). Deferred: genuinely awkward with `httputil.ReverseProxy`; needs its own design pass.

### Why P0 and P1 must ship together

P0 alone is dangerous. Retrying every request on every key means that during a full upstream outage, each client request multiplies into N upstream attempts — the router would hammer a dying upstream N times harder and add N×timeout latency to every failure. **P1's backoff is what makes P0 safe**: keys that keep failing are parked for progressively longer, so failover quickly converges on the key most likely to answer, and total upstream pressure *drops* instead of rising. The two changes also share every code site (`classifyResponse`, `proxyErrorHandler`, `PickKey`, `KeyEntry`) and one test surface — one review, one race-detector run, one deploy.

---

## Open Questions

- **Were the 18:02–18:14 cooldowns timeouts or 401/403s?** Both produce `duration=10s`. The presence of `transparent_retry` events in that window would indicate 401/403 (retried); their absence indicates timeouts (not retried). The P0/P1 fixes cover both paths identically, so this does not gate implementation.
- **What is the client-side timeout?** If it is shorter than `upstream_timeout_seconds` (60s), Root Cause 2 is the dominant 17:00 path. Worth confirming with the agent configuration; the fix is the same either way.

---

## Verification Plan (for the implementation PR)

| Check | Method |
|-------|--------|
| Failover on 5xx | Test: key A returns 502 via `httptest`, key B returns 200 → client sees 200, one `failover_retry` logged |
| Cancel classification | Test: cancelled context → no cooldown, no 502 write, `request_canceled` logged |
| Deadline classification | Test: `ResponseHeaderTimeout` fires → cooldown applied, retry on next key |
| Backoff escalation | Test: repeated failures on one key → cooldown durations 10s, 30s, 60s; `MarkSuccess` resets |
| No flap loop | Test: two keys, persistent failure → selection converges, cooldown grows to cap |
| Regression | `go test -race -count=1 ./...`, `go vet ./...`, `gofmt -l .` clean |

---

*Incident window: 2026-07-24 17:00–18:30 UTC. Analysis based on production logs and main.go audit at commit 698a241.*
