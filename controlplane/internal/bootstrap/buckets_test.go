package bootstrap

import (
	"context"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBucketsStore implements BucketsStore for unit tests.
type fakeBucketsStore struct {
	buckets      []*db.Bucket
	listErr      error
	createErr    error
	created      []db.CreateBucketParams
}

func (f *fakeBucketsStore) ListBuckets(_ context.Context, _ uuid.UUID) ([]*db.Bucket, error) {
	return f.buckets, f.listErr
}

func (f *fakeBucketsStore) CreateBucket(_ context.Context, arg db.CreateBucketParams) (*db.Bucket, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	b := &db.Bucket{
		ID:    uuid.New(),
		OrgID: arg.OrgID,
		Name:  arg.Name,
	}
	f.buckets = append(f.buckets, b)
	f.created = append(f.created, arg)
	return b, nil
}

// ── EnsureDefaultBuckets ──────────────────────────────────────────────────────

func TestEnsureDefaultBuckets_CreatesAllWhenNoneExist(t *testing.T) {
	store := &fakeBucketsStore{}
	err := EnsureDefaultBuckets(context.Background(), store, DefaultOrgID)
	require.NoError(t, err)
	assert.Len(t, store.created, len(defaultBucketList),
		"should have created all default buckets")
	createdNames := make([]string, 0, len(store.created))
	for _, c := range store.created {
		createdNames = append(createdNames, c.Name)
	}
	for _, d := range defaultBucketList {
		assert.Contains(t, createdNames, d.name)
	}
}

func TestEnsureDefaultBuckets_IdempotentWhenAllExist(t *testing.T) {
	// Pre-populate the store with all default buckets.
	existing := make([]*db.Bucket, 0, len(defaultBucketList))
	for _, d := range defaultBucketList {
		existing = append(existing, &db.Bucket{ID: uuid.New(), OrgID: DefaultOrgID, Name: d.name})
	}
	store := &fakeBucketsStore{buckets: existing}

	err := EnsureDefaultBuckets(context.Background(), store, DefaultOrgID)
	require.NoError(t, err)
	assert.Empty(t, store.created, "should not have created any bucket when all already exist")
}

func TestEnsureDefaultBuckets_CreatesOnlyMissing(t *testing.T) {
	// Only the first default bucket exists.
	store := &fakeBucketsStore{
		buckets: []*db.Bucket{
			{ID: uuid.New(), OrgID: DefaultOrgID, Name: defaultBucketList[0].name},
		},
	}
	err := EnsureDefaultBuckets(context.Background(), store, DefaultOrgID)
	require.NoError(t, err)
	assert.Len(t, store.created, len(defaultBucketList)-1,
		"should have created only the missing buckets")
	assert.Equal(t, defaultBucketList[1].name, store.created[0].Name)
}

func TestEnsureDefaultBuckets_ListErrorPropagated(t *testing.T) {
	store := &fakeBucketsStore{listErr: assert.AnError}
	err := EnsureDefaultBuckets(context.Background(), store, DefaultOrgID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list buckets")
}

func TestEnsureDefaultBuckets_CreateErrorPropagated(t *testing.T) {
	store := &fakeBucketsStore{createErr: assert.AnError}
	err := EnsureDefaultBuckets(context.Background(), store, DefaultOrgID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create bucket")
}

func TestEnsureDefaultBuckets_SetsDescription(t *testing.T) {
	store := &fakeBucketsStore{}
	err := EnsureDefaultBuckets(context.Background(), store, DefaultOrgID)
	require.NoError(t, err)
	for i, c := range store.created {
		assert.True(t, c.Description.Valid, "description should be set for bucket %d", i)
		assert.NotEmpty(t, c.Description.String, "description should be non-empty for bucket %d", i)
	}
}
