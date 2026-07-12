package db

// Batch tagging mutations (metadata 6c, Phase 1 — MT-5b). These apply a set or
// clear of one tag key across every file matching the same predicate as
// GET /files, writing a tag_audit row per changed file. They are hand-written
// (like the read filters) because the predicate carries optional filters and a
// dynamic tag-AND clause that sqlc cannot express. The retro-tagging worker runs
// them out of band; both are idempotent (a re-run changes/audits nothing).

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// BatchTagFilter selects the file set a batch operation applies to. It mirrors
// the GET /files predicate (same optional filters + AND-combined tag predicates).
type BatchTagFilter struct {
	OrgID      uuid.UUID
	AgentID    uuid.NullUUID
	BucketID   uuid.NullUUID
	FileTypeID uuid.NullUUID
	Status     NullFileStatus
	Tags       []FileTagFilter
}

// selectionCTE writes the `sel AS (SELECT id FROM file_entries WHERE ...)` common
// table expression and appends its bind args ($1..$5 plus any tag-clause args),
// so batch set/clear share the exact selection semantics of ListFileEntries.
func selectionCTE(f BatchTagFilter, args *[]interface{}) string {
	*args = append(*args, f.OrgID, f.AgentID, f.BucketID, f.FileTypeID, f.Status)
	var b strings.Builder
	b.WriteString(`sel AS (
    SELECT id FROM file_entries
    WHERE org_id = $1
      AND ($2::UUID IS NULL OR agent_id = $2)
      AND ($3::UUID IS NULL OR bucket_id = $3)
      AND ($4::UUID IS NULL OR file_type_id = $4)
      AND ($5::file_status IS NULL OR status = $5)`)
	b.WriteString(tagFilterClause(f.Tags, args))
	b.WriteString(")")
	return b.String()
}

// BatchSetFileTagParams holds parameters for BatchSetFileTag.
type BatchSetFileTagParams struct {
	Filter      BatchTagFilter
	Key         string
	Value       string
	Source      string // file_tags.source (e.g. manual)
	Action      string // tag_audit.action (set)
	AuditSource string // tag_audit.source (e.g. manual)
	ActorUserID uuid.NullUUID
}

// BatchSetFileTag sets Key=Value on every selected file, upserting file_tags and
// writing a tag_audit row for each file whose value actually changed (a file that
// already carries Key=Value is left untouched and unaudited, so re-runs are
// no-ops). It returns the number of changed files.
func (q *Queries) BatchSetFileTag(ctx context.Context, p BatchSetFileTagParams) (int64, error) {
	args := []interface{}{}
	sel := selectionCTE(p.Filter, &args)
	args = append(args, p.Key)
	keyPos := len(args)
	args = append(args, p.Value)
	valPos := len(args)
	args = append(args, p.Source)
	srcPos := len(args)
	args = append(args, p.Action)
	actionPos := len(args)
	args = append(args, p.ActorUserID)
	actorPos := len(args)
	args = append(args, p.AuditSource)
	auditSrcPos := len(args)

	// `before` snapshots each selected file's current value for Key (CTEs read the
	// pre-update snapshot), so audit old_value is accurate and the change filter
	// (old IS DISTINCT FROM new) skips unchanged files.
	query := fmt.Sprintf(`WITH %s,
before AS (
    SELECT s.id AS fid, ft.value AS old_value
    FROM sel s
    LEFT JOIN file_tags ft ON ft.file_entry_id = s.id AND ft.key = $%d
),
upsert AS (
    INSERT INTO file_tags (file_entry_id, key, value, source)
    SELECT id, $%d, $%d, $%d FROM sel
    ON CONFLICT (file_entry_id, key) DO UPDATE SET value = EXCLUDED.value, source = EXCLUDED.source
        WHERE file_tags.value IS DISTINCT FROM EXCLUDED.value
)
INSERT INTO tag_audit (org_id, file_entry_id, key, old_value, new_value, action, actor_user_id, source)
SELECT $1, b.fid, $%d, b.old_value, $%d, $%d, $%d, $%d
FROM before b
WHERE b.old_value IS DISTINCT FROM $%d`,
		sel, keyPos, keyPos, valPos, srcPos,
		keyPos, valPos, actionPos, actorPos, auditSrcPos, valPos)

	res, err := q.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// BatchClearFileTagParams holds parameters for BatchClearFileTag.
type BatchClearFileTagParams struct {
	Filter      BatchTagFilter
	Key         string
	Action      string // tag_audit.action (clear)
	AuditSource string // tag_audit.source (e.g. manual)
	ActorUserID uuid.NullUUID
}

// BatchClearFileTag removes Key from every selected file that carries it, writing
// a tag_audit row per deleted tag. It returns the number of files cleared; a
// re-run deletes nothing and audits nothing.
func (q *Queries) BatchClearFileTag(ctx context.Context, p BatchClearFileTagParams) (int64, error) {
	args := []interface{}{}
	sel := selectionCTE(p.Filter, &args)
	args = append(args, p.Key)
	keyPos := len(args)
	args = append(args, p.Action)
	actionPos := len(args)
	args = append(args, p.ActorUserID)
	actorPos := len(args)
	args = append(args, p.AuditSource)
	auditSrcPos := len(args)

	query := fmt.Sprintf(`WITH %s,
deleted AS (
    DELETE FROM file_tags ft USING sel s
    WHERE ft.file_entry_id = s.id AND ft.key = $%d
    RETURNING ft.file_entry_id AS fid, ft.value AS old_value
)
INSERT INTO tag_audit (org_id, file_entry_id, key, old_value, new_value, action, actor_user_id, source)
SELECT $1, d.fid, $%d, d.old_value, NULL, $%d, $%d, $%d
FROM deleted d`,
		sel, keyPos, keyPos, actionPos, actorPos, auditSrcPos)

	res, err := q.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
