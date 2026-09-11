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
	"google.golang.org/protobuf/proto"
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

	// agentLocks serialises a single agent's rule traffic: building + sending
	// the full snapshot (which spans several DB round-trips) must not
	// interleave with incremental DispatchRule / DispatchRuleCancel sends, or
	// a rule created while the snapshot is in flight gets pushed incrementally
	// and then stopped by the older snapshot's absence (review F1). Per
	// agent — never global: agents are independent and a global lock would
	// serialise all dispatch traffic behind one slow sync.
	//
	// Entries are reclaimed when the agent's connection is torn down
	// (ReleaseAgent, called from Connect's defer) — review R2: keyed by
	// agentID and never removed, the map grows with every agent ever seen,
	// a slow leak on a long-lived CP.
	agentLocks agentLocks
}

// agentLockEntry is one agent's dispatch serialisation lock plus the
// bookkeeping that lets it be reclaimed safely: a lock may be deleted only
// once the agent is gone AND no goroutine is using (or about to use) it —
// deleting under a holder would leave two live entries for one agent and
// reopen the F1 race across a reconnect.
type agentLockEntry struct {
	mu    sync.Mutex
	users int  // goroutines currently using (or about to use) mu
	dead  bool // the agent disconnected; eligible for removal once idle
}

// agentLocks owns the per-agent serialisation entries.
type agentLocks struct {
	mu      sync.Mutex
	entries map[string]*agentLockEntry
}

// acquire returns the entry for agentID, creating it on first use, and marks
// it in use so a concurrent retire cannot reclaim it while the caller is
// still heading for e.mu. A retired entry is replaced, never reused: its
// in-flight holders still release against their own pointer (a no-op once
// replaced), and reusing it would weld a reconnecting agent's traffic to a
// cohort that is being phased out.
func (l *agentLocks) acquire(agentID string) *agentLockEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make(map[string]*agentLockEntry)
	}
	e := l.entries[agentID]
	if e == nil || e.dead {
		e = &agentLockEntry{}
		l.entries[agentID] = e
	}
	e.users++
	return e
}

// release marks the entry no longer in use and reclaims it if the agent is
// gone. The pointer comparison matters: a fresh entry may already have been
// created for a reconnecting agent, and the stale holder must not delete it.
func (l *agentLocks) release(agentID string, e *agentLockEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.users--
	if e.dead && e.users == 0 && l.entries[agentID] == e {
		delete(l.entries, agentID)
	}
}

// retire marks the agent gone; the entry is removed immediately when idle, or
// by the last in-flight user otherwise (review R2).
func (l *agentLocks) retire(agentID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.entries[agentID]; e != nil {
		e.dead = true
		if e.users == 0 {
			delete(l.entries, agentID)
		}
	}
}

// ReleaseAgent reclaims the per-agent dispatch serialisation entry. Called
// when the agent's connection is torn down (grpcserver Connect's deferred
// cleanup), so a long-lived CP does not accumulate one mutex per agent ever
// seen (review R2).
func (d *Dispatcher) ReleaseAgent(agentID string) {
	d.agentLocks.retire(agentID)
}

// agentSerialisation returns the entry to hold around rule-bearing sends for
// agentID. The caller must pair e.mu.Lock/Unlock with a deferred
// agentLocks.release.
func (d *Dispatcher) agentSerialisation(agentID string) *agentLockEntry {
	return d.agentLocks.acquire(agentID)
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
	e := d.agentSerialisation(agentID)
	e.mu.Lock()
	if !d.registry.Send(agentID, msg) {
		e.mu.Unlock()
		d.agentLocks.release(agentID, e)
		d.logger.Warn("dispatch_rule: failed to send to agent", zap.String("agent_id", agentID))
		return fmt.Errorf("dispatch_rule: failed to send rule %s to agent %s: channel full or disconnected",
			rule.ID, agentID)
	}
	e.mu.Unlock()
	d.agentLocks.release(agentID, e)

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
	e := d.agentSerialisation(agentID)
	e.mu.Lock()
	if !d.registry.Send(agentID, msg) {
		d.logger.Warn("dispatch_rule_cancel: failed to send", zap.String("agent_id", agentID))
	}
	e.mu.Unlock()
	d.agentLocks.release(agentID, e)
	return nil
}

// maxRulesSnapshotBytes is the snapshot size at which SyncRulesOnConnect
// degrades instead of sending. The gRPC default receive limit is 4 MiB; a
// snapshot beyond it fails the stream deterministically, and failing the
// stream forces a reconnect that deterministically re-sends the same oversized
// snapshot — an infinite reconnect loop (review F3). The margin absorbs
// protobuf framing and per-message overhead.
const maxRulesSnapshotBytes = 3 << 20

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
//
// Oversized snapshots are the one failure that is NOT rare: they fail on
// every reconnect by construction. They never reach the transport — the sync
// degrades instead: skip the send, alarm at ERROR level, keep the connection.
// The agent keeps its previous rule view (stale but functional) instead of
// looping forever; creation-time rule-count capping makes this state
// practically unreachable (see MaxRulesPerAgent in the REST handler).
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
	e := d.agentSerialisation(agentID)
	e.mu.Lock()
	defer func() {
		e.mu.Unlock()
		d.agentLocks.release(agentID, e)
	}()
	rules, err := d.db.ListCollectionRulesByAgent(ctx, parsed)
	if err != nil {
		return fmt.Errorf("sync_rules: list rules: %w", err)
	}

	snapshot := make([]*agentv1.CollectionRule, 0, len(rules))
	// Bucket names are memoised per distinct bucket: rules overwhelmingly
	// share a handful of buckets, so the N+1 lookups inside the held
	// serialisation window collapse to distinctBuckets+1 (review R2-2). A
	// true batched query is a candidate follow-up card; this keeps the lock
	// hold short without schema or interface changes.
	names := make(map[uuid.UUID]string, len(rules))
	for _, rule := range rules {
		bucketName, ok := names[rule.BucketID]
		if !ok {
			var err error
			bucketName, err = d.lookupBucketName(ctx, rule.BucketID)
			if err != nil {
				return fmt.Errorf("sync_rules: lookup bucket for rule %s: %w", rule.ID, err)
			}
			names[rule.BucketID] = bucketName
		}
		snapshot = append(snapshot, ruleToProto(rule, bucketName))
	}

	syncMsg := &agentv1.ServerMessage{
		Payload: &agentv1.ServerMessage_RulesSync{
			RulesSync: &agentv1.RulesSyncCommand{Rules: snapshot},
		},
	}
	// Pre-flight size check (review F3): a snapshot beyond the gRPC receive
	// limit fails the stream deterministically, and disconnecting on it would
	// loop forever on the same input. Degrade instead: skip the send, alarm,
	// keep the connection.
	if size := proto.Size(syncMsg); size > maxRulesSnapshotBytes {
		d.logger.Error("sync_rules: rule snapshot oversized, degrading to keep-alive — "+
			"agent keeps its previous rule view; reduce this agent's rule count or template sizes "+
			"(creation is capped at maxRulesPerAgent, this agent has legacy or oversized rules)",
			zap.String("agent_id", agentID),
			zap.Int("rules", len(snapshot)),
			zap.Int("bytes", size),
			zap.Int("limit", maxRulesSnapshotBytes))
		return nil
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
