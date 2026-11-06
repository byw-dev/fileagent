// Package main is the entry point for the Control Plane server.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs"
	"github.com/byw-dev/fileagent/controlplane/internal/storage/startupcheck"
	"github.com/byw-dev/fileagent/controlplane/internal/storageevent"
	"github.com/byw-dev/fileagent/controlplane/internal/webui"
	"github.com/byw-dev/fileagent/controlplane/internal/worker"
	"github.com/byw-dev/fileagent/controlplane/migrations"
	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	natsgo "github.com/nats-io/nats.go"
	"github.com/sqlc-dev/pqtype"
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

// minioObjectReader adapts the minio-go client to the SeaweedFS adapter's
// ObjectReader (the S3 HEAD of the D1 read-back). It owns the ONE nontrivial
// mapping in the read-back path: a DEFINITIVE absence (NoSuchKey / any HTTP
// 404, SeaweedFS S3 answers NoSuchBucket with 404 too) becomes
// seaweedfs.ErrObjectAbsent; EVERY other error is returned verbatim so the
// adapter treats it as a processing failure — a timeout/5xx/403 must never be
// misread as "the object is gone" or CP would write wrongful deletions on a
// storage hiccup (D1; see the adapter's mutation ① test).
type minioObjectReader struct {
	client *miniogo.Client
}

// StatObject implements seaweedfs.ObjectReader.
func (r *minioObjectReader) StatObject(ctx context.Context, bucket, key string) (seaweedfs.ObjectInfo, error) {
	info, err := r.client.StatObject(ctx, bucket, key, miniogo.StatObjectOptions{})
	if err == nil {
		return seaweedfs.ObjectInfo{Size: info.Size, ETag: info.ETag}, nil
	}
	if resp := miniogo.ToErrorResponse(err); resp.Code == "NoSuchKey" || resp.StatusCode == 404 {
		return seaweedfs.ObjectInfo{}, seaweedfs.ErrObjectAbsent
	}
	return seaweedfs.ObjectInfo{}, err
}

// storageEventStore adapts the sqlc Queries to the SeaweedFS adapter's
// CursorMarkStore (durable cursor + reconciliation marks, migration 000011).
// Stateless: the source identity travels with each call.
type storageEventStore struct {
	q *db.Queries
}

// DeclaredCursor implements seaweedfs.CursorMarkStore (found=false on the
// no-row fresh-deployment case).
func (s *storageEventStore) DeclaredCursor(ctx context.Context, sourceID string) (int64, bool, error) {
	declared, err := s.q.GetStorageEventCursor(ctx, sourceID)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return declared, true, nil
}

// SaveCursor implements seaweedfs.CursorMarkStore. The upsert's GREATEST guard
// keeps the persisted value monotonic (a completeness claim never regresses).
func (s *storageEventStore) SaveCursor(ctx context.Context, sourceID string, declaredTsNs int64) error {
	return s.q.UpsertStorageEventCursor(ctx, sourceID, declaredTsNs)
}

// RecordMark implements seaweedfs.CursorMarkStore.
func (s *storageEventStore) RecordMark(ctx context.Context, mark seaweedfs.Mark) error {
	return s.q.InsertStorageEventMark(ctx, db.InsertStorageEventMarkParams{
		SourceID: mark.SourceID,
		Reason:   mark.Reason,
		Bucket:   sql.NullString{String: derefOrEmpty(mark.Bucket), Valid: mark.Bucket != nil},
		FromTsNs: nullInt64(mark.FromTsNs),
		ToTsNs:   nullInt64(mark.ToTsNs),
		Detail:   pqtype.NullRawMessage{RawMessage: mark.Detail, Valid: mark.Detail != nil},
	})
}

// CountActiveEntries implements seaweedfs.CursorMarkStore.
func (s *storageEventStore) CountActiveEntries(ctx context.Context, bucket string) (int64, error) {
	return s.q.CountActiveFileEntriesByBucketName(ctx, bucket)
}

// dbClock adapts the sqlc Queries to the SeaweedFS adapter's DBClock (D2: the
// read-back fact's ObservedAt is a DB clock_timestamp() reading taken before
// the S3 HEAD — the same clock domain the index's eviction compares against).
type dbClock struct {
	q *db.Queries
}

