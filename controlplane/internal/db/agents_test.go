package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentColumns lists all columns returned by agent queries.
var agentColumns = []string{
	"id", "org_id", "name", "fingerprint", "status",
	"auth_token_hash", "token_expires_at", "os_info", "ip_address",
	"approved_by", "approved_at", "revoked_by", "revoked_at",
	"last_seen_at", "metadata", "created_at", "updated_at",
}

func newTestQueries(t *testing.T) (*Queries, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mockDB.Close() })
	return New(mockDB), mock, mockDB
}

// addAgentRow adds a minimal Agent row to a rows builder.
func addAgentRow(rows *sqlmock.Rows, id, orgID uuid.UUID, name, fingerprint string) *sqlmock.Rows {
	now := time.Now().UTC()
	return rows.AddRow(
		id.String(), orgID.String(), name, fingerprint, "pending",
		nil, nil, []byte(`{}`), "192.168.1.1",
		nil, nil, nil, nil,
		nil, []byte(`{}`), now, now,
	)
}

// ── CreateAgent ───────────────────────────────────────────────────────────────

func TestCreateAgent_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	id := uuid.New()
	orgID := uuid.New()
	rows := addAgentRow(sqlmock.NewRows(agentColumns), id, orgID, "agent-1", "fp-1")
	mock.ExpectQuery("INSERT INTO agents").WillReturnRows(rows)

	agent, err := q.CreateAgent(context.Background(), CreateAgentParams{
		OrgID:       orgID,
		Name:        "agent-1",
		Fingerprint: "fp-1",
		OsInfo:      []byte(`{}`),
	})
	require.NoError(t, err)
	require.NotNil(t, agent)
	assert.Equal(t, id, agent.ID)
}

func TestCreateAgent_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("INSERT INTO agents").WillReturnError(assert.AnError)

	_, err := q.CreateAgent(context.Background(), CreateAgentParams{OrgID: uuid.New()})
	require.Error(t, err)
}

// ── DeleteAgent ───────────────────────────────────────────────────────────────

func TestDeleteAgent_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("DELETE FROM agents").WillReturnResult(sqlmock.NewResult(1, 1))

	err := q.DeleteAgent(context.Background(), uuid.New())
	require.NoError(t, err)
}

func TestDeleteAgent_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("DELETE FROM agents").WillReturnError(assert.AnError)

	err := q.DeleteAgent(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── GetAgentByFingerprint ─────────────────────────────────────────────────────

func TestGetAgentByFingerprint_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	id := uuid.New()
	orgID := uuid.New()
	rows := addAgentRow(sqlmock.NewRows(agentColumns), id, orgID, "agent-1", "fp-abc")
	mock.ExpectQuery("SELECT .* FROM agents").WillReturnRows(rows)

	agent, err := q.GetAgentByFingerprint(context.Background(), "fp-abc")
	require.NoError(t, err)
	assert.Equal(t, id, agent.ID)
}

func TestGetAgentByFingerprint_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("SELECT .* FROM agents").WillReturnError(assert.AnError)

	_, err := q.GetAgentByFingerprint(context.Background(), "fp-missing")
	require.Error(t, err)
}

// ── GetAgentByID ──────────────────────────────────────────────────────────────

func TestGetAgentByID_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	id := uuid.New()
	orgID := uuid.New()
	rows := addAgentRow(sqlmock.NewRows(agentColumns), id, orgID, "agent-2", "fp-2")
	mock.ExpectQuery("SELECT .* FROM agents").WillReturnRows(rows)

	agent, err := q.GetAgentByID(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, id, agent.ID)
}

