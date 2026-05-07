package bootstrap

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
)

// BucketsStore defines the database methods required by bucket bootstrap logic.
type BucketsStore interface {
	ListBuckets(ctx context.Context, orgID uuid.UUID) ([]*db.Bucket, error)
	CreateBucket(ctx context.Context, arg db.CreateBucketParams) (*db.Bucket, error)
}

// defaultBucketList is the set of bucket names that must exist after bootstrap.
// These names match the physical MinIO buckets created by deploy/scripts/init-minio.sh.
var defaultBucketList = []struct {
	name        string
	description string
}{
	{"data-sensor", "Default sensor data bucket"},
	{"tmp-uploads", "Temporary uploads staging bucket"},
}

// EnsureDefaultBuckets creates the default bucket records in the database for
// the given organisation if they do not already exist. It is safe to call on
// every startup (idempotent): buckets that already have a DB record are
// silently skipped.
func EnsureDefaultBuckets(ctx context.Context, store BucketsStore, orgID uuid.UUID) error {
	existing, err := store.ListBuckets(ctx, orgID)
	if err != nil {
		return fmt.Errorf("ensure default buckets: list buckets: %w", err)
	}

	existingNames := make(map[string]bool, len(existing))
	for _, b := range existing {
		existingNames[b.Name] = true
	}

	for _, d := range defaultBucketList {
		if existingNames[d.name] {
			continue
		}
		_, err := store.CreateBucket(ctx, db.CreateBucketParams{
			OrgID: orgID,
			Name:  d.name,
			Description: sql.NullString{
				String: d.description,
				Valid:  true,
			},
		})
		if err != nil {
			return fmt.Errorf("ensure default buckets: create bucket %q: %w", d.name, err)
		}
	}
	return nil
}
