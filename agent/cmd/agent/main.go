// Package main is the entry point for the Edge Agent binary.
package main

import (
	"context"
	"encoding/json"
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

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/agent/internal/config"
	"github.com/byw-dev/fileagent/agent/internal/credential"
	"github.com/byw-dev/fileagent/agent/internal/executor"
	"github.com/byw-dev/fileagent/agent/internal/grpcclient"
	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/byw-dev/fileagent/agent/internal/scheduler"
	"github.com/byw-dev/fileagent/agent/internal/uploader"
	"github.com/byw-dev/fileagent/agent/internal/watcher"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ruleHandle holds the cancel function for an active rule's watcher or scheduler entry.
type ruleHandle struct {
	cancel context.CancelFunc
}

func main() {
	// ── 1. Load configuration ────────────────────────────────────────────────
	configFlag := flag.String("config", "", "Path to agent TOML configuration file")
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
	q, err := queue.Open(queuePath)
	if err != nil {
		logger.Fatal("queue open failed", zap.String("path", queuePath), zap.Error(err))
	}
	defer q.Close()

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
			Endpoint:    cred.GetEndpoint(),
			AccessKey:   cred.GetAccessKey(),
			SecretKey:   cred.GetSecretKey(),
			SessionToken: cred.GetSessionToken(),
			UseSSL:      cred.GetUseSsl(),
			PartSizeMB:  cfg.Upload.PartSizeMB,
			Concurrency: cfg.Upload.Concurrency,
		}
		uploaderCfgMu.Unlock()

		logger.Info("agent: STS credentials updated",
			zap.Time("expiry", newSTS.Expiry),
			zap.String("endpoint", cred.GetEndpoint()))
	}

	// ── Upload function: creates a fresh Uploader per call using current STS ─
	uploadFn := func(uploadCtx context.Context, task *queue.UploadTask) error {
		uploaderCfgMu.RLock()
		ucfg := currentUploaderCfg
		uploaderCfgMu.RUnlock()
		if ucfg == nil {
			return fmt.Errorf("agent: no upload credentials available yet")
		}
		u, err := uploader.New(*ucfg, q, logger)
		if err != nil {
			return fmt.Errorf("agent: create uploader: %w", err)
		}
		_, err = u.UploadFile(uploadCtx, task)
		return err
	}

	// ── 7. Worker pool (starts goroutines after exec.Start is called) ────────
	exec := executor.New(cfg.Upload.Concurrency, q, uploadFn, logger)

	// ── Scheduler (cron-mode rules) ──────────────────────────────────────────
	sched := scheduler.New(logger)

	// ── Rule lifecycle management ─────────────────────────────────────────────
	var (
		ruleHandlesMu sync.Mutex
		ruleHandles   = make(map[string]*ruleHandle)
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
			go runWatcher(ruleCtx, rule, exec, q, logger)
		case "cron":
			if err := sched.AddRule(rule, func(resolvedPath string) {
				walkAndSubmit(ruleCtx, exec, q, rule, resolvedPath, logger)
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

	// ── 8. Register ServerMessage handler ────────────────────────────────────
	grpcClient.SetMessageHandler(func(msg *agentv1.ServerMessage) {
		switch p := msg.GetPayload().(type) {
		case *agentv1.ServerMessage_Credentials:
			updateCreds(p.Credentials)

		case *agentv1.ServerMessage_PushRule:
			rule := protoToSchedulerRule(p.PushRule.GetRule())
			logger.Info("agent: push rule", zap.String("rule_id", rule.RuleID), zap.String("mode", rule.Mode))
			applyRule(rule)

		case *agentv1.ServerMessage_CancelRule:
			ruleID := p.CancelRule.GetRuleId()
			logger.Info("agent: cancel rule", zap.String("rule_id", ruleID))
			stopRule(ruleID)

		case *agentv1.ServerMessage_Revoke:
			logger.Warn("agent: token revoked by Control Plane",
				zap.String("reason", p.Revoke.GetReason()))
			stop() // trigger graceful shutdown

		case *agentv1.ServerMessage_ListDirectory:
			go handleListDir(p.ListDirectory, grpcClient, logger)

		case *agentv1.ServerMessage_Ping:
			if err := grpcClient.SendMessage(&agentv1.AgentMessage{
				MessageId: uuid.New().String(),
				Payload:   &agentv1.AgentMessage_Heartbeat{Heartbeat: &agentv1.Heartbeat{}},
			}); err != nil {
				logger.Warn("agent: ping response failed", zap.Error(err))
			}

		case *agentv1.ServerMessage_Ack:
			// No-op: acknowledgements are informational.
		}
	})

	if err := grpcClient.Connect(ctx); err != nil {
		logger.Fatal("grpc connect failed", zap.Error(err))
	}
	defer grpcClient.Close()

	// ── 6. Registration / approval lifecycle (blocks until APPROVED) ─────────
	lc := grpcclient.NewLifecycle(tokenMgr, stsMgr)
	if err := lc.Start(ctx, grpcClient.ServiceClient(), cfg, logger); err != nil {
		logger.Fatal("lifecycle start failed", zap.Error(err))
	}
	grpcClient.SetToken(lc.TokenManager.Token())
	grpcClient.SetAgentID(lc.AgentID)
	logger.Info("agent: approved, starting normal operation", zap.String("agent_id", lc.AgentID))

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
				sts := stsMgr.GetSTS()
				if sts == nil || time.Until(sts.Expiry) > 10*time.Minute {
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
		RuleID:             r.GetRuleId(),
		Name:               r.GetName(),
		Mode:               r.GetMode(),
		SourcePathTemplate: r.GetSourcePathTemplate(),
		FileGlob:           r.GetFileGlob(),
		UploadBucket:       r.GetUploadBucket(),
		UploadPathTemplate: r.GetUploadPathTemplate(),
		WatchRecursive:     r.GetWatchRecursive(),
		WatchSubdirPattern: r.GetWatchSubdirPattern(),
		CronExpr:           r.GetCronExpr(),
		RunOnceOnStart:     r.GetRunOnceOnStart(),
		AppendMode:         r.GetAppendMode(),
		Enabled:            r.GetEnabled(),
	}
}

// runWatcher starts a file-system watcher for the given watch-mode rule and
// submits upload tasks to the executor for every create/write event.
func runWatcher(ctx context.Context, rule scheduler.CollectionRule, exec *executor.Executor, q *queue.Queue, logger *zap.Logger) {
	w, err := watcher.New(rule.SourcePathTemplate, rule.FileGlob, rule.WatchRecursive, 0, rule.AppendMode, logger)
	if err != nil {
		logger.Warn("agent: watcher init failed",
			zap.String("rule_id", rule.RuleID), zap.Error(err))
		return
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
			submitFile(exec, q, rule, ev.Path, ev.Size, ev.ModTime, ev.FileOffset, rule.AppendMode, logger)
		}
	}
}

// walkAndSubmit walks basePath and submits an upload task for every file
// matching rule.FileGlob that has not already been processed.
func walkAndSubmit(ctx context.Context, exec *executor.Executor, q *queue.Queue, rule scheduler.CollectionRule, basePath string, logger *zap.Logger) {
	err := filepath.WalkDir(basePath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		matched, _ := filepath.Match(rule.FileGlob, d.Name())
		if !matched {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		submitFile(exec, q, rule, path, info.Size(), info.ModTime(), 0, "", logger)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		logger.Warn("agent: walkdir failed",
			zap.String("rule_id", rule.RuleID), zap.String("path", basePath), zap.Error(err))
	}
}

// submitFile checks deduplication and enqueues an upload task.
func submitFile(exec *executor.Executor, q *queue.Queue, rule scheduler.CollectionRule, localPath string, size int64, mtime time.Time, fileOffset int64, appendMode string, logger *zap.Logger) {
	done, err := q.IsProcessed(rule.RuleID, localPath)
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
		StoragePath: buildStoragePath(rule, localPath),
		Bucket:      rule.UploadBucket,
		FileSize:    size,
		FileMtime:   mtime.Unix(),
		Status:      queue.StatusPending,
	}
	if err := exec.Submit(task); err != nil {
		logger.Warn("agent: submit task failed",
			zap.String("rule_id", rule.RuleID), zap.String("path", localPath), zap.Error(err))
	}
}

// buildStoragePath resolves the upload path template and appends the file name.
func buildStoragePath(rule scheduler.CollectionRule, localPath string) string {
	prefix := scheduler.ResolvePath(rule.UploadPathTemplate, time.Now().UTC())
	base := filepath.Base(localPath)
	if prefix == "" {
		return base
	}
	return strings.TrimRight(prefix, "/") + "/" + base
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
