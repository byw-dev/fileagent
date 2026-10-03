package main

// Tier-4 benchmark shaped like a small-file collection workload:
// keys obs/{station}/{date}/{n}.bin, 200 stations, ~200 objects per leaf dir.
//
//	load     write objects [start,end) and report the write rate
//	measure  recursive list (with LastModified), delimiter list, leaf list,
//	         StatObject; optionally SeaweedFS filer gRPC traversal

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	stations   = 200
	perLeaf    = 200
	perDay     = stations * perLeaf
	benchStart = "2026-01-01"
)

// benchKey maps an object index to its station/date-shaped key.
func benchKey(i int) string {
	day, _ := time.Parse("2006-01-02", benchStart)
	day = day.AddDate(0, 0, i/perDay)
	return fmt.Sprintf("obs/s%03d/%s/%03d.bin", (i/perLeaf)%stations, day.Format("20060102"), i%perLeaf)
}

func leafOf(i int) string { k := benchKey(i); return k[:strings.LastIndex(k, "/")+1] }

type benchResult map[string]any

func emit(out string, r benchResult) {
	b, _ := json.Marshal(r)
	fmt.Println(string(b))
	if out != "" {
		f, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
}

func pctDur(d []time.Duration, p float64) float64 {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return float64(d[min(len(d)-1, int(p*float64(len(d))))].Microseconds()) / 1000
}

func runLoad(args []string) error {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	ep := fs.String("s3", "localhost:19100", "S3 endpoint")
	bucket := fs.String("bucket", "bench", "bucket")
	start := fs.Int("start", 0, "first index")
	end := fs.Int("end", 100000, "end index (exclusive)")
	conc := fs.Int("c", 32, "concurrency")
	size := fs.Int("size", 2048, "object size")
	target := fs.String("target", "", "label")
	out := fs.String("out", "", "results jsonl")
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
	body := randBody(*size)
	var next, fails atomic.Int64
	next.Store(int64(*start))
	var lmu sync.Mutex
	var lats []time.Duration
	t0 := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var local []time.Duration
			for {
				i := int(next.Add(1)) - 1
				if i >= *end {
					break
				}
				s := time.Now()
				var err error
				for try := 0; try < 3; try++ {
					if _, err = c.PutObject(ctx, *bucket, benchKey(i), bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{}); err == nil {
						break
					}
					time.Sleep(200 * time.Millisecond)
				}
				if err != nil {
					fails.Add(1)
				}
				if i%10 == 0 {
					local = append(local, time.Since(s))
				}
			}
			lmu.Lock()
			lats = append(lats, local...)
			lmu.Unlock()
		}()
	}
	wg.Wait()
	d := time.Since(t0)
	n := *end - *start
	emit(*out, benchResult{"target": *target, "op": "load", "from": *start, "to": *end, "objects": n, "failed": fails.Load(),
		"secs": d.Seconds(), "obj_per_s": float64(n) / d.Seconds(), "put_p50_ms": pctDur(lats, .5), "put_p99_ms": pctDur(lats, .99)})
	return nil
}

