package handler_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/stretchr/testify/assert"
)

// ─────────────────────────────────────────────────────────────────────────────
// IC-4a PoC gate — RED tests (IC-BUG-6).
//
// These tests pin the *new* failure semantics of POST /internal/minio-event:
// an indexing failure must NOT be answered with 200 (MinIO drops the event
// forever on 200), it must return 5xx so MinIO retries. On master today the
// handler unconditionally returns 200, so every test in this file fails.
// Once the fix lands they must all pass (and then move next to the other
// webhook tests).
// ─────────────────────────────────────────────────────────────────────────────

// postIC4A posts one event through the authenticated router.
func postIC4A(t *testing.T, h *handler.MinioEventHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	testMinioEventRouter(h).ServeHTTP(w, req)
	return w
}

// TestRED_HandlerReturns5xxWhenIndexingFails: IndexUpload error → 5xx (was 200).
func TestRED_HandlerReturns5xxWhenIndexingFails(t *testing.T) {
	ix := &mockIndexerClient{err: assert.AnError}
	h := handler.NewMinioEventHandler(ix, testWebhookSecret, newTestLogger())
	w := postIC4A(t, h, `{"Records":[{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"b"},"object":{"key":"k","size":1,"sequencer":"17d2f3a1"}}}]}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "indexing failure must return 5xx so MinIO retries, got 200 (IC-BUG-6)")
}

// TestRED_HandlerReturns5xxWhenDeletionFails: IndexDeletion error → 5xx (was 200).
func TestRED_HandlerReturns5xxWhenDeletionFails(t *testing.T) {
	ix := &mockIndexerClient{err: assert.AnError}
	h := handler.NewMinioEventHandler(ix, testWebhookSecret, newTestLogger())
	w := postIC4A(t, h, `{"Records":[{"eventName":"s3:ObjectRemoved:Delete","s3":{"bucket":{"name":"b"},"object":{"key":"k","sequencer":"17d2f3a2"}}}]}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "deletion failure must return 5xx so MinIO retries, got 200 (IC-BUG-6)")
}
