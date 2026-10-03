// Command probe exercises the exact S3 capability surface FileAgent depends on
// (STS AssumeRole + session policy, multipart resume/abort, presigned GET,
// bucket notifications, key semantics) against an S3 endpoint, using the same
// minio-go version as controlplane/agent. Results are printed as a table and
// written as JSON lines.
//
// Subcommands: run (default), mp-prepare, mp-verify (server-restart test).
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/signer"
)

const mb = 1 << 20

// ---------------------------------------------------------------- config

type config struct {
	Target, Endpoint, AltEndpoint string
	AdminAK, AdminSK, CPAK, CPSK  string
	RoleARN, Grant, Other         string
	Events, WebhookAddr           string
	BigMB                         int
	OutDir                        string
	ExpiryTest                    bool
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func loadConfig() config {
	big := 150
	fmt.Sscanf(env("BIG_MB", "150"), "%d", &big)
	return config{
		Target:      env("TARGET", "unknown"),
		Endpoint:    env("S3_ENDPOINT", "localhost:19100"),
		AltEndpoint: env("S3_ALT_ENDPOINT", "127.0.0.1:19100"),
		AdminAK:     env("ADMIN_AK", "minioadmin"),
		AdminSK:     env("ADMIN_SK", "minioadmin"),
		CPAK:        env("CP_AK", "cpadmin"),
		CPSK:        env("CP_SK", "cpadmin-secret"),
		RoleARN:     env("ROLE_ARN", "arn:aws:iam:::role/agent-role"),
		Grant:       env("BUCKET_GRANT", "probe-grant"),
		Other:       env("BUCKET_OTHER", "probe-other"),
		Events:      env("EVENTS", "none"),
		WebhookAddr: env("WEBHOOK_ADDR", ":18990"),
		BigMB:       big,
		OutDir:      env("OUT_DIR", "tmp/probe"),
		ExpiryTest:  env("EXPIRY_TEST", "") == "1",
	}
}

// ---------------------------------------------------------------- results

type result struct {
	ID, Name, Status, Detail string
}

type recorder struct {
	mu  sync.Mutex
	all []result
}

func (r *recorder) add(id, name, status, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.all = append(r.all, result{id, name, status, detail})
	fmt.Printf("%-6s %-5s %s — %s\n", id, status, name, oneLine(detail))
}

func (r *recorder) ok(id, name string, err error, detail string) {
	if err != nil {
		r.add(id, name, "FAIL", errText(err))
		return
	}
	r.add(id, name, "PASS", detail)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 220 {
		s = s[:220] + "…"
	}
	return s
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	er := minio.ToErrorResponse(err)
	if er.Code != "" {
		return fmt.Sprintf("%s (HTTP %d): %s", er.Code, er.StatusCode, er.Message)
	}
	return err.Error()
}

// denied passes when err is an authorization-type rejection.
func (r *recorder) denied(id, name string, err error) {
	if err == nil {
		r.add(id, name, "FAIL", "expected rejection, request SUCCEEDED")
		return
	}
	er := minio.ToErrorResponse(err)
	switch er.Code {
	case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "ExpiredToken",
		"InvalidToken", "InvalidTokenId", "InvalidClientTokenId":
		r.add(id, name, "PASS", "rejected: "+errText(err))
	default:
		if er.StatusCode == http.StatusForbidden || er.StatusCode == http.StatusUnauthorized {
			r.add(id, name, "PASS", "rejected (non-standard code): "+errText(err))
			return
		}
		r.add(id, name, "FAIL", "rejected but not as auth error: "+errText(err))
	}
}

// ---------------------------------------------------------------- helpers

func newCore(endpoint string, creds *credentials.Credentials, trailing bool) *minio.Core {
	c, err := minio.NewCore(endpoint, &minio.Options{Creds: creds, Secure: false, TrailingHeaders: trailing})
	if err != nil {
		panic(err) // probe tool: misconfiguration is fatal
	}
	return c
}

func static(ak, sk, tok string) *credentials.Credentials { return credentials.NewStaticV4(ak, sk, tok) }

// buildSessionPolicy mirrors controlplane/internal/storage/policy.go exactly.
// MUTATE=loose-policy widens it, as a negative control for the probe itself.
func buildSessionPolicy(bucket string) string {
	if os.Getenv("MUTATE") == "loose-policy" {
		return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
			`{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::*"]}]}`) + strings.Repeat("", len(bucket))
	}
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Action":["s3:ListBucketMultipartUploads"],"Resource":["arn:aws:s3:::%[1]s"]},`+
		`{"Effect":"Allow","Action":["s3:PutObject","s3:AbortMultipartUpload","s3:ListMultipartUploadParts"],"Resource":["arn:aws:s3:::%[1]s/*"]}]}`, bucket)
}

func assumeRole(cfg config, ak, sk, policy string, dur int) (credentials.Value, error) {
	li := credentials.New(&credentials.STSAssumeRole{
		STSEndpoint: "http://" + cfg.Endpoint,
		Options: credentials.STSAssumeRoleOptions{
			AccessKey: ak, SecretKey: sk, RoleARN: cfg.RoleARN,
			RoleSessionName: "agent-probe", Policy: policy, DurationSeconds: dur,
		},
		Client: &http.Client{Timeout: 10 * time.Second},
	})
	return li.Get()
}

