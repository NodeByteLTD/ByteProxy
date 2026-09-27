package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/NodeByteLTD/ByteProxy/internal/proxy"
	"github.com/NodeByteLTD/ByteProxy/internal/registry"
)

var (
	discordAPIPrefix = regexp.MustCompile(`^/api(/|$)`)
	discordVersion   = regexp.MustCompile(`^/(v\d+)(/|$)`)
)

func (s *Server) appVersion() string {
	if s.settings.Environment == "prod" {
		return "v" + s.version
	}
	return "v" + s.version + "-" + s.settings.Environment
}

func (s *Server) uptime() float64 { return time.Since(s.started).Seconds() }

func stripAPIKeyParam(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	var kept []string
	for _, pair := range strings.Split(rawQuery, "&") {
		name, _, _ := strings.Cut(pair, "=")
		if decoded, err := url.QueryUnescape(name); err == nil && decoded == "api_key" {
			continue
		}
		if pair != "" {
			kept = append(kept, pair)
		}
	}
	return strings.Join(kept, "&")
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	service := r.PathValue("service")
	svc, ok := s.reg.Get(service)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error":             fmt.Sprintf("Service '%s' not configured", service),
			"availableServices": s.reg.Keys(),
		})
		return
	}

	path := strings.TrimPrefix(r.URL.EscapedPath(), "/proxy/"+url.PathEscape(service))
	base := strings.TrimRight(svc.BaseURL, "/")

	if service == "discord" {
		path = discordAPIPrefix.ReplaceAllString(path, "/")
		if m := discordVersion.FindStringSubmatch(path); m != nil {
			base += "/" + m[1]
			path = strings.TrimPrefix(path, "/"+m[1])
		}
	}
	if service == "github" && strings.HasPrefix(path, "/org/") {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "Invalid GitHub API path. Did you mean '/orgs/:org' or '/orgs/:org/repos'? The GitHub API does not support '/org/'.",
			"hint":  "Use '/proxy/github/orgs/ORG_NAME' or '/proxy/github/orgs/ORG_NAME/repos' instead.",
		})
		return
	}

	var body []byte
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		var err error
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, s.settings.MaxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": fmt.Sprintf("Request body exceeds %d bytes", s.settings.MaxBodyBytes), "service": service})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Failed to read request body", "service": service})
			return
		}
	}

	target := base + path
	if q := stripAPIKeyParam(r.URL.RawQuery); q != "" {
		target += "?" + q
	}
	limiterPath, _ := url.PathUnescape(path)

	start := time.Now()
	resp, err := s.fwd.Do(r.Context(), proxy.Request{
		ServiceKey: service,
		Service:    svc,
		Method:     r.Method,
		TargetURL:  target,
		Path:       limiterPath,
		Header:     r.Header.Clone(),
		Body:       body,
		ProxyKey:   s.settings.ProxyAPIKey,
	})
	if err != nil {
		status := http.StatusInternalServerError
		var perr *proxy.Error
		if errors.As(err, &perr) {
			status = perr.Status
		}
		s.logger.Error("proxy request failed", "service", service, "method", r.Method, "path", limiterPath, "status", status, "error", err.Error())
		w.Header().Set("X-Proxy-Service", service)
		w.Header().Set("X-Proxy-Error", "true")
		writeJSON(w, status, map[string]any{
			"error":           err.Error(),
			"service":         service,
			"path":            limiterPath,
			"troubleshooting": map[string]string{"hint": "Check /manage/diagnostics for configuration and network details"},
		})
		return
	}
	defer resp.Body.Close()

	h := w.Header()
	for k, vs := range resp.Header {
		if strings.HasPrefix(k, "Access-Control-") {
			continue
		}
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	for _, name := range proxy.StrippedResponseHeaders {
		h.Del(name)
	}
	if !resp.Uncompressed && resp.ContentLength >= 0 {
		h.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	h.Set("X-Proxy-Service", service)
	h.Set("X-Proxy-Duration", fmt.Sprintf("%dms", time.Since(start).Milliseconds()))
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		io.Copy(w, resp.Body)
	}
	s.logger.Info("proxied", "service", service, "method", r.Method, "path", limiterPath, "status", resp.StatusCode, "duration", time.Since(start))
}

