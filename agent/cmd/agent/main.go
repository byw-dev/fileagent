// Package main is the entry point for the Edge Agent binary.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/byw-dev/fileagent/agent/internal/config"
	"github.com/byw-dev/fileagent/agent/internal/credential"
	"github.com/byw-dev/fileagent/agent/internal/executor"
	"github.com/byw-dev/fileagent/agent/internal/grpcclient"
	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/byw-dev/fileagent/agent/internal/scheduler"
	"github.com/byw-dev/fileagent/agent/internal/uploader"
	"github.com/byw-dev/fileagent/agent/internal/watcher"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/pkg/trollsift"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// processStart records when the process began, initialized at package load so
// uptime_seconds reflects true process uptime regardless of how long the
// registration/approval flow takes before the heartbeat builder is installed.
var processStart = time.Now()

// ruleHandle holds the cancel function for an active rule's watcher or scheduler entry.
type ruleHandle struct {
	cancel context.CancelFunc
}

// grpcClientTokenSetter captures the gRPC client behavior needed by revoke handling.
type grpcClientTokenSetter interface {
	SetToken(token string)
}

func main() {
	// ── 1. Load configuration ────────────────────────────────────────────────
	configFlag := flag.String("config", "", "Path to agent TOML config file (precedence: --config > AGENT_CONFIG > ./config.toml)")
	flag.Parse()

	cfgPath := *configFlag
	if cfgPath == "" {
		cfgPath = os.Getenv("AGENT_CONFIG")
	}
	if cfgPath == "" {
		cfgPath = "config.toml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: load config: %v\n", err)
		os.Exit(1)
	}

	// ── 2. Initialise logger ─────────────────────────────────────────────────
	logger, err := buildLogger(cfg.Log.Level)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: build logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	// ── 10. Signal context (declared early for clean shutdown throughout) ────
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ── 3. Open SQLite queue ─────────────────────────────────────────────────
	queuePath := filepath.Join(cfg.Agent.DataDir, "queue.db")
	q, err := queue.OpenWithLogger(queuePath, logger)
	if err != nil {
		logger.Fatal("queue open failed", zap.String("path", queuePath), zap.Error(err))
	}
	defer q.Close()
	resetTasks, err := q.ResetRunningToPending(ctx)
	if err != nil {
		logger.Fatal("queue recovery failed", zap.Error(err))
	}
	if resetTasks > 0 {
		logger.Info("agent: recovered interrupted upload tasks", zap.Int64("task_count", resetTasks))
	}

	// ── 4. Credential managers ───────────────────────────────────────────────
	fp, err := grpcclient.LoadOrCreateFingerprint(cfg.Agent.FingerprintFile)
	if err != nil {
		logger.Fatal("fingerprint load failed", zap.Error(err))
	}
	tokenMgr := credential.NewTokenManager(cfg.Agent.TokenFile, fp)
	stsMgr := credential.NewSTSManager()

	// ── Upload credentials (set when CredentialsPayload arrives via stream) ──
	var (
		// grpcClient is declared early so the credential-refresh closure below
		// (used when a put is denied, IC-BUG-20) can capture it; the client is
		// only created after the queue exists (section 5).
		grpcClient *grpcclient.Client

		// credGen assigns a strictly monotonic generation to every credential
		// acquisition — bumped when an acquisition STARTS (RPC) or when a pushed
		// payload is received. With two concurrent writers (periodic refresh and
		// the AccessDenied retry), a response requested earlier but arriving later
		// must never overwrite a newer session (review F2).
		credGen atomic.Uint64
	)
	creds := newCredentialHolder(stsMgr, cfg.Upload.PartSizeMB, cfg.Upload.Concurrency, logger)

	// updateCreds applies a fresh CredentialsPayload to both stsMgr and the
	// upload config captured by uploadFn. A payload whose generation is not
	// strictly newer is dropped whole — half-applying it would desync the
	// session from the uploader config.
	updateCreds := func(cred *agentv1.CredentialsPayload, generation uint64) {
		creds.Apply(cred, generation)
	}

	// ── Upload function: creates a fresh Uploader per call using current STS ─
	singleAttemptUpload := func(uploadCtx context.Context, task *queue.UploadTask) (*uploader.UploadResult, error) {
		ucfg := creds.Current()
		if ucfg == nil {
			return nil, fmt.Errorf("agent: no upload credentials available yet")
		}
		u, err := uploader.New(*ucfg, q, logger)
		if err != nil {
			return nil, fmt.Errorf("agent: create uploader: %w", err)
		}
		return uploadWithTimeout(
			uploadCtx,
			task,
			time.Duration(cfg.Upload.MinTimeoutSeconds)*time.Second,
			cfg.Upload.AssumedUploadBytesPerSecond,
			logger,
			u.UploadFile,
		)
	}

	// refreshSTS invalidates the held STS session and mints a fresh one from the
	// Control Plane. Used by uploadWithAccessDeniedRetry when a put is denied —
	// the held session covers only the bucket set known at mint time (IC-BUG-20).
	// The generation is taken when the request STARTS, so a response that raced
	// with a newer acquisition is dropped instead of overwriting it (review F2).
	refreshSTS := func(refreshCtx context.Context) error {
		stsMgr.Clear()
		generation := credGen.Add(1)
		cred, err := grpcClient.RefreshCredentials(refreshCtx)
		if err != nil {
			return fmt.Errorf("agent: refresh credentials: %w", err)
		}
		updateCreds(cred, generation)
		return nil
	}

	// ── 7. Worker pool (starts goroutines after exec.Start is called) ────────
	exec := executor.New(cfg.Upload.Concurrency, q, func(uploadCtx context.Context, task *queue.UploadTask) (*uploader.UploadResult, error) {
		return uploadWithAccessDeniedRetry(
			uploadCtx,
			task,
			time.Duration(cfg.Upload.MinTimeoutSeconds)*time.Second,
			cfg.Upload.AssumedUploadBytesPerSecond,
			logger,
			singleAttemptUpload,
			refreshSTS,
			creds.Generation,
		)
	}, logger, cfg.Upload.QueueMaxSize)

	// Terminal-state multipart cleanup (IC-3 ②): when the executor gives up on
	// a task (retry budget exhausted, terminal failure, or eviction), the
	// in-flight MinIO multipart upload recorded on it must be aborted or its
	// uploaded parts leak without bound. Best effort — uses the currently held
	// STS session; without credentials the abort is left to the bucket's
	// AbortIncompleteMultipartUpload ILM rule.
	if err := exec.ConfigureAbandon(func(ctx context.Context, task *queue.UploadTask) error {
		ucfg := creds.Current()
		if ucfg == nil {
			return fmt.Errorf("agent: no upload credentials available to abort upload of task %s", task.ID)
		}
		u, err := uploader.New(*ucfg, q, logger)
		if err != nil {
			return fmt.Errorf("agent: create uploader for abort: %w", err)
		}
		return u.AbandonUpload(ctx, task)
	}); err != nil {
		logger.Error("configure abandon hook", zap.Error(err))
		return
	}

	// ── Scheduler (cron-mode rules) ──────────────────────────────────────────
	sched := scheduler.New(logger)

	// ── Rule lifecycle management ─────────────────────────────────────────────
	var (
		ruleHandlesMu sync.Mutex
		ruleHandles   = make(map[string]*ruleHandle)
		agentCtx      trollsift.AgentContext
	)

	stopRule := func(ruleID string) {
		sched.RemoveRule(ruleID)
		ruleHandlesMu.Lock()
		if h, ok := ruleHandles[ruleID]; ok {
			h.cancel()
			delete(ruleHandles, ruleID)
		}
		ruleHandlesMu.Unlock()
	}

	applyRule := func(rule scheduler.CollectionRule) {
		stopRule(rule.RuleID)

		payload, _ := json.Marshal(rule)
		if err := q.UpsertRule(&queue.Rule{
			ID:        rule.RuleID,
			Payload:   string(payload),
			UpdatedAt: time.Now().Unix(),
		}); err != nil {
			logger.Warn("agent: upsert rule failed", zap.String("rule_id", rule.RuleID), zap.Error(err))
		}

		if !rule.Enabled {
			return
		}

		ruleCtx, cancel := context.WithCancel(ctx)

		ruleHandlesMu.Lock()
		ruleHandles[rule.RuleID] = &ruleHandle{cancel: cancel}
		ruleHandlesMu.Unlock()

		switch rule.Mode {
		case "watch":
			go runWatcher(ruleCtx, rule, exec, q, agentCtx, logger)
		case "cron":
			if err := sched.AddRule(rule, func(resolvedPath string) {
				walkAndSubmit(ruleCtx, exec, q, rule, resolvedPath, agentCtx, logger)
			}); err != nil {
				logger.Warn("agent: add cron rule failed",
					zap.String("rule_id", rule.RuleID), zap.Error(err))
			}
		default:
			cancel()
			ruleHandlesMu.Lock()
			delete(ruleHandles, rule.RuleID)
			ruleHandlesMu.Unlock()
			logger.Warn("agent: unknown rule mode",
				zap.String("rule_id", rule.RuleID), zap.String("mode", rule.Mode))
		}
	}

	// ── 5. gRPC client ───────────────────────────────────────────────────────
	grpcClient = grpcclient.New(cfg, logger)
	if err := grpcClient.Dial(); err != nil {
		logger.Error("grpc dial failed", zap.Error(err))
		return
	}
	defer grpcClient.Close()

	if err := exec.ConfigureReporting(grpcClient.SendMessage, time.Duration(cfg.Upload.ReportTimeoutSeconds)*time.Second, cfg.Upload.RetryMax); err != nil {
		logger.Error("configure reporting", zap.Error(err))
		return
	}

	// ── 8. Register ServerMessage handler ────────────────────────────────────
	grpcClient.SetMessageHandler(func(msg *agentv1.ServerMessage) {
		switch p := msg.GetPayload().(type) {
		case *agentv1.ServerMessage_Credentials:
			// A pushed payload carries no generation of its own — it takes the
			// next one at receipt. An in-flight refresh RPC that started
			// before this push therefore loses, which is correct: whatever the
			// CP pushed is at least as fresh as anything requested earlier.
			updateCreds(p.Credentials, credGen.Add(1))

		case *agentv1.ServerMessage_PushRule:
			rule := protoToSchedulerRule(p.PushRule.GetRule())
			if p.PushRule.GetRule().GetDryRun() {
				logger.Info("agent: dry-run rule", zap.String("rule_id", rule.RuleID))
				go handleDryRun(rule, grpcClient, agentCtx, logger)
			} else {
				logger.Info("agent: push rule", zap.String("rule_id", rule.RuleID), zap.String("mode", rule.Mode))
				applyRule(rule)
			}

		case *agentv1.ServerMessage_CancelRule:
			ruleID := p.CancelRule.GetRuleId()
			logger.Info("agent: cancel rule", zap.String("rule_id", ruleID))
			stopRule(ruleID)

		case *agentv1.ServerMessage_RulesSync:
			// IC-BUG-30 delete half, snapshot form (D-033): the snapshot is the
			// agent's complete rule set. Inactive rules stop through
			// applyRule's Enabled=false branch; rules deleted while
			// disconnected only exist as absences here.
			applyRulesSnapshot(
				protoToSchedulerRules(p.RulesSync.GetRules()),
				func() []string {
					ruleHandlesMu.Lock()
					defer ruleHandlesMu.Unlock()
					ids := make([]string, 0, len(ruleHandles))
					for id := range ruleHandles {
						ids = append(ids, id)
					}
					return ids
				},
				stopRule,
				applyRule,
				logger,
			)

		case *agentv1.ServerMessage_Revoke:
			handleRevokeCommand(tokenMgr, stsMgr, grpcClient, stop, logger, p.Revoke.GetReason())

		case *agentv1.ServerMessage_ListDirectory:
			go handleListDir(p.ListDirectory, grpcClient, logger)

		case *agentv1.ServerMessage_Ping:
			if err := grpcClient.SendMessage(&agentv1.AgentMessage{
				MessageId: uuid.New().String(),
				Payload:   &agentv1.AgentMessage_Heartbeat{Heartbeat: grpcClient.BuildHeartbeat()},
			}); err != nil {
				logger.Warn("agent: ping response failed", zap.Error(err))
			}

		case *agentv1.ServerMessage_Ack:
			if err := exec.HandleAcknowledgement(ctx, p.Ack); err != nil {
				logger.Warn("agent: acknowledgement persistence failed", zap.Error(err))
			}
		}
	})

	// ── 6. Registration / approval lifecycle (blocks until APPROVED) ─────────
	lc := grpcclient.NewLifecycle(tokenMgr, stsMgr)
	if err := lc.Start(ctx, grpcClient.ServiceClient(), cfg, logger); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			logger.Info("agent: shutdown during registration", zap.Error(err))
		} else {
			logger.Error("agent: registration failed, exiting", zap.Error(err))
		}
		return
	}
	grpcClient.SetToken(lc.TokenManager.Token())
	grpcClient.SetAgentID(lc.AgentID)
	agentCtx.AgentID = lc.AgentID
	agentCtx.AgentName = lc.AgentName

	// Self-heal: if the Control Plane ever rejects our token (expired or the CP
	// was restarted), re-run the approval poll to mint a fresh one and persist
	// it, so a reconnect after token expiry recovers instead of looping forever
	// on Unauthenticated. See docs/reports/design-gap-analysis G-2 / 06 E-1.
	agentID := lc.AgentID
	grpcClient.SetReauthFunc(func(ctx context.Context) (string, error) {
		token, err := grpcclient.ReAuthenticate(ctx, grpcClient.ServiceClient(), agentID, fp, logger)
		if err != nil {
			return "", err
		}
		if err := tokenMgr.Save(token); err != nil {
			logger.Warn("agent: persist reauth token failed", zap.Error(err))
		}
		return token, nil
	})

	// Populate the heartbeat with live telemetry (G-4): without this the CP and
	// Web UI have no source for queue depth / uptime / version, and the queue
	// backlog alert can never fire. disks and upload_bps are not yet reported.
	grpcClient.SetHeartbeatFunc(func() *agentv1.Heartbeat {
		depth, err := q.CountPending()
		if err != nil {
			logger.Warn("agent: heartbeat queue depth failed", zap.Error(err))
		}
		return &agentv1.Heartbeat{
			AgentId:       lc.AgentID,
			UptimeSeconds: int64(time.Since(processStart).Seconds()),
			QueueDepth:    int32(depth),
			Version:       grpcclient.AgentVersion,
		}
	})

	logger.Info("agent: approved, starting normal operation", zap.String("agent_id", lc.AgentID))

	if err := grpcClient.Connect(ctx); err != nil {
		logger.Error("grpc connect failed", zap.Error(err))
		return
	}

	// ── Start scheduler and executor ─────────────────────────────────────────
	sched.Start()
	defer sched.Stop()
	exec.Start(ctx)
	defer exec.Stop()

	// ── 9. Background STS refresh goroutine ──────────────────────────────────
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// A nil sts used to short-circuit here, so an agent that had
				// never been given credentials could never ask for any
				// (IC-BUG-1). Missing credentials are precisely the case that
				// must trigger a refresh.
				sts := stsMgr.GetSTS()
				if sts != nil && time.Until(sts.Expiry) > 10*time.Minute {
					continue
				}
				logger.Info("agent: refreshing STS credentials")
				// Generation taken at REQUEST time (review F2): a response that
				// races with a newer acquisition is dropped on arrival.
				generation := credGen.Add(1)
				cred, err := grpcClient.RefreshCredentials(ctx)
				if err != nil {
					logger.Warn("agent: refresh credentials failed", zap.Error(err))
					continue
				}
				updateCreds(cred, generation)
			}
		}
	}()

	logger.Info("agent: running",
		zap.String("agent_id", lc.AgentID),
		zap.String("grpc_endpoint", cfg.Server.Endpoint))

	<-ctx.Done()
	logger.Info("agent: shutdown signal received, stopping…")
}

