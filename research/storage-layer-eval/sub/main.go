// Command sub evaluates SeaweedFS filer metadata subscription as the CP's
// inbound storage-event channel (replacing MinIO webhook / D-031 JetStream).
//
//	sub     subscribe from a persisted ts_ns cursor, append events to a JSONL log
//	write   write N small objects (records completion time per key)
//	causal  race put/delete on the same keys from several goroutines
//	check   replay the event log and compare final state with S3 truth
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/signal"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// event is what the CP adapter would persist per storage change.
type event struct {
	TsNs   int64  `json:"ts_ns"`
	RecvNs int64  `json:"recv_ns"`
	Type   string `json:"type"` // create|update|delete|rename
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	OldKey string `json:"old_key,omitempty"`
	Size   uint64 `json:"size"`
	ETag   string `json:"etag,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sub <sub|write|causal|check|load|measure|sink> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "sub":
		err = runSub(os.Args[2:])
	case "write":
		err = runWrite(os.Args[2:])
	case "causal":
		err = runCausal(os.Args[2:])
	case "check":
		err = runCheck(os.Args[2:])
	case "sink":
		err = runSink(os.Args[2:])
	case "load":
		err = runLoad(os.Args[2:])
	case "measure":
		err = runMeasure(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ------------------------------------------------------------------ sub

func readCursor(p string) int64 {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v
}

func writeCursor(p string, v int64) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(v, 10)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// toEvent maps a filer notification to an object event, dropping directories,
// multipart staging (.uploads) and anything outside /buckets/<bucket>/.
func toEvent(resp *filer_pb.SubscribeMetadataResponse) *event {
	n := resp.EventNotification
	if n == nil {
		return nil
	}
	split := func(full string) (string, string, bool) {
		p := strings.TrimPrefix(full, "/buckets/")
		if p == full {
			return "", "", false
		}
		parts := strings.SplitN(p, "/", 2)
		if len(parts) != 2 || strings.HasPrefix(parts[0], ".") || strings.HasPrefix(parts[1], ".uploads/") || parts[1] == ".uploads" {
			return "", "", false
		}
		return parts[0], parts[1], true
	}
	e := &event{TsNs: resp.TsNs, RecvNs: time.Now().UnixNano()}
	switch {
	case n.OldEntry == nil && n.NewEntry != nil:
		if n.NewEntry.IsDirectory {
			return nil
		}
		e.Type = "create"
	case n.OldEntry != nil && n.NewEntry == nil:
		if n.OldEntry.IsDirectory {
			return nil
		}
		e.Type = "delete"
	case n.OldEntry != nil && n.NewEntry != nil:
		if n.NewEntry.IsDirectory {
			return nil
		}
		newDir := n.NewParentPath
		if newDir == "" {
			newDir = resp.Directory
		}
		if newDir == resp.Directory && n.OldEntry.Name == n.NewEntry.Name {
			e.Type = "update"
		} else {
			e.Type = "rename"
			_, ok, _ := split(path.Join(resp.Directory, n.OldEntry.Name))
			e.OldKey = ok
		}
	default:
		return nil
	}
	var full string
	if e.Type == "delete" {
		full = path.Join(resp.Directory, n.OldEntry.Name)
	} else {
		dir := n.NewParentPath
		if dir == "" {
			dir = resp.Directory
		}
		full = path.Join(dir, n.NewEntry.Name)
		if n.NewEntry.Attributes != nil {
			e.Size = n.NewEntry.Attributes.FileSize
		}
		if v, ok := n.NewEntry.Extended["Seaweed-X-Amz-ETag"]; ok {
			e.ETag = string(v)
		}
	}
	b, k, ok := split(full)
	if !ok {
		return nil
	}
	e.Bucket, e.Key = b, k
	return e
}

func runSub(args []string) error {
	fs := flag.NewFlagSet("sub", flag.ExitOnError)
	filer := fs.String("filer", "localhost:19288", "filer gRPC address")
	prefix := fs.String("prefix", "/buckets/", "path prefix")
	cursorPath := fs.String("cursor", "cursor.txt", "cursor file")
	out := fs.String("out", "events.jsonl", "event log")
	since := fs.Int64("since", -1, "override start ts_ns (-1 = cursor, or now if none)")
	lookback := fs.Duration("lookback", 5*time.Second, "rewind on (re)subscribe; duplicates removed by identity")
	syncEvery := fs.Duration("sync", 200*time.Millisecond, "batch fsync/cursor interval")
	fs.Parse(args)

	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)

	cursor := readCursor(*cursorPath)
	if *since >= 0 {
		cursor = *since
	} else if cursor == 0 {
		cursor = time.Now().UnixNano()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var total, kept, dup, inversions int64
	lastSync := time.Now()
	seen := map[string]int64{}
	clientID := rand.Int32()
	for ctx.Err() == nil {
		conn, err := grpc.NewClient(*filer, grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20)))
		if err != nil {
			return err
		}
		start := time.Now()
		stream, err := filer_pb.NewSeaweedFilerClient(conn).SubscribeMetadata(ctx, &filer_pb.SubscribeMetadataRequest{
			ClientName: "fileagent-eval", PathPrefix: *prefix, SinceNs: cursor - lookback.Nanoseconds(), ClientId: clientID, ClientEpoch: int32(time.Now().Unix()),
		})
		if err == nil {
			fmt.Fprintf(os.Stderr, "[sub] subscribed since_ns=%d (%s)\n", cursor, time.Unix(0, cursor).UTC().Format(time.RFC3339Nano))
			for {
				var resp *filer_pb.SubscribeMetadataResponse
				resp, err = stream.Recv()
				if err != nil {
					break
				}
				batch := append([]*filer_pb.SubscribeMetadataResponse{resp}, resp.Events...)
				for _, r := range batch {
					total++
					// The stream is NOT strictly ts-ordered under concurrent writes
					// (observed ~0.1ms inversions), so the durable cursor is the max
					// ts seen, resubscription rewinds by -lookback, and duplicates are
					// removed by identity rather than by "ts <= cursor".
					id := fmt.Sprintf("%d|%s|%v|%v", r.TsNs, r.Directory, r.EventNotification.GetOldEntry().GetName(), r.EventNotification.GetNewEntry().GetName())
					if _, ok := seen[id]; ok {
						dup++
						continue
					}
					seen[id] = r.TsNs
					if r.TsNs < cursor {
						inversions++
					}
					if e := toEvent(r); e != nil {
						b, _ := json.Marshal(e)
						w.Write(append(b, '\n'))
						kept++
					}
					cursor = max(cursor, r.TsNs)
				}
				for k, ts := range seen { // bound the dedupe set to twice the rewind window
					if ts < cursor-2*lookback.Nanoseconds() {
						delete(seen, k)
					}
				}
				// Persist the event log BEFORE advancing the durable cursor, in
				// batches (per-event fsync caps throughput at ~200 events/s).
				if time.Since(lastSync) >= *syncEvery {
					if err = w.Flush(); err == nil {
						err = f.Sync()
					}
					if err == nil {
						err = writeCursor(*cursorPath, cursor)
					}
					if err != nil {
						break
					}
					lastSync = time.Now()
				}
			}
		}
		conn.Close()
		if ctx.Err() != nil {
			break
		}
		fmt.Fprintf(os.Stderr, "[sub] stream ended after %s: %v — reconnecting from %d\n", time.Since(start).Round(time.Millisecond), err, cursor)
		time.Sleep(time.Second)
	}
	w.Flush()
	f.Sync()
	writeCursor(*cursorPath, cursor)
	fmt.Fprintf(os.Stderr, "[sub] exit: received=%d kept=%d duplicates=%d ts_inversions=%d cursor=%d\n", total, kept, dup, inversions, cursor)
	return nil
}

// ------------------------------------------------------------------ S3 helpers

func s3client(ep string) (*minio.Client, error) {
	return minio.New(ep, &minio.Options{Creds: credentials.NewStaticV4("minioadmin", "minioadmin", "")})
}

// ------------------------------------------------------------------ write

func runWrite(args []string) error {
	fs := flag.NewFlagSet("write", flag.ExitOnError)
	ep := fs.String("s3", "localhost:19200", "S3 endpoint")
	bucket := fs.String("bucket", "probe-grant", "bucket")
	prefix := fs.String("prefix", "exp/", "key prefix")
	n := fs.Int("n", 100, "objects")
	conc := fs.Int("c", 1, "concurrency")
	delay := fs.Duration("delay", 0, "delay between writes per worker")
	size := fs.Int("size", 1024, "object size")
	out := fs.String("out", "writes.jsonl", "write log (key, done_ns, ok)")
	retry := fs.Duration("retry", 0, "keep retrying a failed write for this long")
	fs.Parse(args)

	c, err := s3client(*ep)
	if err != nil {
		return err
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	var mu sync.Mutex
	enc := json.NewEncoder(f)
	body := bytes.Repeat([]byte("x"), *size)
	var next, okN, failN atomic.Int64
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < *conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= *n {
					return
				}
				key := fmt.Sprintf("%s%06d", *prefix, i)
				deadline := time.Now().Add(*retry)
				var err error
				for {
					_, err = c.PutObject(context.Background(), *bucket, key, bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{})
					if err == nil || time.Now().After(deadline) {
						break
					}
					time.Sleep(500 * time.Millisecond)
				}
				mu.Lock()
				enc.Encode(map[string]any{"key": key, "done_ns": time.Now().UnixNano(), "ok": err == nil})
				mu.Unlock()
				if err != nil {
					failN.Add(1)
				} else {
					okN.Add(1)
				}
				if *delay > 0 {
					time.Sleep(*delay)
				}
			}
		}()
	}
	wg.Wait()
	d := time.Since(start)
	fmt.Printf("[write] %d ok, %d failed in %s (%.0f obj/s)\n", okN.Load(), failN.Load(), d.Round(time.Millisecond), float64(okN.Load())/d.Seconds())
	return nil
}

// ------------------------------------------------------------------ causal

func runCausal(args []string) error {
	fs := flag.NewFlagSet("causal", flag.ExitOnError)
	ep := fs.String("s3", "localhost:19200", "S3 endpoint")
	bucket := fs.String("bucket", "probe-grant", "bucket")
	prefix := fs.String("prefix", "causal/", "key prefix")
	keys := fs.Int("keys", 50, "distinct keys")
	ops := fs.Int("ops", 2000, "total ops")
	workers := fs.Int("w", 8, "concurrent workers")
	fs.Parse(args)
	c, err := s3client(*ep)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	var done, putOK, putErr, delOK, delErr atomic.Int64
	errs := sync.Map{}
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(seed, 1))
			for done.Add(1) <= int64(*ops) {
				key := fmt.Sprintf("%sk%03d", *prefix, r.IntN(*keys))
				if r.IntN(3) == 0 {
					if err := c.RemoveObject(context.Background(), *bucket, key, minio.RemoveObjectOptions{}); err != nil {
						delErr.Add(1)
						errs.Store(err.Error(), true)
					} else {
						delOK.Add(1)
					}
				} else {
					b := bytes.Repeat([]byte("v"), 1+r.IntN(4096)) // size distinguishes versions
					if _, err := c.PutObject(context.Background(), *bucket, key, bytes.NewReader(b), int64(len(b)), minio.PutObjectOptions{}); err != nil {
						putErr.Add(1)
						errs.Store(err.Error(), true)
					} else {
						putOK.Add(1)
					}
				}
			}
		}(uint64(w + 1))
	}
	wg.Wait()
	fmt.Printf("[causal] %d ops over %d keys with %d workers: put ok=%d err=%d, delete ok=%d err=%d\n", *ops, *keys, *workers, putOK.Load(), putErr.Load(), delOK.Load(), delErr.Load())
	errs.Range(func(k, _ any) bool { fmt.Println("[causal] error:", k); return true })
	return nil
}

// ------------------------------------------------------------------ check

func loadEvents(p string) ([]event, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var evs []event
	dec := json.NewDecoder(f)
	for {
		var e event
		if err := dec.Decode(&e); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		evs = append(evs, e)
	}
	return evs, nil
}

func pct(d []float64, p float64) float64 {
	if len(d) == 0 {
		return 0
	}
	sort.Float64s(d)
	return d[min(len(d)-1, int(p*float64(len(d))))]
}

func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	ep := fs.String("s3", "localhost:19200", "S3 endpoint")
	bucket := fs.String("bucket", "probe-grant", "bucket")
	prefix := fs.String("prefix", "exp/", "key prefix to check")
	evPath := fs.String("events", "events.jsonl", "event log")
	writes := fs.String("writes", "", "write log (optional: completeness + lag)")
	fs.Parse(args)

	evs, err := loadEvents(*evPath)
	if err != nil {
		return err
	}
	// Order check: ts_ns must be non-decreasing in arrival order.
	var regress int
	for i := 1; i < len(evs); i++ {
		if evs[i].TsNs < evs[i-1].TsNs {
			regress++
		}
	}
	// Replay into final state, and count duplicates (same ts_ns+key twice).
	state := map[string]event{}
	seen := map[string]int{}
	var inScope int
	for _, e := range evs {
		if e.Bucket != *bucket || !strings.HasPrefix(e.Key, *prefix) {
			continue
		}
		inScope++
		seen[fmt.Sprintf("%d|%s|%s", e.TsNs, e.Type, e.Key)]++
		switch e.Type {
		case "delete":
			delete(state, e.Key)
		case "rename":
			delete(state, e.OldKey)
			state[e.Key] = e
		default:
			state[e.Key] = e
		}
	}
	var dups int
	for _, n := range seen {
		dups += n - 1
	}

	c, err := s3client(*ep)
	if err != nil {
		return err
	}
	truth := map[string]int64{}
	for o := range c.ListObjects(context.Background(), *bucket, minio.ListObjectsOptions{Prefix: *prefix, Recursive: true}) {
		if o.Err != nil {
			return o.Err
		}
		truth[o.Key] = o.Size
	}
	var missing, ghost, sizeDiff []string
	for k, sz := range truth {
		e, ok := state[k]
		if !ok {
			missing = append(missing, k)
		} else if int64(e.Size) != sz {
			sizeDiff = append(sizeDiff, fmt.Sprintf("%s(replay %d, truth %d)", k, e.Size, sz))
		}
	}
	for k := range state {
		if _, ok := truth[k]; !ok {
			ghost = append(ghost, k)
		}
	}
	fmt.Printf("[check] events in scope=%d, ts regressions=%d, duplicate deliveries=%d\n", inScope, regress, dups)
	fmt.Printf("[check] truth objects=%d, replayed objects=%d\n", len(truth), len(state))
	show := func(name string, l []string) {
		n := len(l)
		sort.Strings(l)
		if n > 5 {
			l = append(l[:5], "…")
		}
		fmt.Printf("[check] %-28s %d %v\n", name, n, l)
	}
	show("MISSING (in S3, not replay):", missing)
	show("GHOST (in replay, not S3):", ghost)
	show("SIZE MISMATCH:", sizeDiff)

	if *writes != "" {
		f, err := os.Open(*writes)
		if err != nil {
			return err
		}
		defer f.Close()
		first := map[string]event{}
		for _, e := range evs {
			if _, ok := first[e.Key]; !ok && e.Type != "delete" {
				first[e.Key] = e
			}
		}
		var lags, srvLags []float64
		var okW, noEvent int
		var lastRecv int64
		dec := json.NewDecoder(f)
		var lastDone int64
		for {
			var w struct {
				Key    string `json:"key"`
				DoneNs int64  `json:"done_ns"`
				OK     bool   `json:"ok"`
			}
			if err := dec.Decode(&w); err != nil {
				break
			}
			if !w.OK {
				continue
			}
			okW++
			lastDone = max(lastDone, w.DoneNs)
			e, ok := first[w.Key]
			if !ok {
				noEvent++
				continue
			}
			lags = append(lags, float64(e.RecvNs-w.DoneNs)/1e6)
			srvLags = append(srvLags, float64(e.RecvNs-e.TsNs)/1e6)
			lastRecv = max(lastRecv, e.RecvNs)
		}
		fmt.Printf("[check] acknowledged writes=%d, without any event=%d\n", okW, noEvent)
		fmt.Printf("[check] lag write-ack→event received (ms): p50=%.1f p99=%.1f max=%.1f\n", pct(lags, .5), pct(lags, .99), pct(lags, 1))
		fmt.Printf("[check] lag filer ts_ns→event received (ms): p50=%.1f p99=%.1f\n", pct(srvLags, .5), pct(srvLags, .99))
		if lastRecv > 0 {
			fmt.Printf("[check] catch-up: last event received %.0f ms after last write ack\n", float64(lastRecv-lastDone)/1e6)
		}
	}
	return nil
}
