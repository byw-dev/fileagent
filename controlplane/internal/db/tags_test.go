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

var tagKeyColumns = []string{
	"id", "org_id", "key", "label", "value_controlled",
	"required_at_collection", "allow_path_var", "system_reserved", "created_at",
}

func tagKeyRow(id, orgID uuid.UUID, key string) *sqlmock.Rows {
	return sqlmock.NewRows(tagKeyColumns).AddRow(
		id, orgID, key, "Label", true, false, true, false, time.Now().UTC(),
	)
}

var tagValueColumns = []string{"id", "tag_key_id", "value", "created_at"}

func tagValueRow(id, keyID uuid.UUID, value string) *sqlmock.Rows {
	return sqlmock.NewRows(tagValueColumns).AddRow(id, keyID, value, time.Now().UTC())
}

func TestListTagKeys(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID, id := uuid.New(), uuid.New()
	mock.ExpectQuery("SELECT.*FROM tag_keys").WillReturnRows(tagKeyRow(id, orgID, "site"))
	got, err := q.ListTagKeys(context.Background(), orgID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "site", got[0].Key)
}

func TestGetTagKey_FoundAndError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID, id := uuid.New(), uuid.New()
	mock.ExpectQuery("SELECT.*FROM tag_keys").WillReturnRows(tagKeyRow(id, orgID, "site"))
	got, err := q.GetTagKey(context.Background(), orgID, "site")
	require.NoError(t, err)
	assert.Equal(t, "site", got.Key)
	assert.True(t, got.AllowPathVar)

	mock.ExpectQuery("SELECT.*FROM tag_keys").WillReturnError(assert.AnError)
	_, err = q.GetTagKey(context.Background(), orgID, "nope")
	require.Error(t, err)
}

func TestCreateTagKey(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID, id := uuid.New(), uuid.New()
	mock.ExpectQuery("INSERT INTO tag_keys").WillReturnRows(tagKeyRow(id, orgID, "vendor"))
	got, err := q.CreateTagKey(context.Background(), CreateTagKeyParams{
		OrgID: orgID, Key: "vendor", Label: "Vendor", ValueControlled: true, AllowPathVar: true})
	require.NoError(t, err)
	assert.Equal(t, "vendor", got.Key)
}

func TestUpdateTagKey(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID, id := uuid.New(), uuid.New()
	mock.ExpectQuery("UPDATE tag_keys").WillReturnRows(tagKeyRow(id, orgID, "site"))
	got, err := q.UpdateTagKey(context.Background(), UpdateTagKeyParams{
		OrgID: orgID, Key: "site", Label: "Site2", ValueControlled: true, AllowPathVar: true})
	require.NoError(t, err)
	assert.Equal(t, "site", got.Key)
}

func TestDeleteTagKey(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("DELETE FROM tag_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	rows, err := q.DeleteTagKey(context.Background(), uuid.New(), "site")
	require.NoError(t, err)
	assert.Equal(t, int64(1), rows)
}

func TestListTagValues(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	keyID, id := uuid.New(), uuid.New()
	mock.ExpectQuery("SELECT.*FROM tag_values").WillReturnRows(tagValueRow(id, keyID, "tokyo"))
	got, err := q.ListTagValues(context.Background(), keyID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "tokyo", got[0].Value)
}

func TestCreateTagValue(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	keyID, id := uuid.New(), uuid.New()
	mock.ExpectQuery("INSERT INTO tag_values").WillReturnRows(tagValueRow(id, keyID, "osaka"))
	got, err := q.CreateTagValue(context.Background(), keyID, "osaka")
	require.NoError(t, err)
	assert.Equal(t, "osaka", got.Value)
}

func TestDeleteTagValue(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("DELETE FROM tag_values").WillReturnResult(sqlmock.NewResult(0, 1))
	rows, err := q.DeleteTagValue(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Equal(t, int64(1), rows)
}

func TestListTagKeys_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("SELECT.*FROM tag_keys").WillReturnError(assert.AnError)
	_, err := q.ListTagKeys(context.Background(), uuid.New())
	require.Error(t, err)
}

func TestListTagValues_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("SELECT.*FROM tag_values").WillReturnError(assert.AnError)
	_, err := q.ListTagValues(context.Background(), uuid.New())
	require.Error(t, err)
}

func TestCreateTagKey_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("INSERT INTO tag_keys").WillReturnError(assert.AnError)
	_, err := q.CreateTagKey(context.Background(), CreateTagKeyParams{OrgID: uuid.New(), Key: "x", Label: "X"})
	require.Error(t, err)
}

func TestDeleteTagKey_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("DELETE FROM tag_keys").WillReturnError(assert.AnError)
	_, err := q.DeleteTagKey(context.Background(), uuid.New(), "x")
	require.Error(t, err)
}
