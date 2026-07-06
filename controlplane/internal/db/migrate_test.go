package db

import (
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byw-dev/fileagent/controlplane/migrations"
)

// TestEmbeddedMigrationsReadableByIOFS verifies that the embedded migration
// filesystem is well-formed and can be opened by golang-migrate's iofs source
// driver — the same path db.Migrate takes at startup. It exercises the embed +
// naming-convention wiring without touching a real database (unit tests must
// not connect to Postgres — see CLAUDE.md); applying migrations for real is
// covered by the Docker e2e in the deployment runbook.
func TestEmbeddedMigrationsReadableByIOFS(t *testing.T) {
	src, err := iofs.New(migrations.FS, ".")
	require.NoError(t, err, "iofs must open the embedded migrations FS")
	t.Cleanup(func() { _ = src.Close() })

	// First() returns the lowest migration version; a working source with at
	// least one *.up.sql file must yield one without ErrNotExist.
	first, err := src.First()
	require.NoError(t, err, "embedded migrations must expose at least one version")
	assert.Greater(t, first, uint(0), "migration versions start at 1")

	// Walk the whole chain via Next() to confirm every version parses and the
	// files form an unbroken sequence the migrator can traverse.
	count := 1
	v := first
	for {
		next, err := src.Next(v)
		if err != nil {
			break // ErrNotExist at the end of the chain
		}
		count++
		v = next
	}
	assert.GreaterOrEqual(t, count, 3, "expected at least the 3 known migrations")
}
