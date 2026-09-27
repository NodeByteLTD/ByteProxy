package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/NodeByteLTD/ByteProxy/internal/config"
	"github.com/NodeByteLTD/ByteProxy/internal/proxy"
	"github.com/NodeByteLTD/ByteProxy/internal/registry"
)

const exposedHeaders = "X-Proxy-Service, X-Proxy-Duration, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset, X-RateLimit-Reset-After, X-RateLimit-Bucket, X-RateLimit-Resource, Retry-After"

type Server struct {
	settings  config.Settings
	reg       *registry.Registry
	fwd       *proxy.Forwarder
	logger    *slog.Logger
	version   string
	started   time.Time
	mux       *http.ServeMux
	pressure  *pressure
	UpdateURL string
}

func New(settings config.Settings, reg *registry.Registry, version string, logger *slog.Logger) *Server {
	s := &Server{
		settings:  settings,
		reg:       reg,
		fwd:       proxy.NewForwarder(settings.Timeout, settings.StrictTLS, settings.DiscordGlobalRPS, logger),
		logger:    logger,
		version:   version,
		started:   time.Now(),
		mux:       http.NewServeMux(),
		pressure:  &pressure{maxHeap: settings.MaxHeapBytes, maxSys: settings.MaxSysBytes},
		UpdateURL: fmt.Sprintf("https://api.github.com/repos/%s/releases", settings.UpdateRepo),
	}
	s.routes()
	return s
}

func (s *Server) Start(ctx context.Context) {
	s.fwd.StartPruning(ctx)
	s.pressure.start(ctx)
}

type route struct {
	pattern string
	handler http.HandlerFunc
}

func (s *Server) routeTable() []route {
	return []route{
		{"GET /{$}", s.handleRoot},
		{"GET /health", s.handleHealth},
		{"GET /status", s.handleStatus},
		{"GET /auth-status", s.handleAuthStatus},
		{"GET /up", s.handleUp},
		{"GET /version", s.handleVersion},
		{"GET /version/{$}", s.handleVersion},
		{"GET /version/check", s.handleVersionCheck},
		{"GET /openapi.json", s.handleOpenAPI},
		{"GET /docs", s.handleDocs},

		{"GET /proxy/services", s.handleProxyServices},
		{"GET /proxy/services/{service}/rate-limit", s.handleProxyRateLimit},
		{"GET /proxy/auth-debug", s.handleAuthDebug},
		{"GET /proxy/auth-test", s.handleAuthTest},
		{"GET /proxy/key-debug", s.handleProxyKeyDebug},
		{"/proxy/{service}", s.handleProxy},
		{"/proxy/{service}/{path...}", s.handleProxy},

		{"GET /manage/services", s.handleListServices},
		{"POST /manage/services", s.handleAddService},
		{"GET /manage/services/{key}", s.handleGetService},
		{"DELETE /manage/services/{key}", s.handleRemoveService},
		{"POST /manage/services/{key}/test", s.handleTestService},
		{"GET /manage/diagnostics", s.handleDiagnostics},
		{"GET /manage/key-debug", s.handleManageKeyDebug},

		{"/", s.handleNotFound},
	}
}

