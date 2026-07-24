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
	"sync"
	"testing"
	"time"
)

// --- Test Helpers ---

func setupTestGlobals(keys []string, strategy string) {
	cfg = &Config{
		ListenAddr:                "0.0.0.0:8080",
		UpstreamURL:               "https://opencode.ai/zen/go",
		Keys:                      keys,
		Strategy:                  strategy,
		CooldownSeconds:           60,
		TimeoutCooldownSeconds:    10,
		HealthCheckTimeoutSeconds: 10,
		UpstreamTimeoutSeconds:    60,
		MaxRequestBodyBytes:       10 * 1024 * 1024,
		ReadyCheckCacheSeconds:    30,
		AdminUser:                 "admin",
		AdminPass:                 "testpass",
		EnablePrometheus:          false,
		EnableLogging:             false,
		LogFile:                   "",
	}
	rotator = NewKeyRotator(keys, strategy)
	resetReadyCache()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func setupTestGlobalsNoAuth(keys []string, strategy string) {
	cfg = &Config{
		ListenAddr:                "0.0.0.0:8080",
		UpstreamURL:               "https://opencode.ai/zen/go",
		Keys:                      keys,
		Strategy:                  strategy,
		CooldownSeconds:           60,
		TimeoutCooldownSeconds:    10,
		HealthCheckTimeoutSeconds: 10,
		UpstreamTimeoutSeconds:    60,
		MaxRequestBodyBytes:       10 * 1024 * 1024,
		ReadyCheckCacheSeconds:    30,
		AdminUser:                 "admin",
		AdminPass:                 "",
		EnablePrometheus:          false,
		EnableLogging:             false,
		LogFile:                   "",
	}
	rotator = NewKeyRotator(keys, strategy)
	resetReadyCache()
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

	if cfg.ListenAddr != "0.0.0.0:8080" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, "0.0.0.0:8080")
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

func TestReadyHandler_UpstreamUnreachable(t *testing.T) {
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
		UpstreamTimeoutSeconds:    60,
		MaxRequestBodyBytes:       10 * 1024 * 1024,
		ReadyCheckCacheSeconds:    30,
		AdminUser:                 "admin",
		AdminPass:                 "testpass",
		EnablePrometheus:          false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	resetReadyCache()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest("GET", "/ready", nil)
	w := httptest.NewRecorder()

	readyHandler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestReadyHandler_HealthyUpstream(t *testing.T) {
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
		UpstreamTimeoutSeconds:    60,
		MaxRequestBodyBytes:       10 * 1024 * 1024,
		ReadyCheckCacheSeconds:    30,
		AdminUser:                 "admin",
		AdminPass:                 "testpass",
		EnablePrometheus:          false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	resetReadyCache()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest("GET", "/ready", nil)
	w := httptest.NewRecorder()

	readyHandler(w, req)

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

func TestReadyHandler_CachesResult(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
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
		UpstreamTimeoutSeconds:    60,
		MaxRequestBodyBytes:       10 * 1024 * 1024,
		ReadyCheckCacheSeconds:    30,
		AdminUser:                 "admin",
		AdminPass:                 "testpass",
		EnablePrometheus:          false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	resetReadyCache()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("GET", "/ready", nil)
		w := httptest.NewRecorder()
		readyHandler(w, req)
	}

	if callCount != 1 {
		t.Errorf("upstream called %d times, want 1 (cached for 30s)", callCount)
	}
}

func TestHealthHandler_LivenessNoUpstreamCall(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamURL:               upstreamURL.String(),
		Keys:                      []string{"key0"},
		Strategy:                  "round_robin",
		CooldownSeconds:           60,
		HealthCheckTimeoutSeconds: 5,
		UpstreamTimeoutSeconds:    60,
		MaxRequestBodyBytes:       10 * 1024 * 1024,
		ReadyCheckCacheSeconds:    30,
		AdminUser:                 "admin",
		AdminPass:                 "testpass",
		EnablePrometheus:          false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	resetReadyCache()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	healthHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if callCount != 0 {
		t.Errorf("upstream called %d times, want 0 (liveness must not call upstream)", callCount)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result["status"] != "alive" {
		t.Errorf("status = %v, want alive", result["status"])
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

	rp := newReverseProxy(upstreamURL, 60)
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

	rp := newReverseProxy(upstreamURL, 60)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (all keys should fail with 401)", w.Code, http.StatusUnauthorized)
	}

	if rotator.keys[0].State != KeyCooldown {
		t.Error("key0 should be in cooldown after 401")
	}
	if rotator.keys[1].State != KeyCooldown {
		t.Error("key1 should be in cooldown after 401")
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

	rp := newReverseProxy(upstreamURL, 60)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d (all keys rate limited)", w.Code, http.StatusTooManyRequests)
	}
}

func TestMarkDisabledPreventsRecovery(t *testing.T) {
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

func TestInsufficientQuotaTriggersLongCooldown(t *testing.T) {
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
	if key.State != KeyCooldown {
		t.Errorf("insufficient_quota should put key in cooldown, got state %d", key.State)
	}
	key.mu.Unlock()

	if holder.result == nil {
		t.Fatal("holder.result should not be nil")
	}
	if !holder.result.ShouldRetry {
		t.Error("ShouldRetry = false, want true for insufficient_quota")
	}
}

func TestFailoverOn5xx(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.Header.Get("Authorization") == "Bearer key0" {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":{"message":"bad gateway"}}`)) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"chatcmpl-123"}`)) //nolint:errcheck
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamURL:            upstreamURL.String(),
		Keys:                   []string{"key0", "key1"},
		Strategy:               "round_robin",
		CooldownSeconds:        60,
		TimeoutCooldownSeconds: 10,
		AdminUser:              "admin",
		AdminPass:              "testpass",
		EnablePrometheus:       false,
	}
	rotator = NewKeyRotator([]string{"key0", "key1"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL, 60)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (502 on key0 should fail over to key1)", w.Code, http.StatusOK)
	}
	if callCount < 2 {
		t.Errorf("callCount = %d, want at least 2 (failover should hit key1)", callCount)
	}
	if body := w.Body.String(); body != `{"id":"chatcmpl-123"}` {
		t.Errorf("body = %q, want key1's success response", body)
	}
}

func TestAllKeys5xx_Returns502(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":{"message":"bad gateway"}}`)) //nolint:errcheck
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamURL:            upstreamURL.String(),
		Keys:                   []string{"key0"},
		Strategy:               "round_robin",
		CooldownSeconds:        60,
		TimeoutCooldownSeconds: 10,
		AdminUser:              "admin",
		AdminPass:              "testpass",
		EnablePrometheus:       false,
	}
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL, 60)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d (all keys failed with 5xx)", w.Code, http.StatusBadGateway)
	}

	var errResp OpenAIError
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("response should be a router-generated error body: %v", err)
	}
	if errResp.Error.Code != "all_keys_exhausted" {
		t.Errorf("error code = %q, want all_keys_exhausted (router-generated, not upstream's body)", errResp.Error.Code)
	}

	key := rotator.keys[0]
	key.mu.Lock()
	if key.State != KeyHealthy {
		t.Error("below the 5xx threshold, key should remain healthy")
	}
	if key.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", key.ConsecutiveFailures)
	}
	key.mu.Unlock()
	if callCount != 1 {
		t.Errorf("callCount = %d, want 1 (single key = single attempt)", callCount)
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
		t.Errorf("key state = %d, want %d (healthy after 2xx success)", key.State, KeyHealthy)
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
		Header:     http.Header{},
	}

	err := classifyResponse(resp)
	if err == nil {
		t.Error("classifyResponse() should return error for 5xx (retry signal)")
	}

	key.mu.Lock()
	if key.State != KeyHealthy {
		t.Errorf("key state = %d, want %d (first 5xx fails over without cooldown)", key.State, KeyHealthy)
	}
	if key.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", key.ConsecutiveFailures)
	}
	key.mu.Unlock()

	if holder.result == nil {
		t.Fatal("holder.result should not be nil")
	}
	if !holder.result.ShouldRetry {
		t.Error("ShouldRetry = false, want true for 5xx (failover to next key)")
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
	if key.State != KeyCooldown {
		t.Errorf("key state = %d, want %d (cooldown)", key.State, KeyCooldown)
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
	if key.State != KeyCooldown {
		t.Errorf("key state = %d, want %d (cooldown)", key.State, KeyCooldown)
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

	rp := newReverseProxy(upstreamURL, 60)
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

	rp := newReverseProxy(upstreamURL, 60)
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

// --- Streaming (SSE) Tests ---

func TestIsStreamingRequest(t *testing.T) {
	tests := []struct {
		name      string
		body      []byte
		acceptHdr string
		want      bool
	}{
		{"stream true in body", []byte(`{"model":"gpt-4","stream":true}`), "", true},
		{"stream false in body", []byte(`{"model":"gpt-4","stream":false}`), "", false},
		{"no stream field", []byte(`{"model":"gpt-4"}`), "", false},
		{"nil body", nil, "", false},
		{"accept event-stream", nil, "text/event-stream", true},
		{"accept json", nil, "application/json", false},
		{"invalid json body", []byte(`{invalid`), "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.acceptHdr != "" {
				h.Set("Accept", tt.acceptHdr)
			}
			got := isStreamingRequest(tt.body, h)
			if got != tt.want {
				t.Errorf("isStreamingRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStreamingResponse_ForwardedDirectly(t *testing.T) {
	sseChunks := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n",
		"data: [DONE]\n\n",
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream server doesn't support flushing")
		}
		for _, chunk := range sseChunks {
			w.Write([]byte(chunk)) //nolint:errcheck
			flusher.Flush()
		}
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

	rp := newReverseProxy(upstreamURL, 60)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	expected := strings.Join(sseChunks, "")
	if w.Body.String() != expected {
		t.Errorf("body = %q, want %q", w.Body.String(), expected)
	}
}

func TestStreamingResponse_RetryOn429BeforeStream(t *testing.T) {
	callCount := 0
	sseResponse := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		auth := r.Header.Get("Authorization")
		if auth == "Bearer key0" {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limit","type":"rate_limit","code":"rate_limit_exceeded"}}`)) //nolint:errcheck
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sseResponse)) //nolint:errcheck
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

	rp := newReverseProxy(upstreamURL, 60)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (should retry 429 then stream)", w.Code, http.StatusOK)
	}
	if callCount < 2 {
		t.Errorf("callCount = %d, want at least 2 (retry then success)", callCount)
	}
	if w.Body.String() != sseResponse {
		t.Errorf("body = %q, want %q (SSE stream)", w.Body.String(), sseResponse)
	}
}

// --- Body Limit Tests ---

func TestRequestBodyLimit_RejectsOversizedBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"ok"}`)) //nolint:errcheck
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
	cfg.MaxRequestBodyBytes = 100
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL, 60)
	handler := proxyHandler(rp, rotator)

	oversizedBody := strings.Repeat("x", 200)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(oversizedBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d (body exceeds limit)", w.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestRequestBodyLimit_AllowsUnderLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"ok"}`)) //nolint:errcheck
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
	cfg.MaxRequestBodyBytes = 1024
	rotator = NewKeyRotator([]string{"key0"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL, 60)
	handler := proxyHandler(rp, rotator)

	smallBody := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(smallBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body under limit)", w.Code, http.StatusOK)
	}
}

