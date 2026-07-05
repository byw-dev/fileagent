package event_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/event"
	"github.com/byw-dev/fileagent/controlplane/internal/indexer"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── ProcessRetries tests ──────────────────────────────────────────────────────

func TestProcessRetries_NoDeliveries_NoOp(t *testing.T) {
	logger := newTestLogger()
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)
	store := &mockEngineStore{pendingDeliveries: nil}
	engine := event.NewEngineWithStore(store, sender, logger)

	// Direct call — must not panic.
	engine.ProcessRetries(context.Background())
}

func TestProcessRetries_ListError_NoOp(t *testing.T) {
	logger := newTestLogger()
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)
	store := &mockEngineStore{pendingErr: assert.AnError}
	engine := event.NewEngineWithStore(store, sender, logger)

	assert.NotPanics(t, func() {
		engine.ProcessRetries(context.Background())
	})
}

func TestProcessRetries_WebhookDelivery_Success(t *testing.T) {
	logger := newTestLogger()

	deliveryReceived := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deliveryReceived <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ruleID := uuid.New()
	actionCfg, _ := json.Marshal(event.WebhookActionConfig{URL: srv.URL})
	rule := &db.EventRule{
		ID:           ruleID,
		ActionType:   db.ActionTypeWebhook,
		ActionConfig: actionCfg,
	}
	delivery := &db.EventDelivery{
		ID:           uuid.New(),
		EventRuleID:  ruleID,
		Payload:      []byte(`{"test":true}`),
		Status:       "failed",
		AttemptCount: 1,
	}

	updated := false
	_ = updated // still check store.updateCalled below
	delivDB := &trackingDeliveryDB{onUpdate: func() { updated = true }}
	sender := event.NewWebhookSender(delivDB, logger)
	store := &mockEngineStore{
		pendingDeliveries: []*db.EventDelivery{delivery},
		ruleByID:          rule,
	}
	engine := event.NewEngineWithStore(store, sender, logger)

	engine.ProcessRetries(context.Background())

	select {
	case <-deliveryReceived:
		// webhook was called
	case <-time.After(3 * time.Second):
		t.Fatal("webhook not called within timeout")
	}
	assert.True(t, store.updateCalled)
}

func TestProcessRetries_WebhookDelivery_Failure_SetsNextRetry(t *testing.T) {
	logger := newTestLogger()

	ruleID := uuid.New()
	actionCfg, _ := json.Marshal(event.WebhookActionConfig{URL: "http://127.0.0.1:1"}) // refuse connection
	rule := &db.EventRule{
		ID:           ruleID,
		ActionType:   db.ActionTypeWebhook,
		ActionConfig: actionCfg,
	}
	delivery := &db.EventDelivery{
		ID:           uuid.New(),
		EventRuleID:  ruleID,
		Payload:      []byte(`{}`),
		Status:       "failed",
		AttemptCount: 1,
	}

	var capturedParams indexer.UpdateEventDeliveryParams
	captureStore := &capturingEngineStore{
		pendingDeliveries: []*db.EventDelivery{delivery},
		ruleByID:          rule,
		onUpdate: func(p indexer.UpdateEventDeliveryParams) {
			capturedParams = p
		},
	}
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)
	engine := event.NewEngineWithStore(captureStore, sender, logger)

	engine.ProcessRetries(context.Background())

	// Delivery failed → next_retry_at should be set.
	assert.True(t, capturedParams.NextRetryAt.Valid,
		"NextRetryAt should be set on failure, attempt=%d", capturedParams.AttemptCount)
}

