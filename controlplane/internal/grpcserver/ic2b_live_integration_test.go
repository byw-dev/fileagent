//go:build integration

package grpcserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"
)

// TestIC2BLive runs the three IC-2b acceptance criteria against real CP and
// agent binaries plus the dev compose stack:
//
//  1. rules deleted/disabled while the agent is disconnected stop producing
//     uploads after reconnection, WITHOUT restarting the agent — both paths
//     (IC-BUG-30; delete arrives only as a snapshot absence, disable as an
//     enabled=false snapshot rule);
//  2. a rule pointing at a NEW bucket, created through the REST API while the
//     agent is running, uploads its first file without any 403 — the
//     DispatchRule-driven credential re-push (IC-BUG-20);
//  3. 40 active rules all become effective after connect, with credentials
//     delivered — 40 > 32 crosses the old per-message-push buffer, the
//     snapshot form must carry them all (IC-BUG-30/31).
//
// Opt-in: IC2B_LIVE=1 with deploy/config/controlplane.env loaded, built
// binaries (make build) and the dev compose stack up. Leaves evidence rows.
func TestIC2BLive(t *testing.T) {
	if os.Getenv("IC2B_LIVE") != "1" {
		t.Skip("set IC2B_LIVE=1 with dev controlplane.env loaded and built binaries")
	}
	root, err := filepath.Abs("../../..")
	require.NoError(t, err)
	artifacts, err := os.MkdirTemp("", "ic2b-live-")
	require.NoError(t, err)
	t.Log("artifacts:", artifacts)
	conn, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	defer conn.Close()
	org := "00000000-0000-0000-0000-000000000001"
	ctx := context.Background()

	stopCP := liveProcess(t, root, filepath.Join(artifacts, "cp-1.log"), "controlplane")
	httpURL := "http://127.0.0.1:8080"
	healthy := func() bool {
		client := http.Client{Timeout: time.Second}
		r, e := client.Get(httpURL + "/healthz")
		if e != nil {
			return false
		}
		defer r.Body.Close()
		return r.StatusCode == 200
	}
	require.Eventually(t, healthy, 30*time.Second, 100*time.Millisecond)

	var dataSensor string
	require.NoError(t, conn.QueryRow("SELECT id FROM buckets WHERE name='data-sensor'").Scan(&dataSensor))

	insertRule := func(ruleID, agentID, bucketID, name, basePath string) {
		_, err := conn.Exec(`INSERT INTO collection_rules(id,org_id,agent_id,bucket_id,name,mode,base_path,path_pattern,dest_path_template,recursive,metadata)
			VALUES($1,$2,$3,$4,$5,'watch',$6,'*',$7,false,'{}')`,
			ruleID, org, agentID, bucketID, name, basePath, "/"+name+"/{filename}")
		require.NoError(t, err)
	}

	// startAgent registers an approved agent and starts the binary. The agent
	// keeps running until test cleanup; the returned log path is evidence.
	startAgent := func(name string) (agentID, logPath string) {
		agentID = uuid.NewString()
		fingerprint := uuid.NewString()
		fp := filepath.Join(artifacts, name+"-fingerprint")
		require.NoError(t, os.WriteFile(fp, []byte(fingerprint), 0600))
		dataDir := filepath.Join(artifacts, name+"-data")
		require.NoError(t, os.MkdirAll(dataDir, 0700))
		// NB: the agent's [log].output is not honoured (IC-BUG-38) — all agent
		// evidence lands on the process log (stdout).
		logPath = filepath.Join(artifacts, name+"-process.log")
		config := fmt.Sprintf("[server]\nendpoint=%q\ntls_insecure=true\n[agent]\nfingerprint_file=%q\ntoken_file=%q\ndata_dir=%q\n[upload]\nreport_timeout_seconds=1\n[metrics]\nenabled=false\n[log]\noutput=%q\nlevel=\"debug\"\n",
			"127.0.0.1:9090", fp, filepath.Join(artifacts, name+"-token"), dataDir, logPath)
		require.NoError(t, os.WriteFile(filepath.Join(artifacts, name+".toml"), []byte(config), 0600))
		_, err := conn.Exec("INSERT INTO agents(id,org_id,name,fingerprint,status) VALUES($1,$2,$3,$4,'approved')", agentID, org, name, fingerprint)
		require.NoError(t, err)
		liveProcess(t, root, filepath.Join(artifacts, name+"-process.log"), "agent", "--config", filepath.Join(artifacts, name+".toml"))
		return agentID, logPath
	}

	// waitAgentRulesApplied waits until the agent's SQLite rule table shows
	// want rules — each applied snapshot rule is upserted there, so the count
	// is direct evidence the snapshot arrived and was applied.
	waitAgentRulesApplied := func(dataDir string, want int) {
		require.Eventually(t, func() bool {
			out, e := exec.Command("sqlite3", filepath.Join(dataDir, "queue.db"),
				"SELECT count(*) FROM rules;").Output()
			return e == nil && strings.TrimSpace(string(out)) == fmt.Sprint(want)
		// 90s: the agent's first approval poll fires only 30s after start
		// (registration.go polls with a 30s interval), so connect + sync can
		// legitimately take >30s of wall clock.
		}, 90*time.Second, 500*time.Millisecond, "agent must have applied %d rules", want)
	}

	waitCredUpdates := func(logPath string, atLeast int) {
		require.Eventually(t, func() bool {
			data, e := os.ReadFile(logPath)
			return e == nil && strings.Count(string(data), "STS credentials updated") >= atLeast
		}, 90*time.Second, 500*time.Millisecond)
	}

	readLog := func(logPath string) string {
		data, err := os.ReadFile(logPath)
		require.NoError(t, err)
		return string(data)
	}

	// ── Act 1: disconnect-window delete + disable (IC-BUG-30) ────────────────
	agentA, logA := startAgent("agent-a")
	dir1 := filepath.Join(artifacts, "in1")
	dir2 := filepath.Join(artifacts, "in2")
	for _, d := range []string{dir1, dir2} {
		require.NoError(t, os.MkdirAll(d, 0700))
	}
	rule1, rule2 := uuid.NewString(), uuid.NewString()
	insertRule(rule1, agentA, dataSensor, "ic2b-del", dir1)
	insertRule(rule2, agentA, dataSensor, "ic2b-dis", dir2)
	waitAgentRulesApplied(filepath.Join(artifacts, "agent-a-data"), 2)

	// Sanity: one upload proves the initial sync + credentials work.
	require.NoError(t, os.WriteFile(filepath.Join(dir1, "before.csv"), []byte("a\n"), 0600))
	var n int
	require.Eventually(t, func() bool {
		return conn.QueryRow("SELECT count(*) FROM file_entries WHERE bucket_id=$1 AND storage_path=$2",
			dataSensor, "ic2b-del/before.csv").Scan(&n) == nil && n == 1
	}, 60*time.Second, 500*time.Millisecond, "sanity upload before disconnection")

	// Disconnect by stopping the CP; mutate rules behind the agent's back.
	// NB: the sanity upload left a file_entries row referencing the rule, and
	// the FK blocks rule deletion — clear the scratch row first (the real
	// REST delete path would hit the same constraint; pre-existing product
	// behaviour, out of IC-2b's scope).
	stopCP()
	time.Sleep(2 * time.Second)
	_, err = conn.Exec("DELETE FROM upload_logs WHERE file_entry_id IN (SELECT id FROM file_entries WHERE rule_id=$1)", rule1)
	require.NoError(t, err)
	_, err = conn.Exec("DELETE FROM file_tags WHERE file_entry_id IN (SELECT id FROM file_entries WHERE rule_id=$1)", rule1)
	require.NoError(t, err)
	_, err = conn.Exec("DELETE FROM file_entries WHERE rule_id=$1", rule1)
	require.NoError(t, err)
	_, err = conn.Exec("DELETE FROM collection_rules WHERE id=$1", rule1)
	require.NoError(t, err)
	_, err = conn.Exec("UPDATE collection_rules SET status='inactive' WHERE id=$1", rule2)
	require.NoError(t, err)
	stopCP = liveProcess(t, root, filepath.Join(artifacts, "cp-2.log"), "controlplane")
	require.Eventually(t, healthy, 30*time.Second, 100*time.Millisecond)

	// Wait for the agent to reconnect AND resync: the deletion is only
	// observable as the agent stopping the rule absent from the snapshot.
	require.Eventually(t, func() bool {
		return strings.Contains(readLog(logA), "stopping rule outside the synced full set")
	}, 90*time.Second, 500*time.Millisecond, "the agent must have resynced and stopped the deleted rule")

	// Files dropped AFTER the resync must produce no uploads.
	require.NoError(t, os.WriteFile(filepath.Join(dir1, "deleted-while-offline.csv"), []byte("a\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir2, "disabled-while-offline.csv"), []byte("a\n"), 0600))
	require.Never(t, func() bool {
		var c int
		e := conn.QueryRow(`SELECT count(*) FROM file_entries WHERE storage_path IN ($1,$2)`,
			"ic2b-del/deleted-while-offline.csv", "ic2b-dis/disabled-while-offline.csv").Scan(&c)
		return e == nil && c > 0
	}, 8*time.Second, time.Second,
		"uploads from a deleted/disabled rule must stop without an agent restart")
	require.Equal(t, 1, strings.Count(readLog(logA), "agent: running"),
		"the agent must NOT have been restarted during this act")
	t.Log("PASS act 1: delete + disable while disconnected, no agent restart, no uploads")

	// ── Act 2: new bucket rule created while running (IC-BUG-20) ─────────────
	bucketB := "ic2b-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	minioClient, err := minio.New("localhost:9000", &minio.Options{
		Creds: credentials.NewStaticV4("minioadmin", "minioadmin", ""), Secure: false,
	})
	require.NoError(t, err)
	require.NoError(t, minioClient.MakeBucket(ctx, bucketB, minio.MakeBucketOptions{}))
	bucketBID := uuid.NewString()
	_, err = conn.Exec("INSERT INTO buckets(id,org_id,name) VALUES($1,$2,$3)", bucketBID, org, bucketB)
	require.NoError(t, err)

	token := restLogin(t, httpURL)
	dir3 := filepath.Join(artifacts, "in3")
	require.NoError(t, os.MkdirAll(dir3, 0700))
	rule3 := restCreateRule(t, httpURL, token, agentA, bucketBID, "ic2b-newbucket", dir3)

	// The DispatchRule-driven credentials re-push must have arrived (the
	// initial connect push was the first one).
	waitCredUpdates(logA, 2)

	require.NoError(t, os.WriteFile(filepath.Join(dir3, "first.csv"), []byte("a,b\n1,2\n"), 0600))
	require.Eventually(t, func() bool {
		return conn.QueryRow("SELECT count(*) FROM file_entries WHERE bucket_id=$1 AND storage_path=$2",
			bucketBID, "ic2b-newbucket/first.csv").Scan(&n) == nil && n == 1
	}, 60*time.Second, 500*time.Millisecond, "first file into the new bucket must succeed")
	require.NotContains(t, readLog(logA), "upload denied",
		"the first upload must not 403 — the credential re-push covers the new bucket")
	t.Log("PASS act 2: new-bucket rule created while running, first file uploaded without 403")

	// ── Act 3: 40 active rules all effective after connect (IC-BUG-30/31) ────
	agentB, logB := startAgent("agent-b")
	ruleIDs := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		dir := filepath.Join(artifacts, fmt.Sprintf("b3-%02d", i))
		require.NoError(t, os.MkdirAll(dir, 0700))
		id := uuid.NewString()
		insertRule(id, agentB, dataSensor, fmt.Sprintf("ic2b-b3-%02d", i), dir)
		ruleIDs = append(ruleIDs, id)
	}
	waitAgentRulesApplied(filepath.Join(artifacts, "agent-b-data"), 40)
	waitCredUpdates(logB, 1)

	// Drop the stimulus files only after the snapshot has been applied — the
	// fsnotify path has no initial scan (IC-BUG-37), files dropped before a
	// rule runs are never collected.
	for i := 0; i < 40; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(artifacts, fmt.Sprintf("b3-%02d", i), "f.csv"), []byte("x\n"), 0600))
	}
	require.Eventually(t, func() bool {
		var c int
		e := conn.QueryRow(`SELECT count(*) FROM file_entries fe
			JOIN collection_rules cr ON cr.id = fe.rule_id
			WHERE cr.agent_id=$1`, agentB).Scan(&c)
		return e == nil && c == 40
	}, 120*time.Second, time.Second, "all 40 rules must upload their file")
	t.Log("PASS act 3: 40 active rules via one snapshot message, credentials delivered")

	evidence := fmt.Sprintf("act1 del/dis without restart: rules=%s(deleted) %s(disabled) PASS\nact2 new bucket first file: bucket=%s rule=%s PASS\nact3 40 rules: PASS\n",
		rule1, rule2, bucketB, rule3)
	require.NoError(t, os.WriteFile(filepath.Join(artifacts, "evidence.txt"), []byte(evidence), 0600))
}

// restLogin exchanges the bootstrap admin credentials for an access token.
func restLogin(t *testing.T, httpURL string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "DevAdmin@2026"})
	resp, err := http.Post(httpURL+"/api/auth/login", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "admin login failed")
	var out struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.NotEmpty(t, out.AccessToken)
	return out.AccessToken
}

// restCreateRule creates a rule through the REST API — the real dispatch path
// that triggers DispatchRule and, with IC-BUG-20, the credential re-push.
func restCreateRule(t *testing.T, httpURL, token, agentID, bucketID, name, basePath string) (ruleID string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"bucket_id":          bucketID,
		"name":               name,
		"mode":               "watch",
		"base_path":          basePath,
		"path_pattern":       "*",
		"dest_path_template": "/" + name + "/{filename}",
		"recursive":          false,
	})
	req, err := http.NewRequest(http.MethodPost, httpURL+"/api/v1/agents/"+agentID+"/rules", bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "create rule failed: %s", resp.Status)
	var out struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out.ID
}
