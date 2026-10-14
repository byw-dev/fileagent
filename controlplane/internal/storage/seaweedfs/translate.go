package seaweedfs

import (
	"strings"

	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/filerpb"
)

// bucketsPrefix is the filer path prefix under which the S3 gateway places
// object data (bucket directories are /buckets/<bucket>).
const bucketsPrefix = "/buckets/"

// ChangeKind classifies the effect a filer metadata event has on the object
// namespace the control plane cares about.
type ChangeKind int

// Kinds of Changes produced by Translate.
const (
	// ChangeCreated means a new object appeared, or an existing object was
	// overwritten (filer reports both as one event carrying the new entry).
	ChangeCreated ChangeKind = iota
	// ChangeRemoved means an object was deleted.
	ChangeRemoved
	// ChangeBucketRemoved means the /buckets/<bucket> directory itself was
	// deleted. With the leveldb3 filer store this is a whole-store delete that
	// emits no per-object events (D-037), so it must be surfaced on its own.
	ChangeBucketRemoved
)

// String implements fmt.Stringer for logging.
func (k ChangeKind) String() string {
	switch k {
	case ChangeCreated:
		return "created"
	case ChangeRemoved:
		return "removed"
	case ChangeBucketRemoved:
		return "bucket_removed"
	default:
		return "unknown"
	}
}

// Change is the adapter-internal representation of one object-level change
// extracted from a filer metadata event. Events are only low-latency hints:
// consumers must re-read current object state (via S3) before writing the
// index, so Change carries identity, not truth (D-037).
type Change struct {
	// Kind is what happened to Bucket/Key.
	Kind ChangeKind
	// Bucket and Key identify the object (S3 bucket / key). Key is empty for
	// ChangeBucketRemoved.
	Bucket string
	Key    string
	// Size is the object size in bytes as reported by the filer for the new
	// entry. It is zero for ChangeRemoved and ChangeBucketRemoved; do not
	// treat it as authoritative (re-read instead).
	Size int64
	// TsNs is the filer-side timestamp (ns) of the event that produced this
	// Change. It is not strictly ordered under concurrency and must not be
	// compared with other clock domains (D-037 known constraints).
	TsNs int64
	// IsDirectory reports that the change concerns a directory entry. It is
	// only true for ChangeBucketRemoved; directory entries never produce the
	// other kinds.
	IsDirectory bool
}

// Translate converts one SubscribeMetadataResponse into Changes.
//
// Batching (client_supports_batching): the server packs a backlog batch by
// using the first event as the envelope and appending the rest to
// resp.Events; each member carries its own ts_ns (SeaweedFS 4.48,
// weed/server/filer_grpc_server_sub_meta.go pipelinedSender). Translate
// expands one such response into [envelope] + Events, in order.
//
// Translation rules (D-037 第五条第 4 项, verified against SeaweedFS 4.48 in
// docs/reports/storage-event-source.md §4):
//
//   - Only non-directory entries below /buckets/<bucket>/ produce changes;
//     /buckets/.system/ bookkeeping and <bucket>/.uploads/ multipart staging
//     are filtered. Bucket names starting with "." are internal to the S3
//     gateway and filtered as well (S3 bucket names cannot start with a dot,
//     so no real bucket is lost).
//   - Create and overwrite both map to ChangeCreated; delete maps to
//     ChangeRemoved.
//   - Rename (old and new entry present with different full paths) maps to
//     ChangeRemoved for the old path plus ChangeCreated for the new path,
//     each filtered independently (so a move out of .uploads/ yields only the
//     Created, as multipart finalize does when implemented via rename).
//   - Deletion of the /buckets/<bucket> directory itself maps to
//     ChangeBucketRemoved; deleting /buckets or any deeper directory does
//     not.
//
// Responses without an EventNotification (idle heartbeats, flush reports)
// yield no Changes; use IsIdleHeartbeat to tell them apart.
func Translate(resp *filerpb.SubscribeMetadataResponse) []Change {
	if resp == nil {
		return nil
	}
	var changes []Change
	appendOne(&changes, resp)
	for _, member := range resp.GetEvents() {
		appendOne(&changes, member)
	}
	return changes
}

// IsIdleHeartbeat reports whether resp is an idle heartbeat: an empty
// response carrying only ts_ns, sent to subscribers that opened
// client_supports_idle_heartbeat once they are caught up with the log buffer
// (SeaweedFS 4.48, weed/server/filer_grpc_server_sub_meta.go
// maybeSendIdleHeartbeat). On the local stream (SubscribeLocalMetadata) that
// ts_ns is the delivery claim used as the durable cursor: no further event at
// or below it will arrive on the stream (D-037).
//
// A flush report (only flushed_ts_ns, ts_ns stays zero) is NOT an idle
// heartbeat: it never advances the delivery cursor. Responses batching events
// are not heartbeats either.
func IsIdleHeartbeat(resp *filerpb.SubscribeMetadataResponse) (tsNs int64, ok bool) {
	if resp == nil || resp.GetEventNotification() != nil || len(resp.GetEvents()) > 0 || resp.GetTsNs() <= 0 {
		return 0, false
	}
	return resp.GetTsNs(), true
}

