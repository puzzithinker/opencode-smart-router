# Bug Report: Bugs Found and Fixed During Implementation

This document records every bug discovered during the implementation and testing of the opencode-smart-router key rotation proxy. Each entry includes the symptom, root cause, production impact, and the fix applied.

---

## Summary Table

| Bug # | Severity | Component | Brief Description | Status |
|-------|----------|-----------|-------------------|--------|
| 1 | Critical | `main.go` — `MarkSuccess()` | `MarkSuccess` recovered DISABLED keys back to healthy | Fixed |
| 2 | Critical | `main.go` — `proxyErrorHandler()` | Error handler overwrote classification result, breaking transparent retry | Fixed |
| 3 | High | `main.go` — `proxyHandler()` | All-keys-exhausted fallback always returned 429 regardless of actual error | Fixed |
| 4 | Low | `main_test.go` — `TestMaskKey` | Test expected incorrect masking result | Fixed |
| 5 | Low | `main_test.go` — `TestPickKeyLeastUsed` | Test expected wrong key on tie-break | Fixed |
| 6 | Low | `main_test.go` — `TestPickKeyCooldownExpires` | Flaky sleep-based cooldown expiration test | Fixed |
| 7 | Critical | `main.go` — `classifyResponse()` | 401/403 permanently disabled keys on transient auth failures | Fixed |

---

## Detailed Bug Reports

### BUG-1: MarkSuccess recovering DISABLED keys

- **Severity**: Critical
- **File**: `main.go`, `MarkSuccess()` method
- **Symptom**: `TestDisabledIsPermanent` failed. Calling `MarkSuccess` on a DISABLED key changed its state back to `KeyHealthy` (state 0), when it should remain DISABLED (state 2).
- **Root Cause**: `MarkSuccess` unconditionally set `key.State = KeyHealthy`. The plan explicitly states "DISABLED is permanent — never recovers", but the implementation didn't guard against this.
- **Impact**: A key that received a 401/403/insufficient_quota (permanently invalid) could be accidentally un-disabled by a subsequent successful request through a different key. This caused the proxy to retry a known-bad key, wasting requests and potentially leaking invalid credentials upstream.
- **Fix**: Added an early return guard at the top of `MarkSuccess` to skip any state change if the key is already DISABLED.

```go
func (kr *KeyRotator) MarkSuccess(key *KeyEntry) {
    if key.State == KeyDisabled {
        return
    }
    key.State = KeyHealthy
    key.ConsecutiveFailures = 0
}
```

---

### BUG-2: proxyErrorHandler overwriting classification result

- **Severity**: Critical
- **File**: `main.go`, `proxyErrorHandler()` function
- **Symptom**: Integration test `TestTransparentRetry_429ThenSuccess` returned HTTP 502 to the client instead of transparently retrying with the next key. The upstream was only called once (`callCount=1`).
- **Root Cause**: When `classifyResponse` returns an error for 429/401/403, `httputil.ReverseProxy` calls the `ErrorHandler`. The `proxyErrorHandler` unconditionally set `holder.result = &ClassificationResult{ShouldRetry: false, StatusCode: 502}`, overwriting the `ShouldRetry: true` that `classifyResponse` had already stored in the holder. This caused the proxy handler's retry loop to see `ShouldRetry: false` and forward the error to the client instead of retrying.
- **Impact**: Transparent retry was completely broken. The proxy never retried on 429/401/403, making the entire key rotation mechanism useless. Clients would see 502 errors for what should have been an automatic retry with the next available key.
- **Fix**: Changed `proxyErrorHandler` to only set `holder.result` if it hasn't already been populated by `classifyResponse`.

```go
func proxyErrorHandler(holder *responseHolder) func(http.ResponseWriter, *http.Request, error) {
    return func(w http.ResponseWriter, r *http.Request, err error) {
        if holder != nil && holder.result == nil {
            holder.result = &ClassificationResult{
                ShouldRetry: false,
                StatusCode:  http.StatusBadGateway,
                Error:       err,
            }
        }
    }
}
```

---

### BUG-3: All-keys-exhausted fallback always returning 429

- **Severity**: High
- **File**: `main.go`, `proxyHandler()` function, the fallback path after the retry loop
- **Symptom**: Integration test `TestTransparentRetry_401AllKeys` expected HTTP 401 but got HTTP 429.
- **Root Cause**: When all keys are exhausted after retries, the proxy handler had two code paths. One inside the `PickKey()` error handler correctly checked `lastStatusCode` to return 401 if appropriate. But the fallback path after the retry loop always returned 429, regardless of what the last error status actually was.
- **Impact**: When all keys failed with 401 (invalid credentials), clients received a misleading 429 (rate limited) error instead of the correct 401 (authentication error). This made debugging credential issues impossible for API consumers and violated HTTP semantics.
- **Fix**: Added a `lastStatusCode` check to the fallback path, mirroring the logic already present in the `PickKey()` error handler. If `lastStatusCode` is 401 or 403, return that status code instead of 429.

```go
// After the retry loop, if all keys are exhausted
if lastStatusCode == http.StatusUnauthorized || lastStatusCode == http.StatusForbidden {
    http.Error(w, "All API keys invalid", lastStatusCode)
    return
}
http.Error(w, "All API keys exhausted", http.StatusTooManyRequests)
```

---

### BUG-4: Test MaskKey expected wrong masking result

