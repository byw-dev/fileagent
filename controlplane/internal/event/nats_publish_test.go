package event_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/event"
	"github.com/byw-dev/fileagent/controlplane/internal/indexer"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeNATSPublisher records the subject/data of the last Publish call and can
// be made to fail, to exercise the nats_publish action end-to-end.
type fakeNATSPublisher struct {
	mu      sync.Mutex
	subject string
	data    []byte
	calls   int
	err     error
}

func (f *fakeNATSPublisher) Publish(subject string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.subject = subject
	f.data = append([]byte(nil), data...)
	return nil
}

// capturingListStore extends capturingEngineStore with a set of enabled rules so
// HandleEvent dispatches through the real dispatch path.
type capturingListStore struct {
	*capturingEngineStore
	rules []*db.EventRule
}

func (s *capturingListStore) ListEnabledEventRules(_ context.Context, _ uuid.UUID, _ db.EventType) ([]*db.EventRule, error) {
	return s.rules, nil
}

func natsRule(config string) *db.EventRule {
	return &db.EventRule{
		ID:           uuid.New(),
		ActionType:   db.ActionTypeNatsPublish,
		ActionConfig: json.RawMessage(config),
	}
}

func newNATSEngine(t *testing.T, store event.EngineStore, pub event.NATSConn) *event.Engine {
	t.Helper()
	e := event.NewEngineWithStore(store, event.NewWebhookSender(&dummyDeliveryDB{}, newTestLogger()), newTestLogger())
	if pub != nil {
		e = e.WithPublisher(pub)
	}
	return e
}

func TestDispatchNATS_PublishesAndMarksDelivered(t *testing.T) {
	pub := &fakeNATSPublisher{}
	var updated indexer.UpdateEventDeliveryParams
	base := &capturingEngineStore{onUpdate: func(p indexer.UpdateEventDeliveryParams) { updated = p }}
	store := &capturingListStore{capturingEngineStore: base, rules: []*db.EventRule{natsRule(`{"subject":"events.custom.sink"}`)}}

	engine := newNATSEngine(t, store, pub)
	err := engine.HandleEvent(context.Background(), uuid.New(), db.EventTypeFileUploaded,
		map[string]interface{}{"file": "a.txt"})
	require.NoError(t, err)

	require.Equal(t, 1, pub.calls, "expected exactly one publish")
	assert.Equal(t, "events.custom.sink", pub.subject)
	assert.JSONEq(t, `{"file":"a.txt"}`, string(pub.data))
	assert.Equal(t, "delivered", updated.Status)
	assert.True(t, updated.DeliveredAt.Valid)
}

func TestDispatchNATS_PublishError_MarksFailedWithRetry(t *testing.T) {
	pub := &fakeNATSPublisher{err: errors.New("nats down")}
	var updated indexer.UpdateEventDeliveryParams
	base := &capturingEngineStore{onUpdate: func(p indexer.UpdateEventDeliveryParams) { updated = p }}
	store := &capturingListStore{capturingEngineStore: base, rules: []*db.EventRule{natsRule(`{"subject":"events.custom.sink"}`)}}

	engine := newNATSEngine(t, store, pub)
	require.NoError(t, engine.HandleEvent(context.Background(), uuid.New(), db.EventTypeFileUploaded,
		map[string]interface{}{"file": "a.txt"}))

	assert.Equal(t, "failed", updated.Status)
	assert.False(t, updated.DeliveredAt.Valid)
	assert.True(t, updated.NextRetryAt.Valid, "failed nats delivery must be scheduled for retry")
}

func TestDispatchNATS_NoPublisher_MarksFailed(t *testing.T) {
	var updated indexer.UpdateEventDeliveryParams
	base := &capturingEngineStore{onUpdate: func(p indexer.UpdateEventDeliveryParams) { updated = p }}
	store := &capturingListStore{capturingEngineStore: base, rules: []*db.EventRule{natsRule(`{"subject":"events.custom.sink"}`)}}

	// No publisher wired — nats_publish must record a failed delivery, not silently succeed.
	engine := newNATSEngine(t, store, nil)
	require.NoError(t, engine.HandleEvent(context.Background(), uuid.New(), db.EventTypeFileUploaded,
		map[string]interface{}{"file": "a.txt"}))
	assert.Equal(t, "failed", updated.Status)
}

func TestDispatchNATS_EmptySubject_NoPublishNoDelivery(t *testing.T) {
	pub := &fakeNATSPublisher{}
	updateCalled := false
	base := &capturingEngineStore{onUpdate: func(indexer.UpdateEventDeliveryParams) { updateCalled = true }}
	store := &capturingListStore{capturingEngineStore: base, rules: []*db.EventRule{natsRule(`{"subject":""}`)}}

	engine := newNATSEngine(t, store, pub)
	require.NoError(t, engine.HandleEvent(context.Background(), uuid.New(), db.EventTypeFileUploaded,
		map[string]interface{}{"file": "a.txt"}))
	assert.Equal(t, 0, pub.calls, "empty subject must not publish")
	assert.False(t, updateCalled, "empty subject must not record a delivery")
}

func TestRetryDelivery_NATSPublish_Republishes(t *testing.T) {
	pub := &fakeNATSPublisher{}
	var updated indexer.UpdateEventDeliveryParams
	store := &capturingEngineStore{
		onUpdate: func(p indexer.UpdateEventDeliveryParams) { updated = p },
		ruleByID: natsRule(`{"subject":"events.custom.retry"}`),
		pendingDeliveries: []*db.EventDelivery{
			{ID: uuid.New(), Payload: json.RawMessage(`{"file":"b.txt"}`), AttemptCount: 1},
		},
	}
	engine := newNATSEngine(t, store, pub)
	engine.ProcessRetries(context.Background())

	require.Equal(t, 1, pub.calls, "retry must re-publish to NATS")
	assert.Equal(t, "events.custom.retry", pub.subject)
	assert.Equal(t, "delivered", updated.Status)
}