func TestGetAgentByID_NotFound(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("SELECT .* FROM agents").WillReturnError(sql.ErrNoRows)

	_, err := q.GetAgentByID(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── ListAgents ────────────────────────────────────────────────────────────────

func TestListAgents_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	orgID := uuid.New()
	rows := addAgentRow(addAgentRow(sqlmock.NewRows(agentColumns),
		uuid.New(), orgID, "agent-1", "fp-1"),
		uuid.New(), orgID, "agent-2", "fp-2")
	mock.ExpectQuery("SELECT .* FROM agents").WillReturnRows(rows)

	agents, err := q.ListAgents(context.Background(), orgID)
	require.NoError(t, err)
	require.Len(t, agents, 2)
}

func TestListAgents_Empty(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	rows := sqlmock.NewRows(agentColumns)
	mock.ExpectQuery("SELECT .* FROM agents").WillReturnRows(rows)

	agents, err := q.ListAgents(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Empty(t, agents)
}

func TestListAgents_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("SELECT .* FROM agents").WillReturnError(assert.AnError)

	_, err := q.ListAgents(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── ListAgentsByStatus ────────────────────────────────────────────────────────

func TestListAgentsByStatus_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	orgID := uuid.New()
	rows := addAgentRow(sqlmock.NewRows(agentColumns), uuid.New(), orgID, "agent-1", "fp-1")
	mock.ExpectQuery("SELECT .* FROM agents").WillReturnRows(rows)

	agents, err := q.ListAgentsByStatus(context.Background(), orgID, AgentStatusPending)
	require.NoError(t, err)
	require.Len(t, agents, 1)
}

func TestListAgentsByStatus_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("SELECT .* FROM agents").WillReturnError(assert.AnError)

	_, err := q.ListAgentsByStatus(context.Background(), uuid.New(), AgentStatusPending)
	require.Error(t, err)
}

// ── UpdateAgentAuthToken ──────────────────────────────────────────────────────

func TestUpdateAgentAuthToken_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	id := uuid.New()
	orgID := uuid.New()
	rows := addAgentRow(sqlmock.NewRows(agentColumns), id, orgID, "agent-1", "fp-1")
	mock.ExpectQuery("UPDATE agents").WillReturnRows(rows)

	agent, err := q.UpdateAgentAuthToken(context.Background(), id,
		sql.NullString{String: "hash123", Valid: true},
		sql.NullTime{Time: time.Now().Add(time.Hour), Valid: true})
	require.NoError(t, err)
	assert.Equal(t, id, agent.ID)
}

func TestUpdateAgentAuthToken_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("UPDATE agents").WillReturnError(assert.AnError)

	_, err := q.UpdateAgentAuthToken(context.Background(), uuid.New(),
		sql.NullString{}, sql.NullTime{})
	require.Error(t, err)
}

// ── UpdateAgentHeartbeat ──────────────────────────────────────────────────────

func TestUpdateAgentHeartbeat_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("UPDATE agents").WillReturnResult(sqlmock.NewResult(1, 1))

	err := q.UpdateAgentHeartbeat(context.Background(), uuid.New(), pqtype.Inet{})
	require.NoError(t, err)
}

func TestUpdateAgentHeartbeat_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("UPDATE agents").WillReturnError(assert.AnError)

	err := q.UpdateAgentHeartbeat(context.Background(), uuid.New(), pqtype.Inet{})
	require.Error(t, err)
}

// ── UpdateAgentStatus ─────────────────────────────────────────────────────────

func TestUpdateAgentStatus_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	id := uuid.New()
	orgID := uuid.New()
	rows := addAgentRow(sqlmock.NewRows(agentColumns), id, orgID, "agent-1", "fp-1")
	mock.ExpectQuery("UPDATE agents").WillReturnRows(rows)

	agent, err := q.UpdateAgentStatus(context.Background(), id, AgentStatusApproved)
	require.NoError(t, err)
	assert.Equal(t, id, agent.ID)
}

func TestUpdateAgentStatus_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("UPDATE agents").WillReturnError(assert.AnError)

	_, err := q.UpdateAgentStatus(context.Background(), uuid.New(), AgentStatusRevoked)
	require.Error(t, err)
}