// uploadWithTimeout gives each upload its own size-derived deadline so a
// stalled object-store request cannot occupy an executor worker forever.
// The deadline is derived from the file's CURRENT size (os.Stat at upload
// time), because uploader.UploadFile stats the file itself and moves that
// many bytes; task.FileSize is frozen at detection time and understates
// files that grew while waiting in the queue (PR #100 review R2). Never
// derived from the tail increment: multipart uploads ignore FileOffset and
// move the whole file, and UploadFile hashes the entire file before
// transferring (PR #100 review F4). The deadline is a safety net; wider is
// better than shorter.
func uploadWithTimeout(parent context.Context, task *queue.UploadTask, minimum time.Duration, assumedBytesPerSecond int64, logger *zap.Logger, upload executor.UploadFunc) (*uploader.UploadResult, error) {
	uploadSize := task.FileSize
	if info, err := os.Stat(task.LocalPath); err != nil {
		logger.Warn("agent: cannot stat file for upload deadline, using stored size",
			zap.String("path", task.LocalPath), zap.Error(err))
	} else {
		uploadSize = info.Size()
	}
	uploadCtx, cancel := context.WithTimeout(parent, uploadTimeoutForSize(uploadSize, minimum, assumedBytesPerSecond))
	defer cancel()
	return upload(uploadCtx, task)
}

