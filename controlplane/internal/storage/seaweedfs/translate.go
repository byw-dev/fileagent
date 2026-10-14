package seaweedfs

import (
	"strings"

	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/filerpb"
)

// bucketsPrefix is the filer path prefix under which the S3 gateway places
// object data (bucket directories are /buckets/<bucket>).
const bucketsPrefix = "/buckets/"

// prefixObjectExtKey marks a directory entry that also carries the object
// named by its path without a trailing slash: S3's flat key space allows key
// "a" and key "a/b" to coexist, and "a" is then stored as a directory entry
// (SeaweedFS 4.48, weed/s3api/s3_constants/header.go
// SeaweedFSPrefixObject; the marker is written by
// weed/pb/filer_pb/filer_pb_helper.go Entry.MarkPrefixObject on every path
// that lands an object on a directory — mkFile retry, file→directory
// promotion in weed/filer/filer.go, putToFiler retry).
const prefixObjectExtKey = "x-seaweedfs-prefix-object"

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
// Translation rules (D-037 第五条第 4 项, 2026-10-15 修订 per PR #130 评审;
// verified against SeaweedFS 4.48 source and a live probe, see
// docs/reports/storage-event-source.md §4 and D-038):
//
//   - Only entries that represent an object below /buckets/<bucket>/ produce
//     changes: non-directory entries, or directory entries carrying the
//     x-seaweedfs-prefix-object marker (S3 key "a" beside "a/b"). Plain
//     directories — including `PUT d/` directory markers, which carry only a
//     mime — stay filtered, as do /buckets/.system/ bookkeeping and
//     <bucket>/.uploads/ multipart staging. Bucket names starting with "."
//     are internal to the S3 gateway and filtered as well (S3 bucket names
//     cannot start with a dot, so no real bucket is lost).
//   - Create and overwrite both map to ChangeCreated; delete maps to
//     ChangeRemoved. An old entry that represented an object but whose new
//     entry is a plain directory (the S3 gateway demotes a prefix object to
//     a plain directory when the object is deleted while children remain)
//     maps to ChangeRemoved.
//   - Rename (old and new entry present with different full paths) maps to
//     ChangeRemoved for the old path plus ChangeCreated for the new path,
//     each judged and filtered independently (so a move out of .uploads/
//     yields only the Created).
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
	case old == nil && neu != nil: // create (or promotion of a file to a prefix object)
		if !representsObject(neu) {
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
		if representsObject(old) {
			if b, k, ok := splitObjectPath(full); ok {
				*changes = append(*changes, Change{
					Kind:   ChangeRemoved,
					Bucket: b,
					Key:    k,
					TsNs:   resp.GetTsNs(),
				})
			}
			return
		}
		// Plain directory: only deletion of the bucket directory itself is
		// meaningful — leveldb3 bucket deletion is a whole-store delete with
		// a single directory event and no per-object events (D-037).
		if b, ok := bucketRoot(full); ok {
			*changes = append(*changes, Change{
				Kind:        ChangeBucketRemoved,
				Bucket:      b,
				TsNs:        resp.GetTsNs(),
				IsDirectory: true,
			})
		}
	default: // old != nil && neu != nil: overwrite, demotion, or rename
		oldObj, newObj := representsObject(old), representsObject(neu)
		if !oldObj && !newObj {
			return // plain-directory transition (create/move of directories)
		}
		oldFull := joinPath(resp.GetDirectory(), old.GetName())
		newFull := joinPath(newDir(resp, n), neu.GetName())
		if oldFull == newFull {
			// Same path: an object still represented there is Created with
			// its new state; an old object whose new entry is a plain
			// directory (prefix object deleted while children remain) is
			// Removed.
			if b, k, ok := splitObjectPath(newFull); ok {
				if newObj {
					*changes = append(*changes, Change{
						Kind:   ChangeCreated,
						Bucket: b,
						Key:    k,
						Size:   fileSize(neu),
						TsNs:   resp.GetTsNs(),
					})
				} else {
					*changes = append(*changes, Change{
						Kind:   ChangeRemoved,
						Bucket: b,
						Key:    k,
						TsNs:   resp.GetTsNs(),
					})
				}
			}
			return
		}
		// Rename: removal of the old path plus creation of the new path,
		// each judged independently.
		if oldObj {
			if b, k, ok := splitObjectPath(oldFull); ok {
				*changes = append(*changes, Change{
					Kind:   ChangeRemoved,
					Bucket: b,
					Key:    k,
					TsNs:   resp.GetTsNs(),
				})
			}
		}
		if newObj {
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

// representsObject reports whether an entry represents an object at its own
// path: a non-directory entry (the common case), or a directory entry
// carrying the prefix-object marker — S3's flat key space allows key "a"
// beside key "a/b", and 4.48 stores such objects as marker-carrying
// directories.
//
// Why the marker and not size or mime: a zero-byte object carries no size
// (measured live), and an entry promoted from an empty file can carry
// neither size nor mime; a mime alone does not identify an object either,
// because `PUT d/` directory markers carry one. The prefix-object marker is
// the only signal that distinguishes a key without a trailing slash from a
// plain directory (see D-038's measured findings).
//
// Plain directories — including `PUT d/` directory markers, which 4.48
// writes with a mime and ETag but no marker — do not represent objects and
// stay filtered per D-037.
func representsObject(e *filerpb.Entry) bool {
	return !e.GetIsDirectory() || isPrefixObject(e)
}

// isPrefixObject reports whether a directory entry carries the
// x-seaweedfs-prefix-object marker, replicating the exact judgment of
// SeaweedFS 4.48 weed/pb/filer_pb/filer_pb_helper.go Entry.IsPrefixObject:
// marker PRESENCE in Extended decides, the value is irrelevant, and entries
// that are not directories never qualify.
func isPrefixObject(e *filerpb.Entry) bool {
	if e == nil || !e.GetIsDirectory() {
		return false
	}
	_, marked := e.GetExtended()[prefixObjectExtKey]
	return marked
}