func randBytes(n int, seed uint64) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewChaCha8([32]byte{byte(seed), byte(seed >> 8), 7}))
	for i := 0; i+8 <= n; i += 8 {
		v := r.Uint64()
		for j := 0; j < 8; j++ {
			b[i+j] = byte(v >> (8 * j))
		}
	}
	return b
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func getSHA(ctx context.Context, c *minio.Core, bucket, key string) (string, int64, error) {
	rc, _, _, err := c.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func verifyRoundTrip(ctx context.Context, admin *minio.Core, bucket, key string, want []byte) error {
	got, n, err := getSHA(ctx, admin, bucket, key)
	if err != nil {
		return fmt.Errorf("read back: %w", err)
	}
	if got != sha(want) || n != int64(len(want)) {
		return fmt.Errorf("content mismatch: got %d bytes sha %s…, want %d bytes sha %s…", n, got[:12], len(want), sha(want)[:12])
	}
	return nil
}

func putParts(ctx context.Context, c *minio.Core, bucket, key, uploadID string, data []byte, partSize int, from, to int) ([]minio.CompletePart, error) {
	var parts []minio.CompletePart
	for i := from; i <= to; i++ {
		off := (i - 1) * partSize
		end := min(off+partSize, len(data))
		p, err := c.PutObjectPart(ctx, bucket, key, uploadID, i, bytes.NewReader(data[off:end]), int64(end-off), minio.PutObjectPartOptions{})
		if err != nil {
			return parts, fmt.Errorf("part %d: %w", i, err)
		}
		parts = append(parts, minio.CompletePart{PartNumber: i, ETag: p.ETag})
	}
	return parts, nil
}

func ensureBucket(ctx context.Context, c *minio.Core, b string) error {
	ok, err := c.BucketExists(ctx, b)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return c.MakeBucket(ctx, b, minio.MakeBucketOptions{})
}

// ---------------------------------------------------------------- main

func main() {
	cfg := loadConfig()
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		panic(err)
	}
	ctx := context.Background()
	rec := &recorder{}
	admin := newCore(cfg.Endpoint, static(cfg.AdminAK, cfg.AdminSK, ""), false)

	switch cmd {
	case "run":
		run(ctx, cfg, rec, admin)
	case "mp-prepare":
		mpPrepare(ctx, cfg, rec, admin)
	case "mp-verify":
		mpVerify(ctx, cfg, rec, admin)
	case "expiry":
		expiry(ctx, cfg, rec, admin)
	case "sts-save":
		stsSave(ctx, cfg, rec, admin)
	case "sts-use":
		stsUse(ctx, cfg, rec)
	default:
		fmt.Fprintln(os.Stderr, "usage: probe [run|mp-prepare|mp-verify|expiry|sts-save|sts-use]")
		os.Exit(2)
	}
	writeResults(cfg, cmd, rec)
}

