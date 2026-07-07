//go:build integration

// Package db integration tests require a running PostgreSQL instance.
// Start the test environment with:
//
//	docker compose -f deploy/docker-compose.test.yml up -d
//
// Then run:
//
//	cd controlplane && go test ./internal/db/... -tags=integration
package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/byw-dev/fileagent/controlplane/migrations"
)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://fileagent:fileagent@localhost:5432/fileagent_test?sslmode=disable"
	}
	return dsn
}

func testLogger(t *testing.T) *zap.Logger {
	t.Helper()
	l, _ := zap.NewDevelopment()
	return l
}

func TestNew_ConnectsSuccessfully(t *testing.T) {
	dsn := testDSN(t)
	logger := testLogger(t)
	ctx := context.Background()

	db, err := Open(ctx, dsn, logger)
	require.NoError(t, err)
	defer db.Close()

	assert.NoError(t, db.Ping(ctx))
}

func TestMigrate_IdempotentOnCleanDB(t *testing.T) {
	dsn := testDSN(t)
	logger := testLogger(t)

	// First run: apply all migrations from the embedded FS (same source the
	// server uses in production).
	err := Migrate(dsn, migrations.FS, logger)
	require.NoError(t, err)

	// Second run: should be a no-op (ErrNoChange handled gracefully).
	err = Migrate(dsn, migrations.FS, logger)
	require.NoError(t, err)
}

func TestQueriesRoundtrip_Agent(t *testing.T) {
	dsn := testDSN(t)
	logger := testLogger(t)
	ctx := context.Background()

	db, err := Open(ctx, dsn, logger)
	require.NoError(t, err)
	defer db.Close()

	q := db.Queries()

	// The migration inserts the default org with a fixed UUID.
	defaultOrgID := uuid.MustParse("00000000-0000-0000-0000-000000000001")

	params := CreateAgentParams{
		OrgID:       defaultOrgID,
		Name:        "test-agent",
		Fingerprint: "fp-test-" + t.Name(),
		OsInfo:      []byte(`{"os":"linux","arch":"amd64"}`),
		// IpAddress left as zero value (Valid=false → NULL in PostgreSQL).
	}
	agent, err := q.CreateAgent(ctx, params)
	require.NoError(t, err)
	assert.Equal(t, "test-agent", agent.Name)
	assert.Equal(t, AgentStatusPending, agent.Status)

	// GetByID
	got, err := q.GetAgentByID(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, agent.ID, got.ID)

	// GetByFingerprint
	got2, err := q.GetAgentByFingerprint(ctx, params.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, agent.ID, got2.ID)

	// UpdateStatus — sqlc generates positional args for ≤4 params.
	updated, err := q.UpdateAgentStatus(ctx, agent.ID, AgentStatusApproved)
	require.NoError(t, err)
	assert.Equal(t, AgentStatusApproved, updated.Status)

	// Cleanup
	require.NoError(t, q.DeleteAgent(ctx, agent.ID))
}