- **Severity**: Low
- **File**: `main_test.go`, `TestMaskKey` and `TestNewKeyRotator`
- **Symptom**: `MaskKey("sk-abcdefghijklmnop")` returned `"sk-ab...nop"` but the test expected `"sk-abc...opq"`.
- **Root Cause**: The test author miscalculated the masking. The implementation correctly follows the spec: for strings longer than 8 characters, take first 5 characters + "..." + last 3 characters. For `"sk-abcdefghijklmnop"` (16 chars): first 5 = `"sk-ab"`, last 3 = `"nop"`, result = `"sk-ab...nop"`. The test incorrectly assumed first 5 = `"sk-abc"` and last 3 = `"opq"`.
- **Impact**: Test-only bug, no production impact.
- **Fix**: Corrected test expectations to match the actual implementation behavior.

```go
// Before (incorrect)
assert.Equal(t, "sk-abc...opq", MaskKey("sk-abcdefghijklmnop"))

// After (correct)
assert.Equal(t, "sk-ab...nop", MaskKey("sk-abcdefghijklmnop"))
```

---

### BUG-5: Test least_used strategy expectation

- **Severity**: Low
- **File**: `main_test.go`, `TestPickKeyLeastUsed`
- **Symptom**: Third `PickKey()` call with `least_used` strategy returned `key0` but test expected `key1`.
- **Root Cause**: After using key0 once and key1 once, both have `UsageCount=1`. The third call picks the first key with the lowest usage count. Since both are tied at 1, the first key (key0) wins by iteration order. The test incorrectly expected key1.
- **Impact**: Test-only bug, no production impact. The implementation's tie-breaking behavior (first-key-wins) is correct and deterministic.
- **Fix**: Updated test expectation to `key0` with a comment noting the tie-breaking behavior.

```go
// After using key0 and key1 once each, both have UsageCount=1.
// Tie-breaking: first key with lowest count wins (key0).
key, err = rotator.PickKey()
assert.NoError(t, err)
assert.Equal(t, "key0", key.ID)
```

---

### BUG-6: Test PickKeyCooldownExpires used flaky sleep

- **Severity**: Low
- **File**: `main_test.go`, `TestPickKeyCooldownExpires`
- **Symptom**: Test used `time.Sleep(2 * time.Nanosecond)` after `MarkCooldown(key, 1*time.Nanosecond)`, which was unreliable and could fail under CPU load.
- **Root Cause**: Relying on `time.Sleep` for cooldown expiration is flaky. `MarkCooldown` sets `CooldownUntil = time.Now().Add(duration)`, and `PickKey` checks `now.Before(entry.CooldownUntil)`. With nanosecond durations, the check could pass or fail depending on OS scheduling and goroutine timing.
- **Impact**: Test-only flakiness, no production impact.
- **Fix**: Replaced `MarkCooldown` + `time.Sleep` with direct state manipulation. Set `State = KeyCooldown` and `CooldownUntil = time.Now().Add(-time.Second)` (already expired). This makes the test deterministic regardless of system load.

```go
// Before (flaky)
rotator.MarkCooldown(key, 1*time.Nanosecond)
time.Sleep(2 * time.Nanosecond)

// After (deterministic)
key.State = KeyCooldown
key.CooldownUntil = time.Now().Add(-time.Second)
```

---

### BUG-7: 401/403 permanently disabled keys on transient auth failures

- **Severity**: Critical
- **File**: `main.go`, `classifyResponse()` function
- **Symptom**: Both API keys got 401 from OpenCode at the same time (transient auth service hiccup). The router permanently disabled both keys, making it completely unresponsive until manual restart. Health endpoint reported `{"status":"unhealthy","disabled_keys":2,"healthy_keys":0,"upstream":"no_healthy_keys"}`.
- **Root Cause**: `classifyResponse` treated 401/403 as permanent auth failures, calling `MarkDisabled()` which sets `KeyState = KeyDisabled`. Since `MarkSuccess` is guarded to never recover disabled keys, a transient 401 (e.g., brief auth service outage, token rotation delay) permanently killed the key with no recovery path except restart.
- **Impact**: Complete service outage from a single transient auth failure. The router's own resilience mechanism made it *less* resilient than direct API access.
- **Fix**: Changed 401/403 handling from `MarkDisabled` (permanent) to `MarkCooldown` (temporary). Keys now enter cooldown for `cooldown_seconds` and automatically recover. Only `insufficient_quota` on 429 responses permanently disables keys.

```go
// Before (permanent — kills keys on any 401/403)
if statusCode == 401 || statusCode == 403 {
    rotator.MarkDisabled(key)
}

// After (transient — cooldown allows recovery)
if statusCode == 401 || statusCode == 403 {
    rotator.MarkCooldown(key, time.Duration(cfg.CooldownSeconds)*time.Second)
}
```

---

## Lessons Learned

1. **Guard every state transition**: The DISABLED bug shows that even a simple setter like `MarkSuccess` needs guards. State machines should validate transitions, not just blindly assign values.

2. **Error handlers can clobber your data**: The `proxyErrorHandler` bug is a classic case of two code paths writing to the same variable. When using callbacks or hooks (like `httputil.ReverseProxy.ErrorHandler`), always check if the data has already been set by an earlier path.

3. **Keep fallback logic consistent**: The 429/401 mismatch happened because two similar fallback paths had diverged. When you have multiple "all exhausted" or "fallback" branches, extract the logic into a shared helper to avoid drift.

4. **Tests can lie too**: Four of the six bugs were test bugs, not implementation bugs. Tests are code, and code has bugs. When a test fails, always question whether the test itself is wrong before assuming the implementation is broken.

5. **Never use sleep for synchronization**: The nanosecond sleep was a red flag from the start. Time-based tests should manipulate clocks or state directly, never rely on real wall-clock time in unit tests.

6. **Integration tests catch what unit tests miss**: Bugs 1, 2, and 3 were all caught by integration tests that exercised the full request flow. Unit tests alone would not have caught the interaction between `classifyResponse`, `proxyErrorHandler`, and the retry loop.

---

*Report generated during implementation and testing of opencode-smart-router.*