// --- Config Default Tests for New Fields ---

func TestLoadConfigNewFieldDefaults(t *testing.T) {
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

	if cfg.UpstreamTimeoutSeconds != 60 {
		t.Errorf("UpstreamTimeoutSeconds = %d, want 60", cfg.UpstreamTimeoutSeconds)
	}
	if cfg.MaxRequestBodyBytes != 10*1024*1024 {
		t.Errorf("MaxRequestBodyBytes = %d, want %d", cfg.MaxRequestBodyBytes, 10*1024*1024)
	}
	if cfg.ReadyCheckCacheSeconds != 30 {
		t.Errorf("ReadyCheckCacheSeconds = %d, want 30", cfg.ReadyCheckCacheSeconds)
	}
	if cfg.TimeoutCooldownSeconds != 10 {
		t.Errorf("TimeoutCooldownSeconds = %d, want 10", cfg.TimeoutCooldownSeconds)
	}
}

// --- MarkEnabled Tests ---

func TestMarkEnabled_RecoversDisabledKey(t *testing.T) {
	setupTestGlobals([]string{"k0", "k1"}, "round_robin")

	rotator.MarkDisabled(rotator.keys[0])
	if rotator.keys[0].State != KeyDisabled {
		t.Fatalf("setup: state = %d, want %d", rotator.keys[0].State, KeyDisabled)
	}

	rotator.MarkEnabled(rotator.keys[0])

	if rotator.keys[0].State != KeyHealthy {
		t.Errorf("after MarkEnabled: state = %d, want %d", rotator.keys[0].State, KeyHealthy)
	}
	if !rotator.keys[0].CooldownUntil.IsZero() {
		t.Error("cooldown_until should be zero after MarkEnabled")
	}
}

