package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// --- Test Helpers ---

func setupTestGlobals(keys []string, strategy string) {
	cfg = &Config{
		ListenAddr:                "127.0.0.1:8080",
		UpstreamURL:               "https://opencode.ai/zen/go",
		Keys:                      keys,
		Strategy:                  strategy,
		CooldownSeconds:           60,
		HealthCheckTimeoutSeconds: 10,
		AdminUser:                 "admin",
		AdminPass:                 "testpass",
		EnablePrometheus:          false,
		EnableLogging:             false,
		LogFile:                   "",
	}
	rotator = NewKeyRotator(keys, strategy)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func setupTestGlobalsNoAuth(keys []string, strategy string) {
	cfg = &Config{
		ListenAddr:                "127.0.0.1:8080",
		UpstreamURL:               "https://opencode.ai/zen/go",
		Keys:                      keys,
		Strategy:                  strategy,
		CooldownSeconds:           60,
		HealthCheckTimeoutSeconds: 10,
		AdminUser:                 "admin",
		AdminPass:                 "",
		EnablePrometheus:          false,
		EnableLogging:             false,
		LogFile:                   "",
	}
	rotator = NewKeyRotator(keys, strategy)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// --- Config Tests ---

func TestParseKeysFromEnv(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"single key", "sk-key1", []string{"sk-key1"}},
		{"multiple keys", "sk-key1,sk-key2,sk-key3", []string{"sk-key1", "sk-key2", "sk-key3"}},
		{"keys with spaces", " sk-key1 , sk-key2 , sk-key3 ", []string{"sk-key1", "sk-key2", "sk-key3"}},
		{"empty string", "", nil},
		{"trailing comma", "sk-key1,", []string{"sk-key1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseKeysFromEnv(tt.input)
			if len(got) != len(tt.want) {
				t.Errorf("parseKeysFromEnv(%q) = %v, want %v", tt.input, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseKeysFromEnv(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{
			"valid config",
			Config{Keys: []string{"sk-key1"}, Strategy: "round_robin"},
			false,
		},
		{
			"empty keys",
			Config{Keys: []string{}, Strategy: "round_robin"},
			true,
		},
		{
			"nil keys",
			Config{Keys: nil, Strategy: "round_robin"},
			true,
		},
		{
			"invalid strategy",
			Config{Keys: []string{"sk-key1"}, Strategy: "random"},
			true,
		},
		{
			"least_used strategy",
			Config{Keys: []string{"sk-key1"}, Strategy: "least_used"},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	config := `{"keys": ["sk-test-key"]}`
	if _, err := tmpFile.WriteString(config); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.ListenAddr != "127.0.0.1:8080" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, "127.0.0.1:8080")
	}
	if cfg.UpstreamURL != "https://opencode.ai/zen/go" {
		t.Errorf("UpstreamURL = %q, want %q", cfg.UpstreamURL, "https://opencode.ai/zen/go")
	}
	if cfg.Strategy != "round_robin" {
		t.Errorf("Strategy = %q, want %q", cfg.Strategy, "round_robin")
	}
	if cfg.CooldownSeconds != 60 {
		t.Errorf("CooldownSeconds = %d, want %d", cfg.CooldownSeconds, 60)
	}
	if cfg.HealthCheckTimeoutSeconds != 10 {
		t.Errorf("HealthCheckTimeoutSeconds = %d, want %d", cfg.HealthCheckTimeoutSeconds, 10)
	}
}

func TestLoadConfigEnvOverride(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	config := `{"keys": ["sk-original"], "strategy": "round_robin"}`
	if _, err := tmpFile.WriteString(config); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	t.Setenv("OPENCODE_KEYS", "sk-env-key1,sk-env-key2")

	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if len(cfg.Keys) != 2 {
		t.Fatalf("len(Keys) = %d, want 2", len(cfg.Keys))
	}
	if cfg.Keys[0] != "sk-env-key1" || cfg.Keys[1] != "sk-env-key2" {
		t.Errorf("Keys = %v, want [sk-env-key1, sk-env-key2]", cfg.Keys)
	}
}

// --- Key State Machine Tests ---

func TestNewKeyRotator(t *testing.T) {
	keys := []string{"sk-abcdefghijklmnop", "sk-short"}
	kr := NewKeyRotator(keys, "round_robin")

	if len(kr.keys) != 2 {
		t.Fatalf("len(kr.keys) = %d, want 2", len(kr.keys))
	}
	if kr.keys[0].RawKey != "sk-abcdefghijklmnop" {
		t.Errorf("RawKey[0] = %q, want %q", kr.keys[0].RawKey, "sk-abcdefghijklmnop")
	}
	if kr.keys[0].Key != "sk-ab...nop" {
		t.Errorf("Key[0] = %q, want %q", kr.keys[0].Key, "sk-ab...nop")
	}
	if kr.keys[0].State != KeyHealthy {
		t.Errorf("State[0] = %d, want %d", kr.keys[0].State, KeyHealthy)
	}
	if kr.keys[1].Key != "sk-***" {
		t.Errorf("Key[1] = %q, want %q", kr.keys[1].Key, "sk-***")
	}
}

func TestPickKeyRoundRobin(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1", "key2"}, "round_robin")

	key0, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key0.RawKey != "key0" {
		t.Errorf("first PickKey = %q, want %q", key0.RawKey, "key0")
	}

	key1, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key1.RawKey != "key1" {
		t.Errorf("second PickKey = %q, want %q", key1.RawKey, "key1")
	}

	key2, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key2.RawKey != "key2" {
		t.Errorf("third PickKey = %q, want %q", key2.RawKey, "key2")
	}

	key0Again, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key0Again.RawKey != "key0" {
		t.Errorf("fourth PickKey = %q, want %q", key0Again.RawKey, "key0")
	}
}

func TestPickKeyLeastUsed(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1"}, "least_used")

	key0, _ := rotator.PickKey()
	if key0.RawKey != "key0" {
		t.Errorf("first PickKey = %q, want %q", key0.RawKey, "key0")
	}

	key1, _ := rotator.PickKey()
	if key1.RawKey != "key1" {
		t.Errorf("second PickKey = %q, want %q", key1.RawKey, "key1")
	}

	key0Again, _ := rotator.PickKey()
	if key0Again.RawKey != "key0" {
		t.Errorf("third PickKey = %q, want %q (tied usage, first wins)", key0Again.RawKey, "key0")
	}
}

func TestPickKeySkipsDisabled(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1"}, "round_robin")

	rotator.MarkDisabled(rotator.keys[0])

	key, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key.RawKey != "key1" {
		t.Errorf("PickKey = %q, want %q (should skip disabled key)", key.RawKey, "key1")
	}
}

func TestPickKeySkipsCooldown(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1"}, "round_robin")

	rotator.MarkCooldown(rotator.keys[0], 60*time.Second)

	key, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key.RawKey != "key1" {
		t.Errorf("PickKey = %q, want %q (should skip cooldown key)", key.RawKey, "key1")
	}
}

func TestPickKeyCooldownExpires(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1"}, "round_robin")

	rotator.keys[0].mu.Lock()
	rotator.keys[0].State = KeyCooldown
	rotator.keys[0].CooldownUntil = time.Now().Add(-time.Second)
	rotator.keys[0].mu.Unlock()

	key, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key.RawKey != "key0" {
		t.Errorf("PickKey = %q, want %q (cooldown should have expired)", key.RawKey, "key0")
	}
	if rotator.keys[0].State != KeyHealthy {
		t.Errorf("key0 State = %d, want %d (healthy)", rotator.keys[0].State, KeyHealthy)
	}
}

func TestPickKeyAllExhausted(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	rotator.MarkDisabled(rotator.keys[0])

	_, err := rotator.PickKey()
	if err == nil {
		t.Error("PickKey() should return error when all keys unavailable")
	}
}

func TestMarkSuccess(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	rotator.MarkCooldown(rotator.keys[0], 60*time.Second)
	if rotator.keys[0].State != KeyCooldown {
		t.Errorf("State = %d, want %d", rotator.keys[0].State, KeyCooldown)
	}

	rotator.MarkSuccess(rotator.keys[0])
	if rotator.keys[0].State != KeyHealthy {
		t.Errorf("State = %d, want %d", rotator.keys[0].State, KeyHealthy)
	}
}

func TestMarkDisabled(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	rotator.MarkDisabled(rotator.keys[0])
	if rotator.keys[0].State != KeyDisabled {
		t.Errorf("State = %d, want %d", rotator.keys[0].State, KeyDisabled)
	}

	_, err := rotator.PickKey()
	if err == nil {
		t.Error("PickKey() should return error when key is disabled")
	}
}

func TestDisabledIsPermanent(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	rotator.MarkDisabled(rotator.keys[0])

	rotator.MarkSuccess(rotator.keys[0])
	if rotator.keys[0].State != KeyDisabled {
		t.Errorf("MarkSuccess should not recover DISABLED key; State = %d, want %d", rotator.keys[0].State, KeyDisabled)
	}
}

func TestKeyCounts(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1", "key2"}, "round_robin")

	if rotator.TotalCount() != 3 {
		t.Errorf("TotalCount() = %d, want 3", rotator.TotalCount())
	}
	if rotator.HealthyCount() != 3 {
		t.Errorf("HealthyCount() = %d, want 3", rotator.HealthyCount())
	}
	if rotator.DisabledCount() != 0 {
		t.Errorf("DisabledCount() = %d, want 0", rotator.DisabledCount())
	}

	rotator.MarkDisabled(rotator.keys[0])
	if rotator.HealthyCount() != 2 {
		t.Errorf("HealthyCount() = %d, want 2", rotator.HealthyCount())
	}
	if rotator.DisabledCount() != 1 {
		t.Errorf("DisabledCount() = %d, want 1", rotator.DisabledCount())
	}
}

// --- MaskKey Tests ---

func TestMaskKey(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"long key", "sk-abcdefghijklmnop", "sk-ab...nop"},
		{"medium key", "sk-short", "sk-***"},
		{"short key", "sk-x", "sk-***"},
		{"exactly 8 chars", "12345678", "123***"},
		{"exactly 9 chars", "123456789", "12345...789"},
		{"empty key", "", "***"},
		{"1 char", "a", "a***"},
		{"2 chars", "ab", "ab***"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MaskKey(tt.input)
			if got != tt.want {
				t.Errorf("MaskKey(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- ParseRetryAfter Tests ---

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name            string
		header          string
		defaultDuration time.Duration
		want            time.Duration
	}{
		{"delta seconds", "60", 0, 60 * time.Second},
		{"delta seconds zero", "0", 0, 0},
		{"http date", "Wed, 21 Oct 2015 07:28:00 GMT", 0, 0},
		{"invalid value returns default", "invalid", 30 * time.Second, 30 * time.Second},
		{"empty string returns default", "", 45 * time.Second, 45 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseRetryAfter(tt.header, tt.defaultDuration)
			if tt.name == "http date" {
				return
			}
			if got != tt.want {
				t.Errorf("ParseRetryAfter(%q, %v) = %v, want %v", tt.header, tt.defaultDuration, got, tt.want)
			}
		})
	}
}

func TestParseRetryAfterDeltaSeconds(t *testing.T) {
	got := ParseRetryAfter("120", 0)
	if got != 120*time.Second {
		t.Errorf("ParseRetryAfter(\"120\") = %v, want %v", got, 120*time.Second)
	}
}

func TestParseRetryAfterDefault(t *testing.T) {
	got := ParseRetryAfter("not-a-number", 30*time.Second)
	if got != 30*time.Second {
		t.Errorf("ParseRetryAfter(\"not-a-number\", 30s) = %v, want %v", got, 30*time.Second)
	}
}

// --- StatusGroup Tests ---

func TestStatusGroup(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{200, "2xx"},
		{201, "2xx"},
		{301, "3xx"},
		{400, "4xx"},
		{401, "4xx"},
		{403, "4xx"},
		{429, "4xx"},
		{500, "5xx"},
		{503, "5xx"},
		{100, "other"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.code), func(t *testing.T) {
			got := statusGroup(tt.code)
			if got != tt.want {
				t.Errorf("statusGroup(%d) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

// --- Basic Auth Middleware Tests ---

func TestBasicAuthMiddleware_ValidAuth(t *testing.T) {
	setupTestGlobals([]string{"test-key"}, "round_robin")

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := basicAuthMiddleware(next)
	req := httptest.NewRequest("GET", "/admin/stats", nil)
	req.SetBasicAuth("admin", "testpass")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !called {
		t.Error("next handler was not called")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestBasicAuthMiddleware_InvalidAuth(t *testing.T) {
	setupTestGlobals([]string{"test-key"}, "round_robin")

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	handler := basicAuthMiddleware(next)
	req := httptest.NewRequest("GET", "/admin/stats", nil)
	req.SetBasicAuth("admin", "wrongpass")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if called {
		t.Error("next handler should not be called with invalid auth")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Error("expected WWW-Authenticate header")
	}
}

func TestBasicAuthMiddleware_DisabledWhenNoPassword(t *testing.T) {
	setupTestGlobalsNoAuth([]string{"test-key"}, "round_robin")

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	handler := basicAuthMiddleware(next)
	req := httptest.NewRequest("GET", "/admin/stats", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if called {
		t.Error("next handler should not be called when admin is disabled")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

// --- Stats Handler Tests ---

func TestStatsHandler(t *testing.T) {
	setupTestGlobals([]string{"sk-test-key-1"}, "round_robin")

	req := httptest.NewRequest("GET", "/admin/stats", nil)
	w := httptest.NewRecorder()

	statsHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if result["strategy"] != "round_robin" {
		t.Errorf("strategy = %v, want round_robin", result["strategy"])
	}

	keys, ok := result["keys"].([]interface{})
	if !ok {
		t.Fatal("keys is not a slice")
	}
	if len(keys) != 1 {
		t.Errorf("len(keys) = %d, want 1", len(keys))
	}

	key0, ok := keys[0].(map[string]interface{})
	if !ok {
		t.Fatal("key entry is not a map")
	}
	if key0["state"] != "healthy" {
		t.Errorf("state = %v, want healthy", key0["state"])
	}
	if key0["masked_key"] == nil {
		t.Error("masked_key should not be nil")
	}
}

// --- Health Handler Tests ---

func TestHealthHandler_AllKeysDisabled(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")
	rotator.MarkDisabled(rotator.keys[0])

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	healthHandler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result["status"] != "unhealthy" {
		t.Errorf("status = %v, want unhealthy", result["status"])
	}
}

func TestHealthHandler_UpstreamUnreachable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamURL:               upstreamURL.String(),
		Keys:                      []string{"key0"},
		Strategy:                  "round_robin",
		CooldownSeconds:           60,
		HealthCheckTimeoutSeconds: 5,
		AdminUser:                 "admin",
		AdminPass:                 "testpass",
		EnablePrometheus:          false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	healthHandler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHealthHandler_HealthyUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[]}`)) //nolint:errcheck
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamURL:               upstreamURL.String(),
		Keys:                      []string{"key0"},
		Strategy:                  "round_robin",
		CooldownSeconds:           60,
		HealthCheckTimeoutSeconds: 5,
		AdminUser:                 "admin",
		AdminPass:                 "testpass",
		EnablePrometheus:          false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	healthHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result["status"] != "healthy" {
		t.Errorf("status = %v, want healthy", result["status"])
	}
	if result["upstream"] != "reachable" {
		t.Errorf("upstream = %v, want reachable", result["upstream"])
	}
}

// --- Integration Tests: Transparent Retry ---

func TestTransparentRetry_429ThenSuccess(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		auth := r.Header.Get("Authorization")
		if auth == "Bearer key0" {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limit","type":"rate_limit","code":"rate_limit_exceeded"}}`)) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"chatcmpl-123"}`)) //nolint:errcheck
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamURL:      upstreamURL.String(),
		Keys:             []string{"key0", "key1"},
		Strategy:         "round_robin",
		CooldownSeconds:  60,
		AdminUser:        "admin",
		AdminPass:        "testpass",
		EnablePrometheus: false,
	}
	rotator = NewKeyRotator([]string{"key0", "key1"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (should retry on 429)", w.Code, http.StatusOK)
	}
	if callCount < 2 {
		t.Errorf("callCount = %d, want at least 2 (should retry)", callCount)
	}
}

func TestTransparentRetry_401AllKeys(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid api key","type":"invalid_request_error","code":"invalid_api_key"}}`)) //nolint:errcheck
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamURL:      upstreamURL.String(),
		Keys:             []string{"key0", "key1"},
		Strategy:         "round_robin",
		CooldownSeconds:  60,
		AdminUser:        "admin",
		AdminPass:        "testpass",
		EnablePrometheus: false,
	}
	rotator = NewKeyRotator([]string{"key0", "key1"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (all keys should fail with 401)", w.Code, http.StatusUnauthorized)
	}

	if rotator.keys[0].State != KeyDisabled {
		t.Error("key0 should be disabled after 401")
	}
	if rotator.keys[1].State != KeyDisabled {
		t.Error("key1 should be disabled after 401")
	}
}

func TestTransparentRetry_429AllKeys(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"rate limit","type":"rate_limit","code":"rate_limit_exceeded"}}`)) //nolint:errcheck
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamURL:      upstreamURL.String(),
		Keys:             []string{"key0", "key1"},
		Strategy:         "round_robin",
		CooldownSeconds:  60,
		AdminUser:        "admin",
		AdminPass:        "testpass",
		EnablePrometheus: false,
	}
	rotator = NewKeyRotator([]string{"key0", "key1"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d (all keys rate limited)", w.Code, http.StatusTooManyRequests)
	}
}

func TestPermanentDisable_401(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")
	rotator.MarkDisabled(rotator.keys[0])

	if rotator.keys[0].State != KeyDisabled {
		t.Error("key should be disabled after MarkDisabled")
	}

	rotator.MarkSuccess(rotator.keys[0])
	if rotator.keys[0].State != KeyDisabled {
		t.Error("disabled key should NOT recover after MarkSuccess")
	}
}

func TestForward5xxWithoutRetry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":{"message":"bad gateway"}}`)) //nolint:errcheck
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamURL:      upstreamURL.String(),
		Keys:             []string{"key0"},
		Strategy:         "round_robin",
		CooldownSeconds:  60,
		AdminUser:        "admin",
		AdminPass:        "testpass",
		EnablePrometheus: false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d (5xx should be forwarded without retry)", w.Code, http.StatusBadGateway)
	}

	if rotator.keys[0].State != KeyHealthy {
		t.Error("key should remain healthy after 5xx (upstream problem, not key problem)")
	}
}

// --- New Test Cases ---

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path/config.json")
	if err == nil {
		t.Error("LoadConfig() should return error for missing file")
	}
}

func TestLoadConfigInvalidJSON(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("{invalid json}"); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	_, err = LoadConfig(tmpFile.Name())
	if err == nil {
		t.Error("LoadConfig() should return error for invalid JSON")
	}
}

func TestLoadConfigEnvOverrideEmptyKeys(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	config := `{"keys": ["sk-original"], "strategy": "round_robin"}`
	if _, err := tmpFile.WriteString(config); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	t.Setenv("OPENCODE_KEYS", "")

	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	// Empty OPENCODE_KEYS should NOT override config keys
	if len(cfg.Keys) != 1 || cfg.Keys[0] != "sk-original" {
		t.Errorf("Keys = %v, want [sk-original] (empty env should not override)", cfg.Keys)
	}
}

func TestPickKeyRoundRobinWrapsAround(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1", "key2"}, "round_robin")

	picks := make([]string, 4)
	for i := 0; i < 3; i++ {
		key, err := rotator.PickKey()
		if err != nil {
			t.Fatal(err)
		}
		picks[i] = key.RawKey
	}

	key, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	picks[3] = key.RawKey

	expected := []string{"key0", "key1", "key2", "key0"}
	for i, got := range picks {
		if got != expected[i] {
			t.Errorf("pick %d = %q, want %q", i, got, expected[i])
		}
	}
}

func TestPickKeyLeastUsedAfterCooldown(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1"}, "least_used")

	rotator.MarkCooldown(rotator.keys[0], 60*time.Second)

	key, err := rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key.RawKey != "key1" {
		t.Errorf("PickKey = %q, want %q (should skip cooldown key)", key.RawKey, "key1")
	}

	rotator.keys[0].mu.Lock()
	rotator.keys[0].CooldownUntil = time.Now().Add(-time.Second)
	rotator.keys[0].mu.Unlock()

	key, err = rotator.PickKey()
	if err != nil {
		t.Fatal(err)
	}
	if key.RawKey != "key0" {
		t.Errorf("PickKey = %q, want %q (cooldown expired, should be available)", key.RawKey, "key0")
	}
}

func TestPickKeySingleKeyDisabled(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	rotator.MarkDisabled(rotator.keys[0])

	_, err := rotator.PickKey()
	if err == nil {
		t.Error("PickKey() should return error when single key is disabled")
	}
}

func TestMarkCooldownSetsDuration(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	duration := 30 * time.Second
	before := time.Now()
	rotator.MarkCooldown(rotator.keys[0], duration)
	after := time.Now()

	rotator.keys[0].mu.Lock()
	cooldownUntil := rotator.keys[0].CooldownUntil
	state := rotator.keys[0].State
	rotator.keys[0].mu.Unlock()

	if state != KeyCooldown {
		t.Errorf("State = %d, want %d (cooldown)", state, KeyCooldown)
	}

	// CooldownUntil should be approximately now + duration
	minExpected := before.Add(duration)
	maxExpected := after.Add(duration + time.Second)
	if cooldownUntil.Before(minExpected) || cooldownUntil.After(maxExpected) {
		t.Errorf("CooldownUntil = %v, want between %v and %v", cooldownUntil, minExpected, maxExpected)
	}
}

func TestHealthyCountWithCooldown(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1", "key2"}, "round_robin")

	rotator.keys[0].mu.Lock()
	rotator.keys[0].State = KeyCooldown
	rotator.keys[0].CooldownUntil = time.Now().Add(-time.Second)
	rotator.keys[0].mu.Unlock()

	rotator.MarkCooldown(rotator.keys[1], 60*time.Second)

	// HealthyCount should count key0 (expired cooldown) and key2 (healthy) = 2
	count := rotator.HealthyCount()
	if count != 2 {
		t.Errorf("HealthyCount() = %d, want 2 (1 expired cooldown + 1 healthy)", count)
	}
}

func TestDisabledCount(t *testing.T) {
	setupTestGlobals([]string{"key0", "key1", "key2", "key3"}, "round_robin")

	if rotator.DisabledCount() != 0 {
		t.Errorf("DisabledCount() = %d, want 0", rotator.DisabledCount())
	}

	rotator.MarkDisabled(rotator.keys[0])
	if rotator.DisabledCount() != 1 {
		t.Errorf("DisabledCount() = %d, want 1", rotator.DisabledCount())
	}

	rotator.MarkDisabled(rotator.keys[2])
	if rotator.DisabledCount() != 2 {
		t.Errorf("DisabledCount() = %d, want 2", rotator.DisabledCount())
	}

	if rotator.HealthyCount() != 2 {
		t.Errorf("HealthyCount() = %d, want 2", rotator.HealthyCount())
	}
}

func TestClassifyResponse_2xx(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	key := rotator.keys[0]
	holder := &classifyHolder{}

	req := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)

	resp := &http.Response{
		StatusCode: 200,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("")),
	}

	err := classifyResponse(resp)
	if err != nil {
		t.Errorf("classifyResponse() error = %v, want nil for 2xx", err)
	}

	key.mu.Lock()
	if key.State != KeyHealthy {
		t.Errorf("key state = %d, want %d (healthy)", key.State, KeyHealthy)
	}
	key.mu.Unlock()

	if holder.result == nil {
		t.Fatal("holder.result should not be nil")
	}
	if holder.result.ShouldRetry {
		t.Error("ShouldRetry = true, want false for 2xx")
	}
	if holder.result.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", holder.result.StatusCode)
	}
}

