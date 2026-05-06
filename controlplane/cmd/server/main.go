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

	"net/url"

	"github.com/byw-dev/fileagent/controlplane/internal/agent"
	"github.com/byw-dev/fileagent/controlplane/internal/api"
	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/config"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/event"
	"github.com/byw-dev/fileagent/controlplane/internal/grpcserver"
	"github.com/byw-dev/fileagent/controlplane/internal/indexer"
	"github.com/byw-dev/fileagent/controlplane/internal/storage"
	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	natsgo "github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// minioPresigner wraps minio.Client to satisfy handler.MinIOPresigner.
type minioPresigner struct {
	client *miniogo.Client
}

func (m *minioPresigner) PresignedGetObject(ctx context.Context, bucketName, objectName string, expiry time.Duration) (string, error) {
	u, err := m.client.PresignedGetObject(ctx, bucketName, objectName, expiry, url.Values{})
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// natsPublisher wraps a NATS connection to satisfy the NATSPublisher interface.
type natsPublisher struct {
	conn *natsgo.Conn
}

func (n *natsPublisher) Publish(subject string, data []byte) error {
	return n.conn.Publish(subject, data)
}

// natsListener wraps a NATS connection to satisfy the event.NATSListener interface.
type natsListener struct {
	conn *natsgo.Conn
}

func (n *natsListener) Subscribe(subject string, cb func(data []byte)) (func(), error) {
	sub, err := n.conn.Subscribe(subject, func(msg *natsgo.Msg) {
		cb(msg.Data)
	})
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}

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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	// ── Connect to NATS ──────────────────────────────────────────────────────
	natsConn, err := natsgo.Connect(cfg.NATSURL)
	if err != nil {
		logger.Fatal("NATS connection failed", zap.Error(err))
	}
	defer natsConn.Close()
	nats := &natsPublisher{conn: natsConn}
	listener := &natsListener{conn: natsConn}

	// ── Build core services ──────────────────────────────────────────────────
	authSvc := auth.New(cfg.JWTSecret, redisClient)

	queries := db.New(database)
	agentMgr := agent.NewManager(queries, redisClient, authSvc, nats, logger, cfg.JWTAccessTokenTTL)

	registry := grpcserver.NewAgentRegistry()
	dispatcher := agent.NewDispatcher(queries, redisClient, registry, logger)

	ix := indexer.NewIndexer(database, nats, logger)

	stsMgr := storage.NewSTSManager(
		cfg.MinIOEndpoint,
		cfg.MinIOAccessKey,
		cfg.MinIOSecretKey,
		cfg.MinIORoleARN,
		cfg.MinIOUseSSL,
		logger,
	)

	// ── Build MinIO client for presigned URLs ────────────────────────────────
	minioClient, err := miniogo.New(cfg.MinIOEndpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4(cfg.MinIOAccessKey, cfg.MinIOSecretKey, ""),
		Secure: cfg.MinIOUseSSL,
	})
	if err != nil {
		logger.Fatal("minio client init failed", zap.Error(err))
	}

	webhookSender := event.NewWebhookSender(event.NewDBAdapter(database), logger)
	eventEngine := event.NewEngine(database, webhookSender, logger)
	eventEngine.Start(ctx, listener)

	// ── Start gRPC server (background goroutine) ─────────────────────────────
	grpcSrv := grpcserver.New(logger)
	grpcSrv.WithDeps(registry, redisClient, authSvc, nats, agentMgr)
	grpcSrv.WithExtraDeps(dispatcher, ix, stsMgr, queries)
	go func() {
		if err := grpcSrv.Run(cfg.GRPCPort); err != nil {
			logger.Fatal("gRPC server error", zap.Error(err))
		}
	}()
	logger.Info("gRPC server starting", zap.Int("port", cfg.GRPCPort))

	// ── Build HTTP router ────────────────────────────────────────────────────
	router := api.NewRouter(api.RouterConfig{
		JWTSecret:    cfg.JWTSecret,
		Logger:       logger,
		JWTService:   authSvc,
		AuthDB:       handler.NewQueriesAuthDB(queries),
		UsersDB:      queries,
		FileTypesDB:  queries,
		FilesDB:      queries,
		MinIOSigner:  &minioPresigner{client: minioClient},
		BucketsDB:    queries,
		EventRulesDB: queries,
		UploadLogsDB: queries,
		AgentsDB:     queries,
		AgentMgr:     agentMgr,
		Dispatcher:   dispatcher,
		Registry:     registry,
	})

	httpSrv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ── Graceful shutdown ────────────────────────────────────────────────────
	go func() {
		logger.Info("HTTP server starting", zap.Int("port", cfg.HTTPPort))
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("HTTP server error", zap.Error(err))
		}
	}()

	<-ctx.Done()
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
