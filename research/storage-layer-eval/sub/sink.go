package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
)

// runSink records every S3-format webhook record as {event, key} JSON lines.
func runSink(args []string) error {
	fs := flag.NewFlagSet("sink", flag.ExitOnError)
	addr := fs.String("addr", ":18990", "listen address")
	out := fs.String("out", "sink.jsonl", "output")
	fs.Parse(args)
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	var mu sync.Mutex
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Records []struct {
				EventName string `json:"eventName"`
				S3        struct {
					Object struct {
						Key string `json:"key"`
					} `json:"object"`
				} `json:"s3"`
			} `json:"Records"`
		}
		_ = json.Unmarshal(body, &env)
		mu.Lock()
		for _, rec := range env.Records {
			k, _ := url.QueryUnescape(rec.S3.Object.Key)
			b, _ := json.Marshal(map[string]string{"event": rec.EventName, "key": k})
			f.Write(append(b, '\n'))
		}
		f.Sync()
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	fmt.Fprintln(os.Stderr, "[sink] listening on", *addr)
	return http.ListenAndServe(*addr, nil)
}