func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"message":       "Welcome to ByteProxy, feel free to browse around!",
		"source":        "https://github.com/NodeByteLTD/ByteProxy",
		"status":        "/status",
		"health":        "/health",
		"version":       s.appVersion(),
		"checkUpdates":  "/version/check",
		"documentation": "https://proxy.nodebyte.co.uk",
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	keys := s.reg.Keys()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "healthy",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"uptime":    s.uptime(),
		"version":   s.appVersion(),
		"services":  map[string]any{"total": len(keys), "available": keys},
		"config": map[string]any{
			"port":    s.settings.Port,
			"logging": map[string]any{"level": s.settings.LogLevel.String()},
			"cors":    map[string]any{"enabled": s.settings.CORSEnabled, "origins": s.settings.CORSOrigins},
		},
	})
}

func (s *Server) serviceSummary(key string, svc registry.Service) map[string]any {
	return map[string]any{
		"key":                 key,
		"name":                svc.Name,
		"baseUrl":             svc.BaseURL,
		"builtin":             svc.Builtin,
		"hasAuth":             svc.Auth != nil,
		"authTokenConfigured": svc.Auth != nil && svc.Auth.TokenEnvVar != "" && os.Getenv(svc.Auth.TokenEnvVar) != "",
		"rateLimit":           svc.RateLimit,
	}
}

func (s *Server) summaries() []map[string]any {
	var out []map[string]any
	for _, key := range s.reg.Keys() {
		if svc, ok := s.reg.Get(key); ok {
			out = append(out, s.serviceSummary(key, svc))
		}
	}
	return out
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	writeJSON(w, http.StatusOK, map[string]any{
		"application": map[string]any{
			"name":        "ByteProxy",
			"version":     s.version,
			"description": "Extensible web proxy for Discord, GitHub, and other APIs",
		},
		"runtime": map[string]any{
			"go":         runtime.Version(),
			"platform":   runtime.GOOS + "/" + runtime.GOARCH,
			"uptime":     s.uptime(),
			"memory":     map[string]any{"heapAlloc": m.HeapAlloc, "sys": m.Sys, "numGC": m.NumGC},
			"goroutines": runtime.NumGoroutine(),
		},
		"configuration": map[string]any{
			"port":     s.settings.Port,
			"services": s.summaries(),
			"logging":  map[string]any{"level": s.settings.LogLevel.String()},
			"cors":     map[string]any{"enabled": s.settings.CORSEnabled, "origins": s.settings.CORSOrigins},
		},
	})
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	redacted := *r.URL
	redacted.RawQuery = stripAPIKeyParam(r.URL.RawQuery)
	var segments []string
	for _, seg := range strings.Split(r.URL.Path, "/") {
		if seg != "" {
			segments = append(segments, seg)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": r.URL.Path,
		"authConfig": map[string]bool{
			"proxyAuthRequired":       s.settings.RequireProxyAuth,
			"managementAuthRequired":  s.settings.RequireManagementAuth,
			"proxyKeyConfigured":      s.settings.ProxyAPIKey != "",
			"managementKeyConfigured": s.settings.ManagementAPIKey != "",
		},
		"requestInfo": map[string]any{
			"method":          r.Method,
			"hasAuthHeader":   r.Header.Get("Authorization") != "",
			"hasApiKeyHeader": r.Header.Get("X-Api-Key") != "",
			"hasQueryKey":     r.URL.Query().Get("api_key") != "",
			"fullUrl":         redacted.String(),
			"pathSegments":    segments,
		},
		"help": "This endpoint helps diagnose authentication issues",
	})
}

func (s *Server) handleUp(w http.ResponseWriter, _ *http.Request) {
	status := s.pressure.status()
	status["status"] = "ok"
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	full := s.appVersion()
	semver, env, _ := strings.Cut(full, "-")
	if env == "" {
		env = "prod"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     full,
		"semver":      semver,
		"environment": env,
		"builtWith":   map[string]string{"go": runtime.Version()},
	})
}

type UpdateInfo struct {
	CurrentVersion   string  `json:"currentVersion"`
	LatestVersion    *string `json:"latestVersion"`
	LatestReleaseURL *string `json:"latestReleaseUrl"`
	IsLatest         bool    `json:"isLatest"`
	IsPrerelease     bool    `json:"isPrerelease"`
	ReleaseDate      *string `json:"releaseDate"`
	UpdateAvailable  bool    `json:"updateAvailable"`
	Error            string  `json:"error,omitempty"`
}

