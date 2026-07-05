package event

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/indexer"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// WebhookActionConfig is the expected JSON shape of EventRule.ActionConfig for webhook actions.
type WebhookActionConfig struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// NATSActionConfig is the expected JSON shape of EventRule.ActionConfig for
// nats_publish actions. Subject is the NATS subject the event payload is
// re-published to for downstream internal consumers.
type NATSActionConfig struct {
	Subject string `json:"subject"`
}

// NATSListener is the minimal NATS interface needed to subscribe to event subjects.
type NATSListener interface {
	// Subscribe registers a callback for messages on subject. The returned
	// cancel func unsubscribes when called.
	Subscribe(subject string, cb func(data []byte)) (cancel func(), err error)
}

// EngineStore is the minimal database interface used by the Engine. Using an
// interface (rather than db.DBTX directly) makes the Engine unit-testable
// without a real database.
type EngineStore interface {
	ListEnabledEventRules(ctx context.Context, orgID uuid.UUID, eventType db.EventType) ([]*db.EventRule, error)
	CreateEventDelivery(ctx context.Context, params indexer.CreateEventDeliveryParams) (*db.EventDelivery, error)
	ListPendingDeliveries(ctx context.Context) ([]*db.EventDelivery, error)
	UpdateDelivery(ctx context.Context, params indexer.UpdateEventDeliveryParams) error
	GetEventRuleByID(ctx context.Context, id uuid.UUID) (*db.EventRule, error)
}

// dbtxEngineStore adapts db.DBTX to the EngineStore interface.
type dbtxEngineStore struct {
	dbtx db.DBTX
}

// ListEnabledEventRules delegates to the indexer package function.
func (d *dbtxEngineStore) ListEnabledEventRules(ctx context.Context, orgID uuid.UUID, eventType db.EventType) ([]*db.EventRule, error) {
	return indexer.ListEnabledEventRules(ctx, d.dbtx, orgID, eventType)
}

// CreateEventDelivery delegates to the indexer package function.
func (d *dbtxEngineStore) CreateEventDelivery(ctx context.Context, params indexer.CreateEventDeliveryParams) (*db.EventDelivery, error) {
	return indexer.CreateEventDelivery(ctx, d.dbtx, params)
}

// ListPendingDeliveries returns deliveries due for retry.
func (d *dbtxEngineStore) ListPendingDeliveries(ctx context.Context) ([]*db.EventDelivery, error) {
	return indexer.ListPendingEventDeliveries(ctx, d.dbtx)
}

// UpdateDelivery updates a delivery's status and retry metadata.
func (d *dbtxEngineStore) UpdateDelivery(ctx context.Context, params indexer.UpdateEventDeliveryParams) error {
	return indexer.UpdateEventDelivery(ctx, d.dbtx, params)
}

// GetEventRuleByID looks up an event rule by its ID.
func (d *dbtxEngineStore) GetEventRuleByID(ctx context.Context, id uuid.UUID) (*db.EventRule, error) {
	q := db.New(d.dbtx)
	return q.GetEventRuleByID(ctx, id)
}

// Engine routes inbound NATS events to the appropriate rules and creates
// event_delivery records for webhook and other actions.
type Engine struct {
	store     EngineStore
	sender    *WebhookSender
	publisher NATSConn // re-publishes payloads for nats_publish actions; nil until WithPublisher
	logger    *zap.Logger
}

// WithPublisher wires the NATS publisher used by nats_publish action rules and
// returns the Engine for chaining. Without it, nats_publish deliveries fail
// (and are retried) rather than silently succeeding.
func (e *Engine) WithPublisher(p NATSConn) *Engine {
	e.publisher = p
	return e
}

// NewEngine creates an Engine backed by the given db.DBTX. This is the
// primary constructor used in production; tests should use NewEngineWithStore.
func NewEngine(dbtx db.DBTX, sender *WebhookSender, logger *zap.Logger) *Engine {
	return &Engine{
		store:  &dbtxEngineStore{dbtx: dbtx},
		sender: sender,
		logger: logger,
	}
}