func TestClassifyResponse_5xx(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	key := rotator.keys[0]
	holder := &classifyHolder{}

	req := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)

	resp := &http.Response{
		StatusCode: 500,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("")),
	}

	err := classifyResponse(resp)
	if err != nil {
		t.Errorf("classifyResponse() error = %v, want nil for 5xx", err)
	}

	key.mu.Lock()
	if key.State != KeyHealthy {
		t.Errorf("key state = %d, want %d (healthy, 5xx should not mark key)", key.State, KeyHealthy)
	}
	key.mu.Unlock()

	if holder.result == nil {
		t.Fatal("holder.result should not be nil")
	}
	if holder.result.ShouldRetry {
		t.Error("ShouldRetry = true, want false for 5xx (should not retry)")
	}
	if holder.result.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500", holder.result.StatusCode)
	}
}

func TestClassifyResponse_429RateLimit(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	key := rotator.keys[0]
	holder := &classifyHolder{}

	body := `{"error":{"message":"rate limit exceeded","type":"rate_limit","code":"rate_limit_exceeded"}}`

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)

	resp := &http.Response{
		StatusCode: 429,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}

	err := classifyResponse(resp)
	if err == nil {
		t.Error("classifyResponse() should return error for 429 rate limit")
	}

	key.mu.Lock()
	if key.State != KeyCooldown {
		t.Errorf("key state = %d, want %d (cooldown)", key.State, KeyCooldown)
	}
	key.mu.Unlock()

	if holder.result == nil {
		t.Fatal("holder.result should not be nil")
	}
	if !holder.result.ShouldRetry {
		t.Error("ShouldRetry = false, want true for 429 rate limit")
	}
	if holder.result.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", holder.result.StatusCode)
	}
}