// Now implements seaweedfs.DBClock.
func (c *dbClock) Now(ctx context.Context) (time.Time, error) {
	return c.q.SelectDBClockTimestamp(ctx)
}

// derefOrEmpty is a small helper for nullable-string mapping.
func derefOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// nullInt64 maps *int64 to sql.NullInt64.
func nullInt64(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

// main is a thin wrapper so run() can return an exit CODE: os.Exit skips
// deferred cleanup, so all defers must live inside run() and only the final,
// deliberate exit happens here (see the IC-BUG-40 shutdown note below).
func main() {
	os.Exit(run())
}

// run boots the Control Plane and returns the process exit code (0 = clean
// shutdown, 1 = startup/config failure incl. the IC-BUG-40 storage self-check
// window expiring). Every deferred cleanup is registered inside run() so a
// non-zero return still unwinds through all of them before main's os.Exit.
func run() int {
	// ── Load configuration ───────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		return 1
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: invalid configuration: %v\n", err)
		return 1
	}

	// ── Bootstrap logger ─────────────────────────────────────────────────────
	logger, err := buildLogger(cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: build logger: %v\n", err)
		return 1
	}
	defer logger.Sync() //nolint:errcheck

	// ── Run migrations ───────────────────────────────────────────────────────
	if err := db.Migrate(cfg.DatabaseURL, migrations.FS, logger); err != nil {
		logger.Error("database migration failed", zap.Error(err))
		return 1 // rework 2 finding 1: continue-on-failure would serve on a broken schema (master: Fatal)
	}

	// ── Connect to database ──────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.DatabaseURL, logger)
	if err != nil {
		logger.Error("database connection failed", zap.Error(err))
		return 1 // rework 2 finding 1: nil DB would panic on first use; return BEFORE defer database.Close (database is nil)
	}
	defer database.Close()

	// ── Connect to Redis ─────────────────────────────────────────────────────
	redisClient, err := cache.New(cfg.RedisURL, logger)
	if err != nil {
		logger.Error("redis connection failed", zap.Error(err))
		return 1 // rework 2 finding 1: nil client would panic later; return BEFORE its defer
	}
	defer redisClient.Close()

	// ── Connect to NATS ──────────────────────────────────────────────────────
	natsConn, err := natsgo.Connect(cfg.NATSURL)
	if err != nil {
		logger.Error("NATS connection failed", zap.Error(err))
		return 1 // rework 2 finding 1: nil conn would panic later; return BEFORE its defer
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
		logger.Error("bootstrap admin failed", zap.Error(err))
		return 1 // rework 2 finding 1: no admin account = deployment unusable (master: Fatal)
	}
	if bootstrapResult.Created {
		credentialsPath, err := writeBootstrapCredentials(
			cfg.BootstrapAdminCredentialsFile,
			bootstrapResult.Username,
			bootstrapResult.Password,
		)
		if err != nil {
			logger.Error("write bootstrap admin credentials file failed", zap.Error(err))
			return 1 // rework 2 finding 1: first-admin password would be lost with no recovery path (master: Fatal)
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
			logger.Error("write bootstrap admin credentials file failed", zap.Error(err))
			return 1 // rework 2 finding 1: the reset password would be lost with no recovery path (master: Fatal)
		}
		logger.Warn("bootstrap admin password reset by configuration",
			zap.String("username", bootstrapResult.Username),
			zap.String("credentials_file", credentialsPath),
			zap.String("action", "disable BOOTSTRAP_ADMIN_FORCE_RESET after recovery"),
		)
	}
	if err := bootstrap.EnsureDefaultBuckets(ctx, queries, bootstrap.DefaultOrgID); err != nil {
		logger.Error("bootstrap default buckets failed", zap.Error(err))
		return 1 // rework 2 finding 1: the self-check probe bucket would not exist (master: Fatal)
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
		logger.Error("minio admin client init failed", zap.Error(err))
		return 1 // rework 2 finding 1: nil client would panic later (master: Fatal)
	}
	// Presign client on the PUBLIC endpoint. PresignedGetObject signs locally
	// (no network call), so this only fixes the host the URL is signed for — it
	// must match the address browsers/agents actually reach (D-024).
	minioPresignClient, err := miniogo.New(cfg.MinIOPublicEndpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4(cfg.MinIOAccessKey, cfg.MinIOSecretKey, ""),
		Secure: cfg.MinIOPublicUseSSL,
	})
	if err != nil {
		logger.Error("minio presign client init failed", zap.Error(err))
		return 1 // rework 2 finding 1: nil client would panic later (master: Fatal)
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
	// Rework 1+2: a listener (or the storage-event-source adapter, which is
	// initialized later) that dies mid-run means the process cannot continue
	// serving — but it must still exit through the shutdown path (a Fatal here
	// would os.Exit(1) from a goroutine and skip every defer). The error is
	// handed to serveFatal; awaitShutdownOrSelfCheckFailure WAITS on it as a
	// first-class input (rework 2 finding 2: the old single post-hoc poll
	// would hang forever when only a listener died), then the shutdown block
	// below runs and run() returns non-zero.
	serveFatal := make(chan error, 2)
	go func() {
		if err := grpcSrv.Run(cfg.GRPCPort); err != nil {
			logger.Error("gRPC server error", zap.Error(err))
			serveFatal <- err
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

	// S-1 (round-3 review): the webhook_fail_counters lifecycle claim in
	// migration 000009 (stale rows cleaned after 7d) needs a REAL caller —
	// sweep at startup and periodically, until shutdown. Coarse interval: the
	// sweep is hygiene, not latency-sensitive. (Prometheus remains deferred —
	// T4-1; deletions are observable via structured logs.)
	go handler.RunStaleCounterCleanup(ctx, queries, handler.WebhookFailCounterTTL(), time.Hour, logger)

	// Retro-tagging worker drains the retag_jobs outbox (e.g. pending-value merge).
	retagWorker := worker.NewRetagWorker(queries, logger)
	go retagWorker.Run(ctx, 0)

	// IC-4a: the webhook failure policy — persistent per-event failure
	// counters (Redis) and the dead-letter sink (webhook_dead_letters table).
	// Together with the retry cap they form the poison-pill guard: an event
	// that keeps failing is retried (5xx) until the cap, then dead-lettered
	// and answered 200 so MinIO's head-of-line queue is freed.
	webhookFails := handler.NewRedisWebhookFailStore(redisClient, queries)
	webhookDeadLetters := handler.NewDBDeadLetterSink(queries)
	// S-1 (PR #110 round-2 review): verify the dead-letter sink is usable at
	// startup — a missing table or lost grants would otherwise go unnoticed
	// until the first indexing failure blocks the feed (B2 fail-closed). The
	// probe needs only INSERT/UPDATE on webhook_dead_letters, which the
	// UpsertDeadLetter statement exercises. Failures surface as a degraded
	// /healthz plus a loud structured log; the runbook entry lives in
	// consistency-and-ingest.md §3.6.
	// S-2 (round-3 review): the sink readiness must be DYNAMIC — a one-shot
	// startup probe would leave /healthz permanently 503 after a transient
	// startup-window failure, and permanently 200 after a later sink failure.
	// A background goroutine re-probes with backoff and publishes the result
	// atomically; /healthz reads the cached verdict. No per-request writes.
	deadLetterProbe := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := queries.UpsertDeadLetterWrap(ctx, db.UpsertDeadLetterParams{
			DedupKey:  "__healthcheck__",
			EventName: "healthcheck",
			Bucket:    "",
			Key:       "",
		}); err != nil {
			return err
		}
		_, err := queries.DeleteDeadLetterWrap(ctx, "__healthcheck__")
		return err
	}
	var sinkReady atomic.Bool
	var sinkProbeMu sync.Mutex
	probeSink := func(initial bool) {
		sinkProbeMu.Lock()
		defer sinkProbeMu.Unlock()
		err := deadLetterProbe()
		wasReady := sinkReady.Load()
		sinkReady.Store(err == nil)
		switch {
		case err != nil && (initial || wasReady):
			logger.Error("webhook dead-letter sink NOT ready: index processing will be blocked (B2 fail-closed) once any event fails — fix webhook_dead_letters table/grants",
				zap.Error(err))
		case err == nil && !initial && !wasReady:
			logger.Info("webhook dead-letter sink recovered; /healthz exits degraded")
		}
	}
	probeSink(true)
	go func() {
		// Re-probe with capped exponential backoff: fast retries while
		// degraded (recover quickly), slow while healthy (cheap liveness).
		interval := 30 * time.Second
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				probeSink(false)
				if sinkReady.Load() {
					interval = time.Minute
				} else {
					if interval < 5*time.Minute {
						interval *= 2
					}
				}
				timer.Reset(interval)
			}
		}
	}()

	// ── Storage event source (B3-PR3b D6) ────────────────────────────────────
	// minio_webhook (default): nothing to start — the webhook route below
	// feeds the index as always. seaweedfs_filer: run the filer
	// metadata-subscription adapter as a background worker on the same ctx as
	// every other worker (clean shutdown, no half-written cursor on exit).
	// The webhook route stays registered in BOTH modes (harmless, and it
	// keeps the two sources symmetric during the transition).
	// adapterDone is closed when the storage-event-source goroutine returns
	// (nil when the adapter is disabled) — the shutdown path joins it.
	var adapterDone <-chan struct{}
	if cfg.StorageEventSource == config.StorageEventSourceSeaweedFSFiler {
		// N2：必须「赋值」给外层声明的 adapterDone——用 := 会声明新的局部变量
		// 遮蔽外层，退出时的 join 引用的还是 nil channel，永远不会等待。
		done, err := startSeaweedfsEventSource(ctx, seaweedfsSourceConfig{
			SourceID:     cfg.SeaweedfsSourceID(),
			FilerAddr:    cfg.SeaweedfsFilerGRPCAddr,
			ClientName:   cfg.SeaweedfsClientName,
			ClientID:     cfg.SeaweedfsClientID,
			ResumeMargin: cfg.SeaweedfsResumeMargin,
			MaxRetries:   cfg.SeaweedfsApplyMaxRetries,
			RetryBackoff: cfg.SeaweedfsApplyRetryBackoff,
			MinioClient:  minioAdminClient,
			Indexer:      ix,
			Queries:      queries,
			Logger:       logger,
		})
		if err != nil {
			logger.Error("storage event source adapter init failed", zap.Error(err))
			// Rework 2 finding 1: master exited here via Fatal. The gRPC
			// listener is already up, so this must go through the shutdown
			// path, not a bare return: hand the error to serveFatal — the
			// shutdown orchestration below wakes on it, runs the shutdown
			// block (ctx cancel, HTTP shutdown, event-source join), and
			// run() returns non-zero.
			serveFatal <- err
			done = nil // nothing to join; keep adapterDone nil-safe
		}
		adapterDone = done
	} else {
		logger.Info("storage event source: minio webhook (default; SeaweedFS filer subscription disabled)",
			zap.String("source", cfg.StorageEventSource))
	}

	// ── Build HTTP router ────────────────────────────────────────────────────
	router := api.NewRouter(api.RouterConfig{
		JWTSecret:              cfg.JWTSecret,
		Logger:                 logger,
		JWTService:             authSvc,
		AuthDB:                 handler.NewQueriesAuthDB(queries),
		UsersDB:                queries,
		FileTypesDB:            queries,
		TagKeysDB:              queries,
		PendingTagsDB:          queries,
		RetagJobsDB:            queries,
		FileTagsDB:             queries,
		BatchTagDB:             queries,
		FilesDB:                queries,
		MinIOSigner:            &minioPresigner{client: minioPresignClient},
		BucketsDB:              queries,
		MinIOAdmin:             &minioBucketMaker{client: minioAdminClient},
		EventRulesDB:           queries,
		UploadLogsDB:           queries,
		AgentsDB:               queries,
		AgentMgr:               agentMgr,
		Dispatcher:             dispatcher,
		Registry:               registry,
		AgentCache:             redisClient,
		DirStore:               dirStore,
		DryRunStore:            dryRunStore,
		MinioEventSink:         storageevent.NewIndexSink(ix),
		WebhookSecret:          cfg.InternalWebhookSecret,
		WebhookFailCounters:    webhookFails,
		WebhookDeadLetters:     webhookDeadLetters,
		WebhookFailLimit:       cfg.WebhookFailLimit,
		WebhookMaxParseBytes:   cfg.WebhookMaxParseBytes,
		WebhookDeadLetterProbe: sinkReady.Load,
		StatsDB:                queries,
		RateLimiter:            redisClient,
		RateLimitPerMinute:     cfg.APIRateLimitPerMinute,
		WebUIFS:                webui.FS(), // nil in pure-API build; embedded assets under `webui` tag
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
			logger.Error("HTTP server error", zap.Error(err))
			serveFatal <- err
		}
	}()

	// ── Storage startup self-check (IC-BUG-40) ──────────────────────────────
	// Runs in the BACKGROUND, after the listeners are up: init-minio.sh dials
	// the CP's webhook endpoint while it creates the CP identity, so a
	// blocking pre-listen check would deadlock a fresh deployment (the init
	// script could never reach the CP that is waiting on the identity the
	// script creates). Any failure — auth/config included — retries inside
	// MINIO_STARTUP_CHECK_TIMEOUT (default 120s, product decision 2026-11-06);
	// when the window expires still failing, Run logs the terminal Error with
	// ONLY the whitelisted diagnostic fields (item / class / endpoint /
	// access_key_id / s3_code / sts_code / http_status / net_kind / attempts /
	// window / fix_hint — never the secret, never raw error text; see
	// startupcheck/diagnostics.go) and returns a *startupcheck.Failure; the
	// goroutine ONLY hands THAT shape to run() — a plain context error
	// (SIGTERM during a failing check, rework 3 finding 1) sends nothing, so
	// a clean shutdown stays exit 0.
	// /healthz stays 200 during the window: smoke/dev flows rely on reaching
	// the API (incl. the webhook init talks to) before storage initialization
	// completes.
	//
	// ProbeBucket comes from config (MINIO_STARTUP_CHECK_BUCKET, default
	// data-sensor — no hardcoded literal). The bucket must PHYSICALLY exist:
	// NoSuchBucket is a FAILED attempt that retries inside the window
	// (rework 4 finding 1). bootstrap only writes DB records; the physical
	// bucket is created by the init scripts, so on a fresh deployment the
	// check retries until init completes — within the default 120s window.
	selfCheckFailed := make(chan *startupcheck.Failure, 1)
	go func() {
		err := startupcheck.Run(ctx, startupcheck.Config{
			Endpoint:    cfg.MinIOEndpoint,
			AccessKey:   cfg.MinIOAccessKey,
			SecretKey:   cfg.MinIOSecretKey,
			UseSSL:      cfg.MinIOUseSSL,
			RoleARN:     cfg.MinIORoleARN,
			ProbeBucket: cfg.MinIOStartupCheckBucket,
			Timeout:     cfg.MinIOStartupCheckTimeout,
			Logger:      logger,
		})
		if failure, ok := err.(*startupcheck.Failure); ok {
			// Window expired. ONLY deliver the failure — do NOT cancel ctx
			// here: a cancel would race ctx.Done() in the select below (Go
			// picks a ready branch at random), letting the failure be logged
			// as a plain "shutdown signal" and the process exit 0 (rework 1
			// finding 1). Ordering is resolved by awaitShutdownOrSelfCheckFailure's tie-break.
			selfCheckFailed <- failure
		}
	}()

	// ── Graceful shutdown ────────────────────────────────────────────────────
	// Rework 1 findings 1+2: the exit must be DETERMINISTIC (never a random
	// pick between ctx.Done() and the failure) and must run the FULL shutdown
	// path (zap's Fatal would os.Exit(1) from here, skipping httpSrv.Shutdown,
	// the event-source join and every defer in run()). So: wait for whichever
	// of the two happens first, then ALWAYS poll the failure channel once more
	// — a self-check failure wins over a coincidental signal — record the
	// diagnostics with Error, and let the shutdown block below complete before
	// run() returns the non-zero code.
	// Rework 2 finding 2: serveFatal participates IN the wait — a listener
	// (or late-init adapter) that dies must wake the shutdown path itself;
	// a single post-hoc poll would hang forever if nothing else fired.
	exitCode := awaitShutdownOrSelfCheckFailure(ctx, selfCheckFailed, serveFatal, logger, cfg)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// stop() releases the signal.NotifyContext resources and cancels ctx for
	// the workers; harmless on the signal path (already cancelled), REQUIRED
	// on the self-check path (nothing cancelled ctx yet — that is why the
	// shutdown below must do it, not the goroutine).
	stop()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", zap.Error(err))
	}
	// Lifecycle (review P2): wait for the storage-event-source adapter to
	// return so a Recv/Apply in flight at cancellation is not cut off
	// mid-write. Run exits on ctx cancellation; the bound only guards a wedged
	// final DB call from delaying process exit forever.
	if !awaitEventSourceStop(adapterDone, 10*time.Second) {
		logger.Error("storage event source adapter did not stop within 10s of shutdown")
	}
	if exitCode != 0 {
		logger.Error("control plane stopped with a startup/serve failure (non-zero exit)", zap.Int("exit_code", exitCode))
	} else {
		logger.Info("control plane stopped")
	}
	return exitCode
}