// NewEngineWithStore creates an Engine using an explicit EngineStore. This
// constructor is intended for unit tests where the DB layer is mocked.
func NewEngineWithStore(store EngineStore, sender *WebhookSender, logger *zap.Logger) *Engine {
	return &Engine{store: store, sender: sender, logger: logger}
}

// Start subscribes to relevant NATS subjects and launches the retry worker.
// It returns immediately; all processing happens in background goroutines.
// Call-site is responsible for cancelling ctx to stop all goroutines.
func (e *Engine) Start(ctx context.Context, nats NATSListener) {
	type sub struct {
		subject   string
		eventType db.EventType
	}
	subscriptions := []sub{
		{"events.file.uploaded", db.EventTypeFileUploaded},
		{"events.file.deleted", db.EventTypeFileDeleted},
		{"events.agent.online", db.EventTypeAgentOnline},
		{"events.agent.offline", db.EventTypeAgentOffline},
		{"events.agent.approved", db.EventTypeAgentApproved},
		{"events.agent.revoked", db.EventTypeAgentRevoked},
	}

	for _, s := range subscriptions {
		s := s // capture loop variable
		cancel, err := nats.Subscribe(s.subject, func(data []byte) {
			e.handleNATSMessage(ctx, s.subject, s.eventType, data)
		})
		if err != nil {
			e.logger.Error("engine: NATS subscribe failed",
				zap.String("subject", s.subject),
				zap.Error(err),
			)
			continue
		}
		// Unsubscribe when the context is cancelled.
		go func() {
			<-ctx.Done()
			cancel()
		}()
	}

	go e.retryWorker(ctx)
	e.logger.Info("event engine started")
}

// handleNATSMessage processes an inbound NATS message for the given event type.
func (e *Engine) handleNATSMessage(ctx context.Context, subject string, eventType db.EventType, data []byte) {
	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		e.logger.Error("engine: unmarshal NATS message",
			zap.String("subject", subject),
			zap.Error(err),
		)
		return
	}

	// Phase 1 is single-org; use the default org ID.
	orgID := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	if err := e.HandleEvent(ctx, orgID, eventType, payload); err != nil {
		e.logger.Error("engine: handle event",
			zap.String("subject", subject),
			zap.Error(err),
		)
	}
}

// retryWorker periodically retries pending/failed event deliveries.
func (e *Engine) retryWorker(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.processRetries(ctx)
		}
	}
}

// retryBackoffSchedule defines the wait before each successive retry, as a
// 0-based slice: schedule[0] is the delay before the 1st retry, schedule[4]
// before the 5th (final) retry. It is indexed by the number of retries already
// completed — 0 on the initial failure (schedule[0]=30s), then newAttemptCount
// in the retry worker — giving 30s → 2m → 10m → 30m → 2h (design spec §5.9).
// It is the single source of truth for both the initial webhook scheduling
// (WebhookSender.scheduleRetry) and the retry worker, so the two cannot drift.
var retryBackoffSchedule = []time.Duration{
	30 * time.Second, // before retry 1
	2 * time.Minute,  // before retry 2
	10 * time.Minute, // before retry 3
	30 * time.Minute, // before retry 4
	2 * time.Hour,    // before retry 5 (final)
}

// maxRetryAttempts is the maximum number of retry attempts before giving up.
const maxRetryAttempts = 5

// deliveryStatusDead is the terminal status for a delivery that must never be
// retried again (retries exhausted, or an unsupported action type). The retry
// scan only selects 'pending'/'failed', so a 'dead' delivery drops out — without
// it, a terminal row with a NULL next_retry_at is treated as "due now" and
// re-processed (re-sent) every tick forever.
const deliveryStatusDead = "dead"

// ProcessRetries is the exported entry point for the retry worker logic.
// It is called periodically by the retryWorker ticker, and may also be
// invoked directly in tests.
func (e *Engine) ProcessRetries(ctx context.Context) {
	e.processRetries(ctx)
}

