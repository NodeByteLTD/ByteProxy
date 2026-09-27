package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/NodeByteLTD/ByteProxy/internal/ratelimit"
	"github.com/NodeByteLTD/ByteProxy/internal/registry"
)

const (
	discordMaxAttempts = 3
	githubMaxAttempts  = 3
	maxRetryWait       = 30 * time.Second
)

type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

var strippedRequestHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Host", "Content-Length", "Accept-Encoding",
	"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "X-Real-Ip",
	"Cf-Connecting-Ip", "Cf-Ipcountry", "Cf-Ray", "Cf-Visitor", "True-Client-Ip",
	"Authorization", "X-Upstream-Authorization", "X-Api-Key", "Cookie",
}

var StrippedResponseHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Connection", "Te", "Trailer",
	"Transfer-Encoding", "Upgrade", "Content-Length",
}

type Request struct {
	ServiceKey string
	Service    registry.Service
	Method     string
	TargetURL  string
	Path       string
	Header     http.Header
	Body       []byte
	ProxyKey   string
}

type Forwarder struct {
	Client  *http.Client
	discord *ratelimit.Discord
	github  *ratelimit.GitHub
	Window  *ratelimit.Window
	logger  *slog.Logger
}

func NewForwarder(timeout time.Duration, strictTLS bool, discordRPS int, logger *slog.Logger) *Forwarder {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: !strictTLS, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &Forwarder{
		Client:  &http.Client{Transport: transport},
		discord: ratelimit.NewDiscord(discordRPS),
		github:  ratelimit.NewGitHub(),
		Window:  ratelimit.NewWindow(),
		logger:  logger,
	}
}

func (f *Forwarder) StartPruning(ctx context.Context) {
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				f.discord.Prune(10 * time.Minute)
				f.github.Prune(time.Hour)
			}
		}
	}()
}

func KeyEquals(provided, key string) bool {
	if provided == "" || key == "" {
		return false
	}
	hp, hk := sha256.Sum256([]byte(provided)), sha256.Sum256([]byte(key))
	return subtle.ConstantTimeCompare(hp[:], hk[:]) == 1
}

func IsProxyBearer(authorization, proxyKey string) bool {
	if !strings.HasPrefix(authorization, "Bearer ") {
		return false
	}
	return KeyEquals(strings.TrimSpace(authorization[len("Bearer "):]), proxyKey)
}

func formatToken(authType, token string) string {
	switch authType {
	case "bot":
		return "Bot " + token
	case "bearer":
		return "Bearer " + token
	case "basic":
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(token))
	default:
		return token
	}
}