// uploadTimeoutForSize derives a conservative deadline from the configured
// assumed upload throughput while never returning less than the configured
// minimum. The rate comes from config (upload.assumed_upload_bytes_per_second):
// a hardwired 1 MiB/s made any large file permanently fail on slow links
// (PR #100 review R3).
func uploadTimeoutForSize(fileSize int64, minimum time.Duration, assumedBytesPerSecond int64) time.Duration {
	if fileSize <= 0 {
		return minimum
	}
	seconds := fileSize / assumedBytesPerSecond
	if fileSize%assumedBytesPerSecond != 0 {
		seconds++
	}
	const maxDurationSeconds = int64((1<<63 - 1) / int64(time.Second))
	if seconds > maxDurationSeconds {
		return time.Duration(1<<63 - 1)
	}
	derived := time.Duration(seconds) * time.Second
	if derived < minimum {
		return minimum
	}
	return derived
}

// credentialSession is the STS-session dependency of credentialHolder.
// A small interface (not the concrete manager) so tests can inject a store
// whose SetSTS blocks deterministically and thereby construct the exact
// interleaving that tears generation and uploader credentials apart (review
// R4 — the probabilistic version of that test needed scheduling luck and
// survived the split mutation 5/5).
type credentialSession interface {
	SetSTS(cred *credential.STSCredentials, generation uint64) bool
	Generation() uint64
}

