package event

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// DeliveryRecord represents a pending/failed event delivery that needs retry.
type DeliveryRecord struct {
	ID          string
	URL         string
	Payload     []byte
	AttemptNo   int
	NextRetryAt time.Time
}

// EventDeliveryDB is the minimal DB interface for webhook delivery.
type EventDeliveryDB interface {
	UpdateEventDeliveryStatus(ctx context.Context, id string, status string, responseCode sql.NullInt32, nextRetryAt sql.NullTime) error
}

// WebhookSender sends HTTP webhooks with retry back-off.
type WebhookSender struct {
	db     EventDeliveryDB
	client *http.Client
	logger *zap.Logger
}

// NewWebhookSender creates a WebhookSender.
func NewWebhookSender(db EventDeliveryDB, logger *zap.Logger) *WebhookSender {
	return &WebhookSender{
		db:     db,
		client: &http.Client{Timeout: 10 * time.Second},
		logger: logger,
	}
}

// Send attempts to POST payload to the given URL. It updates the delivery
// record in the database based on the outcome.
func (w *WebhookSender) Send(ctx context.Context, rec DeliveryRecord) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rec.URL, bytes.NewReader(rec.Payload))
	if err != nil {
		return fmt.Errorf("webhook: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		w.logger.Warn("webhook delivery failed",
			zap.String("id", rec.ID),
			zap.String("url", rec.URL),
			zap.Error(err),
		)
		return w.scheduleRetry(ctx, rec, 0)
	}
	defer resp.Body.Close()

	code := int32(resp.StatusCode)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		w.logger.Info("webhook delivered",
			zap.String("id", rec.ID),
			zap.Int("status", resp.StatusCode),
		)
		return w.db.UpdateEventDeliveryStatus(ctx, rec.ID, "delivered",
			sql.NullInt32{Int32: code, Valid: true},
			sql.NullTime{},
		)
	}

	w.logger.Warn("webhook non-2xx response",
		zap.String("id", rec.ID),
		zap.Int("status", resp.StatusCode),
	)
	return w.scheduleRetry(ctx, rec, code)
}

func (w *WebhookSender) scheduleRetry(ctx context.Context, rec DeliveryRecord, code int32) error {
	attempt := rec.AttemptNo
	if attempt >= len(retryBackoffSchedule) {
		// Exhausted all retries — mark terminal ('dead') so the row drops out of
		// the retry scan. A 'failed' row with a NULL next_retry_at is treated as
		// "due now" and would be re-selected (and re-sent) every tick forever.
		return w.db.UpdateEventDeliveryStatus(ctx, rec.ID, deliveryStatusDead,
			sql.NullInt32{Int32: code, Valid: code != 0},
			sql.NullTime{},
		)
	}
	next := time.Now().UTC().Add(retryBackoffSchedule[attempt])
	return w.db.UpdateEventDeliveryStatus(ctx, rec.ID, "pending",
		sql.NullInt32{Int32: code, Valid: code != 0},
		sql.NullTime{Time: next, Valid: true},
	)
}

// MarshalDeliveryPayload converts an event payload map to JSON bytes.
func MarshalDeliveryPayload(payload map[string]interface{}) ([]byte, error) {
	return json.Marshal(payload)
}
