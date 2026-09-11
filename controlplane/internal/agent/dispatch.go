package agent

import (
	"context"
	"fmt"
	"sync"
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

	// agentMu serialises a single agent's rule traffic: building + sending the
	// full snapshot (which spans several DB round-trips) must not interleave
	// with incremental DispatchRule / DispatchRuleCancel sends, or a rule
	// created while the snapshot is in flight gets pushed incrementally and
	// then stopped by the older snapshot's absence (review F1). Per agent —
	// never global: agents are independent and a global lock would serialise
	// all dispatch traffic behind one slow sync.
	agentMu sync.Map // agentID string -> *sync.Mutex
}

// agentLock returns the per-agent dispatch mutex, creating it on first use.
func (d *Dispatcher) agentLock(agentID string) *sync.Mutex {
	m, _ := d.agentMu.LoadOrStore(agentID, &sync.Mutex{})
	return m.(*sync.Mutex)
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

	// Serialize against snapshot sync (review F1): the incremental push must
	// land strictly before or after the snapshot's build+send window, never
	// inside it — a push landing inside an older snapshot's window would be
	// stopped by that snapshot's absence semantics.
	mu := d.agentLock(agentID)
	mu.Lock()
	if !d.registry.Send(agentID, msg) {
		mu.Unlock()
		d.logger.Warn("dispatch_rule: failed to send to agent", zap.String("agent_id", agentID))
		return fmt.Errorf("dispatch_rule: failed to send rule %s to agent %s: channel full or disconnected",
			rule.ID, agentID)
	}
	mu.Unlock()

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
	// Serialize against snapshot sync (review F1): a cancel landing inside the
	// snapshot's window could be undone by the snapshot re-pushing the rule.
	mu := d.agentLock(agentID)
	mu.Lock()
	defer mu.Unlock()
	if !d.registry.Send(agentID, msg) {
		d.logger.Warn("dispatch_rule_cancel: failed to send", zap.String("agent_id", agentID))
	}
	return nil
}

// SyncRulesOnConnect pushes the agent's complete rule snapshot on (re)connect
// as ONE message (IC-BUG-30 / D-033).
//
// The snapshot is every rule in the database for this agent — active AND
// inactive (the agent's applyRule stops a rule whose Enabled is false, which
// is the disconnect-window enable-toggle half) — and deleted rules are absent
// by design; their absence is the only way the agent can learn they were
// deleted, so the snapshot must be hole-free: a bucket lookup failure fails
// the whole sync rather than quietly omitting a rule the agent would then
// stop even though it still exists.
//
// Snapshot, not per-rule pushes: with ≥33 rules a per-message push burst
// overflows the connection's bounded send buffer faster than the send
// goroutine drains it — measured 5/5 runs losing 8 of 40 rules plus the
// credentials push behind them (deterministic, not flaky: production rate is
// a microsecond-scale loop, consumption is per-message stream.Send with gRPC
// framing). One snapshot message cannot overflow the buffer, and its failure
// mode is loud (whole message) rather than silent partial loss. PushRuleCommand
// remains the incremental path for live rule create/update.
//
// Send failure must fail the connection (Connect ends the stream and the
// agent reconnects into a clean full re-sync): the snapshot is the agent's
// entire rule view — running with an untrustworthy view is worse than
// disconnecting. The safety of that choice rests on the failure being rare
// (one message into an empty buffer), so the log distinguishes a full buffer
// from a disconnected agent: a repeating full-buffer failure would mean the
// message is oversized and would turn reconnect-and-resync into a reconnect
// loop — that must be visible at a glance.
func (d *Dispatcher) SyncRulesOnConnect(ctx context.Context, agentID string) error {
	parsed, err := uuid.Parse(agentID)
	if err != nil {
		return fmt.Errorf("sync_rules: invalid agent_id: %w", err)
	}

	// Serialize the whole build+send window against incremental dispatch
	// (review F1): the snapshot reflects the DB as of the list below, so a
	// rule created while it is in flight must be pushed AFTER the snapshot
	// arrives, or the snapshot's absence semantics would permanently stop a
	// rule that still exists. The lock spans the DB round-trips — bounded by
	// the rule list size, and only for this one agent.
	mu := d.agentLock(agentID)
	mu.Lock()
	defer mu.Unlock()
	rules, err := d.db.ListCollectionRulesByAgent(ctx, parsed)
	if err != nil {
		return fmt.Errorf("sync_rules: list rules: %w", err)
	}

	snapshot := make([]*agentv1.CollectionRule, 0, len(rules))
	for _, rule := range rules {
		bucketName, err := d.lookupBucketName(ctx, rule.BucketID)
		if err != nil {
			return fmt.Errorf("sync_rules: lookup bucket for rule %s: %w", rule.ID, err)
		}
		snapshot = append(snapshot, ruleToProto(rule, bucketName))
	}

	syncMsg := &agentv1.ServerMessage{
		Payload: &agentv1.ServerMessage_RulesSync{
			RulesSync: &agentv1.RulesSyncCommand{Rules: snapshot},
		},
	}
	if !d.registry.Send(agentID, syncMsg) {
		// See the godoc above: the snapshot is the delete half of IC-BUG-30 in
		// its entirety, so this failure must end the connection — but the two
		// causes call for very different responses (rare transient vs a
		// would-be reconnect loop), so the log names them apart.
		if d.registry.IsOnline(agentID) {
			d.logger.Error("sync_rules: rule snapshot not delivered: send channel full (cap 32) — "+
				"if this repeats, the snapshot is oversized and disconnect-and-resync becomes a reconnect loop",
				zap.String("agent_id", agentID),
				zap.Int("rules", len(snapshot)))
			return fmt.Errorf("sync_rules: send channel full for agent %s (%d rules)", agentID, len(snapshot))
		}
		d.logger.Error("sync_rules: rule snapshot not delivered: agent disconnected",
			zap.String("agent_id", agentID),
			zap.Int("rules", len(snapshot)))
		return fmt.Errorf("sync_rules: agent %s disconnected before rule snapshot delivery", agentID)
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
