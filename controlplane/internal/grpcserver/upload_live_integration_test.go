//go:build integration

package grpcserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// ackLossProxy forwards real RPCs while deliberately dropping acknowledgements.
type ackLossProxy struct {
	agentv1.UnimplementedAgentServiceServer
	upstream  agentv1.AgentServiceClient
	dropFirst atomic.Bool
	hold      atomic.Bool
	dropped   chan string
}

// Register forwards the actual agent registration.
func (p *ackLossProxy) Register(ctx context.Context, r *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
	return p.upstream.Register(ctx, r)
}

// PollApproval forwards the actual token issuance.
func (p *ackLossProxy) PollApproval(ctx context.Context, r *agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error) {
	return p.upstream.PollApproval(ctx, r)
}

// RefreshCredentials forwards authenticated STS refreshes.
func (p *ackLossProxy) RefreshCredentials(ctx context.Context, r *agentv1.RefreshCredentialsRequest) (*agentv1.RefreshCredentialsResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	return p.upstream.RefreshCredentials(metadata.NewOutgoingContext(ctx, md), r)
}

// Connect preserves stream ordering and drops only explicitly selected acks.
func (p *ackLossProxy) Connect(s grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.ServerMessage]) error {
	md, _ := metadata.FromIncomingContext(s.Context())
	ctx, cancel := context.WithCancel(metadata.NewOutgoingContext(s.Context(), md))
	defer cancel()
	upstream, err := p.upstream.Connect(ctx)
	if err != nil {
		return err
	}
	errs := make(chan error, 2)
	go func() {
		for {
			msg, err := s.Recv()
			if err != nil {
				errs <- err
				return
			}
			if err = upstream.Send(msg); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		for {
			msg, err := upstream.Recv()
			if err != nil {
				errs <- err
				return
			}
			if ack := msg.GetAck(); ack != nil && (p.dropFirst.CompareAndSwap(true, false) || p.hold.Load()) {
				select {
				case p.dropped <- ack.GetRefMessageId():
				default:
				}
				continue
			}
			if err = s.Send(msg); err != nil {
				errs <- err
				return
			}
		}
	}()
	return <-errs
}

// liveProcess starts a binary with durable logs and returns a synchronous stop.
func liveProcess(t *testing.T, root, logPath, binary string, args ...string) func() {
	t.Helper()
	out, err := os.Create(logPath)
	require.NoError(t, err)
	cmd := exec.Command(filepath.Join(root, "bin", binary), args...)
	cmd.Dir = root
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = append(os.Environ(), "MIGRATIONS_PATH="+filepath.Join(root, "controlplane/migrations"))
	require.NoError(t, cmd.Start())
	var stopped bool
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = out.Close()
	}
	t.Cleanup(stop)
	return stop
}

// liveQueueCompleted reads the real agent SQLite queue with the installed CLI.
func liveQueueCompleted(path string) bool {
	out, err := exec.Command("sqlite3", path, "SELECT count(*) FROM upload_tasks WHERE status='completed';").Output()
	return err == nil && strings.TrimSpace(string(out)) != "0" && strings.TrimSpace(string(out)) != ""
}

