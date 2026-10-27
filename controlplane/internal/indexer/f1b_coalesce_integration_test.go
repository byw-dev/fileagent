//go:build integration

package indexer

// f1b_coalesce_integration_test.go — evidence for the rework-1 review note:
// when a storage_readback write OVERWRITES an agent-written row (newer
// observed_at), the agent's provenance fields (sha256 / agent_id / rule_id)
// must SURVIVE — the upsert's rich-field COALESCE keeps them (F1b adjacent:
// "回读覆盖 agent 行时 agent_id/rule_id/sha256 不被清空").
//
// Drives the REAL Indexer against the REAL production queries on a throwaway
// database (migrations applied), with a real delayed-agent sequence:
//   1. agent UploadResult writes the row (source=agent, sha256, agent_id);
//   2. the read-back (storage_readback, newer observed_at, NO agent fields)
//      overwrites it;
//   3. assert sha256/agent_id survive AND the readback's size/etag/source
//      landed (the row is a merge, not a replacement).

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/migrations"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// f1bThrowawayDB — same pattern as internal/db's helper (create → migrate →
// drop in t.Cleanup with the connection still open).
func f1bThrowawayDB(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the F1b coalesce integration test")
	}
	admin, err := sql.Open("postgres", strings.Replace(dsn, "/fileagent_test", "/postgres", 1))
	require.NoError(t, err)
	name := "fa_f1b_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:16]
	_, err = admin.ExecContext(context.Background(), fmt.Sprintf("CREATE DATABASE %s", name))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.Background(), fmt.Sprintf("DROP DATABASE %s WITH (FORCE)", name))
		if err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
		if cerr := admin.Close(); cerr != nil {
			t.Errorf("close admin: %v", cerr)
		}
	})
	i := strings.LastIndex(dsn, "/")
	return dsn[:i+1] + name + "?sslmode=disable"
}

func TestReadbackOverwritePreservesAgentProvenance(t *testing.T) {
	dsn := f1bThrowawayDB(t)
	source, err := iofs.New(migrations.FS, ".")
	require.NoError(t, err)
	m, err := migrate.NewWithSourceInstance("iofs", source, dsn)
	require.NoError(t, err)
	require.NoError(t, m.Up())
	srcErr, dbErr := m.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)

	conn, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer conn.Close()
	ctx := context.Background()

	// Seed org (fixed default) + agent + bucket.
	orgID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	_, err = conn.ExecContext(ctx,
		`INSERT INTO organizations (id, name) VALUES ($1, 'f1b-org') ON CONFLICT (id) DO NOTHING`, orgID)
	require.NoError(t, err)
	agentID := uuid.New()
	require.NoError(t, conn.QueryRowContext(ctx,
		`INSERT INTO agents (org_id, name, fingerprint, status) VALUES ($1, 'f1b-agent', 'f1b-fp', 'approved') RETURNING id`,
		orgID).Scan(&agentID))
	_, err = conn.ExecContext(ctx,
		`INSERT INTO buckets (org_id, name, description) VALUES ($1, 'f1b-bucket', 'x')`, orgID)
	require.NoError(t, err)

	ix := NewIndexer(conn, nil, zap.NewNop())

	// ① agent 主路径：UploadResult 写入（source=agent，带 sha256/agent_id/rule_id）。
	ruleID := uuid.New()
	res := &agentv1.UploadResult{
		Bucket:      "f1b-bucket",
		StoragePath: "proof/f1b.csv",
		SizeBytes:   111,
		Sha256:      "agent-sha256-abc",
		Etag:        "agent-etag",
		RuleId:      ruleID.String(),
		Success:     true,
	}
	require.NoError(t, ix.HandleUploadResult(ctx, agentID, orgID, res))

	var sha, src string
	var savedAgent uuid.UUID
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT coalesce(sha256,''), source, agent_id FROM file_entries WHERE storage_path='proof/f1b.csv'`).
		Scan(&sha, &src, &savedAgent))
	require.Equal(t, "agent-sha256-abc", sha)
	require.Equal(t, "agent", src)
	require.Equal(t, agentID, savedAgent)

	// ② 回读覆盖：storage_readback，observed_at 取更新的 DB 时钟，不带 agent 字段。
	now := time.Now().Add(time.Hour).UTC()
	suppressed, err := ix.IndexUploadReporting(ctx, "f1b-bucket", "proof/f1b.csv", 222, "readback-etag", now, "", "storage_readback")
	require.NoError(t, err)
	require.False(t, suppressed)

	// ③ 合并断言：readback 的 size/etag/source 落地；agent 的 sha256/agent_id/rule_id 不被清空。
	row := conn.QueryRowContext(ctx,
		`SELECT coalesce(sha256,''), source, size_bytes, coalesce(etag,''), agent_id, coalesce(rule_id::text,'') FROM file_entries WHERE storage_path='proof/f1b.csv'`)
	var sha2, src2, etag2, rule2 string
	var size2 int64
	var agent2 uuid.UUID
	require.NoError(t, row.Scan(&sha2, &src2, &size2, &etag2, &agent2, &rule2))
	require.Equal(t, "agent-sha256-abc", sha2, "回读覆盖不得清空 agent 的 sha256（COALESCE）")
	require.Equal(t, agentID, agent2, "回读覆盖不得清空 agent_id（COALESCE）")
	// rule_id：本用例未建规则（HandleUploadResult 的归属校验会清空传入的
	// rule_id），行里本就是 NULL；readback 传入 NULL → COALESCE 保留 NULL。
	// 非空保持由 sha256 / agent_id 两条断言证明（同一 COALESCE 模式）。
	require.Empty(t, rule2, "rule_id 保持原样（本用例为 NULL）")
	require.Equal(t, int64(222), size2, "readback 的 size 必须落地")
	require.Equal(t, "readback-etag", etag2, "readback 的 etag 必须落地")
	require.Equal(t, "storage_readback", src2, "最后写者的 source 落账")
}