// credentialHolder owns the agent's live STS session and the uploader config
// derived from it, keyed by a strictly monotonic generation (review F2). The
// two writers — the periodic refresh goroutine and the AccessDenied retry
// path — both funnel through Apply; a payload whose generation is not newer
// than the held one is dropped WHOLE, so a late response can never resurrect
// stale credentials (e.g. ones not covering a just-added bucket) and turn the
// retry's second attempt into a terminal failure.
type credentialHolder struct {
	sts credentialSession
	mu  sync.RWMutex
	// cfg and generation are the published credential pair: both are written
	// inside Apply's single critical section and both are read under the same
	// mutex, so no reader — Current(), Generation(), or Snapshot() — can ever
	// see a generation whose credentials have not been published (R5-A).
	cfg         *uploader.Config
	generation  uint64
	partSizeMB  int
	concurrency int
	logger      *zap.Logger
}

// newCredentialHolder creates a holder over the given STS session store.
func newCredentialHolder(sts credentialSession, partSizeMB, concurrency int, logger *zap.Logger) *credentialHolder {
	return &credentialHolder{sts: sts, logger: logger, partSizeMB: partSizeMB, concurrency: concurrency}
}

// Apply validates the generation, then applies the payload to both the STS
// session and the uploader config in ONE critical section, and reports
// whether it was applied.
//
// The single critical section is the point: with two concurrent writers (the
// periodic refresh and the AccessDenied retry), a split application lets an
// older-but-late response slip between the generation check and the config
// write — the observed state becomes "generation N, uploader credentials N-1"
// and every upload then fails on stale credentials until the next
// acquisition. A concurrency probe reproduced exactly that (generation=64 /
// uploader=AK-63); the review (R1) requires the check, both writes and the
// publication to be inseparable. Nothing here may be moved outside h.mu.
//
// Consistency scope: readers of Current() take the same mutex, so an upload
// never sees a torn pair. The periodic refresh ticker reads only sts.Expiry
// through the STS manager's own lock; a mid-apply read of a newer expiry
// there is conservative-correct (it defers a refresh that is unnecessary —
// newer credentials are being installed at that very moment).
func (h *credentialHolder) Apply(cred *agentv1.CredentialsPayload, generation uint64) bool {
	if cred == nil {
		return false
	}
	newSTS := &credential.STSCredentials{
		AccessKey:    cred.GetAccessKey(),
		SecretKey:    cred.GetSecretKey(),
		SessionToken: cred.GetSessionToken(),
		Expiry:       cred.GetExpiresAt().AsTime(),
	}
	cfg := &uploader.Config{
		Endpoint:     cred.GetEndpoint(),
		AccessKey:    cred.GetAccessKey(),
		SecretKey:    cred.GetSecretKey(),
		SessionToken: cred.GetSessionToken(),
		UseSSL:       cred.GetUseSsl(),
		PartSizeMB:   h.partSizeMB,
		Concurrency:  h.concurrency,
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.sts.SetSTS(newSTS, generation) {
		h.logger.Info("agent: stale credentials response dropped",
			zap.Uint64("generation", generation),
			zap.Uint64("current_generation", h.generation))
		return false
	}
	h.cfg = cfg
	h.generation = generation
	h.logger.Info("agent: STS credentials updated",
		zap.Uint64("generation", generation),
		zap.Time("expiry", newSTS.Expiry),
		zap.String("endpoint", cfg.Endpoint))
	return true
}

// Current returns the uploader config derived from the applied credentials,
// or nil when no credentials have been applied yet.
func (h *credentialHolder) Current() *uploader.Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

// Snapshot returns the uploader config and the generation of the credentials
// it was built from, read under one lock — the pair always comes from the
// same Apply. Callers that need both values must use this, never Current()
// followed by Generation(): two separate reads can straddle an Apply and
// yield a pair from two different publications (R5-A).
func (h *credentialHolder) Snapshot() (*uploader.Config, uint64) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg, h.generation
}

