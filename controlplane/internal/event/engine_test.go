package event

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── Mock NATSConn ─────────────────────────────────────────────────────────────

type mockConn struct {
	msgs map[string][][]byte
}

func newMockConn() *mockConn {
	return &mockConn{msgs: make(map[string][][]byte)}
}

func (m *mockConn) Publish(subject string, data []byte) error {
	m.msgs[subject] = append(m.msgs[subject], data)
	return nil
}

// ── Mock EventDeliveryDB ──────────────────────────────────────────────────────

type mockDeliveryDB struct {
	updates []string
}

func (m *mockDeliveryDB) UpdateEventDeliveryStatus(_ context.Context, id string, status string, _ sql.NullInt32, _ sql.NullTime) error {
	m.updates = append(m.updates, id+":"+status)
	return nil
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestPublisher_Publish(t *testing.T) {
	conn := newMockConn()
	pub := NewPublisher(conn)
	require.NotNil(t, pub)

	payload := map[string]string{"key": "value"}
	err := pub.Publish("test.subject", payload)
	require.NoError(t, err)

	msgs, ok := conn.msgs["test.subject"]
	require.True(t, ok)
	require.Len(t, msgs, 1)

	var out map[string]string
	require.NoError(t, json.Unmarshal(msgs[0], &out))
	assert.Equal(t, "value", out["key"])
}

func TestPublisher_Publish_Marshal_Error(t *testing.T) {
	conn := newMockConn()
	pub := NewPublisher(conn)

	// channels can't be marshalled to JSON
	err := pub.Publish("subject", make(chan int))
	require.Error(t, err)
}

func TestWebhookSender_Send_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		assert.Contains(t, string(body), "file.log")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	db := &mockDeliveryDB{}
	logger, _ := zap.NewDevelopment()
	sender := NewWebhookSender(db, logger)

	rec := DeliveryRecord{
		ID:        "delivery-1",
		URL:       srv.URL,
		Payload:   []byte(`{"file":"file.log"}`),
		AttemptNo: 0,
	}
	err := sender.Send(context.Background(), rec)
	require.NoError(t, err)

	require.Len(t, db.updates, 1)
	assert.Contains(t, db.updates[0], "delivered")
}

func TestWebhookSender_Send_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	db := &mockDeliveryDB{}
	logger, _ := zap.NewDevelopment()
	sender := NewWebhookSender(db, logger)

	rec := DeliveryRecord{
		ID:        "delivery-2",
		URL:       srv.URL,
		Payload:   []byte(`{}`),
		AttemptNo: 0,
	}
	err := sender.Send(context.Background(), rec)
	require.NoError(t, err)

	require.Len(t, db.updates, 1)
	// Should be scheduled for retry, not delivered.
	assert.Contains(t, db.updates[0], "pending")
}

func TestWebhookSender_Send_Exhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	db := &mockDeliveryDB{}
	logger, _ := zap.NewDevelopment()
	sender := NewWebhookSender(db, logger)

	rec := DeliveryRecord{
		ID:        "delivery-3",
		URL:       srv.URL,
		Payload:   []byte(`{}`),
		AttemptNo: len(retryBackoffSchedule), // already at max retries
	}
	err := sender.Send(context.Background(), rec)
	require.NoError(t, err)

	require.Len(t, db.updates, 1)
	assert.Contains(t, db.updates[0], "failed")
}

func TestMarshalDeliveryPayload(t *testing.T) {
	payload := map[string]interface{}{
		"event_type":   "file.uploaded",
		"storage_path": "uploads/file.log",
	}
	data, err := MarshalDeliveryPayload(payload)
	require.NoError(t, err)
	assert.True(t, json.Valid(data))
	assert.True(t, bytes.Contains(data, []byte("file.uploaded")))
}