func writeResults(cfg config, cmd string, rec *recorder) {
	f, err := os.Create(filepath.Join(cfg.OutDir, fmt.Sprintf("%s-%s.jsonl", cfg.Target, cmd)))
	if err != nil {
		panic(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	counts := map[string]int{}
	for _, r := range rec.all {
		_ = enc.Encode(r)
		counts[r.Status]++
	}
	fmt.Printf("\n== %s/%s: PASS %d  FAIL %d  INFO %d  SKIP %d\n", cfg.Target, cmd, counts["PASS"], counts["FAIL"], counts["INFO"], counts["SKIP"])
}

func run(ctx context.Context, cfg config, rec *recorder, admin *minio.Core) {
	for _, b := range []string{cfg.Grant, cfg.Other} {
		if err := ensureBucket(ctx, admin, b); err != nil {
			rec.add("S0", "admin creates bucket "+b, "FAIL", errText(err))
			return
		}
	}
	rec.add("S0", "admin creates buckets", "PASS", cfg.Grant+", "+cfg.Other)

	stsVal := testSTS(ctx, cfg, rec, admin)
	if stsVal.AccessKeyID == "" {
		rec.add("G2", "multipart under STS creds", "SKIP", "no STS credentials")
	} else {
		testMultipart(ctx, cfg, rec, admin, stsVal)
		testChecksums(ctx, cfg, rec, admin, stsVal)
	}
	testPresign(ctx, cfg, rec, admin)
	testSemantics(ctx, cfg, rec, admin)
	if cfg.Events == "webhook" {
		testEvents(ctx, cfg, rec, admin)
	} else {
		rec.add("G4", "bucket notifications", "SKIP", "EVENTS="+cfg.Events)
	}
}

// ---------------------------------------------------------------- G1 STS

func testSTS(ctx context.Context, cfg config, rec *recorder, admin *minio.Core) credentials.Value {
	val, err := assumeRole(cfg, cfg.CPAK, cfg.CPSK, buildSessionPolicy(cfg.Grant), 3600)
	if err != nil {
		rec.add("G1.1", "AssumeRole (CP IAM user, session policy, 3600s)", "FAIL", errText(err))
		return credentials.Value{}
	}
	if val.AccessKeyID == "" || val.SecretAccessKey == "" || val.SessionToken == "" {
		rec.add("G1.1", "AssumeRole (CP IAM user, session policy, 3600s)", "FAIL", "incomplete credential set")
		return credentials.Value{}
	}
	rec.add("G1.1", "AssumeRole (CP IAM user, session policy, 3600s)", "PASS", "AK "+val.AccessKeyID[:min(8, len(val.AccessKeyID))]+"…")

	switch d := time.Until(val.Expiration); {
	case val.Expiration.IsZero():
		rec.add("G1.2", "Expiration honours DurationSeconds=3600", "INFO", "no expiration reported")
	case d > 55*time.Minute && d < 65*time.Minute:
		rec.add("G1.2", "Expiration honours DurationSeconds=3600", "PASS", "expires in "+d.Round(time.Second).String())
	default:
		rec.add("G1.2", "Expiration honours DurationSeconds=3600", "FAIL", "expires in "+d.Round(time.Second).String())
	}

	sc := newCore(cfg.Endpoint, static(val.AccessKeyID, val.SecretAccessKey, val.SessionToken), false)
	body := []byte("sts probe")
	_, err = sc.Client.PutObject(ctx, cfg.Grant, "sts/ok.txt", bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{})
	rec.ok("G1.3", "STS creds PutObject into granted bucket", err, "")

	_, err = sc.Client.PutObject(ctx, cfg.Other, "sts/nope.txt", bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{})
	rec.denied("G1.4", "STS creds PutObject into NON-granted bucket is denied", err)

	// The CP IAM user itself may GetObject; the session policy does not grant it.
	// A denial here proves the session policy actually narrows the user policy.
	_, _, _, err = sc.GetObject(ctx, cfg.Grant, "sts/ok.txt", minio.GetObjectOptions{})
	rec.denied("G1.5", "STS creds GetObject denied (session policy narrows user policy)", err)

	err = sc.Client.RemoveObject(ctx, cfg.Grant, "sts/ok.txt", minio.RemoveObjectOptions{})
	rec.denied("G1.6", "STS creds RemoveObject denied", err)

	_, err = sc.ListObjectsV2(cfg.Grant, "", "", "", "", 10)
	rec.denied("G1.7", "STS creds ListObjectsV2 denied", err)

	tok := []byte(val.SessionToken)
	tok[len(tok)/2] ^= 0x01
	bad := newCore(cfg.Endpoint, static(val.AccessKeyID, val.SecretAccessKey, string(tok)), false)
	_, err = bad.Client.PutObject(ctx, cfg.Grant, "sts/tampered.txt", bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{})
	rec.denied("G1.8", "tampered session token denied", err)

	bad = newCore(cfg.Endpoint, static(val.AccessKeyID, val.SecretAccessKey, ""), false)
	_, err = bad.Client.PutObject(ctx, cfg.Grant, "sts/notoken.txt", bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{})
	rec.denied("G1.9", "STS AK/SK without session token denied", err)

	if _, err := assumeRole(cfg, cfg.CPAK, cfg.CPSK+"x", buildSessionPolicy(cfg.Grant), 3600); err != nil {
		rec.add("G1.10", "AssumeRole with wrong secret rejected", "PASS", errText(err))
	} else {
		rec.add("G1.10", "AssumeRole with wrong secret rejected", "FAIL", "issued credentials")
	}

	if v, err := assumeRole(cfg, cfg.CPAK, cfg.CPSK, "", 3600); err != nil {
		rec.add("G1.11", "AssumeRole without session policy", "INFO", errText(err))
	} else {
		nc := newCore(cfg.Endpoint, static(v.AccessKeyID, v.SecretAccessKey, v.SessionToken), false)
		_, _, _, gerr := nc.GetObject(ctx, cfg.Grant, "sts/ok.txt", minio.GetObjectOptions{})
		rec.add("G1.11", "AssumeRole without session policy", "INFO", "issued; GetObject (inherits user policy): "+errText(gerr))
	}

	if cfg.ExpiryTest {
		v, err := assumeRole(cfg, cfg.CPAK, cfg.CPSK, buildSessionPolicy(cfg.Grant), 900)
		if err != nil {
			rec.add("G1.12", "expired STS creds denied", "FAIL", "assume 900s: "+errText(err))
		} else {
			fmt.Println("   … waiting 15m30s for STS expiry")
			time.Sleep(15*time.Minute + 30*time.Second)
			ec := newCore(cfg.Endpoint, static(v.AccessKeyID, v.SecretAccessKey, v.SessionToken), false)
			_, err = ec.Client.PutObject(ctx, cfg.Grant, "sts/expired.txt", bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{})
			rec.denied("G1.12", "expired STS creds denied", err)
		}
	} else {
		rec.add("G1.12", "expired STS creds denied", "SKIP", "EXPIRY_TEST!=1 (takes 15 min)")
	}
	return val
}

// ---------------------------------------------------------------- G2 multipart

func testMultipart(ctx context.Context, cfg config, rec *recorder, admin *minio.Core, v credentials.Value) {
	sc := newCore(cfg.Endpoint, static(v.AccessKeyID, v.SecretAccessKey, v.SessionToken), false)
	const part = 64 * mb
	data := randBytes(cfg.BigMB*mb, 1)
	nParts := (len(data) + part - 1) / part

	// 2.1 full upload, agent-style.
	key := "mp/full.bin"
	id, err := sc.NewMultipartUpload(ctx, cfg.Grant, key, minio.PutObjectOptions{})
	if err == nil {
		var parts []minio.CompletePart
		if parts, err = putParts(ctx, sc, cfg.Grant, key, id, data, part, 1, nParts); err == nil {
			var info minio.UploadInfo
			if info, err = sc.CompleteMultipartUpload(ctx, cfg.Grant, key, id, parts, minio.PutObjectOptions{}); err == nil {
				if err = verifyRoundTrip(ctx, admin, cfg.Grant, key, data); err == nil {
					rec.add("G2.1", fmt.Sprintf("multipart %dMB / 64MB parts (STS creds), sha256 round-trip", cfg.BigMB), "PASS", "ETag "+info.ETag)
				}
			}
		}
	}
	if err != nil {
		rec.add("G2.1", fmt.Sprintf("multipart %dMB / 64MB parts (STS creds), sha256 round-trip", cfg.BigMB), "FAIL", errText(err))
	}

	// 2.2 resume: a fresh client rediscovers the upload and its parts.
	key = "mp/resume.bin"
	err = func() error {
		id, err := sc.NewMultipartUpload(ctx, cfg.Grant, key, minio.PutObjectOptions{})
		if err != nil {
			return fmt.Errorf("new: %w", err)
		}
		first, err := putParts(ctx, sc, cfg.Grant, key, id, data, part, 1, 1)
		if err != nil {
			return err
		}
		fresh := newCore(cfg.Endpoint, static(v.AccessKeyID, v.SecretAccessKey, v.SessionToken), false)
		lr, err := fresh.ListMultipartUploads(ctx, cfg.Grant, key, "", "", "", 1000)
		if err != nil {
			return fmt.Errorf("ListMultipartUploads: %w", err)
		}
		found := false
		for _, u := range lr.Uploads {
			found = found || (u.UploadID == id && u.Key == key)
		}
		if !found {
			return fmt.Errorf("ListMultipartUploads did not return upload %s (got %d uploads)", id, len(lr.Uploads))
		}
		pr, err := fresh.ListObjectParts(ctx, cfg.Grant, key, id, 0, 1000)
		if err != nil {
			return fmt.Errorf("ListObjectParts: %w", err)
		}
		if len(pr.ObjectParts) != 1 || pr.ObjectParts[0].Size != part || strings.Trim(pr.ObjectParts[0].ETag, `"`) != strings.Trim(first[0].ETag, `"`) {
			return fmt.Errorf("ListObjectParts mismatch: %+v vs put etag %s", pr.ObjectParts, first[0].ETag)
		}
		rest, err := putParts(ctx, fresh, cfg.Grant, key, id, data, part, 2, nParts)
		if err != nil {
			return err
		}
		listed := []minio.CompletePart{{PartNumber: 1, ETag: pr.ObjectParts[0].ETag}}
		if _, err := fresh.CompleteMultipartUpload(ctx, cfg.Grant, key, id, append(listed, rest...), minio.PutObjectOptions{}); err != nil {
			return fmt.Errorf("complete: %w", err)
		}
		return verifyRoundTrip(ctx, admin, cfg.Grant, key, data)
	}()
	rec.ok("G2.2", "resume: ListMultipartUploads + ListObjectParts rediscover, finish, sha256", err, "")

	// 2.3 abort.
	key = "mp/abort.bin"
	err = func() error {
		id, err := sc.NewMultipartUpload(ctx, cfg.Grant, key, minio.PutObjectOptions{})
		if err != nil {
			return err
		}
		if _, err := putParts(ctx, sc, cfg.Grant, key, id, data[:6*mb], 6*mb, 1, 1); err != nil {
			return err
		}
		if err := sc.AbortMultipartUpload(ctx, cfg.Grant, key, id); err != nil {
			return fmt.Errorf("abort: %w", err)
		}
		_, err = sc.ListObjectParts(ctx, cfg.Grant, key, id, 0, 1000)
		if code := minio.ToErrorResponse(err).Code; code != "NoSuchUpload" {
			return fmt.Errorf("after abort ListObjectParts = %s, want NoSuchUpload", errText(err))
		}
		lr, err := sc.ListMultipartUploads(ctx, cfg.Grant, key, "", "", "", 1000)
		if err != nil {
			return err
		}
		for _, u := range lr.Uploads {
			if u.UploadID == id {
				return fmt.Errorf("aborted upload still listed")
			}
		}
		return nil
	}()
	rec.ok("G2.3", "abort: parts gone (NoSuchUpload) and upload no longer listed", err, "")

	// 2.4 complete with a wrong ETag must fail (integrity of the part manifest).
	key = "mp/badetag.bin"
	err = func() error {
		id, err := sc.NewMultipartUpload(ctx, cfg.Grant, key, minio.PutObjectOptions{})
		if err != nil {
			return err
		}
		defer sc.AbortMultipartUpload(ctx, cfg.Grant, key, id)
		if _, err := putParts(ctx, sc, cfg.Grant, key, id, data[:6*mb], 6*mb, 1, 1); err != nil {
			return err
		}
		_, err = sc.CompleteMultipartUpload(ctx, cfg.Grant, key, id, []minio.CompletePart{{PartNumber: 1, ETag: "00000000000000000000000000000000"}}, minio.PutObjectOptions{})
		if err == nil {
			return fmt.Errorf("complete with bogus ETag SUCCEEDED")
		}
		return nil
	}()
	rec.ok("G2.4", "complete with wrong part ETag is rejected", err, "")

	// 2.5 agent single-put path (<=64MB) with default PutObjectOptions.
	for _, sz := range []int{10 * mb, 64 * mb} {
		k := fmt.Sprintf("put/single-%dMB.bin", sz/mb)
		d := data[:sz]
		_, err := sc.Client.PutObject(ctx, cfg.Grant, k, bytes.NewReader(d), int64(sz), minio.PutObjectOptions{})
		if err == nil {
			err = verifyRoundTrip(ctx, admin, cfg.Grant, k, d)
		}
		rec.ok("G2.5", fmt.Sprintf("agent PutObject %dMB default opts (STS), sha256", sz/mb), err, "")
	}
}

// ---------------------------------------------------------------- T2 checksums / signing modes

func testChecksums(ctx context.Context, cfg config, rec *recorder, admin *minio.Core, v credentials.Value) {
	d := randBytes(5*mb, 2)
	cases := []struct {
		name     string
		trailing bool
		opts     minio.PutObjectOptions
		size     int64
	}{
		{"SendContentMd5", false, minio.PutObjectOptions{SendContentMd5: true}, int64(len(d))},
		{"DisableContentSha256 (UNSIGNED-PAYLOAD)", false, minio.PutObjectOptions{DisableContentSha256: true}, int64(len(d))},
		{"Checksum CRC32C (trailing headers)", true, minio.PutObjectOptions{Checksum: minio.ChecksumCRC32C}, int64(len(d))},
		{"Checksum SHA256 (trailing headers)", true, minio.PutObjectOptions{Checksum: minio.ChecksumSHA256}, int64(len(d))},
		{"unknown size (-1) streaming", false, minio.PutObjectOptions{}, -1},
	}
	for i, c := range cases {
		sc := newCore(cfg.Endpoint, static(v.AccessKeyID, v.SecretAccessKey, v.SessionToken), c.trailing)
		k := fmt.Sprintf("cksum/%d.bin", i)
		_, err := sc.Client.PutObject(ctx, cfg.Grant, k, bytes.NewReader(d), c.size, c.opts)
		if err == nil {
			err = verifyRoundTrip(ctx, admin, cfg.Grant, k, d)
		}
		rec.ok(fmt.Sprintf("T2.%d", i+1), "PutObject "+c.name+" (STS)", err, "")
	}
}

// ---------------------------------------------------------------- G3 presign

func httpGet(u string) (int, []byte, error) {
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(u)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func testPresign(ctx context.Context, cfg config, rec *recorder, admin *minio.Core) {
	d := randBytes(1*mb, 3)
	key := "presign/中文 file+1.bin"
	if _, err := admin.Client.PutObject(ctx, cfg.Grant, key, bytes.NewReader(d), int64(len(d)), minio.PutObjectOptions{}); err != nil {
		rec.add("G3", "presign setup", "FAIL", errText(err))
		return
	}
	cp := newCore(cfg.Endpoint, static(cfg.CPAK, cfg.CPSK, ""), false)
	params := url.Values{}
	params.Set("response-content-disposition", `attachment; filename="probe.bin"`)
	u, err := cp.Client.PresignedGetObject(ctx, cfg.Grant, key, 10*time.Minute, params)
	if err != nil {
		rec.add("G3.1", "presigned GET (CP creds, unicode key, content-disposition)", "FAIL", errText(err))
		return
	}
	code, b, err := httpGet(u.String())
	switch {
	case err != nil:
		rec.add("G3.1", "presigned GET (CP creds, unicode key, content-disposition)", "FAIL", err.Error())
	case code != 200 || sha(b) != sha(d):
		rec.add("G3.1", "presigned GET (CP creds, unicode key, content-disposition)", "FAIL", fmt.Sprintf("HTTP %d, %d bytes: %s", code, len(b), oneLine(string(b))))
	default:
		rec.add("G3.1", "presigned GET (CP creds, unicode key, content-disposition)", "PASS", "sha256 ok")
	}

	alt := *u
	alt.Host = cfg.AltEndpoint
	code, _, err = httpGet(alt.String())
	rec.add("G3.2", "presigned URL fetched via a different Host (D-024 context)", "INFO", fmt.Sprintf("HTTP %d err=%v (403 = signature binds Host)", code, err))

	t := *u
	q := t.Query()
	sig := q.Get("X-Amz-Signature")
	q.Set("X-Amz-Signature", strings.Repeat("0", len(sig)))
	t.RawQuery = q.Encode()
	code, _, _ = httpGet(t.String())
	if code == 403 {
		rec.add("G3.3", "tampered presign signature rejected", "PASS", "HTTP 403")
	} else {
		rec.add("G3.3", "tampered presign signature rejected", "FAIL", fmt.Sprintf("HTTP %d", code))
	}

	short, err := cp.Client.PresignedGetObject(ctx, cfg.Grant, key, 1*time.Second, nil)
	if err == nil {
		time.Sleep(3 * time.Second)
		code, _, _ = httpGet(short.String())
		if code == 403 {
			rec.add("G3.4", "expired presigned URL rejected", "PASS", "HTTP 403")
		} else {
			rec.add("G3.4", "expired presigned URL rejected", "FAIL", fmt.Sprintf("HTTP %d", code))
		}
	} else {
		rec.add("G3.4", "expired presigned URL rejected", "FAIL", errText(err))
	}
}

// ---------------------------------------------------------------- T1 semantics

func testSemantics(ctx context.Context, cfg config, rec *recorder, admin *minio.Core) {
	put := func(k string, b []byte, o minio.PutObjectOptions) error {
		_, err := admin.Client.PutObject(ctx, cfg.Grant, k, bytes.NewReader(b), int64(len(b)), o)
		return err
	}
	b := func(s string) []byte { return []byte(s) }

	// Object "a" and "a/b" coexisting — trivial in S3, hard for POSIX-backed gateways.
	err := put("sem/a", b("file-a"), minio.PutObjectOptions{})
	if err == nil {
		err = put("sem/a/b", b("file-ab"), minio.PutObjectOptions{})
	}
	if err == nil {
		if err = verifyRoundTrip(ctx, admin, cfg.Grant, "sem/a", b("file-a")); err == nil {
			err = verifyRoundTrip(ctx, admin, cfg.Grant, "sem/a/b", b("file-ab"))
		}
	}
	rec.ok("T1.1", `objects "x/a" and "x/a/b" coexist`, err, "")

	// Reverse order: directory-ish prefix first, then a file at the prefix.
	err = put("sem/c/d", b("file-cd"), minio.PutObjectOptions{})
	if err == nil {
		err = put("sem/c", b("file-c"), minio.PutObjectOptions{})
	}
	if err == nil {
		err = verifyRoundTrip(ctx, admin, cfg.Grant, "sem/c", b("file-c"))
	}
	rec.ok("T1.2", `object "x/c" created after "x/c/d" exists`, err, "")

	err = put("sem/dir/", nil, minio.PutObjectOptions{})
	rec.add("T1.3", "zero-byte key with trailing slash", "INFO", "err="+errText(err))

	keys := []string{"sem/k/sp ace.txt", "sem/k/中文/文件.txt", "sem/k/plus+sign.txt", "sem/k/pct%41.txt",
		"sem/k/hash#q?.txt", "sem/k/tilde~=&;.txt", "sem/k/emoji😀.txt", "sem/k/dot..dot.txt"}
	var bad []string
	for _, k := range keys {
		if err := put(k, b(k), minio.PutObjectOptions{}); err != nil {
			bad = append(bad, k+": put "+errText(err))
			continue
		}
		if err := verifyRoundTrip(ctx, admin, cfg.Grant, k, b(k)); err != nil {
			bad = append(bad, k+": "+err.Error())
		}
	}
	listed := map[string]bool{}
	for o := range admin.Client.ListObjects(ctx, cfg.Grant, minio.ListObjectsOptions{Prefix: "sem/k/", Recursive: true}) {
		listed[o.Key] = true
	}
	for _, k := range keys {
		if !listed[k] {
			bad = append(bad, k+": missing from listing")
		}
	}
	if len(bad) > 0 {
		rec.add("T1.4", "special-character keys put/get/list exact", "FAIL", strings.Join(bad, "; "))
	} else {
		rec.add("T1.4", "special-character keys put/get/list exact", "PASS", fmt.Sprintf("%d keys", len(keys)))
	}

	err = put("sem/meta.txt", b("m"), minio.PutObjectOptions{ContentType: "text/x-probe", UserMetadata: map[string]string{"Agent-Id": "abc-123", "Source-Path": "/data/中文"}})
	if err == nil {
		var st minio.ObjectInfo
		if st, err = admin.Client.StatObject(ctx, cfg.Grant, "sem/meta.txt", minio.StatObjectOptions{}); err == nil {
			if st.ContentType != "text/x-probe" || st.UserMetadata["Agent-Id"] != "abc-123" {
				err = fmt.Errorf("got content-type %q meta %v", st.ContentType, st.UserMetadata)
			}
		}
	}
	rec.ok("T1.5", "Content-Type + x-amz-meta-* round-trip", err, "")

	err = put("sem/ow.txt", b("v1"), minio.PutObjectOptions{})
	if err == nil {
		err = put("sem/ow.txt", b("version-2"), minio.PutObjectOptions{})
	}
	if err == nil {
		err = verifyRoundTrip(ctx, admin, cfg.Grant, "sem/ow.txt", b("version-2"))
	}
	rec.ok("T1.6", "overwrite then read-after-write returns new content", err, "")

	for i := 0; i < 5; i++ {
		_ = put(fmt.Sprintf("sem/page/%d", i), b("p"), minio.PutObjectOptions{})
	}
	total, pages, token := 0, 0, ""
	for {
		r, err := admin.ListObjectsV2(cfg.Grant, "sem/page/", "", token, "", 2)
		if err != nil {
			rec.add("T1.7", "ListObjectsV2 pagination (maxKeys=2 over 5)", "FAIL", errText(err))
			break
		}
		pages++
		total += len(r.Contents)
		if !r.IsTruncated {
			if total == 5 && pages == 3 {
				rec.add("T1.7", "ListObjectsV2 pagination (maxKeys=2 over 5)", "PASS", "3 pages")
			} else {
				rec.add("T1.7", "ListObjectsV2 pagination (maxKeys=2 over 5)", "FAIL", fmt.Sprintf("%d objects in %d pages", total, pages))
			}
			break
		}
		token = r.NextContinuationToken
		if pages > 10 {
			rec.add("T1.7", "ListObjectsV2 pagination (maxKeys=2 over 5)", "FAIL", "runaway pagination")
			break
		}
	}

	r, err := admin.ListObjectsV2(cfg.Grant, "sem/", "", "", "/", 1000)
	if err != nil {
		rec.add("T1.8", "delimiter listing returns CommonPrefixes", "FAIL", errText(err))
	} else {
		var cps []string
		for _, p := range r.CommonPrefixes {
			cps = append(cps, p.Prefix)
		}
		sort.Strings(cps)
		ok := strings.Contains(strings.Join(cps, ","), "sem/page/") && strings.Contains(strings.Join(cps, ","), "sem/k/")
		rec.add("T1.8", "delimiter listing returns CommonPrefixes", map[bool]string{true: "PASS", false: "FAIL"}[ok], strings.Join(cps, ","))
	}

	_, err = admin.Client.StatObject(ctx, cfg.Grant, "sem/does-not-exist", minio.StatObjectOptions{})
	code := minio.ToErrorResponse(err).Code
	rec.add("T1.9", "StatObject on missing key → NoSuchKey", map[bool]string{true: "PASS", false: "FAIL"}[code == "NoSuchKey"], errText(err))

	err = admin.Client.RemoveObject(ctx, cfg.Grant, "sem/does-not-exist", minio.RemoveObjectOptions{})
	rec.ok("T1.10", "DeleteObject on missing key is idempotent (no error)", err, "")
}

// ---------------------------------------------------------------- G4 events

type eventSink struct {
	mu      sync.Mutex
	records []map[string]any
	raw     [][]byte
	auth    []string
	srv     *http.Server
}

func (s *eventSink) start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Records []map[string]any `json:"Records"`
		}
		_ = json.Unmarshal(body, &env)
		if len(env.Records) == 0 {
			if rec := seaweedToS3(body); rec != nil {
				env.Records = []map[string]any{rec}
			}
		}
		s.mu.Lock()
		s.raw = append(s.raw, body)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.records = append(s.records, env.Records...)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})}
	go s.srv.Serve(ln)
	return nil
}

