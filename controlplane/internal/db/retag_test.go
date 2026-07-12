package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var retagJobColumns = []string{
	"id", "org_id", "kind", "spec", "status", "attempts", "affected_count",
	"last_error", "actor_user_id", "created_at", "started_at", "finished_at",
}

func retagJobRow(id, orgID uuid.UUID, status string) *sqlmock.Rows {
	return sqlmock.NewRows(retagJobColumns).AddRow(
		id.String(), orgID.String(), "merge", []byte(`{}`), status, 0, 0,
		nil, nil, time.Now().UTC(), nil, nil,
	)
}

func TestEnqueueRetagJob(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID, id := uuid.New(), uuid.New()
	mock.ExpectQuery("INSERT INTO retag_jobs").WillReturnRows(retagJobRow(id, orgID, "pending"))
	job, err := q.EnqueueRetagJob(context.Background(), orgID, "merge", json.RawMessage(`{}`), uuid.NullUUID{})
	require.NoError(t, err)
	assert.Equal(t, "pending", job.Status)
	assert.Equal(t, "merge", job.Kind)
}

func TestGetRetagJob(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID, id := uuid.New(), uuid.New()
	mock.ExpectQuery("SELECT.*FROM retag_jobs").WillReturnRows(retagJobRow(id, orgID, "done"))
	job, err := q.GetRetagJob(context.Background(), id, orgID)
	require.NoError(t, err)
	assert.Equal(t, "done", job.Status)
}

func TestClaimNextRetagJob(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID, id := uuid.New(), uuid.New()
	mock.ExpectQuery("UPDATE retag_jobs").WillReturnRows(retagJobRow(id, orgID, "running"))
	job, err := q.ClaimNextRetagJob(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "running", job.Status)
}

func TestMarkRetagJobDone(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("UPDATE retag_jobs").WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, q.MarkRetagJobDone(context.Background(), uuid.New(), 5))
}

func TestMarkRetagJobFailed(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("UPDATE retag_jobs").WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, q.MarkRetagJobFailed(context.Background(), uuid.New(),
		sql.NullString{String: "boom", Valid: true}))
}

func TestMergeTagValue(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	// The rewrite+audit statement's RowsAffected is the number of files re-tagged.
	mock.ExpectExec("WITH tk AS").WillReturnResult(sqlmock.NewResult(0, 4))
	rows, err := q.MergeTagValue(context.Background(), MergeTagValueParams{
		OrgID: uuid.New(), TagKeyID: uuid.New(), FromValue: "TYO", ToValue: "tokyo"})
	require.NoError(t, err)
	assert.Equal(t, int64(4), rows)
}