// --- ApplyDisabledIndices Tests ---

func TestApplyDisabledIndices_MarksSpecifiedKeys(t *testing.T) {
	setupTestGlobals([]string{"k0", "k1", "k2"}, "round_robin")

	if err := rotator.ApplyDisabledIndices([]int{0, 2}); err != nil {
		t.Fatalf("ApplyDisabledIndices error: %v", err)
	}

	if rotator.keys[0].State != KeyDisabled {
		t.Errorf("keys[0] state = %d, want %d", rotator.keys[0].State, KeyDisabled)
	}
	if rotator.keys[1].State != KeyHealthy {
		t.Errorf("keys[1] state = %d, want %d", rotator.keys[1].State, KeyHealthy)
	}
	if rotator.keys[2].State != KeyDisabled {
		t.Errorf("keys[2] state = %d, want %d", rotator.keys[2].State, KeyDisabled)
	}
}

func TestApplyDisabledIndices_OutOfRange(t *testing.T) {
	setupTestGlobals([]string{"k0", "k1"}, "round_robin")

	err := rotator.ApplyDisabledIndices([]int{5})
	if err == nil {
		t.Fatal("expected error for out-of-range index, got nil")
	}
}

// --- Config DisabledKeyIndices Validation ---

func TestConfigValidation_DisabledKeysOutOfRange(t *testing.T) {
	c := Config{Keys: []string{"k0", "k1"}, Strategy: "round_robin", DisabledKeyIndices: []int{5}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for out-of-range disabled_keys index, got nil")
	}
}