func (s *eventSink) stop() { _ = s.srv.Close() }

// seaweedToS3 is the adapter the CP would need for SeaweedFS filer webhook
// events: it drops directories and multipart staging entries, maps
// create/update/delete onto S3 event names, and lifts size/ETag out of the
// filer entry. Multipart completion is indistinguishable from a plain put, so
// both map to "s3:ObjectCreated:Put" with a "seaweed" marker.
func seaweedToS3(body []byte) map[string]any {
	var m struct {
		EventType string `json:"event_type"`
		Key       string `json:"key"`
		Message   struct {
			NewEntry *struct {
				IsDirectory bool              `json:"is_directory"`
				Attributes  map[string]any    `json:"attributes"`
				Extended    map[string]string `json:"extended"`
			} `json:"new_entry"`
			OldEntry *struct {
				IsDirectory bool `json:"is_directory"`
			} `json:"old_entry"`
		} `json:"message"`
	}
	if json.Unmarshal(body, &m) != nil || m.EventType == "" {
		return nil
	}
	parts := strings.SplitN(strings.TrimPrefix(m.Key, "/buckets/"), "/", 2)
	if len(parts) != 2 || strings.HasPrefix(parts[1], ".uploads/") || strings.HasPrefix(parts[0], ".") {
		return nil
	}
	obj := map[string]any{"key": url.QueryEscape(parts[1])}
	name := ""
	switch m.EventType {
	case "create", "update":
		if m.Message.NewEntry == nil || m.Message.NewEntry.IsDirectory {
			return nil
		}
		name = "s3:ObjectCreated:Put"
		obj["size"] = m.Message.NewEntry.Attributes["file_size"]
		if e, ok := m.Message.NewEntry.Extended["Seaweed-X-Amz-ETag"]; ok {
			obj["eTag"] = e // base64 of the hex ETag
		}
	case "delete":
		if m.Message.OldEntry != nil && m.Message.OldEntry.IsDirectory {
			return nil
		}
		name = "s3:ObjectRemoved:Delete"
	default:
		return nil
	}
	return map[string]any{
		"eventName": name, "eventTime": time.Now().UTC().Format(time.RFC3339), "adapter": "seaweed",
		"s3": map[string]any{"bucket": map[string]any{"name": parts[0]}, "object": obj},
	}
}

