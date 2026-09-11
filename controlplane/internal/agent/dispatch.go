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

// CredentialPusher re-issues and delivers an STS session to a connected agent.
//
// The Dispatcher uses it when a dispatched rule changes the agent's bucket set
// (IC-BUG-20): the held session covers only the buckets known when it was
// minted, so a rule pointing at a new bucket would 403 until the agent's own
// refresh tick fired — up to ~50 minutes.
type CredentialPusher interface {
	PushCredentials(ctx context.Context, agentID string)
}

// Dispatcher sends rule commands to online agents.
type Dispatcher struct {
	db       DispatchDB
	buckets  BucketQuerier
	cache    DispatchCacheClient
	registry RegistryClient
	logger   *zap.Logger

	pusher CredentialPusher
}

// SetCredentialPusher wires the callback used to re-push STS credentials when
// a dispatched rule changes the agent's bucket set. Optional: without a pusher
// the Dispatcher still dispatches rules but never re-pushes credentials.
func (d *Dispatcher) SetCredentialPusher(p CredentialPusher) {
	d.pusher = p
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
		return fmt.Errorf("dispatch_rule: failed to send rule %s to agent %s: channel full or disconnected",
			rule.ID, agentID)
	}

	// IC-BUG-20: credentials are minted for the bucket set of the agent's
	// active rules at issue time. If this rule points at a bucket no other
	// active rule already covers, the held session cannot write it — re-push
	// so the first upload succeeds instead of 403ing for up to ~50 minutes.
	// Removing a bucket is harmless (the session stays a superset), so only
	// additions trigger the push, and only for active rules.
	if d.pusher != nil && rule.Status == db.RuleStatusActive {
		covers, err := d.otherRulesCoverBucket(ctx, agentID, rule)
		if err != nil {
			d.logger.Warn("dispatch_rule: cannot compute agent bucket set, credentials not re-pushed",
				zap.String("agent_id", agentID),
				zap.String("rule_id", rule.ID.String()),
				zap.Error(err))
		} else if !covers {
			d.pusher.PushCredentials(ctx, agentID)
		}
	}
	return nil
}

// otherRulesCoverBucket reports whether any of the agent's active rules other
// than the given one already target the same bucket — i.e. whether the held
// credential session is expected to cover the bucket being dispatched.
func (d *Dispatcher) otherRulesCoverBucket(ctx context.Context, agentID string, rule *db.CollectionRule) (bool, error) {
	parsed, err := uuid.Parse(agentID)
	if err != nil {
		return false, fmt.Errorf("parse agent_id: %w", err)
	}
	rules, err := d.db.ListCollectionRulesByAgent(ctx, parsed)
	if err != nil {
		return false, fmt.Errorf("list rules: %w", err)
	}
	for _, r := range rules {
		if r.ID == rule.ID || r.Status != db.RuleStatusActive {
			continue
		}
		if r.BucketID == rule.BucketID {
			return true, nil
		}
	}
	return false, nil
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
		RuleId:           rule.ID.String(),
		Name:             rule.Name,
		Mode:             string(rule.Mode),
		BasePath:         rule.BasePath,
		PathPattern:      rule.PathPattern,
		DestPathTemplate: rule.DestPathTemplate,
		UploadBucket:     bucketName,
		Recursive:        rule.Recursive,
		CronExpr:         rule.CronExpr.String,
		RunOnceOnStart:   rule.RunOnceOnStart,
		AppendMode:       rule.AppendMode,
		Enabled:          rule.Status == db.RuleStatusActive,
	}
}
