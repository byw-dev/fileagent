package event

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

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

// Engine routes inbound NATS events to the appropriate rules and creates
// event_delivery records for webhook and other actions.
type Engine struct {
	dbtx   db.DBTX
	sender *WebhookSender
	logger *zap.Logger
}

// NewEngine creates an Engine.
func NewEngine(dbtx db.DBTX, sender *WebhookSender, logger *zap.Logger) *Engine {
	return &Engine{dbtx: dbtx, sender: sender, logger: logger}
}

// HandleEvent processes an event of the given type for an organisation, looks up
// matching enabled rules and dispatches deliveries.
func (e *Engine) HandleEvent(ctx context.Context, orgID uuid.UUID, eventType db.EventType, payload map[string]interface{}) error {
	rules, err := indexer.ListEnabledEventRules(ctx, e.dbtx, orgID, eventType)
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
		// NATS re-publish is handled by the publisher; create a delivery record only.
		_, err := indexer.CreateEventDelivery(ctx, e.dbtx, indexer.CreateEventDeliveryParams{
			EventRuleID: rule.ID,
			EventType:   eventType,
			Payload:     payload,
			Status:      "delivered",
		})
		return err
	default:
		e.logger.Warn("engine: unsupported action type", zap.String("action_type", string(rule.ActionType)))
		return nil
	}
}

func (e *Engine) dispatchWebhook(ctx context.Context, rule *db.EventRule, eventType db.EventType, payload []byte) error {
	var cfg WebhookActionConfig
	if err := json.Unmarshal(rule.ActionConfig, &cfg); err != nil {
		return fmt.Errorf("engine: parse webhook config: %w", err)
	}
	if cfg.URL == "" {
		return fmt.Errorf("engine: webhook url empty for rule %s", rule.ID)
	}

	delivery, err := indexer.CreateEventDelivery(ctx, e.dbtx, indexer.CreateEventDeliveryParams{
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