// appendOne translates a single (un-batched) response into changes.
func appendOne(changes *[]Change, resp *filerpb.SubscribeMetadataResponse) {
	n := resp.GetEventNotification()
	if n == nil {
		return // idle heartbeat or flush report: no change
	}
	switch old, neu := n.GetOldEntry(), n.GetNewEntry(); {
	case old == nil && neu != nil: // create
		if neu.GetIsDirectory() {
			return
		}
		if b, k, ok := splitObjectPath(joinPath(newDir(resp, n), neu.GetName())); ok {
			*changes = append(*changes, Change{
				Kind:   ChangeCreated,
				Bucket: b,
				Key:    k,
				Size:   fileSize(neu),
				TsNs:   resp.GetTsNs(),
			})
		}
	case old != nil && neu == nil: // delete
		full := joinPath(resp.GetDirectory(), old.GetName())
		if old.GetIsDirectory() {
			// Only deletion of the bucket directory itself is meaningful:
			// leveldb3 bucket deletion is a whole-store delete with a single
			// directory event and no per-object events (D-037).
			if b, ok := bucketRoot(full); ok {
				*changes = append(*changes, Change{
					Kind:        ChangeBucketRemoved,
					Bucket:      b,
					TsNs:        resp.GetTsNs(),
					IsDirectory: true,
				})
			}
			return
		}
		if b, k, ok := splitObjectPath(full); ok {
			*changes = append(*changes, Change{
				Kind:   ChangeRemoved,
				Bucket: b,
				Key:    k,
				TsNs:   resp.GetTsNs(),
			})
		}
	default: // old != nil && neu != nil: overwrite or rename
		if neu.GetIsDirectory() {
			return
		}
		oldFull := joinPath(resp.GetDirectory(), old.GetName())
		newFull := joinPath(newDir(resp, n), neu.GetName())
		if oldFull == newFull {
			// Overwrite of the same path: one Created with the new state.
			if b, k, ok := splitObjectPath(newFull); ok {
				*changes = append(*changes, Change{
					Kind:   ChangeCreated,
					Bucket: b,
					Key:    k,
					Size:   fileSize(neu),
					TsNs:   resp.GetTsNs(),
				})
			}
			return
		}
		// Rename: removal of the old path plus creation of the new path.
		// A directory old entry never yields a Removed (only non-directory
		// entries are object changes).
		if !old.GetIsDirectory() {
			if b, k, ok := splitObjectPath(oldFull); ok {
				*changes = append(*changes, Change{
					Kind:   ChangeRemoved,
					Bucket: b,
					Key:    k,
					TsNs:   resp.GetTsNs(),
				})
			}
		}
		if b, k, ok := splitObjectPath(newFull); ok {
			*changes = append(*changes, Change{
				Kind:   ChangeCreated,
				Bucket: b,
				Key:    k,
				Size:   fileSize(neu),
				TsNs:   resp.GetTsNs(),
			})
		}
	}
}

// newDir returns the directory of the new entry: EventNotification.NewParentPath
// when set, else the response Directory (SeaweedFS 4.48, weed/filer/meta_replay.go).
func newDir(resp *filerpb.SubscribeMetadataResponse, n *filerpb.EventNotification) string {
	if d := n.GetNewParentPath(); d != "" {
		return d
	}
	return resp.GetDirectory()
}

// joinPath rebuilds a full filer path from a directory and an entry name.
func joinPath(dir, name string) string {
	if dir == "" {
		return "/" + name
	}
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

// splitObjectPath splits a full filer path into bucket and key, reporting
// whether the path denotes an object below /buckets/<bucket>/:
//
//   - /buckets itself, /buckets/, and /buckets/<name> (the bucket directory,
//     or a stray file directly under the root) are not objects;
//   - dot-prefixed buckets (/buckets/.system/ bookkeeping included) are
//     internal to the S3 gateway;
//   - <bucket>/.uploads and everything below it is multipart staging.
func splitObjectPath(full string) (bucket, key string, ok bool) {
	rest, isBucketPath := strings.CutPrefix(full, bucketsPrefix)
	if !isBucketPath {
		return "", "", false
	}
	b, k, hasKey := strings.Cut(rest, "/")
	if !hasKey || b == "" || k == "" {
		return "", "", false
	}
	if isHidden(b) || isUploadsStaging(k) {
		return "", "", false
	}
	return b, k, true
}

// bucketRoot reports whether full is exactly /buckets/<bucket> with a
// non-hidden bucket name.
func bucketRoot(full string) (bucket string, ok bool) {
	rest, isBucketPath := strings.CutPrefix(full, bucketsPrefix)
	if !isBucketPath || rest == "" || strings.Contains(rest, "/") || isHidden(rest) {
		return "", false
	}
	return rest, true
}

// isHidden reports whether a bucket name belongs to the S3 gateway's internal
// namespace (/buckets/.system/ and siblings). S3 bucket names cannot start
// with a dot, so this never hides a real bucket.
func isHidden(bucket string) bool {
	return strings.HasPrefix(bucket, ".")
}

// isUploadsStaging reports whether a key lives in the multipart-upload
// staging area at the bucket root (S3 gateway uploads <bucket>/.uploads/...).
func isUploadsStaging(key string) bool {
	return key == ".uploads" || strings.HasPrefix(key, ".uploads/")
}

// fileSize returns the entry's reported file size in bytes, zero when the
// entry carries no attributes.
func fileSize(e *filerpb.Entry) int64 {
	return int64(e.GetAttributes().GetFileSize())
}
