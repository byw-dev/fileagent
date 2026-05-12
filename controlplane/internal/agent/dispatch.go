package agent

import (
	"context"
	"fmt"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// DispatchDB is the database interface required by Dispatcher.
type DispatchDB interface {
	ListCollectionRulesByAgent(ctx context.Context, agentID uuid.UUID) ([]*db.CollectionRule, error)
}

// BucketQuerier looks up buckets by ID.
type BucketQuerier interface {
	GetBucketByID(ctx context.Context, id uuid.UUID) (*db.Bucket, error)
}

// DispatchCacheClient is the cache interface required by Dispatcher.
type DispatchCacheClient interface {
	SetNX(ctx context.Context, key string, value interface{}, ttl time.Duration) (bool, error)
	Del(ctx context.Context, keys ...string) error
}

// RegistryClient provides access to connected agent streams.
type RegistryClient interface {
	IsOnline(agentID string) bool
	Send(agentID string, msg *agentv1.ServerMessage) bool
}

// Dispatcher sends rule commands to online agents.
type Dispatcher struct {
	db       DispatchDB
	buckets  BucketQuerier
	cache    DispatchCacheClient
	registry RegistryClient
	logger   *zap.Logger
}

// NewDispatcher creates a new Dispatcher.
func NewDispatcher(
	dispatchDB DispatchDB,
	bucketQuerier BucketQuerier,
	cacheClient DispatchCacheClient,
	registry RegistryClient,
	logger *zap.Logger,
) *Dispatcher {
	return &Dispatcher{
		db:       dispatchDB,
		buckets:  bucketQuerier,
		cache:    cacheClient,
		registry: registry,
		logger:   logger,
	}
}

// DispatchRule pushes a collection rule to the target agent if it is online.
func (d *Dispatcher) DispatchRule(ctx context.Context, rule *db.CollectionRule) error {
	agentID := rule.AgentID.String()
	if !d.registry.IsOnline(agentID) {
		d.logger.Debug("dispatch_rule: agent offline, skipping", zap.String("agent_id", agentID))
		return nil
	}

	lockKey := cache.LockRuleDispatchKey(rule.ID.String())
	acquired, err := d.cache.SetNX(ctx, lockKey, "1", 30*time.Second)
	if err != nil {
		return fmt.Errorf("dispatch_rule: acquire lock: %w", err)
	}
	if !acquired {
		d.logger.Debug("dispatch_rule: lock already held", zap.String("rule_id", rule.ID.String()))
		return nil
	}
	defer func() { _ = d.cache.Del(ctx, lockKey) }()

	bucketName, err := d.lookupBucketName(ctx, rule.BucketID)
	if err != nil {
		return fmt.Errorf("dispatch_rule: lookup bucket: %w", err)
	}

	protoRule := ruleToProto(rule, bucketName)
	msg := &agentv1.ServerMessage{
		Payload: &agentv1.ServerMessage_PushRule{
			PushRule: &agentv1.PushRuleCommand{Rule: protoRule},
		},
	}

	if !d.registry.Send(agentID, msg) {
		d.logger.Warn("dispatch_rule: failed to send to agent", zap.String("agent_id", agentID))
	}
	return nil
}

// DispatchRuleCancel sends a cancel command for the given rule to the agent.
func (d *Dispatcher) DispatchRuleCancel(ctx context.Context, ruleID, agentID string) error {
	if !d.registry.IsOnline(agentID) {
		return nil
	}

	msg := &agentv1.ServerMessage{
		Payload: &agentv1.ServerMessage_CancelRule{
			CancelRule: &agentv1.CancelRuleCommand{RuleId: ruleID},
		},
	}
	if !d.registry.Send(agentID, msg) {
		d.logger.Warn("dispatch_rule_cancel: failed to send", zap.String("agent_id", agentID))
	}
	return nil
}

// SyncRulesOnConnect pushes all active rules to an agent that just connected.
func (d *Dispatcher) SyncRulesOnConnect(ctx context.Context, agentID string) error {
	parsed, err := uuid.Parse(agentID)
	if err != nil {
		return fmt.Errorf("sync_rules: invalid agent_id: %w", err)
	}

	rules, err := d.db.ListCollectionRulesByAgent(ctx, parsed)
	if err != nil {
		return fmt.Errorf("sync_rules: list rules: %w", err)
	}

	for _, rule := range rules {
		if rule.Status != db.RuleStatusActive {
			continue
		}
		bucketName, err := d.lookupBucketName(ctx, rule.BucketID)
		if err != nil {
			d.logger.Warn("sync_rules: lookup bucket failed",
				zap.String("rule_id", rule.ID.String()),
				zap.Error(err),
			)
			continue
		}
		protoRule := ruleToProto(rule, bucketName)
		msg := &agentv1.ServerMessage{
			Payload: &agentv1.ServerMessage_PushRule{
				PushRule: &agentv1.PushRuleCommand{Rule: protoRule},
			},
		}
		if !d.registry.Send(agentID, msg) {
			d.logger.Warn("sync_rules: failed to send rule",
				zap.String("agent_id", agentID),
				zap.String("rule_id", rule.ID.String()),
			)
		}
	}
	return nil
}

// lookupBucketName retrieves the MinIO bucket name for the given bucket UUID.
func (d *Dispatcher) lookupBucketName(ctx context.Context, bucketID uuid.UUID) (string, error) {
	bucket, err := d.buckets.GetBucketByID(ctx, bucketID)
	if err != nil {
		return "", fmt.Errorf("get bucket %s: %w", bucketID, err)
	}
	return bucket.Name, nil
}

// ruleToProto converts a db.CollectionRule to the proto representation.
// bucketName is the MinIO bucket name string (not UUID) that the agent will use
// when calling S3 PutObject.
func ruleToProto(rule *db.CollectionRule, bucketName string) *agentv1.CollectionRule {
	return &agentv1.CollectionRule{
		RuleId:             rule.ID.String(),
		Name:               rule.Name,
		Mode:               string(rule.Mode),
		SourcePathTemplate: rule.SourcePathTemplate,
		FileGlob:           rule.FileGlob,
		UploadBucket:       bucketName,
		UploadPathTemplate: rule.UploadPathTemplate,
		WatchRecursive:     rule.WatchRecursive,
		WatchSubdirPattern: rule.WatchSubdirPattern.String,
		CronExpr:           rule.CronExpr.String,
		RunOnceOnStart:     rule.RunOnceOnStart,
		AppendMode:         rule.AppendMode,
		Enabled:            rule.Status == db.RuleStatusActive,
	}
}