func (e *Engine) processRetries(ctx context.Context) {
	deliveries, err := e.store.ListPendingDeliveries(ctx)
	if err != nil {
		e.logger.Error("engine: list pending deliveries", zap.Error(err))
		return
	}
	for _, d := range deliveries {
		if err := e.retryDelivery(ctx, d); err != nil {
			e.logger.Warn("engine: retry delivery failed",
				zap.String("delivery_id", d.ID.String()),
				zap.Error(err),
			)
		}
	}
}

func (e *Engine) retryDelivery(ctx context.Context, d *db.EventDelivery) error {
	rule, err := e.store.GetEventRuleByID(ctx, d.EventRuleID)
	if err != nil {
		return fmt.Errorf("retry: get rule %s: %w", d.EventRuleID, err)
	}

	newAttemptCount := d.AttemptCount + 1

	// Config/validation failures below are deterministic — they can never succeed
	// on retry — so they are marked terminal ("dead") with the reason persisted to
	// response_body, instead of being retried to max and looping in the scan.
	var sendErr error
	switch rule.ActionType {
	case db.ActionTypeWebhook:
		var cfg WebhookActionConfig
		if err := json.Unmarshal(rule.ActionConfig, &cfg); err != nil {
			return e.markDeliveryDeadf(ctx, d, "webhook config parse error: %v", err)
		}
		if cfg.URL == "" {
			return e.markDeliveryDeadf(ctx, d, "webhook url empty for rule %s", rule.ID)
		}
		sendErr = postWebhook(ctx, cfg.URL, d.Payload)
	case db.ActionTypeNatsPublish:
		var cfg NATSActionConfig
		if err := json.Unmarshal(rule.ActionConfig, &cfg); err != nil {
			return e.markDeliveryDeadf(ctx, d, "nats config parse error: %v", err)
		}
		if cfg.Subject == "" {
			return e.markDeliveryDeadf(ctx, d, "nats subject empty for rule %s", rule.ID)
		}
		sendErr = e.publishNATS(cfg.Subject, d.Payload)
	default:
		return e.markDeliveryDeadf(ctx, d, "unsupported action type %q", string(rule.ActionType))
	}

	var newStatus string
	var deliveredAt sql.NullTime
	var nextRetryAt sql.NullTime
	// Default to carrying diagnostics forward (retryDelivery captures no fresh
	// response code); refreshed with the latest error on failure, cleared on
	// success so a delivered row does not keep a stale error.
	respCode := d.ResponseCode
	respBody := d.ResponseBody

	if sendErr != nil {
		respBody = sql.NullString{String: sendErr.Error(), Valid: true}
		if int(newAttemptCount) >= maxRetryAttempts {
			// Exceeded maximum retries — mark terminal so it drops out of the
			// retry scan (a 'failed' row with NULL next_retry_at would loop).
			newStatus = deliveryStatusDead
		} else {
			newStatus = "failed"
			// The initial failure already used schedule[0] (attempt_count stays 0),
			// so the delay before the *next* retry is indexed by newAttemptCount,
			// giving 30s → 2m → 10m → 30m → 2h across the retry sequence.
			idx := int(newAttemptCount)
			if idx >= len(retryBackoffSchedule) {
				idx = len(retryBackoffSchedule) - 1
			}
			nextRetryAt = sql.NullTime{Time: time.Now().UTC().Add(retryBackoffSchedule[idx]), Valid: true}
		}
	} else {
		newStatus = "delivered"
		deliveredAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
		// Clear stale error diagnostics from prior failed attempts.
		respCode = sql.NullInt32{}
		respBody = sql.NullString{}
	}

	e.logger.Debug("engine: retry delivery complete",
		zap.String("delivery_id", d.ID.String()),
		zap.String("status", newStatus),
		zap.Int32("attempt", newAttemptCount),
	)

	return e.store.UpdateDelivery(ctx, indexer.UpdateEventDeliveryParams{
		ID:           d.ID,
		Status:       newStatus,
		ResponseCode: respCode,
		ResponseBody: respBody,
		AttemptCount: newAttemptCount,
		NextRetryAt:  nextRetryAt,
		DeliveredAt:  deliveredAt,
	})
}