// awaitShutdownOrSelfCheckFailure blocks until either the shutdown context is
// cancelled or the background storage self-check reports a terminal failure,
// and returns the process exit code for that outcome (0 = clean signal path,
// 1 = IC-BUG-40 fail-closed).
//
// Determinism (rework 1 finding 1): when BOTH are ready — e.g. SIGTERM lands
// in the same scheduler slice as the window expiring — a bare select would
// pick a branch at random, and picking ctx.Done() would log a plain
// "shutdown signal" and let the process exit 0 with the failure swallowed.
// The tie-break poll below makes the failure win EVERY time. It is extracted
// from run() precisely so the determinism property can be loop-tested
// (TestAwaitShutdownOrSelfCheckFailure_DeterministicUnderRace).
func awaitShutdownOrSelfCheckFailure(
	ctx context.Context,
	selfCheckFailed <-chan *startupcheck.Failure,
	serveFatal <-chan error,
	logger *zap.Logger,
	cfg *config.Config,
) int {
	exitCode := 0
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, stopping…")
	case failure := <-selfCheckFailed:
		exitCode = 1
		logSelfCheckFailure(logger, cfg, failure)
	case err := <-serveFatal:
		// Rework 2 finding 2: a listener (gRPC/HTTP serve) or the storage
		// event source adapter died — the process cannot continue serving.
		// The error itself was already logged at its origin; here we only
		// convert it into the fail-closed exit code.
		_ = err
		exitCode = 1
	}
	// Deterministic tie-break: even if the signal fired first, a pending
	// failure converts the exit into the fail-closed one. buffered(1) +
	// single producer means this receive sees the failure if it was delivered.
	if failure, ok := drainStartupCheckFailure(selfCheckFailed); ok && exitCode == 0 {
		exitCode = 1
		logSelfCheckFailure(logger, cfg, failure)
	}
	return exitCode
}

// drainStartupCheckFailure receives a pending self-check failure without
// blocking; it is the non-blocking half of the tie-break above.
func drainStartupCheckFailure(ch <-chan *startupcheck.Failure) (*startupcheck.Failure, bool) {
	select {
	case f := <-ch:
		return f, true
	default:
		return nil, false
	}
}

// logSelfCheckFailure records the terminal IC-BUG-40 diagnostics at Error
// level (Fatal would os.Exit and skip the shutdown path — rework 1 finding
// 2). Only whitelisted fields (diagnostics.go) — no error text.
func logSelfCheckFailure(logger *zap.Logger, cfg *config.Config, failure *startupcheck.Failure) {
	logger.Error("storage startup self-check failed — refusing to run with a broken storage identity (IC-BUG-40)",
		failure.LogFields(cfg.MinIOEndpoint, cfg.MinIOAccessKey,
			startupcheck.FixHint(cfg.MinIORoleARN, cfg.MinIOStartupCheckBucket, failure.Item, failure.Diag))...)
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
