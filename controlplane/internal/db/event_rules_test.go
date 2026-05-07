package db

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var eventRuleColumns = []string{
	"id", "org_id", "name", "event_type", "filter",
	"action_type", "action_config", "enabled", "created_by", "created_at", "updated_at",
}

func eventRuleRow(id, orgID uuid.UUID, name string, enabled bool) *sqlmock.Rows {
	now := time.Now().UTC()
	return sqlmock.NewRows(eventRuleColumns).AddRow(
		id.String(), orgID.String(), name, "file_uploaded", []byte(`{}`),
		"webhook", []byte(`{"url":"https://example.com/hook"}`), enabled, nil, now, now,
	)
}

// ── ListEventRules ────────────────────────────────────────────────────────────

func TestListEventRules_ReturnsRows(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID := uuid.New()
	id := uuid.New()

	mock.ExpectQuery("SELECT.*FROM event_rules").
		WillReturnRows(eventRuleRow(id, orgID, "on-upload", true))

	result, err := q.ListEventRules(context.Background(), orgID)
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, id, result[0].ID)
	assert.True(t, result[0].Enabled)
}

func TestListEventRules_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM event_rules").WillReturnError(assert.AnError)

	_, err := q.ListEventRules(context.Background(), uuid.New())
	require.Error(t, err)
}

func TestListEventRules_IncludesDisabled(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID := uuid.New()

	rows := sqlmock.NewRows(eventRuleColumns)
	rows.AddRow(
		uuid.New().String(), orgID.String(), "rule-a", "file_uploaded", []byte(`{}`),
		"webhook", []byte(`{}`), true, nil, time.Now().UTC(), time.Now().UTC(),
	)
	rows.AddRow(
		uuid.New().String(), orgID.String(), "rule-b", "agent_online", []byte(`{}`),
		"webhook", []byte(`{}`), false, nil, time.Now().UTC(), time.Now().UTC(),
	)
	mock.ExpectQuery("SELECT.*FROM event_rules").WillReturnRows(rows)

	result, err := q.ListEventRules(context.Background(), orgID)
	require.NoError(t, err)
	// Both enabled and disabled rules returned (unlike ListEnabledEventRules).
	assert.Len(t, result, 2)
}

// ── GetEventRuleByID ──────────────────────────────────────────────────────────

func TestGetEventRuleByID_Found(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	id := uuid.New()
	orgID := uuid.New()

	mock.ExpectQuery("SELECT.*FROM event_rules").
		WillReturnRows(eventRuleRow(id, orgID, "on-upload", true))

	got, err := q.GetEventRuleByID(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
}

func TestGetEventRuleByID_NotFound(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM event_rules").
		WillReturnRows(sqlmock.NewRows(eventRuleColumns))

	_, err := q.GetEventRuleByID(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── CreateEventRule ───────────────────────────────────────────────────────────

func TestCreateEventRule_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	id := uuid.New()
	orgID := uuid.New()

	mock.ExpectQuery("INSERT INTO event_rules").
		WillReturnRows(eventRuleRow(id, orgID, "new-rule", true))

	got, err := q.CreateEventRule(context.Background(), CreateEventRuleParams{
		OrgID:        orgID,
		Name:         "new-rule",
		EventType:    EventTypeFileUploaded,
		Filter:       []byte(`{}`),
		ActionType:   ActionTypeWebhook,
		ActionConfig: []byte(`{"url":"https://example.com/hook"}`),
		Enabled:      true,
	})
	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
}

func TestCreateEventRule_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("INSERT INTO event_rules").WillReturnError(assert.AnError)

	_, err := q.CreateEventRule(context.Background(), CreateEventRuleParams{
		OrgID:        uuid.New(),
		Name:         "rule",
		EventType:    EventTypeFileUploaded,
		Filter:       []byte(`{}`),
		ActionType:   ActionTypeWebhook,
		ActionConfig: []byte(`{}`),
		Enabled:      true,
	})
	require.Error(t, err)
}

// ── UpdateEventRule ───────────────────────────────────────────────────────────

func TestUpdateEventRule_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	id := uuid.New()
	orgID := uuid.New()

	mock.ExpectQuery("UPDATE event_rules").
		WillReturnRows(eventRuleRow(id, orgID, "updated-rule", false))

	got, err := q.UpdateEventRule(context.Background(), UpdateEventRuleParams{
		ID:           id,
		Name:         "updated-rule",
		EventType:    EventTypeAgentOnline,
		Filter:       []byte(`{}`),
		ActionType:   ActionTypeWebhook,
		ActionConfig: []byte(`{}`),
		Enabled:      false,
	})
	require.NoError(t, err)
	assert.False(t, got.Enabled)
}

func TestUpdateEventRule_NotFound(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("UPDATE event_rules").
		WillReturnRows(sqlmock.NewRows(eventRuleColumns))

	_, err := q.UpdateEventRule(context.Background(), UpdateEventRuleParams{
		ID:           uuid.New(),
		Filter:       []byte(`{}`),
		ActionConfig: []byte(`{}`),
	})
	require.Error(t, err)
}

// ── DeleteEventRule ───────────────────────────────────────────────────────────

func TestDeleteEventRule_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectExec("DELETE FROM event_rules").WillReturnResult(sqlmock.NewResult(1, 1))

	err := q.DeleteEventRule(context.Background(), uuid.New())
	require.NoError(t, err)
}

func TestDeleteEventRule_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectExec("DELETE FROM event_rules").WillReturnError(assert.AnError)

	err := q.DeleteEventRule(context.Background(), uuid.New())
	require.Error(t, err)
}
