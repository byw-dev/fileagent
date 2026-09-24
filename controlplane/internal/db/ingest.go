package db

import (
	"context"
	"database/sql"
	"errors"
)

// UpsertObservedFile returns the current row even when the ordering guard
// suppresses an update. Suppressed writes must not trigger tags or events.
func (q *Queries) UpsertObservedFile(ctx context.Context, arg UpsertIndexedFileParams) (*FileEntry, bool, error) {
	entry, err := q.UpsertIndexedFile(ctx, arg)
	if !errors.Is(err, sql.ErrNoRows) {
		return entry, false, err
	}
	entry, err = q.GetIndexedFileByKey(ctx, arg.BucketID, arg.StoragePath)
	return entry, true, err
}

// MarkObservedFileDeleted distinguishes stale deletions from absent objects.
// Neither case is an error; callers must skip events unless a write was applied.
func (q *Queries) MarkObservedFileDeleted(ctx context.Context, arg DeleteIndexedFileParams) (*FileEntry, bool, bool, error) {
	entry, err := q.DeleteIndexedFile(ctx, arg)
	if !errors.Is(err, sql.ErrNoRows) {
		return entry, false, err == nil, err
	}
	entry, err = q.GetIndexedFileByKey(ctx, arg.BucketID, arg.StoragePath)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, false, nil
	}
	return entry, err == nil, err == nil, err
}

// UpsertDeadLetter stores (or refreshes) one webhook dead letter (IC-4a).
// Upsert by dedup key: a re-drowned event refreshes its row.
func (q *Queries) UpsertDeadLetterWrap(ctx context.Context, arg UpsertDeadLetterParams) error {
	_, err := q.UpsertDeadLetter(ctx, arg)
	return err
}

// DeleteDeadLetter removes a dead letter row after a successful redrive
// (operator replay flow, §3.6). Returns whether a row was deleted.
func (q *Queries) DeleteDeadLetterWrap(ctx context.Context, dedupKey string) (bool, error) {
	rows, err := q.DeleteDeadLetter(ctx, dedupKey)
	return rows > 0, err
}