func (s *Server) routes() {
	for _, r := range s.routeTable() {
		s.mux.HandleFunc(r.pattern, r.handler)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	defer func() {
		if p := recover(); p != nil {
			s.logger.Error("panic serving request", "path", r.URL.Path, "panic", p)
			writeJSON(rec, http.StatusInternalServerError, map[string]any{"error": "Internal server error"})
		}
		s.logger.Debug("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start))
	}()

	s.applyCORS(rec, r)
	if r.Method == http.MethodOptions {
		if s.settings.CORSEnabled {
			rec.Header().Set("Access-Control-Max-Age", "86400")
		}
		rec.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path != "/up" {
		if kind, value, bad := s.pressure.check(); bad {
			writeJSON(rec, http.StatusServiceUnavailable, map[string]any{
				"error": "Server under pressure", "type": kind, "value": value, "message": "Service Unavailable",
			})
			return
		}
	}
	if !s.checkAuth(rec, r) {
		return
	}
	s.mux.ServeHTTP(rec, r)
}

func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) {
	if !s.settings.CORSEnabled {
		return
	}
	h := w.Header()
	origin := r.Header.Get("Origin")
	switch {
	case slices.Contains(s.settings.CORSOrigins, "*"):
		h.Set("Access-Control-Allow-Origin", "*")
	case origin != "" && slices.Contains(s.settings.CORSOrigins, origin):
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Add("Vary", "Origin")
	default:
		return
	}
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, X-Api-Key, X-Upstream-Authorization, X-Audit-Log-Reason")
	h.Set("Access-Control-Expose-Headers", exposedHeaders)
}

func (s *Server) keyFor(path string) (kind, key string, required bool) {
	switch {
	case strings.HasPrefix(path, "/proxy/"):
		return "proxy", s.settings.ProxyAPIKey, s.settings.RequireProxyAuth
	case strings.HasPrefix(path, "/manage/"):
		return "management", s.settings.ManagementAPIKey, s.settings.RequireManagementAuth
	}
	return "", "", false
}

func providedKeyMatches(r *http.Request, key string) bool {
	return proxy.IsProxyBearer(r.Header.Get("Authorization"), key) ||
		proxy.KeyEquals(strings.TrimSpace(r.Header.Get("X-Api-Key")), key) ||
		proxy.KeyEquals(strings.TrimSpace(r.URL.Query().Get("api_key")), key)
}

func (s *Server) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	kind, key, required := s.keyFor(r.URL.Path)
	if !required || providedKeyMatches(r, key) {
		return true
	}
	envVar := "PROXY_API_KEY"
	if kind == "management" {
		envVar = "MANAGEMENT_API_KEY"
	}
	noAuth := r.Header.Get("Authorization") == "" && r.Header.Get("X-Api-Key") == "" && r.URL.Query().Get("api_key") == ""
	message := fmt.Sprintf("Authentication failed. Invalid API key for %s routes.", kind)
	if noAuth {
		message = fmt.Sprintf("No authentication provided. API key required for %s routes.", kind)
	}
	s.logger.Warn("unauthorized request", "kind", kind, "path", r.URL.Path, "method", r.Method)
	writeJSON(w, http.StatusUnauthorized, map[string]any{
		"error":   "Unauthorized",
		"message": message,
		"howToAuthenticate": []string{
			`Add header: "X-Api-Key: YOUR_API_KEY"`,
			`Add header: "Authorization: Bearer YOUR_API_KEY" (then send upstream credentials in X-Upstream-Authorization)`,
			`Add query param: "?api_key=YOUR_API_KEY"`,
		},
		"keyHelp": map[string]string{"requiredKeyType": kind, "envVar": envVar},
	})
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

type pressure struct {
	mu         sync.RWMutex
	maxHeap    uint64
	maxSys     uint64
	heap       uint64
	sys        uint64
	goroutines int
}

func (p *pressure) sample() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	p.mu.Lock()
	p.heap, p.sys, p.goroutines = m.HeapAlloc, m.Sys, runtime.NumGoroutine()
	p.mu.Unlock()
}

func (p *pressure) start(ctx context.Context) {
	p.sample()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.sample()
			}
		}
	}()
}

func (p *pressure) check() (string, uint64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	switch {
	case p.maxHeap > 0 && p.heap > p.maxHeap:
		return "heapUsedBytes", p.heap, true
	case p.maxSys > 0 && p.sys > p.maxSys:
		return "rssBytes", p.sys, true
	}
	return "", 0, false
}

func (p *pressure) status() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	healthy := !(p.maxHeap > 0 && p.heap > p.maxHeap) && !(p.maxSys > 0 && p.sys > p.maxSys)
	return map[string]any{"heapUsed": p.heap, "rssBytes": p.sys, "goroutines": p.goroutines, "healthy": healthy}
}
