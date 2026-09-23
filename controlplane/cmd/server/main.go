// Package main is the entry point for the Control Plane server.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"net/url"

	"github.com/byw-dev/fileagent/controlplane/internal/agent"
	"github.com/byw-dev/fileagent/controlplane/internal/api"
	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/bootstrap"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/config"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/dirstore"
	"github.com/byw-dev/fileagent/controlplane/internal/dryrun"
	"github.com/byw-dev/fileagent/controlplane/internal/event"
	"github.com/byw-dev/fileagent/controlplane/internal/grpcserver"
	"github.com/byw-dev/fileagent/controlplane/internal/indexer"
	"github.com/byw-dev/fileagent/controlplane/internal/storage"
	"github.com/byw-dev/fileagent/controlplane/internal/webui"
	"github.com/byw-dev/fileagent/controlplane/internal/worker"
	"github.com/byw-dev/fileagent/controlplane/migrations"
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

// minioBucketMaker wraps minio.Client to satisfy handler.MinioBucketMaker.
type minioBucketMaker struct {
	client *miniogo.Client
}

// MakeBucket creates a bucket with the given name using the default region.
func (m *minioBucketMaker) MakeBucket(ctx context.Context, bucketName string) error {
	return m.client.MakeBucket(ctx, bucketName, miniogo.MakeBucketOptions{})
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
	if err := db.Migrate(cfg.DatabaseURL, migrations.FS, logger); err != nil {
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
	bootstrapResult, err := bootstrap.EnsureAdminAccount(
		ctx,
		queries,
		cfg.BootstrapAdminUsername,
		cfg.BootstrapAdminPassword,
		cfg.BootstrapAdminForceReset,
	)
	if err != nil {
		logger.Fatal("bootstrap admin failed", zap.Error(err))
	}
	if bootstrapResult.Created {
		credentialsPath, err := writeBootstrapCredentials(
			cfg.BootstrapAdminCredentialsFile,
			bootstrapResult.Username,
			bootstrapResult.Password,
		)
		if err != nil {
			logger.Fatal("write bootstrap admin credentials file failed", zap.Error(err))
		}
		logger.Warn("bootstrap admin user created",
			zap.String("username", bootstrapResult.Username),
			zap.String("credentials_file", credentialsPath),
			zap.String("action", "please login and change password immediately"),
		)
	}
	if bootstrapResult.Reset {
		credentialsPath, err := writeBootstrapCredentials(
			cfg.BootstrapAdminCredentialsFile,
			bootstrapResult.Username,
			bootstrapResult.Password,
		)
		if err != nil {
			logger.Fatal("write bootstrap admin credentials file failed", zap.Error(err))
		}
		logger.Warn("bootstrap admin password reset by configuration",
			zap.String("username", bootstrapResult.Username),
			zap.String("credentials_file", credentialsPath),
			zap.String("action", "disable BOOTSTRAP_ADMIN_FORCE_RESET after recovery"),
		)
	}
	if err := bootstrap.EnsureDefaultBuckets(ctx, queries, bootstrap.DefaultOrgID); err != nil {
		logger.Fatal("bootstrap default buckets failed", zap.Error(err))
	}

	agentMgr := agent.NewManager(queries, redisClient, authSvc, nats, logger, cfg.AgentTokenTTL)

	registry := grpcserver.NewAgentRegistry()
	dispatcher := agent.NewDispatcher(queries, queries, redisClient, registry, logger)

	ix := indexer.NewIndexer(database, nats, logger)

	// AssumeRole is dialed on the internal endpoint; the endpoint returned to
	// agents is the client-facing public one (D-024).
	stsMgr := storage.NewSTSManager(
		cfg.MinIOEndpoint,
		cfg.MinIOAccessKey,
		cfg.MinIOSecretKey,
		cfg.MinIORoleARN,
		cfg.MinIOUseSSL,
		logger,
	).WithPublicEndpoint(cfg.MinIOPublicEndpoint, cfg.MinIOPublicUseSSL)

	// ── Build MinIO clients ───────────────────────────────────────────────────
	// Admin client on the INTERNAL endpoint (bucket create hits MinIO over the
	// network, so it must use the in-cluster address to avoid a host hairpin).
	minioAdminClient, err := miniogo.New(cfg.MinIOEndpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4(cfg.MinIOAccessKey, cfg.MinIOSecretKey, ""),
		Secure: cfg.MinIOUseSSL,
	})
	if err != nil {
		logger.Fatal("minio admin client init failed", zap.Error(err))
	}
	// Presign client on the PUBLIC endpoint. PresignedGetObject signs locally
	// (no network call), so this only fixes the host the URL is signed for — it
	// must match the address browsers/agents actually reach (D-024).
	minioPresignClient, err := miniogo.New(cfg.MinIOPublicEndpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4(cfg.MinIOAccessKey, cfg.MinIOSecretKey, ""),
		Secure: cfg.MinIOPublicUseSSL,
	})
	if err != nil {
		logger.Fatal("minio presign client init failed", zap.Error(err))
	}

	webhookSender := event.NewWebhookSender(event.NewDBAdapter(database), logger)
	eventEngine := event.NewEngine(database, webhookSender, logger).WithPublisher(nats)
	eventEngine.Start(ctx, listener)

	// ── Start gRPC server (background goroutine) ─────────────────────────────
	dirStore := dirstore.New()
	dryRunStore := dryrun.New()
	grpcSrv := grpcserver.New(logger)
	grpcSrv.WithDeps(registry, redisClient, authSvc, nats, agentMgr)
	grpcSrv.WithExtraDeps(dispatcher, ix, stsMgr, queries)
	// IC-BUG-20: a dispatched rule pointing at a bucket the agent's held STS
	// session does not cover must trigger a credentials re-push.
	dispatcher.SetCredentialPusher(grpcSrv)
	grpcSrv.WithStateDB(queries)
	grpcSrv.WithDirResultStore(dirStore)
	grpcSrv.WithDryRunStore(dryRunStore)
	go func() {
		if err := grpcSrv.Run(cfg.GRPCPort); err != nil {
			logger.Fatal("gRPC server error", zap.Error(err))
		}
	}()
	logger.Info("gRPC server starting", zap.Int("port", cfg.GRPCPort))

	// ── Offline sweeper: TTL-driven fallback for stale agent status (CC-6) ────
	// The gRPC disconnect defer marks agents offline, but it never runs on CP
	// crash/restart or a half-open TCP connection. This loop reconciles agents
	// whose Redis presence key has expired back to offline and republishes
	// events.agent.offline. Stops when ctx is cancelled on shutdown.
	offlineSweeper := worker.NewOfflineSweeper(queries, redisClient, nats, bootstrap.DefaultOrgID, logger)
	go offlineSweeper.Run(ctx, 0)

	// Retro-tagging worker drains the retag_jobs outbox (e.g. pending-value merge).
	retagWorker := worker.NewRetagWorker(queries, logger)
	go retagWorker.Run(ctx, 0)

	// IC-4a: the webhook failure policy — persistent per-event failure
	// counters (Redis) and the dead-letter sink (webhook_dead_letters table).
	// Together with the retry cap they form the poison-pill guard: an event
	// that keeps failing is retried (5xx) until the cap, then dead-lettered
	// and answered 200 so MinIO's head-of-line queue is freed.
	webhookFails := handler.NewRedisWebhookFailStore(redisClient)
	webhookDeadLetters := handler.NewDBDeadLetterSink(queries)

	// ── Build HTTP router ────────────────────────────────────────────────────
	router := api.NewRouter(api.RouterConfig{
		JWTSecret:          cfg.JWTSecret,
		Logger:             logger,
		JWTService:         authSvc,
		AuthDB:             handler.NewQueriesAuthDB(queries),
		UsersDB:            queries,
		FileTypesDB:        queries,
		TagKeysDB:          queries,
		PendingTagsDB:      queries,
		RetagJobsDB:        queries,
		FileTagsDB:         queries,
		BatchTagDB:         queries,
		FilesDB:            queries,
		MinIOSigner:        &minioPresigner{client: minioPresignClient},
		BucketsDB:          queries,
		MinIOAdmin:         &minioBucketMaker{client: minioAdminClient},
		EventRulesDB:       queries,
		UploadLogsDB:       queries,
		AgentsDB:           queries,
		AgentMgr:           agentMgr,
		Dispatcher:         dispatcher,
		Registry:           registry,
		AgentCache:         redisClient,
		DirStore:           dirStore,
		DryRunStore:        dryRunStore,
		MinioIndexer:       ix,
		WebhookSecret:      cfg.InternalWebhookSecret,
		WebhookFailCounters: webhookFails,
		WebhookDeadLetters: webhookDeadLetters,
		WebhookFailLimit:   cfg.WebhookFailLimit,
		StatsDB:            queries,
		RateLimiter:        redisClient,
		RateLimitPerMinute: cfg.APIRateLimitPerMinute,
		WebUIFS:            webui.FS(), // nil in pure-API build; embedded assets under `webui` tag
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

// writeBootstrapCredentials writes temporary bootstrap credentials to a local file.
func writeBootstrapCredentials(path, username, password string) (string, error) {
	targetPath := path
	if _, err := os.Stat(targetPath); err == nil {
		targetPath = fmt.Sprintf("%s.%d", targetPath, time.Now().UTC().Unix())
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if dir := filepath.Dir(targetPath); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
	}
	content := fmt.Sprintf("username=%s\npassword=%s\n", username, password)
	if err := os.WriteFile(targetPath, []byte(content), 0o600); err != nil {
		return "", err
	}
	return targetPath, nil
}