func TestProcessRetries_MaxRetries_NoNextRetry(t *testing.T) {
	logger := newTestLogger()

	ruleID := uuid.New()
	actionCfg, _ := json.Marshal(event.WebhookActionConfig{URL: "http://127.0.0.1:1"})
	rule := &db.EventRule{
		ID:           ruleID,
		ActionType:   db.ActionTypeWebhook,
		ActionConfig: actionCfg,
	}
	// AttemptCount at max → no next retry should be scheduled.
	delivery := &db.EventDelivery{
		ID:           uuid.New(),
		EventRuleID:  ruleID,
		Payload:      []byte(`{}`),
		Status:       "failed",
		AttemptCount: 4, // next attempt will be 5 == maxRetryAttempts
	}

	var capturedParams indexer.UpdateEventDeliveryParams
	captureStore := &capturingEngineStore{
		pendingDeliveries: []*db.EventDelivery{delivery},
		ruleByID:          rule,
		onUpdate: func(p indexer.UpdateEventDeliveryParams) {
			capturedParams = p
		},
	}
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)
	engine := event.NewEngineWithStore(captureStore, sender, logger)

	engine.ProcessRetries(context.Background())

	assert.False(t, capturedParams.NextRetryAt.Valid,
		"NextRetryAt should not be set after max retries")
	assert.Equal(t, "dead", capturedParams.Status,
		"exhausted retries must be marked terminal so the scan stops re-selecting them")
}

func TestProcessRetries_UnknownActionType_MarkedTerminal(t *testing.T) {
	logger := newTestLogger()
	ruleID := uuid.New()
	// kafka_publish has no implementation; a stale delivery for it must not be
	// re-scanned every tick — it should be marked terminal ("dead") and drop out.
	rule := &db.EventRule{ID: ruleID, ActionType: db.ActionTypeKafkaPublish, ActionConfig: []byte(`{}`)}
	delivery := &db.EventDelivery{ID: uuid.New(), EventRuleID: ruleID, Payload: []byte(`{}`), Status: "failed"}

	var captured indexer.UpdateEventDeliveryParams
	updateCalled := false
	store := &capturingEngineStore{
		pendingDeliveries: []*db.EventDelivery{delivery},
		ruleByID:          rule,
		onUpdate:          func(p indexer.UpdateEventDeliveryParams) { captured = p; updateCalled = true },
	}
	engine := event.NewEngineWithStore(store, event.NewWebhookSender(&dummyDeliveryDB{}, logger), logger)

	engine.ProcessRetries(context.Background())

	require.True(t, updateCalled, "unknown action delivery must be updated, not left eligible")
	assert.Equal(t, "dead", captured.Status)
	assert.False(t, captured.NextRetryAt.Valid)
}

func TestProcessRetries_RuleNotFound_SkipsDelivery(t *testing.T) {
	logger := newTestLogger()

	delivery := &db.EventDelivery{
		ID:          uuid.New(),
		EventRuleID: uuid.New(),
		Payload:     []byte(`{}`),
	}
	store := &mockEngineStore{
		pendingDeliveries: []*db.EventDelivery{delivery},
		ruleByIDErr:       assert.AnError,
	}
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)
	engine := event.NewEngineWithStore(store, sender, logger)

	assert.NotPanics(t, func() {
		engine.ProcessRetries(context.Background())
	})
}

func TestProcessRetries_NatsRuleBadConfig_NoPanic(t *testing.T) {
	logger := newTestLogger()

	ruleID := uuid.New()
	// nats_publish rule with an unparseable/empty action_config: the retry must
	// surface an error and be skipped for this tick without panicking. (The
	// happy nats retry path is covered in nats_publish_test.go.)
	rule := &db.EventRule{
		ID:         ruleID,
		ActionType: db.ActionTypeNatsPublish,
	}
	delivery := &db.EventDelivery{
		ID:          uuid.New(),
		EventRuleID: ruleID,
		Payload:     []byte(`{}`),
	}
	store := &mockEngineStore{
		pendingDeliveries: []*db.EventDelivery{delivery},
		ruleByID:          rule,
	}
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)
	engine := event.NewEngineWithStore(store, sender, logger)

	assert.NotPanics(t, func() {
		engine.ProcessRetries(context.Background())
	})
}

