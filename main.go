package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NodeByteLTD/ByteProxy/internal/config"
	"github.com/NodeByteLTD/ByteProxy/internal/registry"
	"github.com/NodeByteLTD/ByteProxy/internal/server"
)

var version = "2.0.0"

func main() {
	config.LoadDotEnv(".env.local", ".env")
	settings, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid configuration:", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: settings.LogLevel}))
	slog.SetDefault(logger)

	for _, name := range []string{"DISCORD_BOT_TOKEN", "GITHUB_TOKEN"} {
		if os.Getenv(name) != "" {
			logger.Warn(name + " is set but no longer used; callers must send their own upstream credentials")
		}
	}
	if !settings.RequireManagementAuth {
		logger.Warn("management API authentication is disabled (REQUIRE_AUTH_FOR_MANAGEMENT=false); anyone who can reach this server can register services")
	}

	reg, err := registry.New(settings.ServicesFile, registry.Builtins(version, "https://discord.com/api/", "https://api.github.com/"))
	if err != nil {
		logger.Error("failed to load services", "error", err)
		os.Exit(1)
	}

	srv := server.New(settings, reg, version, logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv.Start(ctx)

	if !settings.SkipUpdateCheck && settings.Environment != "dev" {
		go func() {
			info := srv.CheckForUpdates(ctx)
			switch {
			case info.Error != "":
				logger.Warn("update check failed", "error", info.Error)
			case info.UpdateAvailable:
				logger.Warn("a newer ByteProxy release is available", "current", info.CurrentVersion, "latest", *info.LatestVersion, "url", *info.LatestReleaseURL)
			}
		}()
	}

	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%d", settings.Port),
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		logger.Info("ByteProxy started", "port", settings.Port, "version", version, "services", len(reg.Keys()))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}
