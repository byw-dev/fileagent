package handler_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// IC-4a round-4 — guards that existed in code but were pinned by no test.
//
// Found by mutating the round-3 result: removing the parse-cap arithmetic left
// every test green. A guard nothing pins is a guard a later change can delete
// silently, which is the failure mode this whole track keeps hitting (the code
// is right, CI is green, and nothing notices when it stops being right).
// ─────────────────────────────────────────────────────────────────────────────

// bodyOfExactly builds a valid, parseable webhook payload whose serialized
// length is exactly total bytes, by padding a filler field.
func bodyOfExactly(t *testing.T, total int64) string {
	t.Helper()
	const prefix = `{"filler":"`
	const suffix = `","Records":[]}`
	pad := total - int64(len(prefix)+len(suffix))
	require.Positive(t, pad, "total must exceed the JSON shell")
	return prefix + strings.Repeat("z", int(pad)) + suffix
}

// TestParseCap_ExactBoundary pins the cap arithmetic itself. readBodyCapped
// reads cap+1 bytes and flags oversized only when the body is STRICTLY larger
// than the cap, so a body of exactly cap bytes must still parse normally.
// An off-by-one here turns legitimate at-the-limit notifications into a
// permanent 5xx with no operator remedy — the same head-of-line failure mode
// IC-4a exists to eliminate (round-3 B-2).
func TestParseCap_ExactBoundary(t *testing.T) {
	cap := handler.MinWebhookParseCap

	t.Run("exactly at the cap parses normally", func(t *testing.T) {
		ix := &mockIndexerClient{}
		fails := newCountingFailStore()
		dead := &recordingSink{}
		h := handler.NewMinioEventHandlerWithPolicyParseCap(
			ix, testWebhookSecret, fails, dead, 5, cap, newTestLogger())

		body := bodyOfExactly(t, cap)
		require.Equal(t, cap, int64(len(body)), "test payload must be exactly the cap")

		w := postIC4A(t, h, body)
		assert.Equal(t, http.StatusOK, w.Code, "a body of exactly the cap must not be rejected")
		assert.Zero(t, dead.count(), "a body of exactly the cap must not be dead-lettered")
	})

	t.Run("one byte over the cap is oversized", func(t *testing.T) {
		ix := &mockIndexerClient{}
		fails := newCountingFailStore()
		dead := &recordingSink{}
		h := handler.NewMinioEventHandlerWithPolicyParseCap(
			ix, testWebhookSecret, fails, dead, 5, cap, newTestLogger())

		body := bodyOfExactly(t, cap+1)
		require.Equal(t, cap+1, int64(len(body)))

		w := postIC4A(t, h, body)
		assert.Equal(t, http.StatusInternalServerError, w.Code,
			"over the cap must answer 5xx, never a silent 200")
		assert.Equal(t, 1, dead.count(), "over the cap must be recorded as a dead letter")
	})
}