func TestProcessRetries_BadWebhookConfig_Error(t *testing.T) {
	logger := newTestLogger()

	ruleID := uuid.New()
	rule := &db.EventRule{
		ID:           ruleID,
		ActionType:   db.ActionTypeWebhook,
		ActionConfig: []byte("not-json"),
	}
	delivery := &db.EventDelivery{
		ID:          uuid.New(),
		EventRuleID: ruleID,
	}
	store := &mockEngineStore{
		pendingDeliveries: []*db.EventDelivery{delivery},
		ruleByID:          rule,
	}
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)
	engine := event.NewEngineWithStore(store, sender, logger)

	assert.NotPanics(t, func() {
		engine.ProcessRetries(context.Background())
	})
}

// ── Retry backoff schedule tests ──────────────────────────────────────────────

func TestRetryBackoffSchedule_IncreasesOverAttempts(t *testing.T) {
	// Each attempt's next_retry must be later than the previous.
	// We verify by checking delivery updates at attempt counts 1–4.
	delays := []struct {
		attemptCount int32 // current attempt count
		minDelay     time.Duration
	}{
		{0, 25 * time.Second},   // next = attempt 1 → 30s
		{1, 100 * time.Second},  // next = attempt 2 → 2min
		{2, 500 * time.Second},  // next = attempt 3 → 10min
		{3, 1500 * time.Second}, // next = attempt 4 → 30min
	}

	for _, tc := range delays {
		logger := newTestLogger()
		ruleID := uuid.New()
		actionCfg, _ := json.Marshal(event.WebhookActionConfig{URL: "http://127.0.0.1:1"})
		rule := &db.EventRule{
			ID:           ruleID,
			ActionType:   db.ActionTypeWebhook,
			ActionConfig: actionCfg,
		}
		delivery := &db.EventDelivery{
			ID:           uuid.New(),
			EventRuleID:  ruleID,
			Payload:      []byte(`{}`),
			AttemptCount: tc.attemptCount,
		}

		var capturedParams indexer.UpdateEventDeliveryParams
		captureStore := &capturingEngineStore{
			pendingDeliveries: []*db.EventDelivery{delivery},
			ruleByID:          rule,
			onUpdate: func(p indexer.UpdateEventDeliveryParams) {
				capturedParams = p
			},
		}
		sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)
		engine := event.NewEngineWithStore(captureStore, sender, logger)
		engine.ProcessRetries(context.Background())

		require.True(t, capturedParams.NextRetryAt.Valid,
			"attempt=%d: NextRetryAt should be set", tc.attemptCount)
		retryIn := time.Until(capturedParams.NextRetryAt.Time)
		assert.Greater(t, retryIn, tc.minDelay,
			"attempt=%d: expected delay > %s, got %s", tc.attemptCount, tc.minDelay, retryIn)
	}
}

// ── capturingEngineStore ──────────────────────────────────────────────────────

type capturingEngineStore struct {
	pendingDeliveries []*db.EventDelivery
	pendingErr        error
	ruleByID          *db.EventRule
	ruleByIDErr       error
	onUpdate          func(indexer.UpdateEventDeliveryParams)
}

func (s *capturingEngineStore) ListEnabledEventRules(_ context.Context, _ uuid.UUID, _ db.EventType) ([]*db.EventRule, error) {
	return nil, nil
}

func (s *capturingEngineStore) CreateEventDelivery(_ context.Context, _ indexer.CreateEventDeliveryParams) (*db.EventDelivery, error) {
	return &db.EventDelivery{ID: uuid.New()}, nil
}

func (s *capturingEngineStore) ListPendingDeliveries(_ context.Context) ([]*db.EventDelivery, error) {
	return s.pendingDeliveries, s.pendingErr
}

func (s *capturingEngineStore) UpdateDelivery(_ context.Context, p indexer.UpdateEventDeliveryParams) error {
	if s.onUpdate != nil {
		s.onUpdate(p)
	}
	return nil
}

func (s *capturingEngineStore) GetEventRuleByID(_ context.Context, _ uuid.UUID) (*db.EventRule, error) {
	return s.ruleByID, s.ruleByIDErr
}