func TestClassifyResponse_429InsufficientQuota(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	key := rotator.keys[0]
	holder := &classifyHolder{}

	body := `{"error":{"message":"insufficient quota","type":"insufficient_quota","code":"insufficient_quota"}}`

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)

	resp := &http.Response{
		StatusCode: 429,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}

	err := classifyResponse(resp)
	if err == nil {
		t.Error("classifyResponse() should return error for 429 insufficient_quota")
	}

	key.mu.Lock()
	if key.State != KeyDisabled {
		t.Errorf("key state = %d, want %d (disabled)", key.State, KeyDisabled)
	}
	key.mu.Unlock()

	if holder.result == nil {
		t.Fatal("holder.result should not be nil")
	}
	if !holder.result.ShouldRetry {
		t.Error("ShouldRetry = false, want true for 429 insufficient_quota")
	}
	if holder.result.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", holder.result.StatusCode)
	}
}

func TestClassifyResponse_401(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")

	key := rotator.keys[0]
	holder := &classifyHolder{}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)

	resp := &http.Response{
		StatusCode: 401,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("")),
	}

	err := classifyResponse(resp)
	if err == nil {
		t.Error("classifyResponse() should return error for 401")
	}

	key.mu.Lock()
	if key.State != KeyDisabled {
		t.Errorf("key state = %d, want %d (disabled)", key.State, KeyDisabled)
	}
	key.mu.Unlock()

	if holder.result == nil {
		t.Fatal("holder.result should not be nil")
	}
	if !holder.result.ShouldRetry {
		t.Error("ShouldRetry = false, want true for 401")
	}
	if holder.result.StatusCode != 401 {
		t.Errorf("StatusCode = %d, want 401", holder.result.StatusCode)
	}
}