func randBody(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(7, 7))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func runMeasure(args []string) error {
	fs := flag.NewFlagSet("measure", flag.ExitOnError)
	ep := fs.String("s3", "localhost:19100", "S3 endpoint")
	bucket := fs.String("bucket", "bench", "bucket")
	total := fs.Int("total", 100000, "objects currently loaded (indices [0,total))")
	target := fs.String("target", "", "label")
	out := fs.String("out", "", "results jsonl")
	listCap := fs.Duration("listcap", 10*time.Minute, "cap for the full recursive listing")
	filer := fs.String("filer", "", "SeaweedFS filer gRPC addr (optional)")
	only := fs.String("only", "", "run a single step: list|delim|leaf|stat|filer")
	fs.Parse(args)
	run := func(step string) bool { return *only == "" || *only == step }
	c, err := s3client(*ep)
	if err != nil {
		return err
	}
	ctx := context.Background()
	base := benchResult{"target": *target, "total": *total}
	with := func(kv ...any) benchResult {
		r := benchResult{}
		for k, v := range base {
			r[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			r[kv[i].(string)] = kv[i+1]
		}
		return r
	}

	// 1) full recursive listing with LastModified (minio-inventory's pain point).
	if run("list") {
		lctx, cancel := context.WithTimeout(ctx, *listCap)
		t0 := time.Now()
		var n int
		var firstPage time.Duration
		var lastErr error
		for o := range c.ListObjects(lctx, *bucket, minio.ListObjectsOptions{Prefix: "obs/", Recursive: true}) {
			if o.Err != nil {
				lastErr = o.Err
				break
			}
			if o.LastModified.IsZero() {
				lastErr = fmt.Errorf("LastModified missing for %s", o.Key)
				break
			}
			n++
			if n == 1000 {
				firstPage = time.Since(t0)
			}
		}
		cancel()
		d := time.Since(t0)
		emit(*out, with("op", "list_recursive", "listed", n, "complete", n == *total, "secs", d.Seconds(),
			"obj_per_s", float64(n)/d.Seconds(), "first_1000_ms", float64(firstPage.Microseconds())/1000, "err", fmt.Sprint(lastErr)))

	}
	// 2) one delimiter level: stations under obs/.
	if run("delim") {
		var dl []time.Duration
		var prefixes int
		for k := 0; k < 5; k++ {
			s := time.Now()
			prefixes = 0
			for o := range c.ListObjects(ctx, *bucket, minio.ListObjectsOptions{Prefix: "obs/", Recursive: false}) {
				if o.Err == nil {
					prefixes++
				}
			}
			dl = append(dl, time.Since(s))
		}
		emit(*out, with("op", "list_delimiter_one_level", "entries", prefixes, "p50_ms", pctDur(dl, .5), "max_ms", pctDur(dl, 1)))

	}
	// 3) leaf directory listings (~200 objects each), random leaves.
	if run("leaf") {
		r := rand.New(rand.NewPCG(uint64(*total), 3))
		var ll []time.Duration
		var leafObjs int
		for k := 0; k < 50; k++ {
			leaf := leafOf(r.IntN(*total))
			s := time.Now()
			cnt := 0
			for o := range c.ListObjects(ctx, *bucket, minio.ListObjectsOptions{Prefix: leaf, Recursive: true}) {
				if o.Err == nil {
					cnt++
				}
			}
			ll = append(ll, time.Since(s))
			leafObjs += cnt
		}
		emit(*out, with("op", "list_leaf", "leaves", 50, "avg_objects", leafObjs/50, "p50_ms", pctDur(ll, .5), "p99_ms", pctDur(ll, .99)))

	}
	// 4) StatObject on random keys, c=16.
	if run("stat") {
		const stats = 4000
		var sl []time.Duration
		var smu sync.Mutex
		var sfail atomic.Int64
		var idx atomic.Int64
		t0 := time.Now()
		var wg sync.WaitGroup
		for w := 0; w < 16; w++ {
			wg.Add(1)
			go func(seed uint64) {
				defer wg.Done()
				rr := rand.New(rand.NewPCG(seed, uint64(*total)))
				var local []time.Duration
				for idx.Add(1) <= stats {
					s := time.Now()
					if _, err := c.StatObject(ctx, *bucket, benchKey(rr.IntN(*total)), minio.StatObjectOptions{}); err != nil {
						sfail.Add(1)
					}
					local = append(local, time.Since(s))
				}
				smu.Lock()
				sl = append(sl, local...)
				smu.Unlock()
			}(uint64(w))
		}
		wg.Wait()
		d := time.Since(t0)
		emit(*out, with("op", "stat_random", "n", stats, "failed", sfail.Load(), "ops_per_s", float64(stats)/d.Seconds(), "p50_ms", pctDur(sl, .5), "p99_ms", pctDur(sl, .99)))

	}
	// 5) SeaweedFS: walk the filer tree directly over gRPC (metadata store scan).
	if *filer != "" && run("filer") {
		res, err := filerWalk(ctx, *filer, "/buckets/"+*bucket+"/obs", *listCap)
		r := with("op", "filer_grpc_walk")
		for k, v := range res {
			r[k] = v
		}
		r["err"] = fmt.Sprint(err)
		emit(*out, r)
	}
	return nil
}

// filerWalk lists every file under root with concurrent ListEntries calls.
func filerWalk(ctx context.Context, addr, root string, limit time.Duration) (benchResult, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20)))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	cl := filer_pb.NewSeaweedFilerClient(conn)
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	dirs := make(chan string, 1<<20)
	var pending sync.WaitGroup
	var files, ndirs atomic.Int64
	var firstErr atomic.Value
	pending.Add(1)
	dirs <- root
	t0 := time.Now()
	for w := 0; w < 16; w++ {
		go func() {
			for dir := range dirs {
				last := ""
				for {
					st, err := cl.ListEntries(ctx, &filer_pb.ListEntriesRequest{Directory: dir, StartFromFileName: last, Limit: 10000})
					if err != nil {
						firstErr.CompareAndSwap(nil, err)
						break
					}
					got := 0
					for {
						resp, err := st.Recv()
						if err == io.EOF {
							break
						}
						if err != nil {
							firstErr.CompareAndSwap(nil, err)
							break
						}
						got++
						e := resp.Entry
						last = e.Name
						if e.IsDirectory {
							ndirs.Add(1)
							pending.Add(1)
							dirs <- dir + "/" + e.Name
						} else {
							files.Add(1)
						}
					}
					if got < 10000 {
						break
					}
				}
				pending.Done()
			}
		}()
	}
	pending.Wait()
	close(dirs)
	d := time.Since(t0)
	var e error
	if v := firstErr.Load(); v != nil {
		e = v.(error)
	}
	return benchResult{"listed": files.Load(), "dirs": ndirs.Load(), "secs": d.Seconds(), "obj_per_s": float64(files.Load()) / d.Seconds()}, e
}