// find returns the first record whose eventName has the prefix and whose
// decoded object key equals key.
func (s *eventSink) find(name, key string) (map[string]any, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		en, _ := r["eventName"].(string)
		s3, _ := r["s3"].(map[string]any)
		obj, _ := s3["object"].(map[string]any)
		raw, _ := obj["key"].(string)
		dec, _ := url.QueryUnescape(raw)
		if strings.HasPrefix(en, name) && dec == key {
			return r, raw
		}
	}
	return nil, ""
}

func (s *eventSink) wait(name, key string, d time.Duration) (map[string]any, string) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if r, raw := s.find(name, key); r != nil {
			return r, raw
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil, ""
}

func testEvents(ctx context.Context, cfg config, rec *recorder, admin *minio.Core) {
	sink := &eventSink{}
	if err := sink.start(cfg.WebhookAddr); err != nil {
		rec.add("G4", "webhook sink", "FAIL", err.Error())
		return
	}
	defer func() {
		sink.mu.Lock()
		_ = os.WriteFile(filepath.Join(cfg.OutDir, cfg.Target+"-events-raw.json"), bytes.Join(sink.raw, []byte("\n")), 0o644)
		auth := sink.auth
		sink.mu.Unlock()
		sink.stop()
		if len(auth) > 0 {
			rec.add("G4.6", "webhook Authorization header present", map[bool]string{true: "PASS", false: "FAIL"}[auth[0] != ""], "first: "+auth[0])
		}
	}()

	put := func(k string, b []byte) error {
		_, err := admin.Client.PutObject(ctx, cfg.Grant, k, bytes.NewReader(b), int64(len(b)), minio.PutObjectOptions{})
		return err
	}
	key := "ev/中文 a+b%20.txt"
	if err := put(key, []byte("event")); err != nil {
		rec.add("G4.1", "ObjectCreated:Put delivered, key decodes exactly", "FAIL", errText(err))
	} else if r, raw := sink.wait("s3:ObjectCreated:Put", key, 15*time.Second); r == nil {
		rec.add("G4.1", "ObjectCreated:Put delivered, key decodes exactly", "FAIL", fmt.Sprintf("no matching record within 15s (%d records seen)", len(sink.records)))
	} else {
		rec.add("G4.1", "ObjectCreated:Put delivered, key decodes exactly", "PASS", "raw key "+raw)
	}

	mkey := "ev/mp.bin"
	d := randBytes(11*mb, 4)
	err := func() error {
		id, err := admin.NewMultipartUpload(ctx, cfg.Grant, mkey, minio.PutObjectOptions{})
		if err != nil {
			return err
		}
		parts, err := putParts(ctx, admin, cfg.Grant, mkey, id, d, 6*mb, 1, 2)
		if err != nil {
			return err
		}
		_, err = admin.CompleteMultipartUpload(ctx, cfg.Grant, mkey, id, parts, minio.PutObjectOptions{})
		return err
	}()
	if err != nil {
		rec.add("G4.2", "ObjectCreated:CompleteMultipartUpload delivered", "FAIL", errText(err))
	} else if r, _ := sink.wait("s3:ObjectCreated:", mkey, 15*time.Second); r == nil {
		rec.add("G4.2", "ObjectCreated:CompleteMultipartUpload delivered", "FAIL", "no record within 15s")
	} else {
		s3, _ := r["s3"].(map[string]any)
		obj, _ := s3["object"].(map[string]any)
		st := map[bool]string{true: "PASS", false: "FAIL"}[fmt.Sprint(obj["size"]) == fmt.Sprint(float64(len(d)))]
		rec.add("G4.2", "ObjectCreated:CompleteMultipartUpload delivered", st, fmt.Sprintf("%v size=%v eTag=%v", r["eventName"], obj["size"], obj["eTag"]))
	}

	if err := admin.Client.RemoveObject(ctx, cfg.Grant, key, minio.RemoveObjectOptions{}); err != nil {
		rec.add("G4.3", "ObjectRemoved:Delete delivered", "FAIL", errText(err))
	} else if r, _ := sink.wait("s3:ObjectRemoved:", key, 15*time.Second); r == nil {
		rec.add("G4.3", "ObjectRemoved:Delete delivered", "FAIL", "no record within 15s")
	} else {
		rec.add("G4.3", "ObjectRemoved:Delete delivered", "PASS", fmt.Sprint(r["eventName"]))
	}

	// Record shape the CP handler relies on.
	if r, _ := sink.find("s3:ObjectCreated:", mkey); r != nil {
		s3, _ := r["s3"].(map[string]any)
		b, _ := s3["bucket"].(map[string]any)
		obj, _ := s3["object"].(map[string]any)
		var missing []string
		if b["name"] != cfg.Grant {
			missing = append(missing, "s3.bucket.name")
		}
		for _, f := range []string{"key", "size", "eTag"} {
			if _, ok := obj[f]; !ok {
				missing = append(missing, "s3.object."+f)
			}
		}
		if _, ok := r["eventTime"]; !ok {
			missing = append(missing, "eventTime")
		}
		rec.add("G4.4", "S3 Records schema fields present", map[bool]string{true: "PASS", false: "FAIL"}[len(missing) == 0], strings.Join(missing, ","))
	}

	// Target down: event must be queued and redelivered.
	sink.stop()
	dkey := "ev/while-down.txt"
	if err := put(dkey, []byte("down")); err != nil {
		rec.add("G4.5", "event queued while webhook down, redelivered after", "FAIL", errText(err))
		return
	}
	time.Sleep(5 * time.Second)
	if err := sink.start(cfg.WebhookAddr); err != nil {
		rec.add("G4.5", "event queued while webhook down, redelivered after", "FAIL", err.Error())
		return
	}
	start := time.Now()
	if r, _ := sink.wait("s3:ObjectCreated:", dkey, 90*time.Second); r == nil {
		rec.add("G4.5", "event queued while webhook down, redelivered after", "FAIL", "not redelivered within 90s")
	} else {
		rec.add("G4.5", "event queued while webhook down, redelivered after", "PASS", "redelivered after "+time.Since(start).Round(time.Second).String())
	}
}