// markDeliveryDeadf transitions a delivery to the terminal 'dead' status for
// failures that can never succeed on retry (unsupported action type,
// unrecoverable config/validation errors), persisting the formatted reason to
// response_body so operators can see why without digging through logs. It keeps
// response_code and attempt_count, logs the reason, and leaves next_retry_at
// NULL so the row drops out of the retry scan.
func (e *Engine) markDeliveryDeadf(ctx context.Context, d *db.EventDelivery, format string, args ...interface{}) error {
	reason := fmt.Sprintf(format, args...)
	e.logger.Warn("engine: marking delivery dead",
		zap.String("delivery_id", d.ID.String()), zap.String("reason", reason))
	return e.store.UpdateDelivery(ctx, indexer.UpdateEventDeliveryParams{
		ID:           d.ID,
		Status:       deliveryStatusDead,
		ResponseCode: d.ResponseCode,
		ResponseBody: sql.NullString{String: reason, Valid: true},
		AttemptCount: d.AttemptCount,
	})
}

// HandleEvent processes an event of the given type for an organisation, looks up
// matching enabled rules and dispatches deliveries.
func (e *Engine) HandleEvent(ctx context.Context, orgID uuid.UUID, eventType db.EventType, payload map[string]interface{}) error {
	rules, err := e.store.ListEnabledEventRules(ctx, orgID, eventType)
	if err != nil {
		return fmt.Errorf("engine: list event rules: %w", err)
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("engine: marshal payload: %w", err)
	}

	for _, rule := range rules {
		if err := e.dispatch(ctx, rule, eventType, payloadBytes); err != nil {
			e.logger.Error("engine: dispatch rule failed",
				zap.String("rule_id", rule.ID.String()),
				zap.Error(err),
			)
		}
	}
	return nil
}

func (e *Engine) dispatch(ctx context.Context, rule *db.EventRule, eventType db.EventType, payload []byte) error {
	switch rule.ActionType {
	case db.ActionTypeWebhook:
		return e.dispatchWebhook(ctx, rule, eventType, payload)
	case db.ActionTypeNatsPublish:
		return e.dispatchNATS(ctx, rule, eventType, payload)
	default:
		e.logger.Warn("engine: unsupported action type", zap.String("action_type", string(rule.ActionType)))
		return nil
	}
}

// dispatchNATS re-publishes the event payload to the rule's configured NATS
// subject and records the outcome as an event_delivery. On publish failure the
// delivery is marked failed with a retry schedule so the retry worker re-attempts
// it, mirroring the webhook lifecycle.
func (e *Engine) dispatchNATS(ctx context.Context, rule *db.EventRule, eventType db.EventType, payload []byte) error {
	var cfg NATSActionConfig
	if err := json.Unmarshal(rule.ActionConfig, &cfg); err != nil {
		return fmt.Errorf("engine: parse nats config: %w", err)
	}
	if cfg.Subject == "" {
		return fmt.Errorf("engine: nats subject empty for rule %s", rule.ID)
	}

	delivery, err := e.store.CreateEventDelivery(ctx, indexer.CreateEventDeliveryParams{
		EventRuleID: rule.ID,
		EventType:   eventType,
		Payload:     payload,
		Status:      "pending",
	})
	if err != nil {
		return fmt.Errorf("engine: create delivery record: %w", err)
	}

	pubErr := e.publishNATS(cfg.Subject, payload)

	status := "delivered"
	var deliveredAt, nextRetryAt sql.NullTime
	var respBody sql.NullString
	if pubErr != nil {
		e.logger.Warn("engine: nats publish error",
			zap.String("delivery_id", delivery.ID.String()),
			zap.String("subject", cfg.Subject),
			zap.Error(pubErr),
		)
		status = "failed"
		nextRetryAt = sql.NullTime{Time: time.Now().UTC().Add(retryBackoffSchedule[0]), Valid: true}
		// Persist why it failed (incl. the "no publisher configured" case) so
		// operators can see it on the delivery record.
		respBody = sql.NullString{String: pubErr.Error(), Valid: true}
	} else {
		deliveredAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
	}

	// AttemptCount stays 0 for the initial attempt — the retry worker increments
	// it on each retry, matching the webhook path (which never touches
	// attempt_count on the first send).
	return e.store.UpdateDelivery(ctx, indexer.UpdateEventDeliveryParams{
		ID:           delivery.ID,
		Status:       status,
		ResponseBody: respBody,
		AttemptCount: 0,
		NextRetryAt:  nextRetryAt,
		DeliveredAt:  deliveredAt,
	})
}

