//go:build integration

package db

// storage_event_migration_test.go — migration 000011 acceptance (B3-PR3b, D3/D4):
//
//   - file_entries.source CHECK extended to accept 'storage_readback' (and to
//     keep rejecting unknown values);
//   - storage_event_cursors / storage_event_marks tables exist with the shape
//     the sqlc queries are generated against;
//   - the down migration reverts 000011 exactly, and the up migration applies
//     again cleanly afterwards (up/down roundtrip).
//
// The test builds a THROWAWAY database per run (created/dropped via the admin
// connection) so it never depends on or disturbs the state of the shared test
// database other integration tests use.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/byw-dev/fileagent/controlplane/migrations"
)

// migrateTestDB applies every up-migration (or steps the given delta) on dsn
// using the embedded migrations.FS — the same source production uses.
func migrateTestDB(t *testing.T, dsn string, steps int) {
	t.Helper()
	source, err := iofs.New(migrations.FS, ".")
	require.NoError(t, err)
	m, err := migrate.NewWithSourceInstance("iofs", source, dsn)
	require.NoError(t, err)
	defer func() {
		srcErr, dbErr := m.Close()
		require.NoError(t, srcErr)
		require.NoError(t, dbErr)
	}()
	if steps >= 0 {
		require.NoError(t, m.Up())
		return
	}
	require.NoError(t, m.Steps(steps))
}

// testAdminDSN returns a DSN to the administrative "postgres" database on the
// same server as TEST_DATABASE_URL, for CREATE/DROP DATABASE.
func testAdminDSN(t *testing.T) string {
	t.Helper()
	dsn := testDSN(t)
	i := strings.LastIndex(dsn, "/")
	require.Positive(t, i, "TEST_DATABASE_URL must contain a database path")
	return dsn[:i+1] + "postgres" + dsn[strings.IndexAny(dsn[i:], "?")+i:]
}

// storageEventTestDB creates a uniquely-named database, migrates it fully up,
// and returns its DSN; the database is dropped when the test ends.
func storageEventTestDB(t *testing.T) string {
	t.Helper()
	admin, err := sql.Open("postgres", testAdminDSN(t))
	require.NoError(t, err)
	name := "fa_pr3b_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:16]
	_, err = admin.ExecContext(context.Background(), fmt.Sprintf("CREATE DATABASE %s", name))
	require.NoError(t, err)
	// F5: the DROP must run on the STILL-OPEN admin connection and complete
	// before it is closed — the previous version deferred admin.Close() at
	// helper-return time, so the cleanup's DROP hit "sql: database is closed"
	// and the error was swallowed, leaking one migrated database per test run.
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.Background(),
			fmt.Sprintf("DROP DATABASE %s WITH (FORCE)", name))
		if err != nil {
			t.Errorf("drop throwaway database %s: %v", name, err)
		}
		if cerr := admin.Close(); cerr != nil {
			t.Errorf("close admin connection after dropping %s: %v", name, cerr)
		}
	})
	// Swap the database name in TEST_DATABASE_URL for the throwaway one:
	// everything up to the last "/" is preserved (scheme, credentials, host),
	// the query string after "?" is preserved (sslmode).
	base := testDSN(t)
	query := ""
	if i := strings.Index(base, "?"); i >= 0 {
		query, base = base[i:], base[:i]
	}
	slash := strings.LastIndex(base, "/")
	require.Positive(t, slash, "TEST_DATABASE_URL must contain a database path")
	return base[:slash+1] + name + query
}

// migrateTestDBExpectFailure runs the migration step expecting it to FAIL with
// a server-side error containing all the given substrings (rework N3: assert
// the actual pq error message — the migrate wrapper's Error() includes the
// whole submitted SQL, so matching against it would pass regardless of whether
// the server actually raised our guard). Returns the server-side message.
func migrateTestDBExpectFailure(t *testing.T, dsn string, steps int, substrings ...string) string {
	t.Helper()
	source, err := iofs.New(migrations.FS, ".")
	require.NoError(t, err)
	m, err := migrate.NewWithSourceInstance("iofs", source, dsn)
	require.NoError(t, err)
	stepErr := m.Steps(steps)
	srcErr, dbErr := m.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)
	require.Error(t, stepErr, "the migration step must fail")

	// The server-side message arrives in the `(details: pq: …)` suffix.
	const marker = "(details: pq: "
	i := strings.Index(stepErr.Error(), marker)
	require.GreaterOrEqual(t, i, 0, "migration error must carry a pq details section: %v", stepErr)
	serverMsg := stepErr.Error()[i+len(marker):]
	for _, sub := range substrings {
		require.Contains(t, serverMsg, sub, "the SERVER error must explain the remedy (not just the wrapped SQL text)")
	}
	return serverMsg
}