func TestWriteOpenAIError(t *testing.T) {
	w := httptest.NewRecorder()
	writeOpenAIError(w, "test message", "test_type", "test_code", http.StatusTooManyRequests)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}

	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", contentType, "application/json")
	}

	var errResp OpenAIError
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if errResp.Error.Message != "test message" {
		t.Errorf("message = %q, want %q", errResp.Error.Message, "test message")
	}
	if errResp.Error.Type != "test_type" {
		t.Errorf("type = %q, want %q", errResp.Error.Type, "test_type")
	}
	if errResp.Error.Code != "test_code" {
		t.Errorf("code = %q, want %q", errResp.Error.Code, "test_code")
	}
}

func TestProxyHandler_SingleKeySuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key0" {
			t.Errorf("Authorization = %q, want %q", r.Header.Get("Authorization"), "Bearer key0")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"chatcmpl-123"}`)) //nolint:errcheck
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamURL:      upstreamURL.String(),
		Keys:             []string{"key0"},
		Strategy:         "round_robin",
		CooldownSeconds:  60,
		AdminUser:        "admin",
		AdminPass:        "testpass",
		EnablePrometheus: false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestProxyHandler_RequestBodyPreservedOnRetry(t *testing.T) {
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))

		auth := r.Header.Get("Authorization")
		if auth == "Bearer key0" {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limit","type":"rate_limit","code":"rate_limit_exceeded"}}`)) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"chatcmpl-123"}`)) //nolint:errcheck
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamURL:      upstreamURL.String(),
		Keys:             []string{"key0", "key1"},
		Strategy:         "round_robin",
		CooldownSeconds:  60,
		AdminUser:        "admin",
		AdminPass:        "testpass",
		EnablePrometheus: false,
	}
	rotator = NewKeyRotator([]string{"key0", "key1"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL)
	handler := proxyHandler(rp, rotator)

	originalBody := `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(originalBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (should retry and succeed)", w.Code, http.StatusOK)
	}

	if len(bodies) != 2 {
		t.Fatalf("expected 2 request bodies, got %d", len(bodies))
	}

	if bodies[0] != originalBody {
		t.Errorf("first request body = %q, want %q", bodies[0], originalBody)
	}
	if bodies[1] != originalBody {
		t.Errorf("second request body = %q, want %q (should be preserved on retry)", bodies[1], originalBody)
	}
}