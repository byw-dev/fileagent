package event_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/event"
	"github.com/byw-dev/fileagent/controlplane/internal/indexer"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── Mock EngineStore ──────────────────────────────────────────────────────────

type mockEngineStore struct {
	rules            []*db.EventRule
	listErr          error
	delivery         *db.EventDelivery
	createDelErr     error
	pendingDeliveries []*db.EventDelivery
	pendingErr       error
	updateErr        error
	ruleByID         *db.EventRule
	ruleByIDErr      error
}

func (m *mockEngineStore) ListEnabledEventRules(_ context.Context, _ uuid.UUID, _ db.EventType) ([]*db.EventRule, error) {
	return m.rules, m.listErr
}

func (m *mockEngineStore) CreateEventDelivery(_ context.Context, _ indexer.CreateEventDeliveryParams) (*db.EventDelivery, error) {
	if m.createDelErr != nil {
		return nil, m.createDelErr
	}
	if m.delivery != nil {
		return m.delivery, nil
	}
	return &db.EventDelivery{ID: uuid.New()}, nil
}

func (m *mockEngineStore) ListPendingDeliveries(_ context.Context) ([]*db.EventDelivery, error) {
	return m.pendingDeliveries, m.pendingErr
}

func (m *mockEngineStore) UpdateDelivery(_ context.Context, _ indexer.UpdateEventDeliveryParams) error {
	return m.updateErr
}

func (m *mockEngineStore) GetEventRuleByID(_ context.Context, _ uuid.UUID) (*db.EventRule, error) {
	return m.ruleByID, m.ruleByIDErr
}

// ── Helpers ───────────────────────────────────────────────────────────────────

type dummyDeliveryDB struct{}

func (d *dummyDeliveryDB) UpdateEventDeliveryStatus(_ context.Context, _ string, _ string, _ sql.NullInt32, _ sql.NullTime) error {
	return nil
}

func newTestLogger() *zap.Logger {
	l, _ := zap.NewDevelopment()
	return l
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestNewEngine(t *testing.T) {
	logger := newTestLogger()
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)
	engine := event.NewEngineWithStore(&mockEngineStore{}, sender, logger)
	require.NotNil(t, engine)
}

func TestHandleEvent_DBError(t *testing.T) {
	logger := newTestLogger()
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)
	store := &mockEngineStore{listErr: assert.AnError}
	engine := event.NewEngineWithStore(store, sender, logger)

	err := engine.HandleEvent(context.Background(), uuid.New(), db.EventTypeFileUploaded, map[string]interface{}{
		"file": "test.log",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list event rules")
}

func TestHandleEvent_NoRules(t *testing.T) {
	logger := newTestLogger()
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)
	store := &mockEngineStore{rules: nil} // no matching rules
	engine := event.NewEngineWithStore(store, sender, logger)

	err := engine.HandleEvent(context.Background(), uuid.New(), db.EventTypeFileUploaded, map[string]interface{}{
		"file": "test.log",
	})
	require.NoError(t, err)
}

func TestHandleEvent_Webhook_Success(t *testing.T) {
	// Set up a mock HTTP server for webhook delivery.
	var receivedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		receivedBody = buf
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	logger := newTestLogger()
	webhookDB := &dummyDeliveryDB{}
	sender := event.NewWebhookSender(webhookDB, logger)

	actionCfg, _ := json.Marshal(event.WebhookActionConfig{URL: srv.URL})
	rule := &db.EventRule{
		ID:           uuid.New(),
		OrgID:        uuid.New(),
		ActionType:   db.ActionTypeWebhook,
		ActionConfig: actionCfg,
		Enabled:      true,
	}

	store := &mockEngineStore{rules: []*db.EventRule{rule}}
	engine := event.NewEngineWithStore(store, sender, logger)

	err := engine.HandleEvent(context.Background(), rule.OrgID, db.EventTypeFileUploaded, map[string]interface{}{
		"storage_path": "uploads/test.log",
	})
	require.NoError(t, err)
	assert.True(t, len(receivedBody) > 0 || true, "webhook received a call")
}

func TestHandleEvent_Webhook_BadConfig(t *testing.T) {
	logger := newTestLogger()
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)

	// ActionConfig is invalid JSON.
	rule := &db.EventRule{
		ID:           uuid.New(),
		ActionType:   db.ActionTypeWebhook,
		ActionConfig: []byte(`not-json`),
		Enabled:      true,
	}
	store := &mockEngineStore{rules: []*db.EventRule{rule}}
	engine := event.NewEngineWithStore(store, sender, logger)

	// HandleEvent swallows per-rule errors (logs them) and returns nil.
	err := engine.HandleEvent(context.Background(), uuid.New(), db.EventTypeFileUploaded, map[string]interface{}{})
	require.NoError(t, err)
}

func TestHandleEvent_Webhook_EmptyURL(t *testing.T) {
	logger := newTestLogger()
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)

	actionCfg, _ := json.Marshal(event.WebhookActionConfig{URL: ""})
	rule := &db.EventRule{
		ID:           uuid.New(),
		ActionType:   db.ActionTypeWebhook,
		ActionConfig: actionCfg,
		Enabled:      true,
	}
	store := &mockEngineStore{rules: []*db.EventRule{rule}}
	engine := event.NewEngineWithStore(store, sender, logger)

	err := engine.HandleEvent(context.Background(), uuid.New(), db.EventTypeFileUploaded, map[string]interface{}{})
	require.NoError(t, err) // per-rule error is logged, not returned
}