// sourceCheckDef returns the CHECK constraint definition text for
// file_entries.source ("" when none exists).
func sourceCheckDef(t *testing.T, conn *sql.DB) string {
	t.Helper()
	var def string
	err := conn.QueryRowContext(context.Background(), `
		SELECT pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_class rel ON rel.oid = c.conrelid
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)
		WHERE rel.relname = 'file_entries' AND c.contype = 'c' AND a.attname = 'source'
		LIMIT 1`).Scan(&def)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	require.NoError(t, err)
	return def
}

// tryInsertSource reports the error (or nil) of inserting a file_entries row
// carrying the given source value. It first ensures the seeded default org and
// the seeded 'data-sensor' bucket exist (migrations 000001/000002) so the row
// only exercises the source CHECK, never the foreign keys.
func tryInsertSource(t *testing.T, conn *sql.DB, source string) error {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO organizations (id, name)
		VALUES ('00000000-0000-0000-0000-000000000001', 'pr3b-probe-org')
		ON CONFLICT (id) DO NOTHING`); err != nil {
		require.NoError(t, err, "seed org")
	}
	var bucketID string
	err := conn.QueryRowContext(ctx, `SELECT id FROM buckets WHERE name = 'data-sensor' LIMIT 1`).Scan(&bucketID)
	if errors.Is(err, sql.ErrNoRows) {
		require.NoError(t, conn.QueryRowContext(ctx, `
			INSERT INTO buckets (org_id, name, description)
			VALUES ('00000000-0000-0000-0000-000000000001', 'data-sensor', 'pr3b probe')
			RETURNING id`).Scan(&bucketID), "seed probe bucket")
	} else {
		require.NoError(t, err, "lookup probe bucket")
	}
	_, err = conn.ExecContext(ctx, `
		INSERT INTO file_entries (org_id, bucket_id, storage_path, file_name, source, observed_at)
		VALUES (
			'00000000-0000-0000-0000-000000000001',
			$2,
			'probe/' || $1, 'probe', $1, now())`, source, bucketID)
	return err
}

func TestMigration000011_StorageReadbackAndEventTables(t *testing.T) {
	dsn := storageEventTestDB(t)
	migrateTestDB(t, dsn, +1)

	conn, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer conn.Close()
	ctx := context.Background()

	// ── D3: the CHECK accepts storage_readback …
	require.NoError(t, tryInsertSource(t, conn, "storage_readback"),
		"migration 000011 must extend the source CHECK to accept 'storage_readback'")
	// … still accepts the pre-existing values …
	require.NoError(t, tryInsertSource(t, conn, "minio_event"))
	// … and still rejects unknown values.
	require.Error(t, tryInsertSource(t, conn, "teleport"),
		"the extended CHECK must keep rejecting unknown source values")
	def := sourceCheckDef(t, conn)
	require.Contains(t, def, "storage_readback", "CHECK definition should name storage_readback")

	// ── D4: cursor table shape (one row per source instance, monotonic value).
	var declared int64
	err = conn.QueryRowContext(ctx, `
		INSERT INTO storage_event_cursors (source_id, declared_ts_ns)
		VALUES ('seaweedfs:test:1', 42)
		ON CONFLICT (source_id) DO UPDATE
		    SET declared_ts_ns = GREATEST(storage_event_cursors.declared_ts_ns, EXCLUDED.declared_ts_ns),
		        updated_at    = now()
		RETURNING declared_ts_ns`).Scan(&declared)
	require.NoError(t, err)
	require.Equal(t, int64(42), declared)
	// An older declared value must NOT regress the cursor (GREATEST guard).
	err = conn.QueryRowContext(ctx, `
		INSERT INTO storage_event_cursors (source_id, declared_ts_ns)
		VALUES ('seaweedfs:test:1', 7)
		ON CONFLICT (source_id) DO UPDATE
		    SET declared_ts_ns = GREATEST(storage_event_cursors.declared_ts_ns, EXCLUDED.declared_ts_ns),
		        updated_at    = now()
		RETURNING declared_ts_ns`).Scan(&declared)
	require.NoError(t, err)
	require.Equal(t, int64(42), declared, "cursor upsert must never move declared_ts_ns backwards")

	// ── D4: marks table accepts the three reasons and rejects others.
	for _, reason := range []string{"gap", "cursor_before_earliest_log", "bucket_removed"} {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO storage_event_marks (source_id, reason, bucket, from_ts_ns, to_ts_ns, detail)
			VALUES ('seaweedfs:test:1', $1, NULLIF($2,''), NULLIF($3,0), NULLIF($4,0), NULL)
			`, reason, "data-sensor", int64(1), int64(2))
		require.NoError(t, err, "reason %q must be accepted", reason)
	}
	require.Error(t, func() error {
		_, err := conn.ExecContext(ctx,
			`INSERT INTO storage_event_marks (source_id, reason) VALUES ('seaweedfs:test:1', 'alien')`)
		return err
	}(), "unknown mark reason must be rejected by CHECK")
}