func TestConfigValidation_DisabledKeysValid(t *testing.T) {
	c := Config{Keys: []string{"k0", "k1"}, Strategy: "round_robin", DisabledKeyIndices: []int{0, 1}}
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error for valid disabled_keys: %v", err)
	}
}

// --- Disable Key Handler Tests ---

func TestDisableKeyHandler_ValidIndex(t *testing.T) {
	setupTestGlobals([]string{"k0", "k1"}, "round_robin")

	req := httptest.NewRequest("POST", "/admin/keys/1/disable", nil)
	req.SetPathValue("index", "1")
	w := httptest.NewRecorder()

	disableKeyHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if rotator.keys[1].State != KeyDisabled {
		t.Errorf("keys[1] state = %d, want %d", rotator.keys[1].State, KeyDisabled)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result["state"] != "disabled" {
		t.Errorf("response state = %v, want disabled", result["state"])
	}
	if result["index"].(float64) != 1 {
		t.Errorf("response index = %v, want 1", result["index"])
	}
}

func TestDisableKeyHandler_OutOfRange(t *testing.T) {
	setupTestGlobals([]string{"k0"}, "round_robin")

	req := httptest.NewRequest("POST", "/admin/keys/5/disable", nil)
	req.SetPathValue("index", "5")
	w := httptest.NewRecorder()

	disableKeyHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestDisableKeyHandler_NonNumericIndex(t *testing.T) {
	setupTestGlobals([]string{"k0"}, "round_robin")

	req := httptest.NewRequest("POST", "/admin/keys/abc/disable", nil)
	req.SetPathValue("index", "abc")
	w := httptest.NewRecorder()

	disableKeyHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// --- Enable Key Handler Tests ---

func TestEnableKeyHandler_RecoversDisabledKey(t *testing.T) {
	setupTestGlobals([]string{"k0", "k1"}, "round_robin")
	rotator.MarkDisabled(rotator.keys[1])

	req := httptest.NewRequest("POST", "/admin/keys/1/enable", nil)
	req.SetPathValue("index", "1")
	w := httptest.NewRecorder()

	enableKeyHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if rotator.keys[1].State != KeyHealthy {
		t.Errorf("keys[1] state = %d, want %d", rotator.keys[1].State, KeyHealthy)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result["state"] != "healthy" {
		t.Errorf("response state = %v, want healthy", result["state"])
	}
}

func TestEnableKeyHandler_OutOfRange(t *testing.T) {
	setupTestGlobals([]string{"k0"}, "round_robin")

	req := httptest.NewRequest("POST", "/admin/keys/9/enable", nil)
	req.SetPathValue("index", "9")
	w := httptest.NewRecorder()

	enableKeyHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// --- Cooldown Key Handler Tests ---

func TestCooldownKeyHandler_ValidBody(t *testing.T) {
	setupTestGlobals([]string{"k0", "k1"}, "round_robin")

	req := httptest.NewRequest("POST", "/admin/keys/0/cooldown", strings.NewReader(`{"seconds": 3600}`))
	req.SetPathValue("index", "0")
	w := httptest.NewRecorder()

	cooldownKeyHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if rotator.keys[0].State != KeyCooldown {
		t.Errorf("keys[0] state = %d, want %d", rotator.keys[0].State, KeyCooldown)
	}
	if !rotator.keys[0].CooldownUntil.After(time.Now().Add(3500 * time.Second)) {
		t.Error("cooldown_until not set far enough into the future")
	}
	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result["cooldown_seconds"].(float64) != 3600 {
		t.Errorf("cooldown_seconds = %v, want 3600", result["cooldown_seconds"])
	}
}

func TestCooldownKeyHandler_InvalidJSON(t *testing.T) {
	setupTestGlobals([]string{"k0"}, "round_robin")

	req := httptest.NewRequest("POST", "/admin/keys/0/cooldown", strings.NewReader("not json"))
	req.SetPathValue("index", "0")
	w := httptest.NewRecorder()

	cooldownKeyHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestCooldownKeyHandler_NonPositiveSeconds(t *testing.T) {
	setupTestGlobals([]string{"k0"}, "round_robin")

	req := httptest.NewRequest("POST", "/admin/keys/0/cooldown", strings.NewReader(`{"seconds": 0}`))
	req.SetPathValue("index", "0")
	w := httptest.NewRecorder()

	cooldownKeyHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// --- Stats Handler New Fields Tests ---

func TestStatsHandler_IncludesIndexAndCooldownUntil(t *testing.T) {
	setupTestGlobals([]string{"k0", "k1"}, "round_robin")
	rotator.MarkCooldown(rotator.keys[1], 60*time.Second)

	req := httptest.NewRequest("GET", "/admin/stats", nil)
	w := httptest.NewRecorder()

	statsHandler(w, req)

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	keys := result["keys"].([]interface{})

	key0 := keys[0].(map[string]interface{})
	if key0["index"].(float64) != 0 {
		t.Errorf("keys[0] index = %v, want 0", key0["index"])
	}
	if key0["cooldown_until"] != nil {
		t.Errorf("keys[0] cooldown_until = %v, want nil", key0["cooldown_until"])
	}

	key1 := keys[1].(map[string]interface{})
	if key1["index"].(float64) != 1 {
		t.Errorf("keys[1] index = %v, want 1", key1["index"])
	}
	if key1["cooldown_until"] == nil {
		t.Error("keys[1] cooldown_until should be set after MarkCooldown")
	}
}

// --- Admin Key Control Mux Routing (E2E) ---

func TestAdminKeyControl_MuxRoutingAndAuth(t *testing.T) {
	setupTestGlobals([]string{"k0", "k1"}, "round_robin")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/keys/{index}/disable", basicAuthMiddleware(disableKeyHandler))
	mux.HandleFunc("POST /admin/keys/{index}/enable", basicAuthMiddleware(enableKeyHandler))

	req := httptest.NewRequest("POST", "/admin/keys/1/disable", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no auth: status = %d, want %d", w.Code, http.StatusUnauthorized)
	}

	req = httptest.NewRequest("POST", "/admin/keys/1/disable", nil)
	req.SetBasicAuth("admin", "testpass")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("with auth disable: status = %d, want %d", w.Code, http.StatusOK)
	}
	if rotator.keys[1].State != KeyDisabled {
		t.Errorf("after disable: keys[1] state = %d, want %d", rotator.keys[1].State, KeyDisabled)
	}

	req = httptest.NewRequest("POST", "/admin/keys/1/enable", nil)
	req.SetBasicAuth("admin", "testpass")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("with auth enable: status = %d, want %d", w.Code, http.StatusOK)
	}
	if rotator.keys[1].State != KeyHealthy {
		t.Errorf("after enable: keys[1] state = %d, want %d", rotator.keys[1].State, KeyHealthy)
	}
}

func TestAdminKeyControl_MuxRejectsWrongMethod(t *testing.T) {
	setupTestGlobals([]string{"k0", "k1"}, "round_robin")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/keys/{index}/disable", basicAuthMiddleware(disableKeyHandler))

	req := httptest.NewRequest("GET", "/admin/keys/1/disable", nil)
	req.SetBasicAuth("admin", "testpass")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on POST-only route: status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// --- Failover and Backoff Tests (peak-hour 502 incident) ---

func TestBackoffDuration(t *testing.T) {
	base := 10 * time.Second
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 10 * time.Second}, // defensive: no failure recorded yet
		{1, 10 * time.Second},
		{2, 30 * time.Second},
		{3, 60 * time.Second},
		{4, 120 * time.Second},
		{5, 300 * time.Second},
		{6, 300 * time.Second},   // multiplier capped at last step
		{100, 300 * time.Second}, // never exceeds maxFailureCooldown
	}
	for _, tc := range cases {
		if got := backoffDuration(base, tc.failures); got != tc.want {
			t.Errorf("backoffDuration(10s, %d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}

func TestRecordFailure_Increments(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")
	key := rotator.keys[0]

	for want := 1; want <= 3; want++ {
		if got := rotator.RecordFailure(key); got != want {
			t.Errorf("RecordFailure() = %d, want %d", got, want)
		}
	}
}

func TestMarkSuccess_ResetsConsecutiveFailures(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")
	key := rotator.keys[0]

	rotator.RecordFailure(key)
	rotator.RecordFailure(key)
	rotator.MarkSuccess(key)

	key.mu.Lock()
	defer key.mu.Unlock()
	if key.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0 after success", key.ConsecutiveFailures)
	}
	if key.State != KeyHealthy {
		t.Errorf("State = %v, want healthy after success", key.State)
	}
}

func TestClassifyResponse_5xx_CooldownAfterThreshold(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")
	key := rotator.keys[0]

	classify502 := func() {
		holder := &classifyHolder{}
		req := httptest.NewRequest("GET", "/v1/chat/completions", nil)
		ctx := context.WithValue(req.Context(), keyCtxKey, key)
		ctx = context.WithValue(ctx, classifyCtxKey, holder)
		ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
		req = req.WithContext(ctx)
		resp := &http.Response{
			StatusCode: 502,
			Request:    req,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{},
		}
		classifyResponse(resp) //nolint:errcheck
	}

	classify502()
	key.mu.Lock()
	if key.State != KeyHealthy {
		t.Errorf("after 1st 5xx: state = %v, want healthy (failover only, no cooldown)", key.State)
	}
	key.mu.Unlock()

	classify502()
	key.mu.Lock()
	if key.State != KeyHealthy {
		t.Errorf("after 2nd 5xx: state = %v, want healthy (below threshold)", key.State)
	}
	key.mu.Unlock()

	classify502()
	key.mu.Lock()
	defer key.mu.Unlock()
	if key.State != KeyCooldown {
		t.Fatalf("after 3rd 5xx: state = %v, want cooldown", key.State)
	}
	d := time.Until(key.CooldownUntil)
	if d < 55*time.Second || d > 65*time.Second {
		t.Errorf("cooldown = %v, want ~60s (backoff step 3 with 10s base)", d)
	}
}

func TestClassifyResponse_5xx_HonorsRetryAfter(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")
	key := rotator.keys[0]
	rotator.RecordFailure(key)
	rotator.RecordFailure(key)

	holder := &classifyHolder{}
	req := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	ctx := context.WithValue(req.Context(), keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
	req = req.WithContext(ctx)
	resp := &http.Response{
		StatusCode: 503,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{"Retry-After": []string{"120"}},
	}

	classifyResponse(resp) //nolint:errcheck

	key.mu.Lock()
	defer key.mu.Unlock()
	if key.State != KeyCooldown {
		t.Fatalf("state = %v, want cooldown after 3rd consecutive 5xx", key.State)
	}
	d := time.Until(key.CooldownUntil)
	if d < 115*time.Second || d > 125*time.Second {
		t.Errorf("cooldown = %v, want ~120s from Retry-After header", d)
	}
}

func TestFailoverOnUpstreamTimeout(t *testing.T) {
	var mu sync.Mutex
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The stalled key0 handler outlives the failover to key1, so the
		// counter is shared across two concurrent handler goroutines.
		mu.Lock()
		callCount++
		mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer key0" {
			time.Sleep(2 * time.Second) // stall past the 1s header timeout
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"ok"}`)) //nolint:errcheck
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamURL:            upstreamURL.String(),
		Keys:                   []string{"key0", "key1"},
		Strategy:               "round_robin",
		CooldownSeconds:        60,
		TimeoutCooldownSeconds: 10,
		AdminUser:              "admin",
		AdminPass:              "testpass",
		EnablePrometheus:       false,
	}
	rotator = NewKeyRotator([]string{"key0", "key1"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL, 1) // 1s ResponseHeaderTimeout
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	start := time.Now()
	handler(w, req)
	elapsed := time.Since(start)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (timeout on key0 should fail over to key1)", w.Code, http.StatusOK)
	}
	mu.Lock()
	if callCount < 2 {
		t.Errorf("callCount = %d, want at least 2 (timeout then failover)", callCount)
	}
	mu.Unlock()
	if elapsed > 1900*time.Millisecond {
		t.Errorf("elapsed = %v, want < 1.9s (failover at the 1s header timeout, not the 2s stall)", elapsed)
	}

	key := rotator.keys[0]
	key.mu.Lock()
	if key.State != KeyCooldown {
		t.Error("key0 should be in cooldown after a header timeout")
	}
	if key.ConsecutiveFailures != 1 {
		t.Errorf("key0 ConsecutiveFailures = %d, want 1", key.ConsecutiveFailures)
	}
	key.mu.Unlock()
}

func TestProxyErrorHandler_ClientCancelled(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")
	key := rotator.keys[0]
	holder := &classifyHolder{}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ctx, cancel := context.WithCancel(req.Context())
	ctx = context.WithValue(ctx, keyCtxKey, key)
	ctx = context.WithValue(ctx, classifyCtxKey, holder)
	cancel() // client gave up before the upstream answered
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	proxyErrorHandler(w, req, context.Canceled)

	key.mu.Lock()
	if key.State != KeyHealthy {
		t.Errorf("state = %v, want healthy (client cancel is not the key's fault)", key.State)
	}
	if key.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0 (cancel must not count as failure)", key.ConsecutiveFailures)
	}
	key.mu.Unlock()

	if w.Body.Len() != 0 {
		t.Error("nothing should be written to a client that is already gone")
	}
	if holder.result == nil || holder.result.ShouldRetry {
		t.Error("holder.result should be a terminal non-retryable result")
	}
}

func TestProxyErrorHandler_TransportErrorFailsOver(t *testing.T) {
	setupTestGlobals([]string{"key0"}, "round_robin")
	key := rotator.keys[0]

	callHandler := func() *classifyHolder {
		holder := &classifyHolder{}
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		ctx := context.WithValue(req.Context(), keyCtxKey, key)
		ctx = context.WithValue(ctx, classifyCtxKey, holder)
		req = req.WithContext(ctx)
		w := httptest.NewRecorder()
		proxyErrorHandler(w, req, fmt.Errorf("net/http: timeout awaiting response headers"))
		if w.Body.Len() != 0 {
			t.Error("nothing should be written before the retry loop decides")
		}
		return holder
	}

	holder := callHandler()
	if holder.result == nil || !holder.result.ShouldRetry {
		t.Fatal("transport error should mark the attempt retryable (failover)")
	}
	if holder.result.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want 502", holder.result.StatusCode)
	}

	key.mu.Lock()
	if key.State != KeyCooldown {
		t.Errorf("state = %v, want cooldown after transport timeout", key.State)
	}
	first := time.Until(key.CooldownUntil)
	key.mu.Unlock()
	if first < 9*time.Second || first > 11*time.Second {
		t.Errorf("first cooldown = %v, want ~10s (backoff step 1)", first)
	}

	callHandler()
	key.mu.Lock()
	second := time.Until(key.CooldownUntil)
	failures := key.ConsecutiveFailures
	key.mu.Unlock()
	if failures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2", failures)
	}
	if second < 29*time.Second || second > 31*time.Second {
		t.Errorf("second cooldown = %v, want ~30s (backoff step 2)", second)
	}
}

func TestStreamingResponse_FailoverOn502BeforeStream(t *testing.T) {
	callCount := 0
	sseResponse := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.Header.Get("Authorization") == "Bearer key0" {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":{"message":"bad gateway"}}`)) //nolint:errcheck
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sseResponse)) //nolint:errcheck
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	cfg = &Config{
		UpstreamURL:            upstreamURL.String(),
		Keys:                   []string{"key0", "key1"},
		Strategy:               "round_robin",
		CooldownSeconds:        60,
		TimeoutCooldownSeconds: 10,
		AdminUser:              "admin",
		AdminPass:              "testpass",
		EnablePrometheus:       false,
	}
	rotator = NewKeyRotator([]string{"key0", "key1"}, "round_robin")
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rp := newReverseProxy(upstreamURL, 60)
	handler := proxyHandler(rp, rotator)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (502 on key0 should fail over, then stream)", w.Code, http.StatusOK)
	}
	if callCount < 2 {
		t.Errorf("callCount = %d, want at least 2 (failover then stream)", callCount)
	}
	if w.Body.String() != sseResponse {
		t.Errorf("body = %q, want %q (SSE stream from key1)", w.Body.String(), sseResponse)
	}
}