// ---------------------------------------------------------------- restart test

type mpState struct {
	Key, UploadID, ETag, Committed string
	CommittedSHA                   string
}

func stateFile(cfg config) string { return filepath.Join(cfg.OutDir, cfg.Target+"-mpstate.json") }

func mpPrepare(ctx context.Context, cfg config, rec *recorder, admin *minio.Core) {
	if err := ensureBucket(ctx, admin, cfg.Grant); err != nil {
		rec.add("R0", "bucket", "FAIL", errText(err))
		return
	}
	d := randBytes(6*mb, 5)
	st := mpState{Key: "restart/pending.bin", Committed: "restart/committed.bin", CommittedSHA: sha(d)}
	_, err := admin.Client.PutObject(ctx, cfg.Grant, st.Committed, bytes.NewReader(d), int64(len(d)), minio.PutObjectOptions{})
	if err == nil {
		st.UploadID, err = admin.NewMultipartUpload(ctx, cfg.Grant, st.Key, minio.PutObjectOptions{})
	}
	if err == nil {
		var p []minio.CompletePart
		p, err = putParts(ctx, admin, cfg.Grant, st.Key, st.UploadID, d, 6*mb, 1, 1)
		if err == nil {
			st.ETag = p[0].ETag
		}
	}
	if err != nil {
		rec.add("R1", "prepare committed object + in-flight multipart", "FAIL", errText(err))
		return
	}
	b, _ := json.Marshal(st)
	_ = os.WriteFile(stateFile(cfg), b, 0o644)
	rec.add("R1", "prepare committed object + in-flight multipart", "PASS", "upload "+st.UploadID)
}