func TestMigration000011_DownRevertsAndUpReapplies(t *testing.T) {
	dsn := storageEventTestDB(t)
	migrateTestDB(t, dsn, +1)

	// Down exactly one step: 000011 must revert cleanly.
	migrateTestDB(t, dsn, -1)

	conn, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer conn.Close()
	ctx := context.Background()

	// Tables are gone.
	for _, table := range []string{"storage_event_cursors", "storage_event_marks"} {
		var exists bool
		require.NoError(t, conn.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table).Scan(&exists))
		require.False(t, exists, "%s must not survive the down migration", table)
	}
	// The CHECK is back to the pre-000011 list: storage_readback rejected,
	// minio_event still accepted.
	require.Error(t, tryInsertSource(t, conn, "storage_readback"),
		"down migration must restore the source CHECK without storage_readback")
	require.NoError(t, tryInsertSource(t, conn, "minio_event"))
	require.NotContains(t, sourceCheckDef(t, conn), "storage_readback")

	// Up again: 000011 applies cleanly a second time.
	migrateTestDB(t, dsn, +1)
	require.NoError(t, tryInsertSource(t, conn, "storage_readback"))
}

// TestMigration000011_DownRejectsWhenStorageReadbackRowsExist (rework F4):
// once the migration is in use, storage_readback rows exist and a plain down
// would either fail on the restored CHECK or (worse) require silently
// rewriting provenance. The down migration must REFUSE with a clear message
// and leave the database untouched; only a database without such rows can
// roundtrip.
func TestMigration000011_DownRejectsWhenStorageReadbackRowsExist(t *testing.T) {
	dsn := storageEventTestDB(t)
	migrateTestDB(t, dsn, +1)

	conn, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, tryInsertSource(t, conn, "storage_readback"))

	// The down step must fail loudly with the SERVER-side guard message.
	migrateTestDBExpectFailure(t, dsn, -1, "storage_readback", "人工处理")

	// The database is untouched: version still 000011, tables present, the
	// offending row intact.
	var exists bool
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'storage_event_cursors')`).Scan(&exists))
	require.True(t, exists, "the failed down must leave the schema alone")
	var status, source string
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT status, source FROM file_entries WHERE source = 'storage_readback' LIMIT 1`).Scan(&status, &source))
	// tryInsertSource leaves status at the column default ('uploading'); what
	// matters is that the row (and its provenance) survived untouched.
	require.Equal(t, "uploading", status, "the row must be intact after the refused down")
}

// TestMigration000011_DirtyRecoveryAfterRefusedDown (rework N3): a failed
// guarded down leaves schema_migrations at (11, dirty=true); subsequent Ups
// are refused until the dirty flag is cleared. The documented recovery flow —
// handle the rows, `migrate force 11` to clean the dirty flag, then down —
// must be walkable end to end.
func TestMigration000011_DirtyRecoveryAfterRefusedDown(t *testing.T) {
	dsn := storageEventTestDB(t)
	migrateTestDB(t, dsn, +1)

	conn, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, tryInsertSource(t, conn, "storage_readback"))

	// The guarded down refuses (server-side message).
	serverMsg := migrateTestDBExpectFailure(t, dsn, -1, "storage_readback", "人工处理")
	t.Logf("server guard: %s", serverMsg)

	// The migration metadata is dirty: a subsequent Up is refused.
	source, err := iofs.New(migrations.FS, ".")
	require.NoError(t, err)
	m, err := migrate.NewWithSourceInstance("iofs", source, dsn)
	require.NoError(t, err)
	require.Error(t, m.Up(), "up must be refused while dirty")
	// Recovery step 3: force the version to 11 (clears the dirty flag).
	require.NoError(t, m.Force(11))
	// Recovery step 2: handle the rows (rewrite provenance to an allowed value).
	_, err = conn.ExecContext(context.Background(),
		`UPDATE file_entries SET source = 'audit' WHERE source = 'storage_readback'`)
	require.NoError(t, err)
	// Recovery step 4: down now succeeds (guard count is 0).
	require.NoError(t, m.Steps(-1))
	srcErr, dbErr := m.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)

	// The schema is back at 10: tables gone, CHECK without storage_readback.
	var exists bool
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'storage_event_cursors')`).Scan(&exists))
	require.False(t, exists, "tables must be dropped after the completed down")
	require.Error(t, tryInsertSource(t, conn, "storage_readback"), "CHECK must reject storage_readback after down")
}