// Generation reports the generation of the held credentials, served from the
// holder's own cache under the same mutex as Current(). It deliberately does
// NOT consult the STS manager's internal lock: reading the session store
// directly let a generation slip out while the credentials it describes were
// still unpublished, and uploadWithAccessDeniedRetry — which decides terminal
// failure from this value — would then judge on a torn view (R5-A: the write
// side was made atomic in R1, the read side had to follow).
func (h *credentialHolder) Generation() uint64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.generation
}

// isAccessDenied reports whether err is (or wraps) a MinIO AccessDenied
// response — the signal that the held STS session cannot write the target
// bucket (IC-BUG-20). The uploader wraps transport errors with %w, so the
// chain is unwrapped rather than the surface type inspected.
func isAccessDenied(err error) bool {
	if err == nil {
		return false
	}
	var resp minio.ErrorResponse
	return errors.As(err, &resp) && resp.Code == "AccessDenied"
}

// uploadWithAccessDeniedRetry runs one upload attempt and, on AccessDenied,
// invalidates the held STS session, refreshes it and retries (IC-BUG-20).
//
// The held session covers only the bucket set known when the Control Plane
// minted it, so a rule pointing at a newly added bucket denies every put
// until the session is re-issued — up to ~50 minutes on the agent's own
// refresh tick, retrying exhausted without self-healing.
//
// generation reports the credential generation in force (review F2). Two
// writers race here: the periodic refresh goroutine and this path. The
// generation guard in the credential holder already drops stale responses, so
// the retry normally runs on exactly the credentials this path refreshed. But
// if the generation CHANGES while the second attempt runs, that attempt may
// have executed on someone else's credentials — in that case one further
// attempt is made on the newest credentials instead of declaring the task
// terminally failed on possibly-stale grounds. A terminal verdict requires
// the generation to be stable across an attempt (at most one bounce; every
// attempt re-reads the newest credentials).
//
// A second AccessDenied on stable credentials is terminally failed
// (executor.ErrTerminalUpload) and alarmed: a persistent denial means bucket
// policy or rule configuration is wrong, and retrying would turn that into a
// silent refresh loop. A refresh failure propagates the original
// AccessDenied unchanged — that is transient, not a policy verdict, so the
// executor's backoff may retry later.
func uploadWithAccessDeniedRetry(
	parent context.Context,
	task *queue.UploadTask,
	minimum time.Duration,
	assumedBytesPerSecond int64,
	logger *zap.Logger,
	upload executor.UploadFunc,
	refresh func(ctx context.Context) error,
	generation func() uint64,
) (*uploader.UploadResult, error) {
	result, err := uploadWithTimeout(parent, task, minimum, assumedBytesPerSecond, logger, upload)
	if !isAccessDenied(err) {
		return result, err
	}
	logger.Warn("agent: upload denied, invalidating credentials and refreshing once",
		zap.String("task_id", task.ID),
		zap.String("bucket", task.Bucket),
		zap.Error(err))
	if rErr := refresh(parent); rErr != nil {
		logger.Error("agent: credential refresh after AccessDenied failed, will retry on executor backoff",
			zap.String("task_id", task.ID), zap.Error(rErr))
		return nil, err
	}
	genBefore := generation()
	result, err = uploadWithTimeout(parent, task, minimum, assumedBytesPerSecond, logger, upload)
	if !isAccessDenied(err) {
		return result, err
	}
	if generation() == genBefore {
		// The generation held steady across the attempt: these ARE the
		// credentials this path refreshed, and they are still denied — a
		// genuine policy/configuration verdict, not a race.
		logger.Error("agent: upload still denied after one credential refresh; failing task terminally "+
			"(check the bucket policy and the rule's template/configuration)",
			zap.String("task_id", task.ID),
			zap.String("bucket", task.Bucket),
			zap.Error(err))
		return nil, fmt.Errorf("%w: still AccessDenied after one credential refresh: %v",
			executor.ErrTerminalUpload, err)
	}
	// The generation moved while the attempt ran: the attempt may have
	// executed on someone else's credentials. One further attempt on the
	// newest credentials — a policy verdict requires stable credentials.
	logger.Warn("agent: credential generation changed during retry, attempting once more on the newest credentials",
		zap.String("task_id", task.ID),
		zap.Uint64("generation_before", genBefore),
		zap.Uint64("generation_now", generation()))
	result, err = uploadWithTimeout(parent, task, minimum, assumedBytesPerSecond, logger, upload)
	if isAccessDenied(err) {
		logger.Error("agent: upload denied on the newest credentials; failing task terminally "+
			"(check the bucket policy and the rule's template/configuration)",
			zap.String("task_id", task.ID),
			zap.String("bucket", task.Bucket),
			zap.Error(err))
		return nil, fmt.Errorf("%w: still AccessDenied on the newest credentials: %v",
			executor.ErrTerminalUpload, err)
	}
	return result, err
}

// handleRevokeCommand clears local credentials and triggers graceful shutdown.
func handleRevokeCommand(tokenMgr *credential.TokenManager, stsMgr *credential.STSManager, client grpcClientTokenSetter, stop context.CancelFunc, logger *zap.Logger, reason string) {
	logger.Warn("agent: token revoked by Control Plane", zap.String("reason", reason))
	if err := tokenMgr.Clear(); err != nil {
		logger.Warn("agent: clear token failed", zap.Error(err))
	}
	stsMgr.Clear()
	if client != nil {
		client.SetToken("")
	}
	stop()
}

// buildLogger creates a zap logger configured for the given level string.
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

