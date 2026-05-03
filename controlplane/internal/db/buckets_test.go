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

var bucketColumns = []string{
	"id", "org_id", "name", "description", "policy_json", "sts_role_arn", "created_at", "updated_at",
}

func bucketRow(id, orgID uuid.UUID, name string) *sqlmock.Rows {
	now := time.Now().UTC()
	return sqlmock.NewRows(bucketColumns).AddRow(
		id.String(), orgID.String(), name, nil, nil, nil, now, now,
	)
}

// ── ListBuckets ───────────────────────────────────────────────────────────────

func TestListBuckets_ReturnsRows(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID := uuid.New()
	id := uuid.New()

	mock.ExpectQuery("SELECT.*FROM buckets").
		WillReturnRows(bucketRow(id, orgID, "data-sensor"))

	result, err := q.ListBuckets(context.Background(), orgID)
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, id, result[0].ID)
	assert.Equal(t, "data-sensor", result[0].Name)
}

func TestListBuckets_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM buckets").WillReturnError(assert.AnError)

	_, err := q.ListBuckets(context.Background(), uuid.New())
	require.Error(t, err)
}

func TestListBuckets_Empty(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM buckets").
		WillReturnRows(sqlmock.NewRows(bucketColumns))

	result, err := q.ListBuckets(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Empty(t, result)
}

// ── GetBucketByID ─────────────────────────────────────────────────────────────

func TestGetBucketByID_Found(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	id := uuid.New()
	orgID := uuid.New()

	mock.ExpectQuery("SELECT.*FROM buckets").
		WillReturnRows(bucketRow(id, orgID, "tmp-uploads"))

	got, err := q.GetBucketByID(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "tmp-uploads", got.Name)
}

func TestGetBucketByID_NotFound(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM buckets").
		WillReturnRows(sqlmock.NewRows(bucketColumns))

	_, err := q.GetBucketByID(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── CreateBucket ──────────────────────────────────────────────────────────────

func TestCreateBucket_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	id := uuid.New()
	orgID := uuid.New()

	mock.ExpectQuery("INSERT INTO buckets").
		WillReturnRows(bucketRow(id, orgID, "new-bucket"))

	got, err := q.CreateBucket(context.Background(), CreateBucketParams{
		OrgID: orgID,
		Name:  "new-bucket",
	})
	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "new-bucket", got.Name)
}

func TestCreateBucket_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("INSERT INTO buckets").WillReturnError(assert.AnError)

	_, err := q.CreateBucket(context.Background(), CreateBucketParams{
		OrgID: uuid.New(),
		Name:  "bucket",
	})
	require.Error(t, err)
}