func compareVersions(a, b string) int {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

func (s *Server) CheckForUpdates(ctx context.Context) UpdateInfo {
	info := UpdateInfo{CurrentVersion: s.appVersion(), IsLatest: true}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.UpdateURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ByteProxy-UpdateChecker/"+s.version)
	resp, err := s.fwd.Client.Do(req)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		info.Error = fmt.Sprintf("GitHub responded with %d", resp.StatusCode)
		return info
	}
	var releases []struct {
		TagName     string `json:"tag_name"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
		Prerelease  bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&releases); err != nil || len(releases) == 0 {
		return info
	}
	latest := releases[0]
	for _, rel := range releases {
		if !rel.Prerelease {
			latest = rel
			break
		}
	}
	info.LatestVersion, info.LatestReleaseURL, info.ReleaseDate = &latest.TagName, &latest.HTMLURL, &latest.PublishedAt
	info.IsPrerelease = latest.Prerelease
	info.UpdateAvailable = compareVersions(s.version, latest.TagName) < 0
	info.IsLatest = !info.UpdateAvailable
	return info
}

func (s *Server) handleVersionCheck(w http.ResponseWriter, r *http.Request) {
	info := s.CheckForUpdates(r.Context())
	writeJSON(w, http.StatusOK, struct {
		UpdateInfo
		CheckTime string `json:"checkTime"`
	}{info, time.Now().UTC().Format(time.RFC3339)})
}

func (s *Server) handleProxyServices(w http.ResponseWriter, _ *http.Request) {
	list := s.summaries()
	writeJSON(w, http.StatusOK, map[string]any{"services": list, "count": len(list)})
}

func (s *Server) handleProxyRateLimit(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("service")
	svc, ok := s.reg.Get(key)
	if !ok || svc.RateLimit == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "Service not found or rate limiting not configured", "service": key})
		return
	}
	remaining, resetAt := s.fwd.Window.Status(key, svc.RateLimit.MaxRequests)
	resetIn, resetMs := 0, int64(0)
	if !resetAt.IsZero() {
		resetMs = resetAt.UnixMilli()
		resetIn = int(time.Until(resetAt).Seconds() + 0.999)
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": key, "remaining": remaining, "resetTime": resetMs, "resetIn": resetIn})
}

func (s *Server) handleAuthDebug(w http.ResponseWriter, r *http.Request) {
	provided := func(b bool) string {
		if b {
			return "provided"
		}
		return "not provided"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "success",
		"message":   "Authentication successful",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"authMethod": map[string]string{
			"bearer":     provided(strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")),
			"xApiKey":    provided(r.Header.Get("X-Api-Key") != ""),
			"queryParam": provided(r.URL.Query().Get("api_key") != ""),
		},
		"help": "If you see this message, your authentication is working correctly.",
	})
}

func (s *Server) handleAuthTest(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "message": "Authentication successful", "timestamp": time.Now().UTC().Format(time.RFC3339)})
}

func (s *Server) keyDebug() map[string]any {
	return map[string]any{
		"proxyAuthRequired":       s.settings.RequireProxyAuth,
		"managementAuthRequired":  s.settings.RequireManagementAuth,
		"proxyKeyConfigured":      s.settings.ProxyAPIKey != "",
		"managementKeyConfigured": s.settings.ManagementAPIKey != "",
		"keyTypes": map[string]string{
			"proxy":      "Required for /proxy/* routes (PROXY_API_KEY)",
			"management": "Required for /manage/* routes (MANAGEMENT_API_KEY)",
		},
	}
}

func (s *Server) handleProxyKeyDebug(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"message": "API Key Debug Information", "keyInfo": s.keyDebug()})
}

func (s *Server) handleManageKeyDebug(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"message": "API Key Debug Information", "keyInfo": s.keyDebug()})
}

func (s *Server) handleListServices(w http.ResponseWriter, _ *http.Request) {
	list := s.summaries()
	writeJSON(w, http.StatusOK, map[string]any{"services": list, "count": len(list)})
}

func (s *Server) handleAddService(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Key    string            `json:"key"`
		Config *registry.Service `json:"config"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&payload); err != nil || payload.Key == "" || payload.Config == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Body must be JSON: {\"key\": string, \"config\": {...}}"})
		return
	}
	err := s.reg.Add(payload.Key, *payload.Config)
	var verr *registry.ValidationError
	switch {
	case errors.As(err, &verr):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": verr.Msg})
	case errors.Is(err, registry.ErrExists):
		writeJSON(w, http.StatusConflict, map[string]any{"error": fmt.Sprintf("Service '%s' already exists", payload.Key)})
	case err != nil:
		s.logger.Error("failed to add service", "key", payload.Key, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Failed to save service"})
	default:
		s.logger.Info("service added", "key", payload.Key, "baseUrl", payload.Config.BaseURL)
		writeJSON(w, http.StatusOK, map[string]any{"message": fmt.Sprintf("Service '%s' added successfully", payload.Key), "key": payload.Key, "config": payload.Config})
	}
}