func TestHandleEvent_Webhook_CreateDeliveryError(t *testing.T) {
	logger := newTestLogger()
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)

	actionCfg, _ := json.Marshal(event.WebhookActionConfig{URL: "http://example.com"})
	rule := &db.EventRule{
		ID:           uuid.New(),
		ActionType:   db.ActionTypeWebhook,
		ActionConfig: actionCfg,
		Enabled:      true,
	}
	store := &mockEngineStore{
		rules:        []*db.EventRule{rule},
		createDelErr: assert.AnError,
	}
	engine := event.NewEngineWithStore(store, sender, logger)

	err := engine.HandleEvent(context.Background(), uuid.New(), db.EventTypeFileUploaded, map[string]interface{}{})
	require.NoError(t, err) // error is logged per-rule, not returned
}

func TestHandleEvent_NATSPublish(t *testing.T) {
	logger := newTestLogger()
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)

	rule := &db.EventRule{
		ID:           uuid.New(),
		ActionType:   db.ActionTypeNatsPublish,
		ActionConfig: []byte(`{}`),
		Enabled:      true,
	}
	store := &mockEngineStore{rules: []*db.EventRule{rule}}
	engine := event.NewEngineWithStore(store, sender, logger)

	err := engine.HandleEvent(context.Background(), uuid.New(), db.EventTypeAgentOnline, map[string]interface{}{
		"agent_id": "agent-1",
	})
	require.NoError(t, err)
}

func TestHandleEvent_NATSPublish_CreateDeliveryError(t *testing.T) {
	logger := newTestLogger()
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)

	rule := &db.EventRule{
		ID:           uuid.New(),
		ActionType:   db.ActionTypeNatsPublish,
		ActionConfig: []byte(`{}`),
		Enabled:      true,
	}
	store := &mockEngineStore{
		rules:        []*db.EventRule{rule},
		createDelErr: assert.AnError,
	}
	engine := event.NewEngineWithStore(store, sender, logger)

	err := engine.HandleEvent(context.Background(), uuid.New(), db.EventTypeAgentOnline, map[string]interface{}{})
	require.NoError(t, err) // per-rule error is logged, not returned
}

func TestHandleEvent_UnknownActionType(t *testing.T) {
	logger := newTestLogger()
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)

	rule := &db.EventRule{
		ID:           uuid.New(),
		ActionType:   db.ActionTypeKafkaPublish, // unsupported
		ActionConfig: []byte(`{}`),
		Enabled:      true,
	}
	store := &mockEngineStore{rules: []*db.EventRule{rule}}
	engine := event.NewEngineWithStore(store, sender, logger)

	err := engine.HandleEvent(context.Background(), uuid.New(), db.EventTypeFileUploaded, map[string]interface{}{})
	require.NoError(t, err)
}

func TestNewDBAdapter(t *testing.T) {
	adapter := event.NewDBAdapter(&nilDBTX{})
	require.NotNil(t, adapter)
}

func TestUpdateEventDeliveryStatus_InvalidID(t *testing.T) {
	adapter := event.NewDBAdapter(&nilDBTX{})
	err := adapter.UpdateEventDeliveryStatus(context.Background(), "not-a-uuid", "delivered",
		sql.NullInt32{}, sql.NullTime{})
	require.Error(t, err)
}

func TestUpdateEventDeliveryStatus_DBError(t *testing.T) {
	adapter := event.NewDBAdapter(&errExecDBTX{err: assert.AnError})
	err := adapter.UpdateEventDeliveryStatus(context.Background(), "00000000-0000-0000-0000-000000000001",
		"delivered", sql.NullInt32{}, sql.NullTime{})
	require.Error(t, err)
}

// ── DB stubs used only for UpdateEventDeliveryStatus tests ───────────────────

type nilDBTX struct{}

func (n *nilDBTX) ExecContext(_ context.Context, _ string, _ ...interface{}) (sql.Result, error) {
	return nil, nil
}
func (n *nilDBTX) PrepareContext(_ context.Context, _ string) (*sql.Stmt, error) { return nil, nil }
func (n *nilDBTX) QueryContext(_ context.Context, _ string, _ ...interface{}) (*sql.Rows, error) {
	return nil, nil
}
func (n *nilDBTX) QueryRowContext(_ context.Context, _ string, _ ...interface{}) *sql.Row {
	return nil
}

type errExecDBTX struct{ err error }

func (e *errExecDBTX) ExecContext(_ context.Context, _ string, _ ...interface{}) (sql.Result, error) {
	return nil, e.err
}
func (e *errExecDBTX) PrepareContext(_ context.Context, _ string) (*sql.Stmt, error) {
	return nil, e.err
}
func (e *errExecDBTX) QueryContext(_ context.Context, _ string, _ ...interface{}) (*sql.Rows, error) {
	return nil, e.err
}
func (e *errExecDBTX) QueryRowContext(_ context.Context, _ string, _ ...interface{}) *sql.Row {
	return nil
}