// publishNATS publishes payload to subject via the wired publisher, returning an
// error if no publisher is configured (so the delivery is recorded as failed
// rather than silently dropped).
func (e *Engine) publishNATS(subject string, payload []byte) error {
	if e.publisher == nil {
		return fmt.Errorf("engine: no NATS publisher configured for nats_publish action")
	}
	return e.publisher.Publish(subject, payload)
}

func (e *Engine) dispatchWebhook(ctx context.Context, rule *db.EventRule, eventType db.EventType, payload []byte) error {
	var cfg WebhookActionConfig
	if err := json.Unmarshal(rule.ActionConfig, &cfg); err != nil {
		return fmt.Errorf("engine: parse webhook config: %w", err)
	}
	if cfg.URL == "" {
		return fmt.Errorf("engine: webhook url empty for rule %s", rule.ID)
	}

	delivery, err := e.store.CreateEventDelivery(ctx, indexer.CreateEventDeliveryParams{
		EventRuleID: rule.ID,
		EventType:   eventType,
		Payload:     payload,
		Status:      "pending",
	})
	if err != nil {
		return fmt.Errorf("engine: create delivery record: %w", err)
	}

	rec := DeliveryRecord{
		ID:        delivery.ID.String(),
		URL:       cfg.URL,
		Payload:   payload,
		AttemptNo: 0,
	}
	if err := e.sender.Send(ctx, rec); err != nil {
		e.logger.Warn("engine: webhook send error",
			zap.String("delivery_id", rec.ID),
			zap.Error(err),
		)
	}
	return nil
}

// postWebhook makes a simple HTTP POST to url with the given payload and
// returns an error if the request fails or the server returns a non-2xx status.
// Unlike WebhookSender.Send, this function does not touch the database; the
// caller is responsible for updating the delivery status.
func postWebhook(ctx context.Context, url string, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("retry: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("retry: non-2xx status %d", resp.StatusCode)
	}
	return nil
}

// ── EventDeliveryDB adapter ──────────────────────────────────────────────────

// dbAdapter wraps db.DBTX to satisfy EventDeliveryDB for WebhookSender.
type dbAdapter struct {
	dbtx db.DBTX
}

// NewDBAdapter creates an EventDeliveryDB from a db.DBTX.
func NewDBAdapter(dbtx db.DBTX) EventDeliveryDB {
	return &dbAdapter{dbtx: dbtx}
}

// UpdateEventDeliveryStatus implements EventDeliveryDB.
func (a *dbAdapter) UpdateEventDeliveryStatus(ctx context.Context, id string, status string, responseCode sql.NullInt32, nextRetryAt sql.NullTime) error {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("event delivery db: invalid id %q: %w", id, err)
	}
	return indexer.UpdateEventDelivery(ctx, a.dbtx, indexer.UpdateEventDeliveryParams{
		ID:           parsed,
		Status:       status,
		ResponseCode: responseCode,
		ResponseBody: sql.NullString{},
		AttemptCount: 0,
		NextRetryAt:  nextRetryAt,
		DeliveredAt:  sql.NullTime{},
	})
}