// protoToSchedulerRule converts a protobuf CollectionRule to the scheduler type.
func protoToSchedulerRule(r *agentv1.CollectionRule) scheduler.CollectionRule {
	if r == nil {
		return scheduler.CollectionRule{}
	}
	return scheduler.CollectionRule{
		RuleID:           r.GetRuleId(),
		Name:             r.GetName(),
		Mode:             r.GetMode(),
		BasePath:         r.GetBasePath(),
		PathPattern:      r.GetPathPattern(),
		UploadBucket:     r.GetUploadBucket(),
		DestPathTemplate: r.GetDestPathTemplate(),
		Recursive:        r.GetRecursive(),
		CronExpr:         r.GetCronExpr(),
		RunOnceOnStart:   r.GetRunOnceOnStart(),
		AppendMode:       r.GetAppendMode(),
		Enabled:          r.GetEnabled(),
	}
}

// protoToSchedulerRules converts a snapshot of protobuf CollectionRules to the
// scheduler type (D-033).
func protoToSchedulerRules(rs []*agentv1.CollectionRule) []scheduler.CollectionRule {
	out := make([]scheduler.CollectionRule, 0, len(rs))
	for _, r := range rs {
		out = append(out, protoToSchedulerRule(r))
	}
	return out
}

// runWatcher starts a file-system watcher for the given watch-mode rule and
// submits upload tasks to the executor for every create/write event.
func runWatcher(ctx context.Context, rule scheduler.CollectionRule, exec *executor.Executor, q *queue.Queue, agentCtx trollsift.AgentContext, logger *zap.Logger) {
	w, err := watcher.New(rule.BasePath, "*", rule.Recursive, 0, rule.AppendMode, logger)
	if err != nil {
		logger.Warn("agent: watcher init failed",
			zap.String("rule_id", rule.RuleID), zap.Error(err))
		return
	}
	// Rebuild tail offsets from persisted state before the initial scan:
	// without this, a restart re-sends already-stored bytes from offset 0
	// because the watcher's in-memory map starts empty (PR #100 review F3).
	if rule.AppendMode == watcher.AppendModeTail {
		offsets, err := q.TailOffsets(ctx, rule.RuleID)
		if err != nil {
			logger.Warn("agent: cannot restore tail offsets, tail uploads will restart from 0",
				zap.String("rule_id", rule.RuleID), zap.Error(err))
		} else {
			w.SeedTailOffsets(offsets)
		}
	}
	events := make(chan watcher.FileEvent, 64)
	go func() {
		if err := w.Start(ctx, events); err != nil && ctx.Err() == nil {
			logger.Warn("agent: watcher error",
				zap.String("rule_id", rule.RuleID), zap.Error(err))
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Op == "remove" {
				continue
			}
			matched, matchErr := matchGlob(rule, ev.Path)
			if matchErr != nil {
				logger.Warn("agent: match path failed",
					zap.String("rule_id", rule.RuleID),
					zap.String("path", ev.Path),
					zap.Error(matchErr),
				)
				continue
			}
			if !matched {
				continue
			}
			submitFile(ctx, exec, q, rule, ev.Path, ev.Size, ev.ModTime, ev.FileOffset, rule.AppendMode, agentCtx, logger)
		}
	}
}

