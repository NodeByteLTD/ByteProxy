package config

import (
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Settings struct {
	Port                  int
	CORSEnabled           bool
	CORSOrigins           []string
	LogLevel              slog.Level
	Timeout               time.Duration
	StrictTLS             bool
	ProxyAPIKey           string
	ManagementAPIKey      string
	RequireProxyAuth      bool
	RequireManagementAuth bool
	DiscordGlobalRPS      int
	SkipUpdateCheck       bool
	UpdateRepo            string
	ServicesFile          string
	MaxBodyBytes          int64
	MaxHeapBytes          uint64
	MaxSysBytes           uint64
	Environment           string
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(key string, def int) int {
	v, err := strconv.Atoi(env(key, ""))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func environment() string {
	switch strings.ToLower(env("APP_ENV", env("NODE_ENV", "local"))) {
	case "production", "prod":
		return "prod"
	case "development", "dev":
		return "dev"
	default:
		return "local"
	}
}

func level(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func Load() (Settings, error) {
	var origins []string
	for _, o := range strings.Split(env("CORS_ORIGINS", "*"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}

	s := Settings{
		Port:                  envInt("PORT", 3420),
		CORSEnabled:           env("CORS_ENABLED", "true") != "false",
		CORSOrigins:           origins,
		LogLevel:              level(env("LOG_LEVEL", "info")),
		Timeout:               time.Duration(envInt("NETWORK_TIMEOUT", 30000)) * time.Millisecond,
		StrictTLS:             env("STRICT_TLS", "true") != "false",
		ProxyAPIKey:           env("PROXY_API_KEY", ""),
		ManagementAPIKey:      env("MANAGEMENT_API_KEY", ""),
		RequireProxyAuth:      env("REQUIRE_AUTH_FOR_PROXY", "false") == "true",
		RequireManagementAuth: env("REQUIRE_AUTH_FOR_MANAGEMENT", "false") == "true",
		DiscordGlobalRPS:      envInt("DISCORD_GLOBAL_RPS", 50),
		SkipUpdateCheck:       env("SKIP_UPDATE_CHECK", "false") == "true",
		UpdateRepo:            env("UPDATE_REPO", "NodeByteLTD/ByteProxy"),
		ServicesFile:          env("SERVICES_FILE", "data/services.json"),
		MaxBodyBytes:          int64(envInt("MAX_BODY_MB", 100)) << 20,
		MaxHeapBytes:          uint64(envInt("MAX_HEAP_MB", 512)) << 20,
		MaxSysBytes:           uint64(envInt("MAX_RSS_MB", 1024)) << 20,
		Environment:           environment(),
	}

	var errs []error
	if s.RequireProxyAuth && s.ProxyAPIKey == "" {
		errs = append(errs, errors.New("REQUIRE_AUTH_FOR_PROXY=true but PROXY_API_KEY is empty"))
	}
	if s.RequireManagementAuth && s.ManagementAPIKey == "" {
		errs = append(errs, errors.New("REQUIRE_AUTH_FOR_MANAGEMENT=true but MANAGEMENT_API_KEY is empty"))
	}
	return s, errors.Join(errs...)
}

func LoadDotEnv(files ...string) {
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			if n := len(value); n >= 2 && (value[0] == '"' && value[n-1] == '"' || value[0] == '\'' && value[n-1] == '\'') {
				value = value[1 : n-1]
			}
			if _, exists := os.LookupEnv(key); !exists {
				os.Setenv(key, value)
			}
		}
	}
}
