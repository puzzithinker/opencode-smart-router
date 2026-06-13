package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var version = "dev"

// --- Config ---

type Config struct {
	ListenAddr                string   `json:"listen_addr"`
	UpstreamURL               string   `json:"upstream_url"`
	Keys                      []string `json:"keys"`
	Strategy                  string   `json:"strategy"`
	CooldownSeconds           int      `json:"cooldown_seconds"`
	HealthCheckTimeoutSeconds int      `json:"health_check_timeout_seconds"`
	AdminUser                 string   `json:"admin_user"`
	AdminPass                 string   `json:"admin_pass"`
	EnablePrometheus          bool     `json:"enable_prometheus"`
	EnableLogging             bool     `json:"enable_logging"`
	LogFile                   string   `json:"log_file"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	// Apply defaults
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:8080"
	}
	if cfg.UpstreamURL == "" {
		cfg.UpstreamURL = "https://opencode.ai/zen/go"
	}
	if cfg.Strategy == "" {
		cfg.Strategy = "round_robin"
	}
	if cfg.CooldownSeconds == 0 {
		cfg.CooldownSeconds = 60
	}
	if cfg.HealthCheckTimeoutSeconds == 0 {
		cfg.HealthCheckTimeoutSeconds = 10
	}

	// Env override for keys
	if envKeys := os.Getenv("OPENCODE_KEYS"); envKeys != "" {
		cfg.Keys = parseKeysFromEnv(envKeys)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func parseKeysFromEnv(envValue string) []string {
	parts := strings.Split(envValue, ",")
	keys := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			keys = append(keys, p)
		}
	}
	return keys
}

func (c *Config) Validate() error {
	if len(c.Keys) == 0 {
		return fmt.Errorf("keys must be non-empty (set in config or OPENCODE_KEYS env)")
	}
	if c.Strategy != "round_robin" && c.Strategy != "least_used" {
		return fmt.Errorf("strategy must be 'round_robin' or 'least_used', got: %s", c.Strategy)
	}
	return nil
}

// --- Key State Machine ---

type KeyState int

const (
	KeyHealthy  KeyState = iota
	KeyCooldown
	KeyDisabled
)

func (s KeyState) String() string {
	switch s {
	case KeyHealthy:
		return "healthy"
	case KeyCooldown:
		return "cooldown"
	case KeyDisabled:
		return "disabled"
	default:
		return "unknown"
	}
}

type KeyEntry struct {
	Key           string
	RawKey        string
	State         KeyState
	CooldownUntil time.Time
	UsageCount    int64
	LastUsed      time.Time
	mu            sync.Mutex
}

type KeyRotator struct {
	keys     []*KeyEntry
	strategy string
	counter  atomic.Int64
}

func NewKeyRotator(keys []string, strategy string) *KeyRotator {
	entries := make([]*KeyEntry, len(keys))
	for i, k := range keys {
		entries[i] = &KeyEntry{
			Key:    MaskKey(k),
			RawKey: k,
			State:  KeyHealthy,
		}
	}
	return &KeyRotator{
		keys:     entries,
		strategy: strategy,
	}
}

func (kr *KeyRotator) PickKey() (*KeyEntry, error) {
	now := time.Now()

	switch kr.strategy {
	case "round_robin":
		n := len(kr.keys)
		start := int(kr.counter.Add(1) - 1)
		for i := 0; i < n; i++ {
			idx := (start + i) % n
			entry := kr.keys[idx]
			entry.mu.Lock()
			if entry.State == KeyDisabled {
				entry.mu.Unlock()
				continue
			}
			if entry.State == KeyCooldown && now.Before(entry.CooldownUntil) {
				entry.mu.Unlock()
				continue
			}
			// Key is available (healthy or cooldown expired)
			if entry.State == KeyCooldown {
				entry.State = KeyHealthy
				entry.CooldownUntil = time.Time{}
			}
			entry.UsageCount++
			entry.LastUsed = now
			entry.mu.Unlock()

			if cfg.EnablePrometheus {
				keyUsageTotal.WithLabelValues(entry.Key).Inc()
				keyHealthy.WithLabelValues(entry.Key).Set(1)
			}
			return entry, nil
		}
		return nil, fmt.Errorf("all keys are unavailable")

	case "least_used":
		var best *KeyEntry
		var bestCount int64 = -1
		for _, entry := range kr.keys {
			entry.mu.Lock()
			if entry.State == KeyDisabled {
				entry.mu.Unlock()
				continue
			}
			if entry.State == KeyCooldown && now.Before(entry.CooldownUntil) {
				entry.mu.Unlock()
				continue
			}
			if entry.State == KeyCooldown {
				entry.State = KeyHealthy
				entry.CooldownUntil = time.Time{}
			}
			if bestCount < 0 || entry.UsageCount < bestCount {
				if best != nil {
					best.mu.Unlock()
				}
				bestCount = entry.UsageCount
				best = entry
				// best stays locked, we'll use it
				continue
			}
			entry.mu.Unlock()
		}
		if best == nil {
			return nil, fmt.Errorf("all keys are unavailable")
		}
		best.UsageCount++
		best.LastUsed = now
		best.mu.Unlock()

		if cfg.EnablePrometheus {
			keyUsageTotal.WithLabelValues(best.Key).Inc()
			keyHealthy.WithLabelValues(best.Key).Set(1)
		}
		return best, nil

	default:
		return nil, fmt.Errorf("unknown strategy: %s", kr.strategy)
	}
}

func (kr *KeyRotator) MarkSuccess(key *KeyEntry) {
	key.mu.Lock()
	defer key.mu.Unlock()
	if key.State == KeyDisabled {
		return
	}
	key.State = KeyHealthy
	key.CooldownUntil = time.Time{}

	if cfg.EnablePrometheus {
		keyHealthy.WithLabelValues(key.Key).Set(1)
	}

	slog.Info("key_recovered", "key", key.Key)
}

func (kr *KeyRotator) MarkCooldown(key *KeyEntry, duration time.Duration) {
	key.mu.Lock()
	defer key.mu.Unlock()
	key.State = KeyCooldown
	key.CooldownUntil = time.Now().Add(duration)

	if cfg.EnablePrometheus {
		keyHealthy.WithLabelValues(key.Key).Set(0)
	}

	slog.Info("key_cooldown", "key", key.Key, "duration", duration)
}

func (kr *KeyRotator) MarkDisabled(key *KeyEntry) {
	key.mu.Lock()
	defer key.mu.Unlock()
	key.State = KeyDisabled

	if cfg.EnablePrometheus {
		keyHealthy.WithLabelValues(key.Key).Set(0)
	}

	slog.Info("key_disabled", "key", key.Key)
}

func (kr *KeyRotator) HealthyCount() int {
	now := time.Now()
	count := 0
	for _, entry := range kr.keys {
		entry.mu.Lock()
		if entry.State == KeyHealthy || (entry.State == KeyCooldown && !now.Before(entry.CooldownUntil)) {
			count++
		}
		entry.mu.Unlock()
	}
	return count
}

func (kr *KeyRotator) TotalCount() int {
	return len(kr.keys)
}

func (kr *KeyRotator) DisabledCount() int {
	count := 0
	for _, entry := range kr.keys {
		entry.mu.Lock()
		if entry.State == KeyDisabled {
			count++
		}
		entry.mu.Unlock()
	}
	return count
}

func MaskKey(key string) string {
	l := len(key)
	if l > 8 {
		return key[:5] + "..." + key[l-3:]
	}
	if l > 3 {
		return key[:3] + "***"
	}
	return key + "***"
}

func ParseRetryAfter(header string, defaultDuration time.Duration) time.Duration {
	header = strings.TrimSpace(header)
	if seconds, err := strconv.Atoi(header); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return defaultDuration
}

// --- Proxy ---

type contextKey string

const (
	keyCtxKey       contextKey = "proxy_key"
	classifyCtxKey  contextKey = "classify_result"
	startTimeCtxKey contextKey = "start_time"
)

// ClassificationResult stores whether a response should trigger a retry.
type ClassificationResult struct {
	ShouldRetry bool
	StatusCode  int
}

// classifyHolder wraps a ClassificationResult pointer so ModifyResponse can
// write to it through the request context.
type classifyHolder struct {
	result *ClassificationResult
}

// bufferedResponseWriter captures the full response so we can decide
// whether to forward it to the real client or discard and retry.
type bufferedResponseWriter struct {
	header     http.Header
	body       bytes.Buffer
	statusCode int
	wroteCode  bool
}

func newBufferedResponseWriter() *bufferedResponseWriter {
	return &bufferedResponseWriter{
		header: make(http.Header),
	}
}

func (b *bufferedResponseWriter) Header() http.Header {
	return b.header
}

func (b *bufferedResponseWriter) Write(data []byte) (int, error) {
	if !b.wroteCode {
		b.statusCode = http.StatusOK
		b.wroteCode = true
	}
	return b.body.Write(data)
}

func (b *bufferedResponseWriter) WriteHeader(code int) {
	if b.wroteCode {
		return
	}
	b.statusCode = code
	b.wroteCode = true
}

func (b *bufferedResponseWriter) writeTo(w http.ResponseWriter) {
	for k, v := range b.header {
		w.Header()[k] = v
	}
	w.WriteHeader(b.statusCode)
	if _, err := b.body.WriteTo(w); err != nil {
		slog.Error("failed to write buffered response", "error", err)
	}
}

func newReverseProxy(upstreamURL *url.URL) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstreamURL)
			r.SetXForwarded()

			// Strip hop-by-hop headers
			hopByHop := []string{
				"Connection",
				"Keep-Alive",
				"Proxy-Authenticate",
				"Proxy-Authorization",
				"Transfer-Encoding",
				"Upgrade",
			}
			for _, h := range hopByHop {
				r.Out.Header.Del(h)
			}

			// Set Authorization from context key
			key, _ := r.In.Context().Value(keyCtxKey).(*KeyEntry)
			if key != nil {
				r.Out.Header.Set("Authorization", "Bearer "+key.RawKey)
			}
		},
		ModifyResponse: classifyResponse,
		ErrorHandler:   proxyErrorHandler,
	}
	return rp
}

func proxyHandler(rp *httputil.ReverseProxy, rotator *KeyRotator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Buffer the request body for potential retries
		var bodyBytes []byte
		if r.Body != nil {
			var err error
			bodyBytes, err = io.ReadAll(r.Body)
			if err != nil {
				writeOpenAIError(w, "failed to read request body", "server_error", "request_body_read_error", http.StatusInternalServerError)
				return
			}
			r.Body.Close()
		}

		maxRetries := rotator.TotalCount()
		lastStatusCode := 0

		for attempt := 0; attempt < maxRetries; attempt++ {
			key, err := rotator.PickKey()
			if err != nil {
				// All keys exhausted
				msg := "all API keys are unavailable"
				errType := "server_error"
				code := "all_keys_exhausted"
				statusCode := http.StatusTooManyRequests
				if lastStatusCode == 401 || lastStatusCode == 403 {
					statusCode = lastStatusCode
					msg = "authentication failed with all API keys"
					errType = "authentication_error"
					code = "auth_failed"
				}
				writeOpenAIError(w, msg, errType, code, statusCode)
				return
			}

			slog.Info("key_selected", "key", key.Key, "strategy", rotator.strategy, "attempt", attempt+1)

			// Create a fresh request for each attempt
			newReq := r.Clone(r.Context())
			if bodyBytes != nil {
				newReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				newReq.ContentLength = int64(len(bodyBytes))
			}

			// Store key and classification holder in context
			holder := &classifyHolder{}
			ctx := context.WithValue(newReq.Context(), keyCtxKey, key)
			ctx = context.WithValue(ctx, classifyCtxKey, holder)
			ctx = context.WithValue(ctx, startTimeCtxKey, time.Now())
			newReq = newReq.WithContext(ctx)

			// Use a buffered response writer so we can retry if needed
			buf := newBufferedResponseWriter()

			rp.ServeHTTP(buf, newReq)

			// Check classification result
			if holder.result != nil && holder.result.ShouldRetry {
				lastStatusCode = holder.result.StatusCode
				slog.Info("transparent_retry", "key", key.Key, "status", holder.result.StatusCode, "attempt", attempt+1)
				// Discard buffer and try next key
				continue
			}

			// Success or non-retryable error — forward buffered response to client
			buf.writeTo(w)
			return
		}

// All retries exhausted
	allExhaustedMsg := "all API keys exhausted after retries"
	allExhaustedType := "server_error"
	allExhaustedCode := "all_keys_exhausted"
	allExhaustedStatus := http.StatusTooManyRequests
	if lastStatusCode == http.StatusUnauthorized || lastStatusCode == http.StatusForbidden {
		allExhaustedMsg = "authentication failed with all API keys"
		allExhaustedType = "authentication_error"
		allExhaustedCode = "auth_failed"
		allExhaustedStatus = lastStatusCode
	}
	writeOpenAIError(w, allExhaustedMsg, allExhaustedType, allExhaustedCode, allExhaustedStatus)
	}
}

// --- Error Classification ---

type KeyError struct {
	Op   string
	Key  string
	Err  string
}

func (e *KeyError) Error() string { return e.Op + ": key " + e.Key + ": " + e.Err }

type UpstreamError struct {
	Err error
}

func (e *UpstreamError) Error() string { return "upstream: " + e.Err.Error() }

type ConfigError struct {
	Err error
}

func (e *ConfigError) Error() string { return "config: " + e.Err.Error() }

type errorBody struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

func classifyResponse(resp *http.Response) error {
	key, _ := resp.Request.Context().Value(keyCtxKey).(*KeyEntry)
	holder, _ := resp.Request.Context().Value(classifyCtxKey).(*classifyHolder)
	startTime, _ := resp.Request.Context().Value(startTimeCtxKey).(time.Time)

	if key == nil {
		return nil
	}

	duration := time.Since(startTime)
	statusCode := resp.StatusCode

	// Record metrics helper
	recordMetrics := func() {
		if cfg.EnablePrometheus {
			requestsTotal.WithLabelValues(key.Key, statusGroup(statusCode)).Inc()
			requestDuration.WithLabelValues(key.Key).Observe(duration.Seconds())
		}
	}

	// Success (2xx)
	if statusCode >= 200 && statusCode < 300 {
		rotator.MarkSuccess(key)
		recordMetrics()
		slog.Info("request_forwarded", "key", key.Key, "status", statusCode)
		if holder != nil {
			holder.result = &ClassificationResult{ShouldRetry: false, StatusCode: statusCode}
		}
		return nil
	}

	// 5xx — upstream problem, don't retry
	if statusCode >= 500 {
		recordMetrics()
		if holder != nil {
			holder.result = &ClassificationResult{ShouldRetry: false, StatusCode: statusCode}
		}
		return nil
	}

	// 401/403 — key is invalid
	if statusCode == 401 || statusCode == 403 {
		rotator.MarkDisabled(key)
		recordMetrics()
		if holder != nil {
			holder.result = &ClassificationResult{ShouldRetry: true, StatusCode: statusCode}
		}
		return fmt.Errorf("key disabled: status %d", statusCode)
	}

	// 429 — rate limit or insufficient quota
	if statusCode == 429 {
		// Parse body to check for insufficient_quota
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		var errBody errorBody
		if json.Unmarshal(bodyBytes, &errBody) == nil && errBody.Error.Code == "insufficient_quota" {
			rotator.MarkDisabled(key)
			recordMetrics()
			if holder != nil {
				holder.result = &ClassificationResult{ShouldRetry: true, StatusCode: statusCode}
			}
			return fmt.Errorf("key disabled: insufficient_quota")
		}

		// Regular rate limit
		retryAfter := ParseRetryAfter(resp.Header.Get("Retry-After"), time.Duration(cfg.CooldownSeconds)*time.Second)
		rotator.MarkCooldown(key, retryAfter)
		recordMetrics()
		if holder != nil {
			holder.result = &ClassificationResult{ShouldRetry: true, StatusCode: statusCode}
		}
		return fmt.Errorf("key in cooldown: rate limited")
	}

	// Other 4xx — don't retry
	recordMetrics()
	if holder != nil {
		holder.result = &ClassificationResult{ShouldRetry: false, StatusCode: statusCode}
	}
	return nil
}

func proxyErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	key, _ := r.Context().Value(keyCtxKey).(*KeyEntry)
	holder, _ := r.Context().Value(classifyCtxKey).(*classifyHolder)

	if key != nil {
		if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline exceeded") {
			rotator.MarkCooldown(key, 10*time.Second)
		}
	}

	if holder != nil && holder.result == nil {
		holder.result = &ClassificationResult{ShouldRetry: false, StatusCode: http.StatusBadGateway}
	}

	writeOpenAIError(w, fmt.Sprintf("upstream error: %s", err.Error()), "server_error", "upstream_error", http.StatusBadGateway)
}

// --- Middleware ---

func basicAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// If admin_pass is empty, admin endpoints are disabled
		if cfg.AdminPass == "" {
			http.Error(w, "admin endpoints disabled", http.StatusForbidden)
			return
		}

		user, pass, ok := r.BasicAuth()
		if !ok || user != cfg.AdminUser || pass != cfg.AdminPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="opencode-router"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

// --- Handlers ---

func healthHandler(w http.ResponseWriter, r *http.Request) {
	client := &http.Client{
		Timeout: time.Duration(cfg.HealthCheckTimeoutSeconds) * time.Second,
	}

	// Try to find a healthy key
	var key *KeyEntry
	now := time.Now()
	for _, entry := range rotator.keys {
		entry.mu.Lock()
		if entry.State == KeyHealthy || (entry.State == KeyCooldown && !now.Before(entry.CooldownUntil)) {
			key = entry
			entry.mu.Unlock()
			break
		}
		entry.mu.Unlock()
	}

	result := map[string]interface{}{
		"healthy_keys":  rotator.HealthyCount(),
		"total_keys":    rotator.TotalCount(),
		"disabled_keys": rotator.DisabledCount(),
	}

	if key == nil {
		result["status"] = "unhealthy"
		result["upstream"] = "no_healthy_keys"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(result) //nolint:errcheck
		return
	}

	upstreamURL, _ := url.Parse(cfg.UpstreamURL)
	checkURL := upstreamURL.JoinPath("/v1/models")

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, checkURL.String(), nil)
	if err != nil {
		result["status"] = "unhealthy"
		result["upstream"] = "url_error"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(result) //nolint:errcheck
		return
	}
	req.Header.Set("Authorization", "Bearer "+key.RawKey)

	resp, err := client.Do(req)
	if err != nil {
		result["status"] = "unhealthy"
		result["upstream"] = "unreachable"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(result) //nolint:errcheck
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result["status"] = "healthy"
		result["upstream"] = "reachable"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	} else {
		result["status"] = "unhealthy"
		result["upstream"] = "unreachable"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	json.NewEncoder(w).Encode(result) //nolint:errcheck
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	keys := make([]map[string]interface{}, 0, len(rotator.keys))
	var totalRequests int64

	for _, entry := range rotator.keys {
		entry.mu.Lock()
		keyStat := map[string]interface{}{
			"masked_key":  entry.Key,
			"state":       entry.State.String(),
			"usage_count": entry.UsageCount,
		}
		if !entry.LastUsed.IsZero() {
			keyStat["last_used"] = entry.LastUsed.Format(time.RFC3339)
		} else {
			keyStat["last_used"] = nil
		}
		totalRequests += entry.UsageCount
		entry.mu.Unlock()
		keys = append(keys, keyStat)
	}

	result := map[string]interface{}{
		"keys":           keys,
		"total_requests": totalRequests,
		"strategy":       rotator.strategy,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result) //nolint:errcheck
}

// --- Metrics ---

var (
	requestsTotal   *prometheus.CounterVec
	keyUsageTotal   *prometheus.CounterVec
	keyHealthy      *prometheus.GaugeVec
	requestDuration *prometheus.HistogramVec
)

func initMetrics() {
	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "opencode_router_requests_total",
			Help: "Total number of requests proxied",
		},
		[]string{"key", "status_group"},
	)

	keyUsageTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "opencode_router_key_usage_total",
			Help: "Number of times each key was selected",
		},
		[]string{"key"},
	)

	keyHealthy = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "opencode_router_key_healthy",
			Help: "Whether a key is healthy (1) or not (0)",
		},
		[]string{"key"},
	)

	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "opencode_router_request_duration_seconds",
			Help:    "Request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"key"},
	)

	prometheus.MustRegister(requestsTotal)
	prometheus.MustRegister(keyUsageTotal)
	prometheus.MustRegister(keyHealthy)
	prometheus.MustRegister(requestDuration)

	// Initialize gauges for all keys
	for _, entry := range rotator.keys {
		keyHealthy.WithLabelValues(entry.Key).Set(1)
	}
}

func statusGroup(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500:
		return "5xx"
	default:
		return "other"
	}
}

// --- Logging ---

var logFile *os.File

func setupLogging() {
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))

	if cfg.EnableLogging && cfg.LogFile != "" {
		f, err := os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			slog.Warn("failed to open log file, logging to stdout", "path", cfg.LogFile, "error", err)
		} else {
			logFile = f
			handler = slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})
			slog.SetDefault(slog.New(handler))
		}
	}
}

// --- OpenAI Error Format ---

type OpenAIError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func writeOpenAIError(w http.ResponseWriter, message, errType, code string, statusCode int) {
	errResp := OpenAIError{}
	errResp.Error.Message = message
	errResp.Error.Type = errType
	errResp.Error.Code = code

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(errResp) //nolint:errcheck
}

// --- Main ---

var (
	cfg     *Config
	rotator *KeyRotator
)

func main() {
	configPath := "config.json"
	if envPath := os.Getenv("OPENCODE_CONFIG"); envPath != "" {
		configPath = envPath
	}

	var err error
	cfg, err = LoadConfig(configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
	os.Exit(1)
	}

	setupLogging()

	rotator = NewKeyRotator(cfg.Keys, cfg.Strategy)

	slog.Info("startup", "keys", rotator.TotalCount(), "strategy", cfg.Strategy, "listen", cfg.ListenAddr, "upstream", cfg.UpstreamURL)
	slog.Info("startup", "version", version)

	upstreamURL, err := url.Parse(cfg.UpstreamURL)
	if err != nil {
		slog.Error("failed to parse upstream URL", "error", err)
	os.Exit(1)
	}

	rp := newReverseProxy(upstreamURL)

	mux := http.NewServeMux()

	// Proxy handler with transparent retry
	mux.HandleFunc("/v1/", proxyHandler(rp, rotator))

	// Health endpoint
	mux.HandleFunc("/health", healthHandler)

	// Admin endpoints with basic auth
	mux.HandleFunc("/admin/stats", basicAuthMiddleware(statsHandler))

	// Prometheus metrics
	if cfg.EnablePrometheus {
		initMetrics()
		mux.Handle("/metrics", promhttp.Handler())
	}

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		slog.Info("listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-done
	slog.Info("shutdown", "message", "received signal, shutting down gracefully")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("server shutdown error", "error", err)
		os.Exit(1)
	}

	if logFile != nil {
		logFile.Close()
	}

	slog.Info("shutdown", "message", "server stopped")
}