func mpVerify(ctx context.Context, cfg config, rec *recorder, admin *minio.Core) {
	b, err := os.ReadFile(stateFile(cfg))
	if err != nil {
		rec.add("R2", "state", "FAIL", err.Error())
		return
	}
	var st mpState
	_ = json.Unmarshal(b, &st)
	got, _, err := getSHA(ctx, admin, cfg.Grant, st.Committed)
	if err == nil && got != st.CommittedSHA {
		err = errors.New("sha mismatch")
	}
	rec.ok("R2", "committed object survives server restart", err, "")

	lr, err := admin.ListMultipartUploads(ctx, cfg.Grant, st.Key, "", "", "", 1000)
	found := false
	for _, u := range lr.Uploads {
		found = found || u.UploadID == st.UploadID
	}
	if err == nil && !found {
		err = fmt.Errorf("upload %s not listed after restart", st.UploadID)
	}
	rec.ok("R3", "in-flight multipart upload still listed after restart", err, "")

	pr, err := admin.ListObjectParts(ctx, cfg.Grant, st.Key, st.UploadID, 0, 1000)
	if err == nil && (len(pr.ObjectParts) != 1 || strings.Trim(pr.ObjectParts[0].ETag, `"`) != strings.Trim(st.ETag, `"`)) {
		err = fmt.Errorf("parts after restart: %+v", pr.ObjectParts)
	}
	if err == nil {
		_, err = admin.CompleteMultipartUpload(ctx, cfg.Grant, st.Key, st.UploadID, []minio.CompletePart{{PartNumber: 1, ETag: st.ETag}}, minio.PutObjectOptions{})
	}
	rec.ok("R4", "uploaded part survives restart and upload completes", err, "")
}

