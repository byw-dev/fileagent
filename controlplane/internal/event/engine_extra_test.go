package event_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/event"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── Mock DBTX ─────────────────────────────────────────────────────────────────

type errorDBTX struct {
	err error
}

func (e *errorDBTX) ExecContext(_ context.Context, _ string, _ ...interface{}) (sql.Result, error) {
	return nil, e.err
}
func (e *errorDBTX) PrepareContext(_ context.Context, _ string) (*sql.Stmt, error) {
	return nil, e.err
}
func (e *errorDBTX) QueryContext(_ context.Context, _ string, _ ...interface{}) (*sql.Rows, error) {
	return nil, e.err
}
func (e *errorDBTX) QueryRowContext(_ context.Context, _ string, _ ...interface{}) *sql.Row {
	return nil
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestNewEngine(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)
	engine := event.NewEngine(&errorDBTX{}, sender, logger)
	require.NotNil(t, engine)
}

func TestHandleEvent_DBError(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	sender := event.NewWebhookSender(&dummyDeliveryDB{}, logger)
	engine := event.NewEngine(&errorDBTX{err: assert.AnError}, sender, logger)

	err := engine.HandleEvent(context.Background(), uuid.New(), db.EventTypeFileUploaded, map[string]interface{}{
		"file": "test.log",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list event rules")
}

func TestNewDBAdapter(t *testing.T) {
	adapter := event.NewDBAdapter(&errorDBTX{})
	require.NotNil(t, adapter)
}

func TestUpdateEventDeliveryStatus_InvalidID(t *testing.T) {
	adapter := event.NewDBAdapter(&errorDBTX{})
	err := adapter.UpdateEventDeliveryStatus(context.Background(), "not-a-uuid", "delivered",
		sql.NullInt32{}, sql.NullTime{})
	require.Error(t, err)
}

func TestUpdateEventDeliveryStatus_DBError(t *testing.T) {
	adapter := event.NewDBAdapter(&errorDBTX{err: assert.AnError})
	err := adapter.UpdateEventDeliveryStatus(context.Background(), "00000000-0000-0000-0000-000000000001",
		"delivered", sql.NullInt32{}, sql.NullTime{})
	require.Error(t, err)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

type dummyDeliveryDB struct{}

func (d *dummyDeliveryDB) UpdateEventDeliveryStatus(_ context.Context, _ string, _ string, _ sql.NullInt32, _ sql.NullTime) error {
	return nil
}
