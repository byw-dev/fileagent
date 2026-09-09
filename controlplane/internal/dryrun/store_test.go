package dryrun_test

import (
	"testing"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/dryrun"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_RegisterAndDeliver(t *testing.T) {
	s := dryrun.New()
	ch := s.Register("req-1", "agent-1")

	result := &agentv1.DryRunResult{RuleId: "req-1", Error: ""}
	s.Deliver("req-1", "agent-1", result)

	select {
	case got := <-ch:
		assert.Equal(t, "req-1", got.GetRuleId())
	case <-time.After(time.Second):
		t.Fatal("timeout: result not delivered")
	}
}

func TestStore_Cancel_NoLeak(t *testing.T) {
	s := dryrun.New()
	_ = s.Register("req-2", "agent-1")
	s.Cancel("req-2")
	// Deliver after cancel should not panic or block.
	s.Deliver("req-2", "agent-1", &agentv1.DryRunResult{RuleId: "req-2"})
}

func TestStore_Deliver_UnknownID_IsNoop(t *testing.T) {
	s := dryrun.New()
	// Should not panic.
	s.Deliver("nonexistent", "agent-1", &agentv1.DryRunResult{RuleId: "nonexistent"})
}

func TestStore_Deliver_CallerTimedOut_IsNoop(t *testing.T) {
	s := dryrun.New()
	ch := s.Register("req-3", "agent-1")
	s.Cancel("req-3")

	// Deliver after cancel: no panic, nothing readable.
	s.Deliver("req-3", "agent-1", &agentv1.DryRunResult{RuleId: "req-3"})

	select {
	case <-ch:
		t.Fatal("unexpected result on cancelled channel")
	default:
	}
}

func TestStore_ConcurrentDelivers(t *testing.T) {
	s := dryrun.New()
	ch := s.Register("req-4", "agent-1")

	result := &agentv1.DryRunResult{RuleId: "req-4"}
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