// walkAndSubmit walks basePath and submits an upload task for every file
// matching rule.PathPattern that has not already been processed.
func walkAndSubmit(ctx context.Context, exec *executor.Executor, q *queue.Queue, rule scheduler.CollectionRule, basePath string, agentCtx trollsift.AgentContext, logger *zap.Logger) {
	err := filepath.WalkDir(basePath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		matched, matchErr := matchGlob(rule, path)
		if matchErr != nil {
			logger.Warn("agent: match path failed",
				zap.String("rule_id", rule.RuleID),
				zap.String("path", path),
				zap.Error(matchErr),
			)
			return nil
		}
		if !matched {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		submitFile(ctx, exec, q, rule, path, info.Size(), info.ModTime(), 0, rule.AppendMode, agentCtx, logger)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		logger.Warn("agent: walkdir failed",
			zap.String("rule_id", rule.RuleID), zap.String("path", basePath), zap.Error(err))
	}
}

// submitFile checks deduplication and enqueues an upload task.
//
// Since IC-BUG-21 a dest_path_template that cannot be resolved refuses the
// task: nothing is enqueued, so nothing is ever written to a guessed object
// key. The task is not enqueued in a doomed state either — a broken-template
// rule watching a busy directory would otherwise flood the queue and its
// capacity eviction would shed healthy backlog. Files re-collect naturally
// once the template is fixed (cron walk / next write event).
func submitFile(ctx context.Context, exec *executor.Executor, q *queue.Queue, rule scheduler.CollectionRule, localPath string, size int64, mtime time.Time, fileOffset int64, appendMode string, agentCtx trollsift.AgentContext, logger *zap.Logger) {
	done, err := q.IsProcessed(ctx, rule.RuleID, localPath, mtime.Unix(), size)
	if err != nil {
		logger.Warn("agent: check processed failed", zap.String("path", localPath), zap.Error(err))
	}
	if done {
		return
	}
	storagePath, pathErr := buildStoragePath(rule, localPath, agentCtx, time.Now().UTC(), logger)
	if pathErr != nil {
		// The first occurrence carries the full detail; duplicates for the same
		// rule are Debug-logged inside buildStoragePath (IC-BUG-21: one Warn,
		// not 5000).
		logger.Debug("agent: upload task refused, object key unresolved",
			zap.String("rule_id", rule.RuleID),
			zap.String("path", localPath),
			zap.Error(pathErr))
		return
	}
	task := &queue.UploadTask{
		ID:          uuid.New().String(),
		RuleID:      rule.RuleID,
		LocalPath:   localPath,
		StoragePath: storagePath,
		Bucket:      rule.UploadBucket,
		FileSize:    size,
		FileMtime:   mtime.Unix(),
		Status:      queue.StatusPending,
		FileOffset:  fileOffset,
		AppendMode:  appendMode,
	}
	if err := exec.Submit(ctx, task); err != nil {
		logger.Warn("agent: submit task failed",
			zap.String("rule_id", rule.RuleID), zap.String("path", localPath), zap.Error(err))
	}
}

// templateRefusals deduplicates the per-rule dest_path_template failure
// warning over the process lifetime (IC-BUG-21): one misconfigured rule
// watching 5000 files must produce one full Warn, not 5000 that an operator
// will mute — a per-file warning storm is how IC-BUG-21 degraded into noise.
var templateRefusals sync.Map

// buildStoragePath resolves the upload path template and returns the object
// key to use in MinIO, or an error when the template cannot be resolved.
//
// The template is normalised first (trollsift.NormalizeTemplate) so the key the
// agent writes and the template the Control Plane later reverse-parses agree on
// the leading separator; see docs/design/contracts.md V-3.
//
// Since IC-BUG-21 resolution failure fails the task instead of guessing a key:
// with D-030's bucket-wide policy a wrong object key is no longer caught by any
// 403 — a guessed key silently lands in the wrong place, polluting the index
// and the IC-6 reconciliation shard tree. A silent fallback was exactly how
// IC-BUG-17 flattened every upload into the bucket root.
func buildStoragePath(rule scheduler.CollectionRule, localPath string, agentCtx trollsift.AgentContext, now time.Time, logger *zap.Logger) (string, error) {
	relPath, err := filepath.Rel(rule.BasePath, localPath)
	if err != nil {
		relPath = filepath.Base(localPath)
	}
	relPath = filepath.ToSlash(relPath)

	fields := map[string]trollsift.Value{}
	if trollsift.IsTrollsiftPattern(rule.PathPattern) {
		p, pErr := trollsift.New(rule.PathPattern)
		if pErr == nil {
			parsed, parseErr := p.Parse(relPath)
			if parseErr == nil {
				fields = parsed
			}
		}
	}

	fields = trollsift.InjectContext(agentCtx, fields)
	fields["filename"] = trollsift.S(filepath.Base(localPath))
	fields["ext"] = trollsift.S(strings.TrimPrefix(filepath.Ext(localPath), "."))
	if strings.Contains(rule.DestPathTemplate, "{time") {
		fields["time"] = trollsift.T(now)
	}

	fail := func(cause string, causeErr error) (string, error) {
		err := fmt.Errorf("dest_path_template for rule %s cannot be resolved (%s): template %q: %w",
			rule.RuleID, cause, rule.DestPathTemplate, causeErr)
		if _, dup := templateRefusals.LoadOrStore(rule.RuleID, true); dup {
			logger.Debug("agent: dest_path_template still unresolvable for this rule (first occurrence already logged)",
				zap.String("rule_id", rule.RuleID))
			return "", err
		}
		logger.Warn("agent: dest_path_template unresolvable, refusing to guess an object key — task will not be enqueued "+
			"(fix the rule's dest_path_template; existing files are re-collected on the next cron walk or write event)",
			zap.String("rule_id", rule.RuleID),
			zap.String("template", rule.DestPathTemplate),
			zap.Error(causeErr))
		return "", err
	}

	destParser, err := trollsift.New(trollsift.NormalizeTemplate(rule.DestPathTemplate))
	if err != nil {
		return fail("not a valid pattern", err)
	}
	storagePath, err := destParser.Compose(fields, false)
	if err != nil {
		return fail("unresolvable field", err)
	}
	if storagePath == "" {
		return fail("resolved to an empty key", fmt.Errorf("composed key is empty"))
	}
	return trollsift.NormalizeObjectKey(storagePath), nil
}

// applyRulesSnapshot replaces the agent's whole rule set with the synced
// snapshot (IC-BUG-30 / D-033). Rules held but absent from the snapshot were
// deleted while the agent was disconnected — their absence is the only signal
// it ever gets, and with D-030's bucket-wide policy their uploads would
// otherwise continue unchecked until process restart. Snapshot rules are then
// applied through applyRule, whose Enabled=false branch stops paused rules;
// an empty snapshot therefore means "everything was deleted".
func applyRulesSnapshot(rules []scheduler.CollectionRule, held func() []string, stop func(ruleID string), apply func(scheduler.CollectionRule), logger *zap.Logger) {
	ids := make([]string, 0, len(rules))
	for _, r := range rules {
		ids = append(ids, r.RuleID)
	}
	stopRulesOutsideSync(ids, held, stop, logger)
	for _, r := range rules {
		apply(r)
	}
}

// stopRulesOutsideSync stops every currently-held rule outside the synced
// full set (IC-BUG-30, D-033). RulesSyncCommand.rule_ids is the complete set
// of rule ids the Control Plane still has for this agent; rules deleted while
// the agent was disconnected are absent by design, and their absence is the
// only signal it ever gets — with D-030's bucket-wide policy their uploads
// would otherwise continue unchecked until process restart. Inactive rules do
// not pass through here: they arrive as ordinary pushes whose Enabled=false
// makes applyRule stop them.
func stopRulesOutsideSync(ruleIDs []string, held func() []string, stop func(ruleID string), logger *zap.Logger) {
	inSet := make(map[string]struct{}, len(ruleIDs))
	for _, id := range ruleIDs {
		inSet[id] = struct{}{}
	}
	for _, id := range held() {
		if _, ok := inSet[id]; !ok {
			logger.Info("agent: stopping rule outside the synced full set (deleted while disconnected)",
				zap.String("rule_id", id))
			stop(id)
		}
	}
}

// matchGlob matches a local absolute path against rule.PathPattern using relative-path semantics.
func matchGlob(rule scheduler.CollectionRule, absPath string) (bool, error) {
	relPath, err := filepath.Rel(rule.BasePath, absPath)
	if err != nil {
		return false, err
	}
	relPath = filepath.ToSlash(relPath)

	if trollsift.IsTrollsiftPattern(rule.PathPattern) {
		parser, err := trollsift.New(rule.PathPattern)
		if err != nil {
			return false, err
		}
		return doublestar.Match(parser.Globify(), relPath)
	}
	return doublestar.Match(rule.PathPattern, relPath)
}

// defaultDryRunLimit is the maximum number of files returned per dry-run scan.
const defaultDryRunLimit = 10

// handleDryRun walks rule.BasePath, matches files against rule.PathPattern,
// composes the destination path for each match, and sends a DryRunResult back
// to the Control Plane over the gRPC stream.
func handleDryRun(rule scheduler.CollectionRule, client *grpcclient.Client, agentCtx trollsift.AgentContext, logger *zap.Logger) {
	result := &agentv1.DryRunResult{RuleId: rule.RuleID}

	var pathParser *trollsift.Parser
	if trollsift.IsTrollsiftPattern(rule.PathPattern) {
		p, err := trollsift.New(rule.PathPattern)
		if err != nil {
			result.Error = err.Error()
			sendDryRunResult(client, result, logger)
			return
		}
		pathParser = p
	}

	var globPat string
	if pathParser != nil {
		globPat = pathParser.Globify()
	} else {
		globPat = rule.PathPattern
	}

	count := 0
	_ = filepath.WalkDir(rule.BasePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if count >= defaultDryRunLimit {
			return filepath.SkipAll
		}

		relPath, relErr := filepath.Rel(rule.BasePath, path)
		if relErr != nil {
			return nil
		}
		relPath = filepath.ToSlash(relPath)

		matched, _ := doublestar.Match(globPat, relPath)
		if !matched {
			return nil
		}

		fileResult := &agentv1.DryRunFileResult{
			LocalPath:    path,
			ParsedFields: make(map[string]string),
		}

		fields := map[string]trollsift.Value{}
		if pathParser != nil {
			if parsed, pErr := pathParser.Parse(relPath); pErr == nil {
				for k, v := range parsed {
					fields[k] = v
					fileResult.ParsedFields[k] = v.Raw // original matched substring for display
				}
			}
		}
		fields = trollsift.InjectContext(agentCtx, fields)
		fields["filename"] = trollsift.S(filepath.Base(path))
		fields["ext"] = trollsift.S(strings.TrimPrefix(filepath.Ext(path), "."))

		// Normalise exactly as buildStoragePath does: the dry-run preview sits
		// next to the Web UI's own preview in the same form, so showing a
		// different key than the upload would actually produce is worse than
		// showing nothing.
		destParser, dErr := trollsift.New(trollsift.NormalizeTemplate(rule.DestPathTemplate))
		if dErr != nil {
			fileResult.ComposeError = dErr.Error()
		} else {
			uploadPath, cErr := destParser.Compose(fields, false)
			if cErr != nil {
				fileResult.ComposeError = cErr.Error()
			} else {
				fileResult.UploadPath = trollsift.NormalizeObjectKey(uploadPath)
			}
		}

		result.Files = append(result.Files, fileResult)
		count++
		return nil
	})

	sendDryRunResult(client, result, logger)
}