// expiry checks that STS credentials work until, and are rejected after, their
// expiry: 900s (the minimum), probed at +5s, +840s and +930s.
func expiry(ctx context.Context, cfg config, rec *recorder, admin *minio.Core) {
	if err := ensureBucket(ctx, admin, cfg.Grant); err != nil {
		rec.add("E0", "bucket", "FAIL", errText(err))
		return
	}
	issued := time.Now()
	// minio-go clamps DurationSeconds < 3600 up to 3600 (assume_role.go:155),
	// so the short-lived request is signed and sent by hand.
	v, err := assumeRoleRaw(cfg, cfg.CPAK, cfg.CPSK, buildSessionPolicy(cfg.Grant), 900)
	if err != nil {
		rec.add("E1", "AssumeRole DurationSeconds=900", "FAIL", errText(err))
		return
	}
	rec.add("E1", "AssumeRole DurationSeconds=900", "PASS", "reported expiry in "+time.Until(v.Expiration).Round(time.Second).String())
	sc := newCore(cfg.Endpoint, static(v.AccessKeyID, v.SecretAccessKey, v.SessionToken), false)
	put := func(tag string) error {
		b := []byte(tag)
		_, err := sc.Client.PutObject(ctx, cfg.Grant, "expiry/"+tag, bytes.NewReader(b), int64(len(b)), minio.PutObjectOptions{})
		return err
	}
	at := func(d time.Duration) { time.Sleep(time.Until(issued.Add(d))) }
	at(5 * time.Second)
	rec.ok("E2", "PUT at +5s succeeds", put("t5"), "")
	at(840 * time.Second)
	rec.ok("E3", "PUT at +840s (60s before expiry) succeeds", put("t840"), "")
	at(930 * time.Second)
	rec.denied("E4", "PUT at +930s (30s after expiry) is rejected", put("t930"))
	// A fresh credential right after must work again (rejection was expiry, not breakage).
	v2, err := assumeRole(cfg, cfg.CPAK, cfg.CPSK, buildSessionPolicy(cfg.Grant), 900)
	if err == nil {
		sc = newCore(cfg.Endpoint, static(v2.AccessKeyID, v2.SecretAccessKey, v2.SessionToken), false)
		err = put("fresh")
	}
	rec.ok("E5", "re-issued credential works after expiry", err, "")
}

// assumeRoleRaw sends AssumeRole without minio-go's DurationSeconds clamp.
func assumeRoleRaw(cfg config, ak, sk, policy string, dur int) (credentials.Value, error) {
	form := url.Values{}
	form.Set("Action", "AssumeRole")
	form.Set("Version", "2011-06-15")
	form.Set("DurationSeconds", fmt.Sprint(dur))
	form.Set("RoleArn", cfg.RoleARN)
	form.Set("RoleSessionName", "agent-probe-expiry")
	form.Set("Policy", policy)
	body := form.Encode()
	req, err := http.NewRequest(http.MethodPost, "http://"+cfg.Endpoint+"/", strings.NewReader(body))
	if err != nil {
		return credentials.Value{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sum := sha256.Sum256([]byte(body))
	req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	req = signer.SignV4STS(*req, ak, sk, "us-east-1")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return credentials.Value{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return credentials.Value{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, oneLine(string(raw)))
	}
	var out struct {
		Result struct {
			Credentials struct {
				AccessKey    string    `xml:"AccessKeyId"`
				SecretKey    string    `xml:"SecretAccessKey"`
				SessionToken string    `xml:"SessionToken"`
				Expiration   time.Time `xml:"Expiration"`
			} `xml:"Credentials"`
		} `xml:"AssumeRoleResult"`
	}
	if err := xml.Unmarshal(raw, &out); err != nil {
		return credentials.Value{}, err
	}
	c := out.Result.Credentials
	return credentials.Value{AccessKeyID: c.AccessKey, SecretAccessKey: c.SecretKey, SessionToken: c.SessionToken, Expiration: c.Expiration}, nil
}

func credFile(cfg config) string { return filepath.Join(cfg.OutDir, cfg.Target+"-stscred.json") }

// stsSave issues a 1h STS credential, proves it works, and persists it so a
// later process can reuse it after the storage service restarts.
func stsSave(ctx context.Context, cfg config, rec *recorder, admin *minio.Core) {
	if err := ensureBucket(ctx, admin, cfg.Grant); err != nil {
		rec.add("H0", "bucket", "FAIL", errText(err))
		return
	}
	v, err := assumeRole(cfg, cfg.CPAK, cfg.CPSK, buildSessionPolicy(cfg.Grant), 3600)
	if err == nil {
		sc := newCore(cfg.Endpoint, static(v.AccessKeyID, v.SecretAccessKey, v.SessionToken), false)
		b := []byte("before")
		_, err = sc.Client.PutObject(ctx, cfg.Grant, "hold/before", bytes.NewReader(b), int64(len(b)), minio.PutObjectOptions{})
	}
	if err == nil {
		raw, _ := json.Marshal(v)
		err = os.WriteFile(credFile(cfg), raw, 0o600)
	}
	rec.ok("H1", "issue STS credential and PUT before restart", err, "")
}

// stsUse reuses the persisted credential after the restart.
func stsUse(ctx context.Context, cfg config, rec *recorder) {
	raw, err := os.ReadFile(credFile(cfg))
	if err != nil {
		rec.add("H2", "state", "FAIL", err.Error())
		return
	}
	var v credentials.Value
	_ = json.Unmarshal(raw, &v)
	sc := newCore(cfg.Endpoint, static(v.AccessKeyID, v.SecretAccessKey, v.SessionToken), false)
	b := []byte("after")
	_, err = sc.Client.PutObject(ctx, cfg.Grant, "hold/after", bytes.NewReader(b), int64(len(b)), minio.PutObjectOptions{})
	rec.ok("H2", "same STS credential still works after storage restart", err, "expires in "+time.Until(v.Expiration).Round(time.Second).String())
}
