package db

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatchSetFileTag(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	// The final INSERT INTO tag_audit's RowsAffected = number of changed files.
	mock.ExpectExec("WITH sel AS").WillReturnResult(sqlmock.NewResult(0, 3))
	n, err := q.BatchSetFileTag(context.Background(), BatchSetFileTagParams{
		Filter:      BatchTagFilter{OrgID: uuid.New(), Tags: []FileTagFilter{{Key: "site", Value: "tokyo"}}},
		Key:         "vendor",
		Value:       "omron",
		Source:      "manual",
		Action:      "set",
		AuditSource: "manual",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
}

func TestBatchClearFileTag(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("WITH sel AS").WillReturnResult(sqlmock.NewResult(0, 2))
	n, err := q.BatchClearFileTag(context.Background(), BatchClearFileTagParams{
		Filter:      BatchTagFilter{OrgID: uuid.New()},
		Key:         "obsolete",
		Action:      "clear",
		AuditSource: "manual",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
}
