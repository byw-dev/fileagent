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
		uploaderCfgMu      sync.RWMutex
		currentUploaderCfg *uploader.Config
	)

	// updateCreds applies a fresh CredentialsPayload to both stsMgr and the
	// upload config captured by uploadFn.
	updateCreds := func(cred *agentv1.CredentialsPayload) {
		newSTS := &credential.STSCredentials{
			AccessKey:    cred.GetAccessKey(),
			SecretKey:    cred.GetSecretKey(),
			SessionToken: cred.GetSessionToken(),
			Expiry:       cred.GetExpiresAt().AsTime(),
		}
		stsMgr.SetSTS(newSTS)

		uploaderCfgMu.Lock()
		currentUploaderCfg = &uploader.Config{
			Endpoint:     cred.GetEndpoint(),
			AccessKey:    cred.GetAccessKey(),
			SecretKey:    cred.GetSecretKey(),
			SessionToken: cred.GetSessionToken(),
			UseSSL:       cred.GetUseSsl(),
			PartSizeMB:   cfg.Upload.PartSizeMB,
			Concurrency:  cfg.Upload.Concurrency,
		}
		uploaderCfgMu.Unlock()

		logger.Info("agent: STS credentials updated",
			zap.Time("expiry", newSTS.Expiry),
			zap.String("endpoint", cred.GetEndpoint()))
	}

	// ── Upload function: creates a fresh Uploader per call using current STS ─
	uploadFn := func(uploadCtx context.Context, task *queue.UploadTask) (*uploader.UploadResult, error) {
		uploaderCfgMu.RLock()
		ucfg := currentUploaderCfg
		uploaderCfgMu.RUnlock()
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

	// ── 7. Worker pool (starts goroutines after exec.Start is called) ────────
	exec := executor.New(cfg.Upload.Concurrency, q, uploadFn, logger, cfg.Upload.QueueMaxSize)

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
	grpcClient := grpcclient.New(cfg, logger)
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
			updateCreds(p.Credentials)

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
				cred, err := grpcClient.RefreshCredentials(ctx)
				if err != nil {
					logger.Warn("agent: refresh credentials failed", zap.Error(err))
					continue
				}
				updateCreds(cred)
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
func submitFile(ctx context.Context, exec *executor.Executor, q *queue.Queue, rule scheduler.CollectionRule, localPath string, size int64, mtime time.Time, fileOffset int64, appendMode string, agentCtx trollsift.AgentContext, logger *zap.Logger) {
	done, err := q.IsProcessed(ctx, rule.RuleID, localPath, mtime.Unix(), size)
	if err != nil {
		logger.Warn("agent: check processed failed", zap.String("path", localPath), zap.Error(err))
	}
	if done {
		return
	}
	task := &queue.UploadTask{
		ID:          uuid.New().String(),
		RuleID:      rule.RuleID,
		LocalPath:   localPath,
		StoragePath: buildStoragePath(rule, localPath, agentCtx, time.Now().UTC(), logger),
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

// buildStoragePath resolves the upload path template and returns the object
// key to use in MinIO.
//
// The template is normalised first (trollsift.NormalizeTemplate) so the key the
// agent writes and the template the Control Plane later reverse-parses agree on
// the leading separator; see docs/design/contracts.md V-3.
//
// If the template contains the {filename} variable it is substituted with the
// file's base name, and the result is used as-is. Otherwise the resolved prefix
// is treated as a directory and the file's base name is appended automatically.
//
// Every fallback to the bare base name is logged: a silent fallback is how
// IC-BUG-17 hid an agent whose identity fields were empty, flattening every
// upload into the bucket root.
func buildStoragePath(rule scheduler.CollectionRule, localPath string, agentCtx trollsift.AgentContext, now time.Time, logger *zap.Logger) string {
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

	fallback := filepath.Base(localPath)
	template := trollsift.NormalizeTemplate(rule.DestPathTemplate)

	destParser, err := trollsift.New(template)
	if err != nil {
		logger.Warn("agent: dest_path_template is not a valid pattern, falling back to base name",
			zap.String("rule_id", rule.RuleID),
			zap.String("template", rule.DestPathTemplate),
			zap.String("storage_path", fallback),
			zap.Error(err))
		return fallback
	}
	storagePath, err := destParser.Compose(fields, false)
	if err != nil {
		logger.Warn("agent: cannot resolve dest_path_template, falling back to base name",
			zap.String("rule_id", rule.RuleID),
			zap.String("template", rule.DestPathTemplate),
			zap.String("storage_path", fallback),
			zap.Error(err))
		return fallback
	}
	if storagePath == "" {
		logger.Warn("agent: dest_path_template resolved to an empty key, falling back to base name",
			zap.String("rule_id", rule.RuleID),
			zap.String("template", rule.DestPathTemplate),
			zap.String("storage_path", fallback))
		return fallback
	}
	return trollsift.NormalizeObjectKey(storagePath)
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
