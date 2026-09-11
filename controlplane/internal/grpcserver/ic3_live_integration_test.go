//go:build integration

package grpcserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"
)

// IC-3 live acceptance (env switch: IC3_LIVE=1 — new in this knife; run with
// deploy/config/controlplane.env sourced, dev compose up, and `make build`
// outputs present). It covers acceptance 1 and 2:
//
//   1. >64MB file, agent SIGKILLed mid-multipart, restart resumes the SAME
//      upload (log shows the skipped part count; upload ID unchanged; not a
//      restart from part 1).
//   2. A task given up on (retry_max=0, source file removed mid-upload)
//      leaves no incomplete multipart upload in MinIO — the executor abandon
//      hook aborted it.
//
// Acceptance 3 (ILM AbortIncompleteMultipartUpload effective) is asserted by
// init-minio.sh's own read-back self-check, not here: every current MinIO
// release silently strips the action (upstream FIXME, see the script), so a
// green test here would be a lie on any deployable MinIO. Acceptance 4
// (IC-BUG-39) is the controlplane unit test TestSessionPolicyMatchesInitScript.

// ic3ProgressRow reads one task's multipart state back from the agent's real
// SQLite queue.
func ic3ProgressRow(t *testing.T, dbPath, localPath string) (uploadID, completedParts string, ok bool) {
	t.Helper()
	out, err := exec.Command("sqlite3", dbPath,
		"SELECT coalesce(upload_id,''), coalesce(completed_parts,'') FROM upload_tasks WHERE local_path='"+localPath+"';").Output()
	if err != nil {
		return "", "", false
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", "", false
	}
	parts := strings.SplitN(line, "|", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ic3CountParts parses the persisted completed-parts JSON blob.
func ic3CountParts(raw string) int {
	if raw == "" {
		return 0
	}
	var doc struct {
		Parts []map[string]any `json:"parts"`
	}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return 0
	}
	return len(doc.Parts)
}

// ic3Spawn starts a binary and returns the raw command; the test decides
// between graceful stop and SIGKILL (IC-3's kill-mid-transfer is a SIGKILL —
// a graceful stop would mask the crash-resume path under test).
func ic3Spawn(t *testing.T, root, logPath, binary string, args ...string) *exec.Cmd {
	t.Helper()
	out, err := os.Create(logPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = out.Close() })
	cmd := exec.Command(filepath.Join(root, "bin", binary), args...)
	cmd.Dir = root
	cmd.Stdout = out
	cmd.Stderr = out
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	return cmd
}

// ic3IncompleteUploads lists incomplete multipart uploads of bucket under
// prefix, using root credentials (the check must not depend on agent STS).
func ic3IncompleteUploads(t *testing.T, core *miniogo.Core, bucket, prefix string) []miniogo.ObjectMultipartInfo {
	t.Helper()
	res, err := core.ListMultipartUploads(context.Background(), bucket, prefix, "", "", "", 100)
	require.NoError(t, err)
	return res.Uploads
}

// ic3ObjectExists stats the object with root credentials.
func ic3ObjectExists(ctx context.Context, bucket, key string) (bool, int64) {
	mc, err := miniogo.New("localhost:9000", &miniogo.Options{
		Creds: credentials.NewStaticV4("minioadmin", "minioadmin", ""), Secure: false,
	})
	if err != nil {
		return false, 0
	}
	info, err := mc.StatObject(ctx, bucket, key, miniogo.StatObjectOptions{})
	if err != nil {
		return false, 0
	}
	return true, info.Size
}

// ic3WriteBigFile writes a deterministic file of the given size.
func ic3WriteBigFile(t *testing.T, path string, size int) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = byte('0' + i%10)
	}
	written := 0
	for written < size {
		n := len(chunk)
		if rem := size - written; rem < n {
			n = rem
		}
		w, err := f.Write(chunk[:n])
		require.NoError(t, err)
		written += w
	}
}

