package indexer

import (
	"context"
	"path/filepath"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
)

// Classifier matches a file storage path against file_type_rules to determine
// its file type.
type Classifier struct {
	db db.DBTX
}

// NewClassifier creates a new Classifier backed by the given DBTX.
func NewClassifier(dbtx db.DBTX) *Classifier {
	return &Classifier{db: dbtx}
}

// Classify returns the file_type_id for a given storage path by matching
// file_type_rules in priority order. Returns uuid.Nil if no rule matches.
func (c *Classifier) Classify(ctx context.Context, storagePath string) (uuid.UUID, error) {
	rules, err := ListFileTypeRules(ctx, c.db)
	if err != nil {
		return uuid.Nil, err
	}

	name := filepath.Base(storagePath)
	for _, rule := range rules {
		matched, err := filepath.Match(rule.PathPattern, storagePath)
		if err == nil && matched {
			return rule.FileTypeID, nil
		}
		// Also try matching just the filename.
		matched, err = filepath.Match(rule.PathPattern, name)
		if err == nil && matched {
			return rule.FileTypeID, nil
		}
	}
	return uuid.Nil, nil
}
