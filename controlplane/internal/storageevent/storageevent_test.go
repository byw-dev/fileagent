package storageevent

import (
	"fmt"
	"testing"
)

// TestKindValuesArePinned asserts the numeric contract of Kind. The values are
// part of the pinned storage-event-source contract (D-037 / B3-PR1): later PRs
// match on them when routing bucket-removal semantics, so a silent renumbering
// would be a behavior change, not a refactor.
func TestKindValuesArePinned(t *testing.T) {
	if KindObjectCreated != 1 {
		t.Errorf("KindObjectCreated = %d, want 1", KindObjectCreated)
	}
	if KindObjectRemoved != 2 {
		t.Errorf("KindObjectRemoved = %d, want 2", KindObjectRemoved)
	}
	if KindBucketRemoved != 3 {
		t.Errorf("KindBucketRemoved = %d, want 3", KindBucketRemoved)
	}
}

func TestKindString(t *testing.T) {
	tests := []struct {
		kind Kind
		want string
	}{
		{KindObjectCreated, "object_created"},
		{KindObjectRemoved, "object_removed"},
		{KindBucketRemoved, "bucket_removed"},
		{Kind(0), "Kind(0)"},
		{Kind(99), "Kind(99)"},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(tt.kind), got, tt.want)
		}
	}
}

// TestEventZeroValue documents the zero Event: no kind, no bucket, no key.
// Sinks must never see it from a well-formed source adapter, but IndexSink
// answers it with ErrUnsupportedKind rather than guessing (see sink_test.go).
func TestEventZeroValue(t *testing.T) {
	var ev Event
	if ev.Kind != Kind(0) || ev.Bucket != "" || ev.Key != "" || ev.Size != 0 ||
		ev.ETag != "" || !ev.ObservedAt.IsZero() || ev.Seq != "" {
		t.Errorf("zero Event = %+v, want all fields zero", ev)
	}
	// fmt.Stringer contract: String must not panic on the zero kind.
	_ = fmt.Sprintf("%s", ev.Kind)
}
