package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collectionRuleColumns lists all columns returned by collection rule queries.
var collectionRuleColumns = []string{
	"id", "org_id", "agent_id", "bucket_id", "name", "mode", "status",
	"base_path", "path_pattern", "dest_path_template",
	"recursive", "cron_expr",
	"run_once_on_start", "append_mode", "metadata", "created_at", "updated_at",
}

// addCollectionRuleRow adds a minimal CollectionRule row to a rows builder.
func addCollectionRuleRow(rows *sqlmock.Rows, id, orgID, agentID, bucketID uuid.UUID, name string) *sqlmock.Rows {
	now := time.Now().UTC()
	return rows.AddRow(
		id.String(), orgID.String(), agentID.String(), bucketID.String(), name, "watch", "active",
		"/data/", "*.log", "uploads/",
		true, nil,
		false, "full", []byte(`{}`), now, now,
	)
}

// ── CreateCollectionRule ──────────────────────────────────────────────────────

func TestCreateCollectionRule_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	id := uuid.New()
	orgID := uuid.New()
	agentID := uuid.New()
	bucketID := uuid.New()
	rows := addCollectionRuleRow(sqlmock.NewRows(collectionRuleColumns), id, orgID, agentID, bucketID, "rule-1")
	mock.ExpectQuery("INSERT INTO collection_rules").WillReturnRows(rows)

	rule, err := q.CreateCollectionRule(context.Background(), CreateCollectionRuleParams{
		OrgID:            orgID,
		AgentID:          agentID,
		BucketID:         bucketID,
		Name:             "rule-1",
		Mode:             UploadModeWatch,
		BasePath:         "/data/",
		PathPattern:      "*.log",
		DestPathTemplate: "uploads/",
		Recursive:        true,
		Status:           RuleStatusActive,
		Metadata:         []byte(`{}`),
	})
	require.NoError(t, err)
	require.NotNil(t, rule)
	assert.Equal(t, id, rule.ID)
}

func TestCreateCollectionRule_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("INSERT INTO collection_rules").WillReturnError(assert.AnError)

	_, err := q.CreateCollectionRule(context.Background(), CreateCollectionRuleParams{
		OrgID: uuid.New(),
	})
	require.Error(t, err)
}

// ── DeleteCollectionRule ──────────────────────────────────────────────────────

func TestDeleteCollectionRule_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	id, agentID, orgID := uuid.New(), uuid.New(), uuid.New()
	// The delete is scoped to the rule's owner (IC-SEC-2 ①): the WHERE clause
	// must pin id, agent_id and org_id together.
	mock.ExpectExec("DELETE FROM collection_rules WHERE id = \\$1 AND agent_id = \\$2 AND org_id = \\$3").
		WithArgs(id, agentID, orgID).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rows, err := q.DeleteCollectionRule(context.Background(), id, agentID, orgID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), rows)
}

func TestDeleteCollectionRule_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("DELETE FROM collection_rules").WillReturnError(assert.AnError)

	_, err := q.DeleteCollectionRule(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.Error(t, err)
}

// IC-SEC-2 ①: a delete that matched no row (rule belongs to another agent or
// org) must be reported as 0 affected rows, not swallowed as success.
func TestDeleteCollectionRule_NoMatch_ReportsZeroRows(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("DELETE FROM collection_rules").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	rows, err := q.DeleteCollectionRule(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Equal(t, int64(0), rows)
}

// ── GetCollectionRuleByID ─────────────────────────────────────────────────────

func TestGetCollectionRuleByID_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	id := uuid.New()
	rows := addCollectionRuleRow(sqlmock.NewRows(collectionRuleColumns), id, uuid.New(), uuid.New(), uuid.New(), "rule-1")
	mock.ExpectQuery("SELECT .* FROM collection_rules").WillReturnRows(rows)

	rule, err := q.GetCollectionRuleByID(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, id, rule.ID)
}

func TestGetCollectionRuleByID_NotFound(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("SELECT .* FROM collection_rules").WillReturnError(sql.ErrNoRows)

	_, err := q.GetCollectionRuleByID(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── ListCollectionRulesByAgent ────────────────────────────────────────────────

func TestListCollectionRulesByAgent_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	agentID := uuid.New()
	orgID := uuid.New()
	rows := addCollectionRuleRow(addCollectionRuleRow(
		sqlmock.NewRows(collectionRuleColumns),
		uuid.New(), orgID, agentID, uuid.New(), "rule-1"),
		uuid.New(), orgID, agentID, uuid.New(), "rule-2")
	mock.ExpectQuery("SELECT .* FROM collection_rules").WillReturnRows(rows)

	rules, err := q.ListCollectionRulesByAgent(context.Background(), agentID)
	require.NoError(t, err)
	require.Len(t, rules, 2)
}

func TestListCollectionRulesByAgent_Empty(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	rows := sqlmock.NewRows(collectionRuleColumns)
	mock.ExpectQuery("SELECT .* FROM collection_rules").WillReturnRows(rows)

	rules, err := q.ListCollectionRulesByAgent(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Empty(t, rules)
}

func TestListCollectionRulesByAgent_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("SELECT .* FROM collection_rules").WillReturnError(assert.AnError)

	_, err := q.ListCollectionRulesByAgent(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── UpdateCollectionRuleStatus ────────────────────────────────────────────────

func TestUpdateCollectionRuleStatus_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	id := uuid.New()
	rows := addCollectionRuleRow(sqlmock.NewRows(collectionRuleColumns), id, uuid.New(), uuid.New(), uuid.New(), "rule-1")
	mock.ExpectQuery("UPDATE collection_rules").WillReturnRows(rows)

	rule, err := q.UpdateCollectionRuleStatus(context.Background(), id, RuleStatusInactive)
	require.NoError(t, err)
	assert.Equal(t, id, rule.ID)
}

func TestUpdateCollectionRuleStatus_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("UPDATE collection_rules").WillReturnError(assert.AnError)

	_, err := q.UpdateCollectionRuleStatus(context.Background(), uuid.New(), RuleStatusActive)
	require.Error(t, err)
}