func (s *Server) handleGetService(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	svc, ok := s.reg.Get(key)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("Service '%s' not found", key)})
		return
	}
	cfg := map[string]any{"name": svc.Name, "baseUrl": svc.BaseURL, "headers": svc.Headers, "rateLimit": svc.RateLimit, "builtin": svc.Builtin}
	if svc.Auth != nil {
		cfg["auth"] = map[string]any{
			"type":            svc.Auth.Type,
			"tokenEnvVar":     svc.Auth.TokenEnvVar,
			"headerName":      svc.Auth.HeaderName,
			"tokenConfigured": svc.Auth.TokenEnvVar != "" && os.Getenv(svc.Auth.TokenEnvVar) != "",
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "config": cfg})
}

func (s *Server) handleRemoveService(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	switch err := s.reg.Remove(key); {
	case errors.Is(err, registry.ErrBuiltin):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	case errors.Is(err, registry.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("Service '%s' not found", key)})
	case err != nil:
		s.logger.Error("failed to remove service", "key", key, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Failed to save services"})
	default:
		s.fwd.Window.Remove(key)
		s.logger.Info("service removed", "key", key)
		writeJSON(w, http.StatusOK, map[string]any{"message": fmt.Sprintf("Service '%s' removed", key)})
	}
}

func (s *Server) handleTestService(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	svc, ok := s.reg.Get(key)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("Service '%s' not found", key)})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, svc.BaseURL, nil)
	for k, v := range svc.Headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := s.fwd.Client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"service": key, "status": "unreachable", "error": err.Error(), "baseUrl": svc.BaseURL})
		return
	}
	resp.Body.Close()
	writeJSON(w, http.StatusOK, map[string]any{
		"service":        key,
		"status":         "reachable",
		"responseStatus": resp.StatusCode,
		"duration":       fmt.Sprintf("%dms", time.Since(start).Milliseconds()),
		"baseUrl":        svc.BaseURL,
	})
}

func (s *Server) handleDiagnostics(w http.ResponseWriter, _ *http.Request) {
	auth := map[string]any{}
	for _, key := range s.reg.Keys() {
		svc, _ := s.reg.Get(key)
		if svc.Auth == nil {
			continue
		}
		auth[key] = map[string]any{
			"authType":        svc.Auth.Type,
			"envVar":          svc.Auth.TokenEnvVar,
			"tokenConfigured": svc.Auth.TokenEnvVar != "" && os.Getenv(svc.Auth.TokenEnvVar) != "",
		}
	}
	zone, _ := time.Now().Zone()
	writeJSON(w, http.StatusOK, map[string]any{
		"system": map[string]any{
			"platform": runtime.GOOS + "/" + runtime.GOARCH,
			"go":       runtime.Version(),
			"runtime":  "Go",
			"uptime":   s.uptime(),
		},
		"network": map[string]any{
			"strictTLS": s.settings.StrictTLS,
			"timeoutMs": s.settings.Timeout.Milliseconds(),
		},
		"environment": map[string]any{
			"timeZone":    zone,
			"currentTime": time.Now().UTC().Format(time.RFC3339),
		},
		"authentication":      auth,
		"upstreamCredentials": "Callers send their own upstream credentials in X-Upstream-Authorization (or Authorization when the proxy key is sent via X-Api-Key). Dynamic services may use BYTEPROXY_TOKEN_* environment variables.",
	})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.logger.Warn("unknown endpoint", "path", r.URL.Path, "method", r.Method)
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error":      "Endpoint not found",
		"message":    fmt.Sprintf("The endpoint '%s' does not exist", r.URL.Path),
		"quickStart": map[string]string{"proxy": "/proxy/:service/*", "health": "/health", "management": "/manage/services"},
	})
}
