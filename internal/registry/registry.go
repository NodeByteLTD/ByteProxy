package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var (
	keyPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	tokenEnvPattern = regexp.MustCompile(`^BYTEPROXY_TOKEN_[A-Z0-9_]+$`)
	headerPattern   = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]+$`)

	ErrExists   = errors.New("service already exists")
	ErrNotFound = errors.New("service not found")
	ErrBuiltin  = errors.New("built-in services can't be changed or removed")
)

var forbiddenStaticHeaders = map[string]bool{
	"authorization":            true,
	"x-upstream-authorization": true,
	"x-api-key":                true,
	"host":                     true,
	"cookie":                   true,
	"content-length":           true,
	"transfer-encoding":        true,
	"connection":               true,
}

var authTypes = map[string]bool{"bearer": true, "basic": true, "api-key": true, "bot": true}

type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

type RateLimit struct {
	MaxRequests int   `json:"maxRequests"`
	WindowMs    int64 `json:"windowMs"`
}

type Auth struct {
	Type        string `json:"type"`
	TokenEnvVar string `json:"tokenEnvVar,omitempty"`
	HeaderName  string `json:"headerName,omitempty"`
}

type Service struct {
	Name      string            `json:"name"`
	BaseURL   string            `json:"baseUrl"`
	Headers   map[string]string `json:"headers,omitempty"`
	RateLimit *RateLimit        `json:"rateLimit,omitempty"`
	Auth      *Auth             `json:"auth,omitempty"`
	Builtin   bool              `json:"-"`
}

func TokenEnvAllowed(name string) bool { return tokenEnvPattern.MatchString(name) }

func Builtins(version, discordBase, githubBase string) map[string]Service {
	return map[string]Service{
		"discord": {
			Name:    "Discord API",
			BaseURL: discordBase,
			Headers: map[string]string{
				"User-Agent": fmt.Sprintf("DiscordBot (https://github.com/ByteBrushStudios/ByteProxy, %s)", version),
			},
			Builtin: true,
		},
		"github": {
			Name:    "GitHub API",
			BaseURL: githubBase,
			Headers: map[string]string{
				"User-Agent":           "ByteProxy/" + version,
				"Accept":               "application/vnd.github+json",
				"X-GitHub-Api-Version": "2022-11-28",
			},
			Builtin: true,
		},
	}
}

func Validate(key string, svc Service) error {
	if !keyPattern.MatchString(key) {
		return &ValidationError{"key must be 1-32 lowercase letters, digits, '-' or '_', starting with a letter or digit"}
	}
	if strings.TrimSpace(svc.Name) == "" || len(svc.Name) > 100 {
		return &ValidationError{"config.name is required (max 100 characters)"}
	}
	u, err := url.Parse(svc.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &ValidationError{"config.baseUrl must be an absolute http(s) URL"}
	}
	for name := range svc.Headers {
		if !headerPattern.MatchString(name) {
			return &ValidationError{fmt.Sprintf("config.headers: %q is not a valid header name", name)}
		}
		if forbiddenStaticHeaders[strings.ToLower(name)] {
			return &ValidationError{fmt.Sprintf("config.headers: %q can't be set statically; use auth or caller headers", name)}
		}
	}
	if rl := svc.RateLimit; rl != nil && (rl.MaxRequests <= 0 || rl.WindowMs <= 0) {
		return &ValidationError{"config.rateLimit.maxRequests and windowMs must be positive"}
	}
	if a := svc.Auth; a != nil {
		if !authTypes[a.Type] {
			return &ValidationError{"config.auth.type must be one of bearer, basic, api-key, bot"}
		}
		if a.Type == "api-key" && !headerPattern.MatchString(a.HeaderName) {
			return &ValidationError{"config.auth.headerName is required for api-key auth"}
		}
		if a.TokenEnvVar != "" && !TokenEnvAllowed(a.TokenEnvVar) {
			return &ValidationError{"config.auth.tokenEnvVar must be named BYTEPROXY_TOKEN_<NAME>; other environment variables can't be used as upstream credentials"}
		}
	}
	return nil
}

type Registry struct {
	mu      sync.RWMutex
	builtin map[string]Service
	dynamic map[string]Service
	file    string
}

func New(file string, builtins map[string]Service) (*Registry, error) {
	r := &Registry{builtin: builtins, dynamic: map[string]Service{}, file: file}
	if file == "" {
		return r, nil
	}
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", file, err)
	}
	var stored map[string]Service
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", file, err)
	}
	for key, svc := range stored {
		if _, clash := builtins[key]; clash {
			return nil, fmt.Errorf("%s: %q clashes with a built-in service", file, key)
		}
		if err := Validate(key, svc); err != nil {
			return nil, fmt.Errorf("%s: service %q: %w", file, key, err)
		}
		r.dynamic[key] = svc
	}
	return r, nil
}

func (r *Registry) Get(key string) (Service, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if svc, ok := r.builtin[key]; ok {
		return svc, true
	}
	svc, ok := r.dynamic[key]
	return svc, ok
}

func (r *Registry) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var builtin, dynamic []string
	for k := range r.builtin {
		builtin = append(builtin, k)
	}
	for k := range r.dynamic {
		dynamic = append(dynamic, k)
	}
	sort.Strings(builtin)
	sort.Strings(dynamic)
	return append(builtin, dynamic...)
}

func (r *Registry) Add(key string, svc Service) error {
	if err := Validate(key, svc); err != nil {
		return err
	}
	svc.Builtin = false
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.builtin[key]; ok {
		return ErrExists
	}
	if _, ok := r.dynamic[key]; ok {
		return ErrExists
	}
	r.dynamic[key] = svc
	if err := r.saveLocked(); err != nil {
		delete(r.dynamic, key)
		return err
	}
	return nil
}

func (r *Registry) Remove(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.builtin[key]; ok {
		return ErrBuiltin
	}
	svc, ok := r.dynamic[key]
	if !ok {
		return ErrNotFound
	}
	delete(r.dynamic, key)
	if err := r.saveLocked(); err != nil {
		r.dynamic[key] = svc
		return err
	}
	return nil
}

func (r *Registry) saveLocked() error {
	if r.file == "" {
		return nil
	}
	data, err := json.MarshalIndent(r.dynamic, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.file), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(r.file), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.file), ".services-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), r.file)
}
