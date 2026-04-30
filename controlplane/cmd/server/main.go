// Package main is the entry point for the Control Plane server.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/config"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/grpcserver"
	"go.uber.org/zap"
)

func main() {
	// ── Load configuration ───────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: invalid configuration: %v\n", err)
		os.Exit(1)
	}

	// ── Bootstrap logger ─────────────────────────────────────────────────────
	logger, err := buildLogger(cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: build logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	// ── Run migrations ───────────────────────────────────────────────────────
	if err := db.Migrate(cfg.DatabaseURL, cfg.MigrationsPath, logger); err != nil {
		logger.Fatal("database migration failed", zap.Error(err))
	}

	// ── Connect to database ──────────────────────────────────────────────────
	ctx := context.Background()
	database, err := db.Open(ctx, cfg.DatabaseURL, logger)
	if err != nil {
		logger.Fatal("database connection failed", zap.Error(err))
	}
	defer database.Close()

	// ── Connect to Redis ─────────────────────────────────────────────────────
	redisClient, err := cache.New(cfg.RedisURL, logger)
	if err != nil {
		logger.Fatal("redis connection failed", zap.Error(err))
	}
	defer redisClient.Close()

	// ── Start gRPC server (background goroutine) ─────────────────────────────
	grpcSrv := grpcserver.New(logger)
	go func() {
		if err := grpcSrv.Run(cfg.GRPCPort); err != nil {
			logger.Fatal("gRPC server error", zap.Error(err))
		}
	}()
	logger.Info("gRPC server starting", zap.Int("port", cfg.GRPCPort))

	// ── Build HTTP router ────────────────────────────────────────────────────
	router := api.NewRouter(api.RouterConfig{
		JWTSecret: cfg.JWTSecret,
		Logger:    logger,
	})

	httpSrv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ── Graceful shutdown ────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.Info("HTTP server starting", zap.Int("port", cfg.HTTPPort))
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("HTTP server error", zap.Error(err))
		}
	}()

	<-quit
	logger.Info("shutdown signal received, stopping…")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", zap.Error(err))
	}
	logger.Info("control plane stopped")
}

// buildLogger creates a production or development zap logger based on level.
func buildLogger(level string) (*zap.Logger, error) {
	var cfg zap.Config
	if level == "debug" {
		cfg = zap.NewDevelopmentConfig()
	} else {
		cfg = zap.NewProductionConfig()
	}

	atomicLevel, err := zap.ParseAtomicLevel(level)
	if err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", level, err)
	}
	cfg.Level = atomicLevel
	return cfg.Build()
}