func BuildHeaders(req Request) (http.Header, string) {
	out := http.Header{}
	for k, v := range req.Service.Headers {
		out.Set(k, v)
	}
	for _, token := range strings.Split(req.Header.Get("Connection"), ",") {
		if token = strings.TrimSpace(token); token != "" {
			req.Header.Del(token)
		}
	}
	for k, vs := range req.Header {
		out.Del(k)
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	for _, h := range strippedRequestHeaders {
		out.Del(h)
	}
	for k, v := range req.Service.Headers {
		if http.CanonicalHeaderKey(k) == "User-Agent" {
			out.Set(k, v)
		}
	}

	upstream := req.Header.Get("X-Upstream-Authorization")
	if upstream == "" {
		if a := req.Header.Get("Authorization"); a != "" && !IsProxyBearer(a, req.ProxyKey) {
			upstream = a
		}
	}
	if upstream == "" {
		if a := req.Service.Auth; a != nil && registry.TokenEnvAllowed(a.TokenEnvVar) {
			if token := os.Getenv(a.TokenEnvVar); token != "" {
				upstream = formatToken(a.Type, token)
			}
		}
	}

	cred := "anonymous"
	if upstream != "" {
		if a := req.Service.Auth; a != nil && a.Type == "api-key" && a.HeaderName != "" {
			out.Set(a.HeaderName, upstream)
		} else {
			out.Set("Authorization", upstream)
		}
		sum := sha256.Sum256([]byte(upstream))
		cred = hex.EncodeToString(sum[:12])
	}
	if len(req.Body) > 0 && out.Get("Content-Type") == "" {
		out.Set("Content-Type", "application/json")
	}
	return out, cred
}

func (f *Forwarder) Do(ctx context.Context, req Request) (*http.Response, error) {
	headers, cred := BuildHeaders(req)
	switch req.ServiceKey {
	case "discord":
		return f.doDiscord(ctx, req, headers, cred)
	case "github":
		return f.doGitHub(ctx, req, headers, cred)
	}
	if rl := req.Service.RateLimit; rl != nil {
		if ok, resetIn := f.Window.Allow(req.ServiceKey, rl.MaxRequests, time.Duration(rl.WindowMs)*time.Millisecond); !ok {
			return nil, &Error{http.StatusTooManyRequests, fmt.Sprintf("Rate limit exceeded for %s. Reset in %d seconds.", req.ServiceKey, int(resetIn.Seconds()+0.999))}
		}
	}
	return f.send(ctx, req, headers)
}

func (f *Forwarder) doDiscord(ctx context.Context, req Request, headers http.Header, cred string) (*http.Response, error) {
	for attempt := 1; ; attempt++ {
		route, err := f.discord.Acquire(ctx, cred, req.Method, req.Path)
		if err != nil {
			return nil, contextError(err)
		}
		resp, err := f.send(ctx, req, headers)
		if err != nil {
			return nil, err
		}
		f.discord.Update(cred, route, req.Path, resp.Header)
		if resp.StatusCode != http.StatusTooManyRequests || attempt == discordMaxAttempts {
			return resp, nil
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		wait := ratelimit.DiscordRetryAfter(resp.Header, body)
		if resp.Header.Get("X-RateLimit-Global") == "true" {
			f.discord.BlockGlobal(cred, wait)
		}
		if wait > maxRetryWait {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return resp, nil
		}
		f.logger.Warn("discord 429, retrying", "path", req.Path, "attempt", attempt, "wait", wait, "global", resp.Header.Get("X-RateLimit-Global") == "true")
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, contextError(err)
		}
	}
}

func (f *Forwarder) doGitHub(ctx context.Context, req Request, headers http.Header, cred string) (*http.Response, error) {
	for attempt := 1; ; attempt++ {
		resource, err := f.github.Acquire(ctx, cred, req.Path)
		if err != nil {
			return nil, contextError(err)
		}
		resp, err := f.send(ctx, req, headers)
		if err != nil {
			return nil, err
		}
		f.github.Update(cred, resource, resp.Header)
		wait, retry := ratelimit.GitHubRetryAfter(resp.StatusCode, resp.Header)
		if !retry || attempt == githubMaxAttempts || wait > maxRetryWait {
			return resp, nil
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		f.logger.Warn("github rate limited, retrying", "path", req.Path, "status", resp.StatusCode, "attempt", attempt, "wait", wait)
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, contextError(err)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{http.StatusGatewayTimeout, "Timed out waiting for the upstream rate limit to reset"}
	}
	return &Error{http.StatusBadGateway, "Request cancelled"}
}

func (f *Forwarder) send(ctx context.Context, req Request, headers http.Header) (*http.Response, error) {
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	r, err := http.NewRequestWithContext(ctx, req.Method, req.TargetURL, body)
	if err != nil {
		return nil, &Error{http.StatusBadRequest, "Invalid upstream request: " + err.Error()}
	}
	r.Header = headers.Clone()
	resp, err := f.Client.Do(r)
	if err == nil {
		return resp, nil
	}

	host := r.URL.Host
	var netErr net.Error
	var urlErr *url.Error
	var certErr *tls.CertificateVerificationError
	switch {
	case errors.Is(err, context.Canceled):
		return nil, &Error{http.StatusBadGateway, "Request cancelled by the caller"}
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return nil, &Error{http.StatusGatewayTimeout, "Upstream timed out (" + host + ")"}
	case errors.As(err, &certErr), strings.Contains(err.Error(), "x509"), strings.Contains(err.Error(), "tls:"):
		return nil, &Error{http.StatusBadGateway, "TLS error connecting to " + host + ". Check the upstream certificate and system clock."}
	case errors.As(err, &urlErr):
		return nil, &Error{http.StatusBadGateway, "Unable to reach " + host}
	default:
		return nil, &Error{http.StatusBadGateway, "Upstream request failed: " + err.Error()}
	}
}