// ic3AgentConfig builds the agent TOML for one IC-3 scenario agent and writes
// its fingerprint file. Distinct scenarios get distinct processes so per-
// process knobs (retry_max) never leak between scenarios.
func ic3AgentConfig(t *testing.T, artifacts string, partSizeMB, retryMax int) (configPath, fingerprintPath string) {
	t.Helper()
	fingerprint := uuid.NewString()
	fingerprintPath = filepath.Join(artifacts, "fingerprint-"+fingerprint)
	require.NoError(t, os.WriteFile(fingerprintPath, []byte(fingerprint), 0o600))
	config := fmt.Sprintf(
		"[server]\nendpoint=%q\ntls_insecure=true\n[agent]\nfingerprint_file=%q\ntoken_file=%q\ndata_dir=%q\n[upload]\npart_size_mb=%d\nretry_max=%d\nreport_timeout_seconds=1\n[metrics]\nenabled=false\n[log]\noutput=%q\nlevel=\"debug\"\n",
		"127.0.0.1:9090",
		fingerprintPath,
		filepath.Join(artifacts, "token-"+fingerprint),
		filepath.Join(artifacts, "data-"+fingerprint),
		partSizeMB,
		retryMax,
		filepath.Join(artifacts, "agent-"+fingerprint+".json.log"),
	)
	dataDir := filepath.Join(artifacts, "data-"+fingerprint)
	require.NoError(t, os.MkdirAll(dataDir, 0o700), "the agent does not create its data_dir itself")

	configPath = filepath.Join(artifacts, "agent-"+fingerprint+".toml")
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))
	return configPath, fingerprintPath
}

// ic3RegisterScenario creates the approved agent row and its watch rule.
func ic3RegisterScenario(t *testing.T, conn *sql.DB, agentID, ruleID, fingerprint, basePath, destTemplate string) {
	t.Helper()
	org := "00000000-0000-0000-0000-000000000001"
	_, err := conn.Exec("INSERT INTO agents(id,org_id,name,fingerprint,status) VALUES($1,$2,$3,$4,'approved')",
		agentID, org, "ic3-"+agentID[:8], fingerprint)
	require.NoError(t, err)
	var bucketID string
	require.NoError(t, conn.QueryRow("SELECT id FROM buckets WHERE name='data-sensor'").Scan(&bucketID))
	_, err = conn.Exec(`INSERT INTO collection_rules(id,org_id,agent_id,bucket_id,name,mode,base_path,path_pattern,dest_path_template,recursive,metadata)
		VALUES($1,$2,$3,$4,$5,'watch',$6,'{filename}',$7,true,'{}')`,
		ruleID, org, agentID, bucketID, "ic3-"+ruleID[:8], basePath, destTemplate)
	require.NoError(t, err)
}

// ic3AgentLog reports whether the agent process log contains needle.
func ic3AgentLog(logPath, needle string) bool {
	data, err := os.ReadFile(logPath)
	return err == nil && strings.Contains(string(data), needle)
}

// ic3LogIntField extracts an integer field value from a zap JSON log file.
func ic3LogIntField(data []byte, field string) int {
	m := regexp.MustCompile(`"` + field + `"\s*:\s*(\d+)`).FindSubmatch(data)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0
	}
	return n
}

const (
	ic3FileSize = 320 << 20 // 320 MiB → 64 parts at part_size_mb=5 (S3 min part size)
	ic3Bucket   = "data-sensor"
	ic3Attempts = 4
)

