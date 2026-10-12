// Command eventsource runs the B2 experiments behind D-037 §5 (storage event source):
//
//	watch  subscribe to filer metadata (local or aggregated stream, idle heartbeats on)
//	       and log every response — events, heartbeats, flush reports — as JSONL
//	load   write objects in bursts separated by idle gaps (so heartbeats get a chance)
//	ops    run a labelled sequence of S3 operations to record how each translates to events
//
// Analysis lives in analyze.py. Nothing here is product code.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
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

// record is one subscription response as observed by the client.
type record struct {
	Kind      string `json:"kind"` // event | heartbeat | flush
	TsNs      int64  `json:"ts_ns"`
	RecvNs    int64  `json:"recv_ns"`
	FlushedNs int64  `json:"flushed_ns,omitempty"`
	Dir       string `json:"dir,omitempty"`
	Old       string `json:"old,omitempty"`
	New       string `json:"new,omitempty"`
	OldIsDir  bool   `json:"old_is_dir,omitempty"`
	NewIsDir  bool   `json:"new_is_dir,omitempty"`
	NewParent string `json:"new_parent,omitempty"`
	Size      uint64 `json:"size,omitempty"`
	DelChunks bool   `json:"delete_chunks,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: eventsource <watch|load|ops> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "watch":
		err = runWatch(os.Args[2:])
	case "load":
		err = runLoad(os.Args[2:])
	case "ops":
		err = runOps(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	filer := fs.String("filer", "localhost:19488", "filer gRPC address")
	prefix := fs.String("prefix", "/buckets/", "path prefix")
	aggregated := fs.Bool("aggregated", false, "use SubscribeMetadata (aggregated) instead of SubscribeLocalMetadata")
	since := fs.Int64("since", 0, "since_ns (0 = now)")
	out := fs.String("out", "watch.jsonl", "output JSONL")
	fs.Parse(args)

	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	conn, err := grpc.NewClient(*filer, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20)))
	if err != nil {
		return err
	}
	defer conn.Close()
	sinceNs := *since
	if sinceNs == 0 {
		sinceNs = time.Now().UnixNano()
	}
	req := &filer_pb.SubscribeMetadataRequest{
		ClientName: "fileagent-b2", PathPrefix: *prefix, SinceNs: sinceNs,
		ClientId: int32(time.Now().UnixNano() & 0x7fffffff), ClientEpoch: int32(time.Now().Unix()),
		ClientSupportsIdleHeartbeat: true,
	}
	client := filer_pb.NewSeaweedFilerClient(conn)
	type recv interface {
		Recv() (*filer_pb.SubscribeMetadataResponse, error)
	}
	var stream recv
	if *aggregated {
		stream, err = client.SubscribeMetadata(ctx, req)
	} else {
		stream, err = client.SubscribeLocalMetadata(ctx, req)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[watch] aggregated=%v since=%d\n", *aggregated, sinceNs)
	var n int
	for {
		resp, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintf(os.Stderr, "[watch] stopped after %d records\n", n)
				return nil
			}
			return err
		}
		now := time.Now().UnixNano()
		for _, r := range append([]*filer_pb.SubscribeMetadataResponse{resp}, resp.Events...) {
			rec := toRecord(r, now)
			b, _ := json.Marshal(rec)
			w.Write(append(b, '\n'))
			n++
		}
		if n%1000 == 0 {
			w.Flush()
		}
	}
}

func toRecord(r *filer_pb.SubscribeMetadataResponse, recvNs int64) record {
	en := r.EventNotification
	if en == nil {
		if r.TsNs == 0 {
			return record{Kind: "flush", RecvNs: recvNs, FlushedNs: r.FlushedTsNs}
		}
		return record{Kind: "heartbeat", TsNs: r.TsNs, RecvNs: recvNs, FlushedNs: r.FlushedTsNs}
	}
	rec := record{Kind: "event", TsNs: r.TsNs, RecvNs: recvNs, Dir: r.Directory, NewParent: en.NewParentPath, DelChunks: en.DeleteChunks}
	if o := en.OldEntry; o != nil {
		rec.Old, rec.OldIsDir = o.Name, o.IsDirectory
	}
	if n := en.NewEntry; n != nil {
		rec.New, rec.NewIsDir = n.Name, n.IsDirectory
		if n.Attributes != nil {
			rec.Size = n.Attributes.FileSize
		}
	}
	if rec.Old == "" && rec.New == "" && r.TsNs != 0 && r.Directory == "" {
		rec.Kind = "heartbeat"
	}
	return rec
}

func s3client(ep string) (*minio.Client, error) {
	return minio.New(ep, &minio.Options{Creds: credentials.NewStaticV4("b2admin", "b2admin-secret", "")})
}

func runLoad(args []string) error {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	ep := fs.String("s3", "localhost:19400", "S3 endpoint")
	bucket := fs.String("bucket", "load", "bucket")
	conc := fs.Int("c", 16, "concurrent writers")
	burst := fs.Duration("burst", 5*time.Second, "burst length")
	idle := fs.Duration("idle", 7*time.Second, "idle gap between bursts")
	rounds := fs.Int("rounds", 5, "burst rounds")
	size := fs.Int("size", 2048, "object size")
	out := fs.String("out", "writes.jsonl", "write log")
	delay := fs.Duration("delay", 0, "pause between writes per writer (light continuous load)")
	fs.Parse(args)

	c, err := s3client(*ep)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if ok, _ := c.BucketExists(ctx, *bucket); !ok {
		if err := c.MakeBucket(ctx, *bucket, minio.MakeBucketOptions{}); err != nil {
			return err
		}
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	var mu sync.Mutex
	enc := json.NewEncoder(f)
	body := bytes.Repeat([]byte("x"), *size)
	var seq, okN, failN atomic.Int64
	for round := 0; round < *rounds; round++ {
		deadline := time.Now().Add(*burst)
		var wg sync.WaitGroup
		for i := 0; i < *conc; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(deadline) {
					key := fmt.Sprintf("r%02d/%08d.bin", round, seq.Add(1))
					_, err := c.PutObject(ctx, *bucket, key, bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{})
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
		fmt.Fprintf(os.Stderr, "[load] round %d done: ok=%d fail=%d\n", round, okN.Load(), failN.Load())
		if round < *rounds-1 {
			time.Sleep(*idle)
		}
	}
	return nil
}

// runOps performs one labelled operation at a time with a pause in between, logging
// the wall-clock window of each so analyze.py can attribute events to operations.
func runOps(args []string) error {
	fs := flag.NewFlagSet("ops", flag.ExitOnError)
	ep := fs.String("s3", "localhost:19400", "S3 endpoint")
	out := fs.String("out", "ops.jsonl", "operation log")
	pause := fs.Duration("pause", 1500*time.Millisecond, "pause after each operation")
	fs.Parse(args)

	c, err := s3client(*ep)
	if err != nil {
		return err
	}
	ctx := context.Background()
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	step := func(name string, fn func() error) {
		start := time.Now().UnixNano()
		err := fn()
		end := time.Now().UnixNano()
		rec := map[string]any{"op": name, "start_ns": start, "end_ns": end, "ok": err == nil}
		if err != nil {
			rec["err"] = err.Error()
		}
		enc.Encode(rec)
		fmt.Fprintf(os.Stderr, "[ops] %-34s ok=%v %v\n", name, err == nil, errOr(err))
		time.Sleep(*pause)
	}
	small := func() *bytes.Reader { return bytes.NewReader([]byte("hello")) }
	big := bytes.Repeat([]byte("m"), 12<<20) // 12MiB, 5MiB parts → 3 parts

	const b = "ops"
	step("make_bucket", func() error { return c.MakeBucket(ctx, b, minio.MakeBucketOptions{}) })
	step("put_new a/b/c.txt", func() error {
		_, err := c.PutObject(ctx, b, "a/b/c.txt", small(), 5, minio.PutObjectOptions{})
		return err
	})
	step("overwrite a/b/c.txt", func() error {
		_, err := c.PutObject(ctx, b, "a/b/c.txt", bytes.NewReader([]byte("hello world")), 11, minio.PutObjectOptions{})
		return err
	})
	step("put_empty_dir_marker d/", func() error {
		_, err := c.PutObject(ctx, b, "d/", bytes.NewReader(nil), 0, minio.PutObjectOptions{})
		return err
	})
	step("multipart big.bin", func() error {
		_, err := c.PutObject(ctx, b, "mp/big.bin", bytes.NewReader(big), int64(len(big)),
			minio.PutObjectOptions{PartSize: 5 << 20, DisableContentSha256: true})
		return err
	})
	step("multipart_aborted aborted.bin", func() error {
		core := minio.Core{Client: c}
		id, err := core.NewMultipartUpload(ctx, b, "mp/aborted.bin", minio.PutObjectOptions{})
		if err != nil {
			return err
		}
		if _, err := core.PutObjectPart(ctx, b, "mp/aborted.bin", id, 1, bytes.NewReader(big[:5<<20]), 5<<20, minio.PutObjectPartOptions{}); err != nil {
			return err
		}
		return core.AbortMultipartUpload(ctx, b, "mp/aborted.bin", id)
	})
	step("copy a/b/c.txt -> copy/c.txt", func() error {
		_, err := c.CopyObject(ctx, minio.CopyDestOptions{Bucket: b, Object: "copy/c.txt"}, minio.CopySrcOptions{Bucket: b, Object: "a/b/c.txt"})
		return err
	})
	step("delete a/b/c.txt", func() error { return c.RemoveObject(ctx, b, "a/b/c.txt", minio.RemoveObjectOptions{}) })
	step("delete_missing nope.txt", func() error { return c.RemoveObject(ctx, b, "nope.txt", minio.RemoveObjectOptions{}) })
	step("delete_bucket_nonempty", func() error { return c.RemoveBucket(ctx, b) })
	// Empty the bucket, then delete it: the D-037 flow.
	step("delete remaining objects", func() error {
		for o := range c.ListObjects(ctx, b, minio.ListObjectsOptions{Recursive: true}) {
			if o.Err != nil {
				return o.Err
			}
			if err := c.RemoveObject(ctx, b, o.Key, minio.RemoveObjectOptions{}); err != nil {
				return err
			}
		}
		return nil
	})
	step("delete_bucket_empty", func() error { return c.RemoveBucket(ctx, b) })
	return nil
}

func errOr(err error) string {
	if err == nil {
		return ""
	}
	return strings.SplitN(err.Error(), "\n", 2)[0]
}
