// Command faultproxy is a minimal killable TCP proxy used by the PR3b fault
// injection scenarios to simulate a broken CP ↔ filer gRPC path (scenario 1):
// killing the proxy severs the stream exactly like a network partition, and
// restarting it lets the CP's resubscribe loop reconnect.
//
// Usage: faultproxy <listen-addr> <target-addr>
// It is deliberately dumb (pure byte relay, no gRPC awareness).
package main

import (
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

func main() {
	if len(os.Args) != 3 {
		log.Fatalf("usage: faultproxy <listen-addr> <target-addr>")
	}
	ln, err := net.Listen("tcp", os.Args[1])
	if err != nil {
		log.Fatalf("listen %s: %v", os.Args[1], err)
	}
	log.Printf("faultproxy %s -> %s ready", os.Args[1], os.Args[2])
	var mu sync.Mutex
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			return
		}
		mu.Lock()
		wg.Add(1)
		mu.Unlock()
		go func() {
			defer wg.Done()
			relay(conn, os.Args[2])
		}()
	}
}

// relay pumps both directions until either side closes.
func relay(client net.Conn, target string) {
	defer client.Close()
	upstream, err := net.Dial("tcp", target)
	if err != nil {
		log.Printf("dial %s: %v", target, err)
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
	// Unblock the other direction promptly instead of waiting for TCP to
	// notice; half-open pairs must not linger.
	_ = upstream.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	_ = client.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	<-done
}