func TestIC3MultipartResumeLive(t *testing.T) {
	if os.Getenv("IC3_LIVE") != "1" {
		t.Skip("set IC3_LIVE=1 with dev controlplane.env loaded, dev compose up and built binaries")
	}
	ctx := context.Background()
	root, err := filepath.Abs("../../..")
	require.NoError(t, err)
	artifacts, err := os.MkdirTemp("", "ic3-live-")
	require.NoError(t, err)
	t.Log("artifacts:", artifacts)

	// ── Control Plane + clients ──────────────────────────────────────────────
	stopCP := liveProcess(t, root, filepath.Join(artifacts, "cp.log"), "controlplane")
	defer stopCP()
	require.Eventually(t, func() bool {
		out, e := exec.Command("curl", "--silent", "--output", "/dev/null",
			"--write-out", "%{http_code}", "http://127.0.0.1:8080/healthz").Output()
		return e == nil && strings.TrimSpace(string(out)) == "200"
	}, 30*time.Second, 200*time.Millisecond)

	conn, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	defer conn.Close()

	core, err := miniogo.NewCore("localhost:9000", &miniogo.Options{
		Creds: credentials.NewStaticV4("minioadmin", "minioadmin", ""), Secure: false,
	})
	require.NoError(t, err)

	// ── Scenario 1: SIGKILL mid-multipart, restart must resume ───────────────
	prefixA := "ic3-resume-" + uuid.NewString()
	inputA := filepath.Join(artifacts, "inputA")
	require.NoError(t, os.MkdirAll(inputA, 0o700))
	agentA, ruleA := uuid.NewString(), uuid.NewString()
	configA, fpA := ic3AgentConfig(t, artifacts, 5, 10)
	fingerprintA, err := os.ReadFile(fpA)
	require.NoError(t, err)
	ic3RegisterScenario(t, conn, agentA, ruleA, strings.TrimSpace(string(fingerprintA)), inputA, "/"+prefixA+"/{filename}")

	fileA := filepath.Join(inputA, "bigA.bin")
	ic3WriteBigFile(t, fileA, ic3FileSize)
	keyA := prefixA + "/bigA.bin"
	queueA := filepath.Join(artifacts, "data-"+strings.TrimSpace(string(fingerprintA)), "queue.db")

	// The scenario is a race (loopback MinIO is fast): if the upload wins, the
	// file is rewritten with a new mtime and the attempt repeats.
	killedUploadID := ""
	resumeVerified := false
	for attempt := 1; attempt <= ic3Attempts && !resumeVerified; attempt++ {
		cmd := ic3Spawn(t, root, filepath.Join(artifacts, fmt.Sprintf("agentA-process-%d.log", attempt)), "agent", "--config", configA)
		if attempt > 1 {
			// Fresh file version (new mtime → new task, dedup guard permits).
			ic3WriteBigFile(t, fileA, ic3FileSize)
		}

		killed := false
		for i := 0; i < 4000; i++ {
			uploadID, parts, ok := ic3ProgressRow(t, queueA, fileA)
			if ok && uploadID != "" && ic3CountParts(parts) >= 1 {
				require.NoError(t, cmd.Process.Kill(), "SIGKILL agent mid-upload")
				_, _ = cmd.Process.Wait()
				killedUploadID = uploadID
				t.Logf("attempt %d: killed agent mid-upload upload_id=%s parts_done=%d", attempt, uploadID, ic3CountParts(parts))
				killed = true
				break
			}
			if exists, _ := ic3ObjectExists(ctx, ic3Bucket, keyA); exists {
				t.Logf("attempt %d: upload completed before the kill; retrying with a fresh file version", attempt)
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !killed {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			continue
		}

		// Restart the SAME agent (same data dir, same token) and let it resume.
		time.Sleep(500 * time.Millisecond)
		restartLog := filepath.Join(artifacts, fmt.Sprintf("agentA-resume-%d.log", attempt))
		ic3Spawn(t, root, restartLog, "agent", "--config", configA)

		require.Eventually(t, func() bool {
			return ic3AgentLog(restartLog, "resuming multipart upload")
		}, 30*time.Second, 50*time.Millisecond, "restart must log the resume evidence line")

		var size int64
		require.Eventually(t, func() bool {
			exists, s := ic3ObjectExists(ctx, ic3Bucket, keyA)
			size = s
			return exists && s == ic3FileSize
		}, 120*time.Second, 250*time.Millisecond, "resumed upload must produce the full object")

		data, err := os.ReadFile(restartLog)
		require.NoError(t, err)
		require.True(t, strings.Contains(string(data), killedUploadID),
			"the resumed upload must be the upload ID recorded before the kill")
		skipped := ic3LogIntField(data, "skipped_parts")
		require.GreaterOrEqual(t, skipped, 1,
			"the resume log must show skipped parts (IC-3 acceptance: not from part 1)")
		t.Logf("attempt %d: resumed upload_id=%s skipped_parts=%d object_size=%d", attempt, killedUploadID, skipped, size)

		resumeVerified = true
	}
	require.True(t, resumeVerified, "resume scenario did not verify")

	// End-to-end: the completed object is indexed by the webhook path.
	require.Eventually(t, func() bool {
		var n int
		_ = conn.QueryRow("SELECT count(*) FROM file_entries WHERE storage_path=$1", keyA).Scan(&n)
		return n == 1
	}, 45*time.Second, 250*time.Millisecond, "file_entries must contain exactly one row for the resumed object")

	// ── Scenario 2: given-up task must leave no incomplete upload ────────────
	prefixB := "ic3-abandon-" + uuid.NewString()
	inputB := filepath.Join(artifacts, "inputB")
	require.NoError(t, os.MkdirAll(inputB, 0o700))
	agentB, ruleB := uuid.NewString(), uuid.NewString()
	// retry_max=0 → the first upload failure is terminal (give-up + abort).
	configB, fpB := ic3AgentConfig(t, artifacts, 5, 0)
	fingerprintB, err := os.ReadFile(fpB)
	require.NoError(t, err)
	ic3RegisterScenario(t, conn, agentB, ruleB, strings.TrimSpace(string(fingerprintB)), inputB, "/"+prefixB+"/{filename}")

	fileB := filepath.Join(inputB, "bigB.bin")
	ic3WriteBigFile(t, fileB, ic3FileSize)
	keyB := prefixB + "/bigB.bin"
	dataDirB := filepath.Join(artifacts, "data-"+strings.TrimSpace(string(fingerprintB)), "queue.db")
	giveUpLog := filepath.Join(artifacts, "agentB-process.log")

	abandonedUploadID := ""
	for attempt := 1; attempt <= ic3Attempts; attempt++ {
		ic3WriteBigFile(t, fileB, ic3FileSize) // fresh mtime on retries
		cmd := ic3Spawn(t, root, giveUpLog, "agent", "--config", configB)

		// Wait for ≥1 durably completed part, then remove the source file: the
		// next part's open fails, and with retry_max=0 the executor gives the
		// task up immediately — which must abort the in-flight upload.
		aborted := false
		for i := 0; i < 4000 && !aborted; i++ {
			uploadID, parts, ok := ic3ProgressRow(t, dataDirB, fileB)
			if ok && uploadID != "" && ic3CountParts(parts) >= 1 {
				require.NoError(t, os.Remove(fileB), "remove source file mid-upload")
				abandonedUploadID = uploadID
				t.Logf("attempt %d: removed source mid-upload upload_id=%s parts_done=%d", attempt, uploadID, ic3CountParts(parts))
				aborted = true
				break
			}
			if exists, _ := ic3ObjectExists(ctx, ic3Bucket, keyB); exists {
				t.Logf("attempt %d: upload won the race; retrying with a fresh file version", attempt)
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !aborted {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			continue
		}

		// The agent must log the abandonment AND the abort must leave nothing.
		require.Eventually(t, func() bool {
			return ic3AgentLog(giveUpLog, "aborted multipart upload of abandoned task")
		}, 30*time.Second, 100*time.Millisecond,
			"give-up must abort the in-flight multipart upload (log evidence missing)")

		require.Eventually(t, func() bool {
			return len(ic3IncompleteUploads(t, core, ic3Bucket, prefixB)) == 0
		}, 30*time.Second, 200*time.Millisecond,
			"MinIO must hold no incomplete multipart upload for the abandoned task")
		exists, _ := ic3ObjectExists(ctx, ic3Bucket, keyB)
		require.False(t, exists, "a given-up task must not have produced an object")
		t.Logf("attempt %d: abandoned task upload_id=%s fully cleaned from MinIO", attempt, abandonedUploadID)
		break
	}
	require.NotEmpty(t, abandonedUploadID, "give-up scenario never ran")

	t.Logf("PASS IC-3 live: resume-after-SIGKILL (upload %s resumed, skipped parts logged, not from part 1) and abandoned-task cleanup (upload %s aborted, no residue); artifacts: %s",
		killedUploadID, abandonedUploadID, artifacts)
}