// TestUploadMainPathLive uses actual CP and agent binaries plus PG/MinIO/NATS.
// It is opt-in because it owns the configured CP ports and leaves evidence rows.
func TestUploadMainPathLive(t *testing.T) {
	if os.Getenv("IC2A_LIVE") != "1" {
		t.Skip("set IC2A_LIVE=1 with dev controlplane.env loaded and built binaries")
	}
	root, err := filepath.Abs("../../..")
	require.NoError(t, err)
	artifacts, err := os.MkdirTemp("", "ic2a-live-")
	require.NoError(t, err)
	t.Log("artifacts:", artifacts)
	conn, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	defer conn.Close()
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
	nc, err := nats.Connect(os.Getenv("NATS_URL"))
	require.NoError(t, err)
	defer nc.Close()
	sub, err := nc.SubscribeSync("events.file.uploaded")
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	upstream, err := grpc.NewClient("127.0.0.1:9090", grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer upstream.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	proxy := &ackLossProxy{upstream: agentv1.NewAgentServiceClient(upstream), dropped: make(chan string, 16)}
	proxy.dropFirst.Store(true)
	srv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(srv, proxy)
	go srv.Serve(listener)
	defer srv.Stop()
	org := "00000000-0000-0000-0000-000000000001"
	agentID, ruleID := uuid.NewString(), uuid.NewString()
	fingerprint := uuid.NewString()
	prefix := "ic2a-" + uuid.NewString()
	tagKey := "ic2a_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	var bucket string
	require.NoError(t, conn.QueryRow("SELECT id FROM buckets WHERE name='data-sensor'").Scan(&bucket))
	_, err = conn.Exec("INSERT INTO agents(id,org_id,name,fingerprint,status) VALUES($1,$2,$3,$4,'approved')", agentID, org, prefix, fingerprint)
	require.NoError(t, err)
	input := filepath.Join(artifacts, "input")
	require.NoError(t, os.MkdirAll(filepath.Join(input, "tokyo"), 0700))
	_, err = conn.Exec("INSERT INTO tag_keys(org_id,key,label,value_controlled,allow_path_var) VALUES($1,$2,$2,false,true)", org, tagKey)
	require.NoError(t, err)
	meta, _ := json.Marshal(map[string]any{"path_tag_map": map[string]string{tagKey: "{site}"}})
	_, err = conn.Exec(`INSERT INTO collection_rules(id,org_id,agent_id,bucket_id,name,mode,base_path,path_pattern,dest_path_template,recursive,metadata) VALUES($1,$2,$3,$4,$5,'watch',$6,'{site}/{filename}',$7,true,$8)`, ruleID, org, agentID, bucket, prefix, input, "/"+prefix+"/{site}/{filename}", string(meta))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(artifacts, "fingerprint"), []byte(fingerprint), 0600))
	config := fmt.Sprintf("[server]\nendpoint=%q\ntls_insecure=true\n[agent]\nfingerprint_file=%q\ntoken_file=%q\ndata_dir=%q\n[upload]\nreport_timeout_seconds=1\n[metrics]\nenabled=false\n[log]\noutput=%q\nlevel=\"debug\"\n", listener.Addr().String(), filepath.Join(artifacts, "fingerprint"), filepath.Join(artifacts, "token"), artifacts, filepath.Join(artifacts, "agent.json.log"))
	configPath := filepath.Join(artifacts, "agent.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(input, "tokyo", "first.csv"), []byte("a,b\n1,2\n"), 0600))
	agentLog := filepath.Join(artifacts, "agent-process.log")
	stopAgent := liveProcess(t, root, agentLog, "agent", "--config", configPath)
	defer stopAgent()
	select {
	case id := <-proxy.dropped:
		t.Log("deliberately dropped first ack:", id)
	case <-time.After(90 * time.Second):
		t.Fatal("no first upload ack; inspect logs in " + artifacts)
	}
	require.Eventually(t, func() bool { return liveQueueCompleted(filepath.Join(artifacts, "queue.db")) }, 10*time.Second, 100*time.Millisecond)
	key := prefix + "/tokyo/first.csv"
	var fileID, gotAgent, gotRule, sha string
	var incomplete bool
	require.NoError(t, conn.QueryRow("SELECT id,agent_id,rule_id,sha256,meta_incomplete FROM file_entries WHERE bucket_id=$1 AND storage_path=$2", bucket, key).Scan(&fileID, &gotAgent, &gotRule, &sha, &incomplete))
	require.Equal(t, agentID, gotAgent)
	require.Equal(t, ruleID, gotRule)
	require.NotEmpty(t, sha)
	require.False(t, incomplete)
	var logs, tags int
	require.NoError(t, conn.QueryRow("SELECT count(*) FROM upload_logs WHERE agent_id=$1 AND storage_path=$2", agentID, key).Scan(&logs))
	require.Positive(t, logs)
	require.NoError(t, conn.QueryRow("SELECT count(*) FROM file_tags WHERE file_entry_id=$1 AND source='path_var'", fileID).Scan(&tags))
	require.Positive(t, tags)
	msg, err := sub.NextMsg(5 * time.Second)
	require.NoError(t, err)
	require.Contains(t, string(msg.Data), fileID)
	event := map[string]any{"Records": []any{map[string]any{"eventName": "s3:ObjectCreated:Put", "eventTime": time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano), "s3": map[string]any{"bucket": map[string]string{"name": "data-sensor"}, "object": map[string]any{"key": url.QueryEscape(key), "size": 8, "eTag": "live-webhook", "sequencer": "000000000000010A"}}}}}
	body, _ := json.Marshal(event)
	req, err := http.NewRequest(http.MethodPost, httpURL+"/internal/minio-event", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+os.Getenv("INTERNAL_WEBHOOK_SECRET"))
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	response.Body.Close()
	require.Equal(t, 200, response.StatusCode)
	var count int
	require.NoError(t, conn.QueryRow("SELECT count(*) FROM file_entries WHERE bucket_id=$1 AND storage_path=$2", bucket, key).Scan(&count))
	require.Equal(t, 1, count)
	t.Logf("PASS live upload: file=%s agent=%s rule=%s sha256=%s logs=%d path_var=%d; lost ack healed and dual writers have one row", fileID, agentID, ruleID, sha, logs, tags)
	proxy.hold.Store(true)
	require.NoError(t, os.WriteFile(filepath.Join(input, "tokyo", "restart.csv"), []byte("restart\n"), 0600))
	select {
	case <-proxy.dropped:
	case <-time.After(30 * time.Second):
		t.Fatal("no restart upload ack")
	}
	stopCP()
	time.Sleep(2 * time.Second)
	stopCP = liveProcess(t, root, filepath.Join(artifacts, "cp-2.log"), "controlplane")
	defer stopCP()
	require.Eventually(t, healthy, 30*time.Second, 100*time.Millisecond)
	proxy.hold.Store(false)
	require.Eventually(t, func() bool {
		out, e := exec.Command("sqlite3", filepath.Join(artifacts, "queue.db"), "SELECT count(*) FROM upload_tasks WHERE status='completed';").Output()
		return e == nil && strings.TrimSpace(string(out)) == "2"
	}, 45*time.Second, 100*time.Millisecond)
	require.NoError(t, conn.QueryRow("SELECT count(*) FROM file_entries WHERE bucket_id=$1 AND storage_path=$2", bucket, prefix+"/tokyo/restart.csv").Scan(&count))
	require.Equal(t, 1, count)
	t.Log("PASS CP stop/restart: durable result resent, queue completed=2, restart object rows=1")
	evidence := fmt.Sprintf("prefix=%s\nagent=%s\nrule=%s\nfile=%s\nsha256=%s\nlogs=%d\npath_var=%d\nlive upload / dual writers / restart / lost ack / leading-slash tags: PASS\n", prefix, agentID, ruleID, fileID, sha, logs, tags)
	require.NoError(t, os.WriteFile(filepath.Join(artifacts, "evidence.txt"), []byte(evidence), 0600))
}
