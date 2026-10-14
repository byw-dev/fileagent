package handler

// events_translate_test.go — unit tests for the pure S3-record → storageevent
// translation (the adapter half of the D-037 storage event source). These run
// in the internal package test because translateRecord is unexported; the
// transport-layer behaviour around it (auth, dead letter, status codes) is
// covered by the external handler tests in events_test.go and the
// webhook_*_test.go family.

import (
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/storageevent"
)

var translateRefTime = time.Date(2026, 10, 15, 8, 30, 5, 0, time.UTC)

// record builds one parsed S3 event record for the translation tests.
func record(eventName, bucket, rawKey string, size int64, etag, seq string) minioEventRecord {
	return minioEventRecord{
		EventTime: translateRefTime,
		EventName: eventName,
		S3: minioS3{
			Bucket: minioS3Bucket{Name: bucket},
			Object: minioS3Object{Sequencer: seq, Key: rawKey, Size: size, ETag: etag},
		},
	}
}

// TestTranslateRecord_Created_HappyPath: every payload field lands in the
// Event with the object key URL-decoded, ready for Sink.Apply.
func TestTranslateRecord_Created_HappyPath(t *testing.T) {
	ev, err, handled := translateRecord(record("s3:ObjectCreated:Put", "data-sensor", "ic19%2Fnested+dir%2F%E4%B8%AD+%E6%96%87.csv", 42, "etag-1", "17KNOB0000000001"))
	if err != nil || !handled {
		t.Fatalf("translateRecord(created) = (%v, %v), want (nil, true)", err, handled)
	}
	want := storageevent.Event{
		Kind:       storageevent.KindObjectCreated,
		Bucket:     "data-sensor",
		Key:        "ic19/nested dir/中 文.csv",
		Size:       42,
		ETag:       "etag-1",
		ObservedAt: translateRefTime,
		Seq:        "17KNOB0000000001",
	}
	if ev != want {
		t.Errorf("translateRecord(created) = %+v, want %+v", ev, want)
	}
}

// TestTranslateRecord_Removed_HappyPath: removals translate with the same
// decode + identity fields; Size/ETag ride along uninterpreted (the sink only
// reads them for Created).
func TestTranslateRecord_Removed_HappyPath(t *testing.T) {
	ev, err, handled := translateRecord(record("s3:ObjectRemoved:Delete", "data-sensor", "a%2Fb%2Fgone.csv", 7, "etag-x", "17KNOB0000000002"))
	if err != nil || !handled {
		t.Fatalf("translateRecord(removed) = (%v, %v), want (nil, true)", err, handled)
	}
	if ev.Kind != storageevent.KindObjectRemoved {
		t.Errorf("Kind = %v, want KindObjectRemoved", ev.Kind)
	}
	if ev.Key != "a/b/gone.csv" || ev.Bucket != "data-sensor" || ev.Seq != "17KNOB0000000002" || !ev.ObservedAt.Equal(translateRefTime) {
		t.Errorf("identity fields = %+v", ev)
	}
}

// TestTranslateRecord_AllPrefixVariants: MinIO emits many subtypes
// (:Put/:Post/:Copy/:Delete/:DeleteMarkerCreated); the routing is by prefix,
// not by exact name.
func TestTranslateRecord_AllPrefixVariants(t *testing.T) {
	tests := []struct {
		eventName string
		want      storageevent.Kind
	}{
		{"s3:ObjectCreated:Put", storageevent.KindObjectCreated},
		{"s3:ObjectCreated:Post", storageevent.KindObjectCreated},
		{"s3:ObjectCreated:Copy", storageevent.KindObjectCreated},
		{"s3:ObjectCreated:CompleteMultipartUpload", storageevent.KindObjectCreated},
		{"s3:ObjectRemoved:Delete", storageevent.KindObjectRemoved},
		{"s3:ObjectRemoved:DeleteMarkerCreated", storageevent.KindObjectRemoved},
	}
	for _, tt := range tests {
		ev, err, handled := translateRecord(record(tt.eventName, "b", "k", 1, "e", "s"))
		if err != nil || !handled {
			t.Fatalf("translateRecord(%s) = (%v, %v), want (nil, true)", tt.eventName, err, handled)
		}
		if ev.Kind != tt.want {
			t.Errorf("%s → Kind %v, want %v", tt.eventName, ev.Kind, tt.want)
		}
	}
}

// TestTranslateRecord_InvalidEncoding returns the decode error (error path).
// The handler routes this through the counted failure state machine under the
// RAW key identity — pinned end-to-end by
// TestMinioEventHandler_InvalidEncoding_NotIndexed.
func TestTranslateRecord_InvalidEncoding(t *testing.T) {
	_, err, _ := translateRecord(record("s3:ObjectCreated:Put", "b", "a%2Gb.csv", 1, "e", "s"))
	if err == nil {
		t.Fatal("translateRecord(invalid key) err = nil, want the QueryUnescape error")
	}
	var escapeErr url.EscapeError
	if !errors.As(err, &escapeErr) {
		t.Errorf("err = %T (%v), want url.EscapeError from url.QueryUnescape", err, err)
	}
}

// TestTranslateRecord_UnknownEventIgnored (edge): event types the index does
// not consume translate to handled=false — including the case-mangled and
// other-family S3 events MinIO can emit.
func TestTranslateRecord_UnknownEventIgnored(t *testing.T) {
	for _, name := range []string{
		"s3:ObjectAccessed:Get",
		"s3:ObjectRestore:Post",
		"s3:ReducedRedundancyLostObject",
		"s3:objectcreated:put", // prefix match is case-sensitive by design
		"",
	} {
		ev, err, handled := translateRecord(record(name, "b", "k", 1, "e", "s"))
		if err != nil {
			t.Fatalf("translateRecord(%q) unexpected error: %v", name, err)
		}
		if handled {
			t.Errorf("translateRecord(%q) handled = true, want false", name)
		}
		if ev.Kind != 0 {
			t.Errorf("translateRecord(%q) Kind = %v, want 0 (unrouted)", name, ev.Kind)
		}
		// The decoded key is still populated: the transport layer logs the
		// received event before ignoring it, matching the pre-B3 handler.
		if ev.Key != "k" {
			t.Errorf("translateRecord(%q) Key = %q, want decoded %q", name, ev.Key, "k")
		}
	}
}

// TestTranslateRecord_EmptyRecords (edge): an empty Records slice never
// reaches the translation — pinned end-to-end by
// TestMinioEventHandler_Handle_EmptyRecords (200, nothing indexed). Here we
// pin the pure-function half: a zero record with no event name and no key is
// ignored, not applied.
func TestTranslateRecord_ZeroRecordIgnored(t *testing.T) {
	_, err, handled := translateRecord(minioEventRecord{})
	if err != nil {
		t.Fatalf("zero record err = %v, want nil", err)
	}
	if handled {
		t.Error("zero record handled = true, want false (no event name → ignored)")
	}
}
