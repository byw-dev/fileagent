package dirstore_test

import (
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/dirstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_RegisterAndDeliver(t *testing.T) {
	s := dirstore.New()
	ch := s.Register("req-1", "agent-1")

	result := dirstore.Result{Entries: []dirstore.DirEntry{{Name: "a", Path: "/a"}}}
	s.Deliver("req-1", "agent-1", result)

	select {
	case got := <-ch:
		require.Len(t, got.Entries, 1)
		assert.Equal(t, "a", got.Entries[0].Name)
	case <-time.After(time.Second):
		t.Fatal("timeout: result not delivered")
	}
}

func TestStore_Cancel_NoLeak(t *testing.T) {
	s := dirstore.New()
	_ = s.Register("req-2", "agent-1")
	s.Cancel("req-2")
	// Deliver after cancel should not panic or block.
	s.Deliver("req-2", "agent-1", dirstore.Result{})
}

func TestStore_Deliver_UnknownID_IsNoop(t *testing.T) {
	s := dirstore.New()
	// Should not panic, and must report refusal for an unregistered id.
	if s.Deliver("nonexistent", "agent-1", dirstore.Result{}) {
		t.Fatal("Deliver reported success for an unregistered request id")
	}
}

func TestStore_Deliver_CallerTimedOut_IsNoop(t *testing.T) {
	s := dirstore.New()
	ch := s.Register("req-3", "agent-1")
	s.Cancel("req-3")

	// Deliver after cancel: no panic, nothing readable.
	s.Deliver("req-3", "agent-1", dirstore.Result{})

	select {
	case <-ch:
		t.Fatal("unexpected result on cancelled channel")
	default:
	}
}

func TestStore_ConcurrentDelivers(t *testing.T) {
	s := dirstore.New()
	ch := s.Register("req-4", "agent-1")

	result := dirstore.Result{}
	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			s.Deliver("req-4", "agent-1", result)
			done <- struct{}{}
		}()
	}
	<-done
	<-done

	// Exactly one delivery should have reached the channel.
	count := 0
	for {
		select {
		case <-ch:
			count++
		default:
			require.Equal(t, 1, count)
			return
		}
	}
}

// The recipient check is the invariant this package exists to protect (IC-SEC-2
// ② / IC-BUG-27), so it needs a test here and not only in the grpcserver
// package that calls it.
func TestStore_Deliver_WrongAgent_IsRefused(t *testing.T) {
	s := dirstore.New()
	ch := s.Register("req-1", "agent-a")

	if s.Deliver("req-1", "agent-b", dirstore.Result{}) {
		t.Fatal("Deliver accepted a result from an agent the request was not sent to")
	}
	select {
	case <-ch:
		t.Fatal("the waiting caller received a result from the wrong agent")
	default:
	}

	// The entry survives the refusal: the rightful agent can still deliver.
	if !s.Deliver("req-1", "agent-a", dirstore.Result{}) {
		t.Fatal("a refused delivery must not consume the pending entry")
	}
}

func TestStore_Deliver_UnknownRequest_ReturnsFalse(t *testing.T) {
	if dirstore.New().Deliver("nope", "agent-a", dirstore.Result{}) {
		t.Fatal("Deliver reported success for an unregistered request id")
	}
}
