package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"testing"
	"time"
)

// TestUploadRuleOwnership exercises valid, absent, foreign and unavailable rules.
func TestUploadRuleOwnership(t *testing.T) {
	for _, name := range []string{"valid", "absent", "foreign", "db_error"} {
		t.Run(name, func(t *testing.T) {
			bucket := newBucket()
			store := &mockIndexerStore{bucket: bucket, fileEntry: newFileEntry(bucket.ID, "file"), ruleMeta: json.RawMessage(`{"static_tags":{"vendor":"other"}}`)}
			if name == "absent" {
				store.ruleMetaErr = sql.ErrNoRows
			}
			if name == "foreign" {
				store.ruleAgentID = uuid.New()
			}
			if name == "db_error" {
				store.ruleMetaErr = context.Canceled
			}
			core, logs := observer.New(zap.InfoLevel)
			ix := NewIndexerWithStore(store, newMockNATS(), zap.New(core))
			err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "file"))
			if name == "db_error" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, name == "valid", store.lastUpsert.RuleID.Valid)
			require.Equal(t, name != "valid", store.lastUpsert.MetaIncomplete)
			if name != "valid" {
				require.Empty(t, store.upsertedTags)
				require.Empty(t, store.insertedTags)
			}
			require.Equal(t, name == "foreign", logs.FilterLevelExact(zap.WarnLevel).Len() > 0)
		})
	}
}

// TestSuppressedWritesHaveNoSideEffects verifies all indexer entry points.
func TestSuppressedWritesHaveNoSideEffects(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "file")
	store := &mockIndexerStore{bucket: bucket, fileEntry: fe, deletedEntry: fe, suppressed: true, deleteSuppressed: true, ruleMeta: json.RawMessage(`{"static_tags":{"vendor":"other"}}`)}
	nats := newMockNATS()
	ix := NewIndexerWithStore(store, nats, zap.NewNop())
	ctx := context.Background()
	require.NoError(t, ix.HandleUploadResult(ctx, testAgentID, bucket.OrgID, newTagResult(uuid.New(), "file")))
	require.NoError(t, ix.IndexUpload(ctx, bucket.Name, "file", 1, "E", time.Now(), "A"))
	require.NoError(t, ix.IndexDeletion(ctx, bucket.Name, "file", time.Now(), "A"))
	require.Empty(t, store.upsertedTags)
	require.Empty(t, store.insertedTags)
	require.Empty(t, nats.published)
	require.Equal(t, uint64(3), ix.suppressedWrites.Load())
}
