package db

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTagFilterClause_Empty(t *testing.T) {
	var args []interface{}
	assert.Equal(t, "", tagFilterClause(nil, &args))
	assert.Empty(t, args)
}

func TestTagFilterClause_BuildsPredicate(t *testing.T) {
	// A leading arg ($1) already exists, so tag placeholders start at $2.
	args := []interface{}{"org"}
	clause := tagFilterClause([]FileTagFilter{
		{Key: "site", Value: "tokyo"},
		{Key: "level", Value: "raw"},
	}, &args)

	assert.Contains(t, clause, "($2, $3)")
	assert.Contains(t, clause, "($4, $5)")
	assert.Contains(t, clause, "HAVING COUNT(*) = 2")
	assert.Equal(t, []interface{}{"org", "site", "tokyo", "level", "raw"}, args)
}

func TestListFileTagsByFileIDs_Empty(t *testing.T) {
	q := New(nil) // no query is issued for an empty id set
	out, err := q.ListFileTagsByFileIDs(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestListFileTagsByFileIDs_GroupsByFile(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	f1 := uuid.New()
	f2 := uuid.New()
	rows := sqlmock.NewRows([]string{"file_entry_id", "key", "value"}).
		AddRow(f1, "site", "tokyo").
		AddRow(f1, "vendor", "omron").
		AddRow(f2, "site", "osaka")
	mock.ExpectQuery("SELECT file_entry_id, key, value FROM file_tags WHERE file_entry_id IN").
		WillReturnRows(rows)

	q := New(mockDB)
	out, err := q.ListFileTagsByFileIDs(context.Background(), []uuid.UUID{f1, f2})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"site": "tokyo", "vendor": "omron"}, out[f1])
	assert.Equal(t, map[string]string{"site": "osaka"}, out[f2])
	require.NoError(t, mock.ExpectationsWereMet())
}