// sendDryRunResult sends a DryRunResult proto message over the gRPC stream.
func sendDryRunResult(client *grpcclient.Client, result *agentv1.DryRunResult, logger *zap.Logger) {
	msg := &agentv1.AgentMessage{
		MessageId: uuid.New().String(),
		Payload:   &agentv1.AgentMessage_DryRunResult{DryRunResult: result},
	}
	if err := client.SendMessage(msg); err != nil {
		logger.Warn("agent: send dry-run result failed",
			zap.String("rule_id", result.GetRuleId()), zap.Error(err))
	}
}

// handleListDir walks the requested path and sends a DirectoryListing response.
func handleListDir(cmd *agentv1.ListDirectoryCommand, client *grpcclient.Client, logger *zap.Logger) {
	listing := &agentv1.DirectoryListing{
		RequestId: cmd.GetRequestId(),
		Path:      cmd.GetPath(),
	}

	walkErr := filepath.WalkDir(cmd.GetPath(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == cmd.GetPath() {
			return nil
		}
		rel, _ := filepath.Rel(cmd.GetPath(), path)
		depth := int32(len(strings.Split(rel, string(os.PathSeparator))))

		if d.IsDir() && (!cmd.GetRecursive() || (cmd.GetMaxDepth() > 0 && depth >= cmd.GetMaxDepth())) {
			info, infoErr := d.Info()
			if infoErr == nil {
				listing.Entries = append(listing.Entries, &agentv1.FsEntry{
					Name:        d.Name(),
					IsDir:       true,
					ModifiedAt:  timestamppb.New(info.ModTime()),
					Permissions: info.Mode().String(),
				})
			}
			return fs.SkipDir
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		listing.Entries = append(listing.Entries, &agentv1.FsEntry{
			Name:        d.Name(),
			IsDir:       d.IsDir(),
			SizeBytes:   info.Size(),
			ModifiedAt:  timestamppb.New(info.ModTime()),
			Permissions: info.Mode().String(),
		})
		return nil
	})

	if walkErr != nil {
		listing.Error = walkErr.Error()
	}

	msg := &agentv1.AgentMessage{
		MessageId: uuid.New().String(),
		Payload: &agentv1.AgentMessage_DirectoryListing{
			DirectoryListing: listing,
		},
	}
	if err := client.SendMessage(msg); err != nil {
		logger.Warn("agent: send directory listing failed",
			zap.String("request_id", cmd.GetRequestId()), zap.Error(err))
	}
}
