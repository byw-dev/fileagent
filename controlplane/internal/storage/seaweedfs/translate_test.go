package seaweedfs

import (
	"path"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/filerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------- fakes

// fileEntry builds a non-directory Entry; size is its FuseAttributes file size.
func fileEntry(name string, size uint64) *filerpb.Entry {
	return &filerpb.Entry{
		Name:       name,
		Attributes: &filerpb.FuseAttributes{FileSize: size},
	}
}

// dirEntry builds a directory Entry.
func dirEntry(name string) *filerpb.Entry {
	return &filerpb.Entry{Name: name, IsDirectory: true}
}

// prefixObjectEntry builds a directory entry that also carries the object
// named by its path (the S3 "prefix object" representation, measured live on
// 4.48: IsDirectory=true, marker in Extended, object size in attributes).
// Zero-byte objects leave FileSize unset.
func prefixObjectEntry(name string, size uint64) *filerpb.Entry {
	return &filerpb.Entry{
		Name:        name,
		IsDirectory: true,
		Attributes:  &filerpb.FuseAttributes{FileSize: size, Mime: "application/octet-stream"},
		Extended: map[string][]byte{
			"x-seaweedfs-prefix-object": []byte("true"),
			"Seaweed-X-Amz-Owner":       []byte("admin"),
		},
	}
}

// implicitDirCreateResp fakes the implicit parent-directory creation event the
// filer emits before the first object below that directory (measured: plain
// directory, old=nil).
func implicitDirCreateResp(bucket, dir string, ts int64) *filerpb.SubscribeMetadataResponse {
	d, name := eventPaths(bucket + "/" + dir)
	return &filerpb.SubscribeMetadataResponse{
		Directory:         d,
		TsNs:              ts,
		EventNotification: &filerpb.EventNotification{NewEntry: dirEntry(name)},
	}
}

// eventPaths splits "bucket/key" into the filer event fields: the directory
// containing the entry and the entry name.
func eventPaths(obj string) (dir, name string) {
	i := len(obj)
	for i > 0 && obj[i-1] != '/' {
		i--
	}
	d := i
	if d > 0 {
		d-- // drop the slash separating directory and name from the dir part
	}
	return "/buckets/" + obj[:d], obj[i:]
}

// createdResp fakes a create/overwrite event for bucket/key.
func createdResp(bucket, key string, size uint64, ts int64) *filerpb.SubscribeMetadataResponse {
	dir, name := eventPaths(bucket + "/" + key)
	return &filerpb.SubscribeMetadataResponse{
		Directory:         dir,
		TsNs:              ts,
		EventNotification: &filerpb.EventNotification{NewEntry: fileEntry(name, size)},
	}
}

// deletedResp fakes a delete event for bucket/key.
func deletedResp(bucket, key string, ts int64) *filerpb.SubscribeMetadataResponse {
	dir, name := eventPaths(bucket + "/" + key)
	return &filerpb.SubscribeMetadataResponse{
		Directory:         dir,
		TsNs:              ts,
		EventNotification: &filerpb.EventNotification{OldEntry: fileEntry(name, 0)},
	}
}

// mkdirResp fakes a directory creation event at /buckets/<bucket>/<dir...>.
func mkdirResp(bucket, dir string, ts int64) *filerpb.SubscribeMetadataResponse {
	d, name := eventPaths(bucket + "/" + dir)
	return &filerpb.SubscribeMetadataResponse{
		Directory:         d,
		TsNs:              ts,
		EventNotification: &filerpb.EventNotification{NewEntry: dirEntry(name)},
	}
}

// rmdirResp fakes a directory deletion event at /buckets/<bucket>/<path>.
func rmdirResp(bucket, dir string, ts int64) *filerpb.SubscribeMetadataResponse {
	d, name := eventPaths(bucket + "/" + dir)
	return &filerpb.SubscribeMetadataResponse{
		Directory:         d,
		TsNs:              ts,
		EventNotification: &filerpb.EventNotification{OldEntry: dirEntry(name)},
	}
}

// bucketDirDeletedResp fakes deletion of the /buckets/<bucket> directory
// itself: Directory is /buckets and the entry name is the bucket name.
func bucketDirDeletedResp(bucket string, ts int64) *filerpb.SubscribeMetadataResponse {
	return &filerpb.SubscribeMetadataResponse{
		Directory:         "/buckets",
		TsNs:              ts,
		EventNotification: &filerpb.EventNotification{OldEntry: dirEntry(bucket)},
	}
}

// bucketDirCreatedResp fakes creation of the /buckets/<bucket> directory.
func bucketDirCreatedResp(bucket string, ts int64) *filerpb.SubscribeMetadataResponse {
	return &filerpb.SubscribeMetadataResponse{
		Directory:         "/buckets",
		TsNs:              ts,
		EventNotification: &filerpb.EventNotification{NewEntry: dirEntry(bucket)},
	}
}

// renamedResp fakes a rename from bucket/oldKey to bucket/newKey.
func renamedResp(bucket, oldKey, newKey string, newSize uint64, ts int64) *filerpb.SubscribeMetadataResponse {
	oldDir, oldName := eventPaths(bucket + "/" + oldKey)
	newDir, newName := eventPaths(bucket + "/" + newKey)
	return &filerpb.SubscribeMetadataResponse{
		Directory: oldDir,
		TsNs:      ts,
		EventNotification: &filerpb.EventNotification{
			OldEntry:      fileEntry(oldName, 0),
			NewEntry:      fileEntry(newName, newSize),
			NewParentPath: newDir,
		},
	}
}

// heartbeatResp fakes an idle heartbeat (no event, only ts_ns).
func heartbeatResp(ts int64) *filerpb.SubscribeMetadataResponse {
	return &filerpb.SubscribeMetadataResponse{TsNs: ts}
}

// flushReportResp fakes a flush report (only flushed_ts_ns, ts_ns stays zero).
func flushReportResp(flushed int64) *filerpb.SubscribeMetadataResponse {
	return &filerpb.SubscribeMetadataResponse{FlushedTsNs: flushed}
}

// rawResp builds a response with explicit directory and entry names, for paths
// that do not fit the bucket/key helpers.
func rawResp(dir string, oldEntry, newEntry *filerpb.Entry, newParent string, ts int64) *filerpb.SubscribeMetadataResponse {
	return &filerpb.SubscribeMetadataResponse{
		Directory: dir,
		TsNs:      ts,
		EventNotification: &filerpb.EventNotification{
			OldEntry:      oldEntry,
			NewEntry:      newEntry,
			NewParentPath: newParent,
		},
	}
}

// ---------------------------------------------------------------- Translate

func TestTranslateCreated(t *testing.T) {
	got := Translate(createdResp("team-a", "docs/2026/report.txt", 4096, 1_000))
	require.Len(t, got, 1)
	assert.Equal(t, Change{
		Kind:   ChangeCreated,
		Bucket: "team-a",
		Key:    "docs/2026/report.txt",
		Size:   4096,
		TsNs:   1_000,
	}, got[0])
}

func TestTranslateCreatedWithoutAttributes(t *testing.T) {
	resp := &filerpb.SubscribeMetadataResponse{
		Directory:         "/buckets/b",
		TsNs:              7,
		EventNotification: &filerpb.EventNotification{NewEntry: &filerpb.Entry{Name: "x"}},
	}
	got := Translate(resp)
	require.Len(t, got, 1)
	assert.Equal(t, int64(0), got[0].Size)
}

func TestTranslateOverwrite(t *testing.T) {
	// Overwrite: old and new entry on the same path, size changes.
	resp := &filerpb.SubscribeMetadataResponse{
		Directory: "/buckets/b",
		TsNs:      9,
		EventNotification: &filerpb.EventNotification{
			OldEntry: fileEntry("k", 10),
			NewEntry: fileEntry("k", 99),
		},
	}
	got := Translate(resp)
	require.Len(t, got, 1)
	assert.Equal(t, ChangeCreated, got[0].Kind)
	assert.Equal(t, int64(99), got[0].Size, "overwrite must report the new size")
}

func TestTranslateDeleted(t *testing.T) {
	got := Translate(deletedResp("team-a", "docs/x.txt", 5_000))
	require.Len(t, got, 1)
	assert.Equal(t, Change{
		Kind:   ChangeRemoved,
		Bucket: "team-a",
		Key:    "docs/x.txt",
		TsNs:   5_000,
	}, got[0])
}

func TestTranslateRenameSameBucket(t *testing.T) {
	got := Translate(renamedResp("b", "old/nested.txt", "new/renamed.txt", 77, 42))
	require.Len(t, got, 2)
	assert.Equal(t, Change{
		Kind:   ChangeRemoved,
		Bucket: "b",
		Key:    "old/nested.txt",
		TsNs:   42,
	}, got[0], "rename must first remove the old path")
	assert.Equal(t, Change{
		Kind:   ChangeCreated,
		Bucket: "b",
		Key:    "new/renamed.txt",
		Size:   77,
		TsNs:   42,
	}, got[1], "rename must then create the new path")
}

func TestTranslateRenameCrossBucket(t *testing.T) {
	oldDir, oldName := eventPaths("src/a.txt")
	newDir, newName := eventPaths("dst/b.txt")
	resp := &filerpb.SubscribeMetadataResponse{
		Directory: oldDir,
		TsNs:      3,
		EventNotification: &filerpb.EventNotification{
			OldEntry:      fileEntry(oldName, 0),
			NewEntry:      fileEntry(newName, 5),
			NewParentPath: newDir,
		},
	}
	got := Translate(resp)
	require.Len(t, got, 2)
	assert.Equal(t, ChangeRemoved, got[0].Kind)
	assert.Equal(t, "src", got[0].Bucket)
	assert.Equal(t, "a.txt", got[0].Key)
	assert.Equal(t, ChangeCreated, got[1].Kind)
	assert.Equal(t, "dst", got[1].Bucket)
	assert.Equal(t, "b.txt", got[1].Key)
}

func TestTranslateRenameOutOfUploadsStaging(t *testing.T) {
	// A move out of the staging area via filer rename (filer HTTP / FUSE; the
	// S3 multipart-complete path does NOT rename — see the measured sequence
	// in TestTranslateMultipartNormalKey448). Only the final object survives
	// the filters.
	got := Translate(renamedResp("b", ".uploads/abc123/2", "mp/big.bin", 1<<20, 11))
	require.Len(t, got, 1)
	assert.Equal(t, ChangeCreated, got[0].Kind)
	assert.Equal(t, "mp/big.bin", got[0].Key)
}

func TestTranslateRenameIntoUploadsStaging(t *testing.T) {
	got := Translate(renamedResp("b", "live.txt", ".uploads/abc/1", 8, 12))
	require.Len(t, got, 1)
	assert.Equal(t, ChangeRemoved, got[0].Kind)
	assert.Equal(t, "live.txt", got[0].Key)
}

func TestTranslateRenameDirectoryOldEntry(t *testing.T) {
	// Directory renamed to a file path: the old entry is a directory, so no
	// Removed may be emitted for it; only the Created for the file.
	resp := &filerpb.SubscribeMetadataResponse{
		Directory:         "/buckets/b",
		TsNs:              4,
		EventNotification: &filerpb.EventNotification{OldEntry: dirEntry("d"), NewEntry: fileEntry("f.txt", 1), NewParentPath: "/buckets/b"},
	}
	got := Translate(resp)
	require.Len(t, got, 1)
	assert.Equal(t, ChangeCreated, got[0].Kind)
	assert.Equal(t, "f.txt", got[0].Key)
}

func TestTranslateDirectoriesIgnored(t *testing.T) {
	assert.Empty(t, Translate(mkdirResp("b", "some/dir", 1)), "nested directory create")
	assert.Empty(t, Translate(rmdirResp("b", "some/dir", 2)), "nested directory delete")
	assert.Empty(t, Translate(bucketDirCreatedResp("b", 3)), "bucket directory create")
}

func TestTranslateFileAndDirectorySameName(t *testing.T) {
	// Directory replaced by a file on the same path: resulting entry is a
	// file, so a Created is emitted for it.
	resp := &filerpb.SubscribeMetadataResponse{
		Directory:         "/buckets/b",
		TsNs:              6,
		EventNotification: &filerpb.EventNotification{OldEntry: dirEntry("x"), NewEntry: fileEntry("x", 3)},
	}
	got := Translate(resp)
	require.Len(t, got, 1)
	assert.Equal(t, ChangeCreated, got[0].Kind)

	// File replaced by a plain directory on the same path: the object the
	// file represented is gone, so a Removed is emitted (entry no longer
	// represents an object; reachable via filer-level UpdateEntry, not via a
	// single S3 operation).
	resp2 := &filerpb.SubscribeMetadataResponse{
		Directory:         "/buckets/b",
		TsNs:              7,
		EventNotification: &filerpb.EventNotification{OldEntry: fileEntry("y", 3), NewEntry: dirEntry("y")},
	}
	got2 := Translate(resp2)
	require.Len(t, got2, 1)
	assert.Equal(t, ChangeRemoved, got2[0].Kind)
	assert.Equal(t, "y", got2[0].Key)
}

func TestTranslateBucketRemoved(t *testing.T) {
	got := Translate(bucketDirDeletedResp("team-a", 8_000))
	require.Len(t, got, 1)
	assert.Equal(t, Change{
		Kind:        ChangeBucketRemoved,
		Bucket:      "team-a",
		TsNs:        8_000,
		IsDirectory: true,
	}, got[0])
}

func TestTranslateBucketsRootItself(t *testing.T) {
	assert.Empty(t, Translate(rawResp("/buckets", nil, dirEntry("buckets-parent"), "", 1)),
		"creation of /buckets itself is not a bucket event")
	assert.Empty(t, Translate(rawResp("/", nil, dirEntry("buckets"), "", 2)),
		"/buckets created from root dir")
	assert.Empty(t, Translate(rawResp("/", dirEntry("buckets"), nil, "", 3)),
		"/buckets deleted")
}

func TestTranslateFileDirectlyUnderBucketsRoot(t *testing.T) {
	// A file at /buckets/name (no key below the bucket): not an object.
	assert.Empty(t, Translate(rawResp("/buckets", nil, fileEntry("stray", 1), "", 4)))
	// And its deletion.
	assert.Empty(t, Translate(rawResp("/buckets", fileEntry("stray", 1), nil, "", 5)))
}

func TestTranslateSystemPrefix(t *testing.T) {
	assert.Empty(t, Translate(rawResp("/buckets/.system/owners", nil, fileEntry("bucket=b", 1), "", 1)),
		".system bookkeeping create")
	assert.Empty(t, Translate(rawResp("/buckets/.system", dirEntry(".system"), nil, "", 2)),
		".system directory delete is not a bucket removal")
}

func TestTranslateDotBucketIgnored(t *testing.T) {
	assert.Empty(t, Translate(createdResp(".hidden", "k", 1, 1)))
	assert.Empty(t, Translate(deletedResp(".hidden", "k", 1)))
}

func TestTranslateUploadsStaging(t *testing.T) {
	assert.Empty(t, Translate(createdResp("b", ".uploads", 0, 1)), ".uploads marker itself")
	assert.Empty(t, Translate(createdResp("b", ".uploads/abc/1", 5, 2)), "staging part")
	assert.Empty(t, Translate(deletedResp("b", ".uploads/abc/1", 3)), "staging cleanup")
	assert.Empty(t, Translate(mkdirResp("b", ".uploads/abc", 4)), "staging dir")
}

func TestTranslateOutsideBuckets(t *testing.T) {
	assert.Empty(t, Translate(rawResp("/other/dir", nil, fileEntry("x", 1), "", 1)))
	assert.Empty(t, Translate(rawResp("/buckets", nil, fileEntry("x", 1), "/other", 2)))
}

func TestTranslateBatch(t *testing.T) {
	// Envelope event + batched members (backlog catch-up), including a
	// heartbeat mixed in: batch members carry their own ts_ns.
	resp := createdResp("b", "a.txt", 1, 100)
	resp.Events = []*filerpb.SubscribeMetadataResponse{
		deletedResp("b", "c.txt", 300),
		heartbeatResp(400),
		createdResp("b", "d/e.txt", 9, 500),
	}
	got := Translate(resp)
	require.Len(t, got, 3)
	assert.Equal(t, []Change{
		{Kind: ChangeCreated, Bucket: "b", Key: "a.txt", Size: 1, TsNs: 100},
		{Kind: ChangeRemoved, Bucket: "b", Key: "c.txt", TsNs: 300},
		{Kind: ChangeCreated, Bucket: "b", Key: "d/e.txt", Size: 9, TsNs: 500},
	}, got)
}

func TestTranslateBatchOnlyNoEnvelope(t *testing.T) {
	// Envelope without its own event but batched members: members still
	// translate.
	resp := &filerpb.SubscribeMetadataResponse{
		Events: []*filerpb.SubscribeMetadataResponse{createdResp("b", "k", 2, 10)},
	}
	got := Translate(resp)
	require.Len(t, got, 1)
	assert.Equal(t, "k", got[0].Key)
}

func TestTranslateBatchMixedFilters(t *testing.T) {
	resp := heartbeatResp(1)
	resp.Events = []*filerpb.SubscribeMetadataResponse{
		createdResp("b", ".uploads/x/1", 1, 2), // filtered
		deletedResp("b", "gone.txt", 3),        // kept
	}
	got := Translate(resp)
	require.Len(t, got, 1)
	assert.Equal(t, ChangeRemoved, got[0].Kind)
}

func TestTranslateNilAndEmpty(t *testing.T) {
	assert.Nil(t, Translate(nil))
	assert.Nil(t, Translate(&filerpb.SubscribeMetadataResponse{}))
	assert.Nil(t, Translate(heartbeatResp(1)))
	assert.Nil(t, Translate(flushReportResp(1)))
}

func TestTranslateNestedBatchNotExpanded(t *testing.T) {
	// A batch member that itself carries Events: only depth-one expansion is
	// guaranteed by the wire format; nested batches are not produced by the
	// server and not expanded here. The child below must never surface.
	child := createdResp("b", "nested.txt", 1, 30)
	inner := createdResp("b", "k", 1, 10)
	inner.Events = []*filerpb.SubscribeMetadataResponse{child}
	resp := &filerpb.SubscribeMetadataResponse{Events: []*filerpb.SubscribeMetadataResponse{inner}}
	got := Translate(resp)
	require.Len(t, got, 1)
	assert.Equal(t, "k", got[0].Key)
	for _, c := range got {
		assert.NotEqual(t, "nested.txt", c.Key, "nested batch child must not be expanded")
	}
}

// ---------------------------------------------------------------- prefix objects
//
// The fixtures below reproduce raw event shapes MEASURED against a live
// one-shot SeaweedFS 4.48 (compose project b3-3a-probe, SubscribeLocalMetadata
// with idle heartbeats + batching; raw JSONL in bin/probe/events.jsonl and
// bin/probe/events-extra.jsonl, excerpted in bin/report-rework1.md). S3's flat
// key space lets key "a" and "a/b" coexist: "a" is then represented as a
// directory entry carrying the x-seaweedfs-prefix-object marker (4.48
// weed/pb/filer_pb/filer_pb_helper.go Entry.IsPrefixObject / MarkPrefixObject).

// prefixUpdateResp builds a same-path old→new event on /buckets/<bucket>/<key>
// with explicit old/new entries (the shape of prefix-object puts and demotions).
func prefixUpdateResp(bucket, key string, old, neu *filerpb.Entry, ts int64) *filerpb.SubscribeMetadataResponse {
	dir, _ := eventPaths(bucket + "/" + key)
	return &filerpb.SubscribeMetadataResponse{
		Directory:         dir,
		TsNs:              ts,
		EventNotification: &filerpb.EventNotification{OldEntry: old, NewEntry: neu},
	}
}

func TestTranslatePrefixObjectPutOntoChildPath(t *testing.T) {
	// Measured seq ① step "put a (12B)" after "put a/b": the plain directory
	// (auto-created for a/b) is replaced by a marker-carrying directory with
	// the object's size.
	got := Translate(prefixUpdateResp("b", "a", dirEntry("a"), prefixObjectEntry("a", 12), 10))
	require.Equal(t, []Change{{Kind: ChangeCreated, Bucket: "b", Key: "a", Size: 12, TsNs: 10}}, got)
}

func TestTranslatePrefixObjectPromotion(t *testing.T) {
	// Measured seq ②: PUT a/b after PUT a promotes the existing file to a
	// directory keeping its data. The promotion event has old=nil and a
	// marker-carrying directory as new.
	got := Translate(&filerpb.SubscribeMetadataResponse{
		Directory:         "/buckets/b",
		TsNs:              11,
		EventNotification: &filerpb.EventNotification{NewEntry: prefixObjectEntry("a", 12)},
	})
	require.Equal(t, []Change{{Kind: ChangeCreated, Bucket: "b", Key: "a", Size: 12, TsNs: 11}}, got)
}

func TestTranslatePrefixObjectDeleteDemotes(t *testing.T) {
	// Measured seq ③ step "delete a (a/b remains)": deleting the object while
	// children remain demotes the entry back to a plain directory (4.48
	// weed/s3api/filer_util.go demoteDirectoryMarkerToImplicitDirectory) —
	// old carries the marker, new is a plain directory, same path.
	got := Translate(prefixUpdateResp("b", "a", prefixObjectEntry("a", 34), dirEntry("a"), 12))
	require.Equal(t, []Change{{Kind: ChangeRemoved, Bucket: "b", Key: "a", TsNs: 12}}, got)
}

func TestTranslateZeroBytePrefixObject(t *testing.T) {
	// Measured seq ④: a zero-byte PUT a (with a/b present) leaves no size on
	// the entry; only the marker identifies it as an object.
	got := Translate(prefixUpdateResp("b", "a", dirEntry("a"), prefixObjectEntry("a", 0), 13))
	require.Equal(t, []Change{{Kind: ChangeCreated, Bucket: "b", Key: "a", Size: 0, TsNs: 13}}, got)
}

func TestTranslateDeletePrefixObjectWithoutChildren(t *testing.T) {
	// Not part of the measured sequences (needs a filer-level delete of a
	// marker-carrying directory with no children): a plain delete event whose
	// old entry carries the marker must surface as Removed.
	got := Translate(&filerpb.SubscribeMetadataResponse{
		Directory:         "/buckets/b",
		TsNs:              14,
		EventNotification: &filerpb.EventNotification{OldEntry: prefixObjectEntry("a", 5)},
	})
	require.Equal(t, []Change{{Kind: ChangeRemoved, Bucket: "b", Key: "a", TsNs: 14}}, got)
}

func TestTranslateRenamePrefixObject(t *testing.T) {
	// Rename where both sides are prefix objects: each side judged as an
	// object on its own.
	resp := &filerpb.SubscribeMetadataResponse{
		Directory: "/buckets/b",
		TsNs:      15,
		EventNotification: &filerpb.EventNotification{
			OldEntry:      prefixObjectEntry("a", 7),
			NewEntry:      prefixObjectEntry("c", 7),
			NewParentPath: "/buckets/b",
		},
	}
	got := Translate(resp)
	require.Len(t, got, 2)
	assert.Equal(t, Change{Kind: ChangeRemoved, Bucket: "b", Key: "a", TsNs: 15}, got[0])
	assert.Equal(t, Change{Kind: ChangeCreated, Bucket: "b", Key: "c", Size: 7, TsNs: 15}, got[1])
}

func TestTranslateRenamePlainDirToPrefixObject(t *testing.T) {
	// Rename where only the new side is an object: Created only.
	resp := &filerpb.SubscribeMetadataResponse{
		Directory: "/buckets/b",
		TsNs:      16,
		EventNotification: &filerpb.EventNotification{
			OldEntry:      dirEntry("a"),
			NewEntry:      prefixObjectEntry("c", 3),
			NewParentPath: "/buckets/b",
		},
	}
	got := Translate(resp)
	require.Equal(t, []Change{{Kind: ChangeCreated, Bucket: "b", Key: "c", Size: 3, TsNs: 16}}, got)
}

func TestTranslatePrefixObjectOverwriteKeepsCreated(t *testing.T) {
	// Same-path overwrite between two prefix objects (PUT a twice while a/b
	// exists): one Created with the new state.
	got := Translate(prefixUpdateResp("b", "a", prefixObjectEntry("a", 1), prefixObjectEntry("a", 2), 17))
	require.Equal(t, []Change{{Kind: ChangeCreated, Bucket: "b", Key: "a", Size: 2, TsNs: 17}}, got)
}

func TestTranslateDirectoryMarkerIsNotAnObject(t *testing.T) {
	// Measured seq ⑥ (contrast): `PUT d/` writes a plain directory with a
	// mime and ETag but NO prefix-object marker — the "directory marker"
	// (4.48 weed/pb/filer_pb/filer_pb_helper.go IsDirectoryKeyObject via
	// mime). Per D-037 it stays ignored, on put and on delete.
	marker := dirEntry("d")
	marker.Attributes = &filerpb.FuseAttributes{Mime: "application/octet-stream"}
	marker.Extended = map[string][]byte{
		"Seaweed-X-Amz-ETag":  []byte("etag"),
		"Seaweed-X-Amz-Owner": []byte("admin"),
	}
	assert.Empty(t, Translate(&filerpb.SubscribeMetadataResponse{
		Directory:         "/buckets/b",
		TsNs:              18,
		EventNotification: &filerpb.EventNotification{NewEntry: marker},
	}), "directory-marker put")
	assert.Empty(t, Translate(&filerpb.SubscribeMetadataResponse{
		Directory:         "/buckets/b",
		TsNs:              19,
		EventNotification: &filerpb.EventNotification{OldEntry: marker},
	}), "directory-marker delete")
}

// translateAll replays a raw event sequence the way the PR3b loop will
// (envelope + batch members in order).
func translateAll(events ...*filerpb.SubscribeMetadataResponse) (got []Change) {
	for _, e := range events {
		got = append(got, Translate(e)...)
	}
	return got
}

func TestTranslateMeasuredSequencePutSubThenPrefix(t *testing.T) {
	// Measured seq ① (bucket probe1): implicit dir a, file a/b, then prefix
	// object a onto the existing directory.
	got := translateAll(
		implicitDirCreateResp("b", "a", 1),
		createdResp("b", "a/b", 12, 2),
		prefixUpdateResp("b", "a", dirEntry("a"), prefixObjectEntry("a", 12), 3),
	)
	assert.Equal(t, []Change{
		{Kind: ChangeCreated, Bucket: "b", Key: "a/b", Size: 12, TsNs: 2},
		{Kind: ChangeCreated, Bucket: "b", Key: "a", Size: 12, TsNs: 3},
	}, got)
}

func TestTranslateMeasuredSequencePromotion(t *testing.T) {
	// Measured seq ② (bucket probe2): file a, then a/b promoting a to a
	// marker-carrying directory, then file a/b. The promotion re-hints "a"
	// (a duplicate Created for the same object is fine: events are hints and
	// consumers re-read).
	got := translateAll(
		createdResp("b", "a", 12, 1),
		&filerpb.SubscribeMetadataResponse{
			Directory:         "/buckets/b",
			TsNs:              2,
			EventNotification: &filerpb.EventNotification{NewEntry: prefixObjectEntry("a", 12)},
		},
		createdResp("b", "a/b", 12, 3),
	)
	assert.Equal(t, []Change{
		{Kind: ChangeCreated, Bucket: "b", Key: "a", Size: 12, TsNs: 1},
		{Kind: ChangeCreated, Bucket: "b", Key: "a", Size: 12, TsNs: 2},
		{Kind: ChangeCreated, Bucket: "b", Key: "a/b", Size: 12, TsNs: 3},
	}, got)
}

func TestTranslateMeasuredSequenceDeleteDemotes(t *testing.T) {
	// Measured seq ③ (bucket probe3): full lifecycle of a prefix object.
	got := translateAll(
		implicitDirCreateResp("b", "a", 1),
		createdResp("b", "a/b", 12, 2),
		prefixUpdateResp("b", "a", dirEntry("a"), prefixObjectEntry("a", 34), 3),
		prefixUpdateResp("b", "a", prefixObjectEntry("a", 34), dirEntry("a"), 4),
	)
	assert.Equal(t, []Change{
		{Kind: ChangeCreated, Bucket: "b", Key: "a/b", Size: 12, TsNs: 2},
		{Kind: ChangeCreated, Bucket: "b", Key: "a", Size: 34, TsNs: 3},
		{Kind: ChangeRemoved, Bucket: "b", Key: "a", TsNs: 4},
	}, got)
}

func TestTranslateMeasuredSequenceZeroBytePrefix(t *testing.T) {
	// Measured seq ④ (bucket probe4).
	got := translateAll(
		implicitDirCreateResp("b", "a", 1),
		createdResp("b", "a/b", 7, 2),
		prefixUpdateResp("b", "a", dirEntry("a"), prefixObjectEntry("a", 0), 3),
	)
	assert.Equal(t, []Change{
		{Kind: ChangeCreated, Bucket: "b", Key: "a/b", Size: 7, TsNs: 2},
		{Kind: ChangeCreated, Bucket: "b", Key: "a", Size: 0, TsNs: 3},
	}, got)
}

func TestTranslateMeasuredSequenceMultipartOntoPrefix(t *testing.T) {
	// Measured seq ⑤ (bucket probe5): multipart complete onto key a while
	// a/b exists. On 4.48 the owner-routed commit fails ("existing entry is a
	// directory", retried until the client gives up), so NO final-object
	// event exists — only staging events, all filtered. (The no-owner code
	// path would retry with MarkPrefixObject and emit the same marked-dir
	// update shape as seq ①; source-verified, not measured.)
	got := translateAll(
		implicitDirCreateResp("b", "a", 1),
		createdResp("b", "a/b", 9, 2),
		mkdirResp("b", ".uploads", 3),
		mkdirResp("b", ".uploads/86f7..1843", 4),
		createdResp("b", ".uploads/86f7..1843/0001_xx.part", 5242880, 5),
		createdResp("b", ".uploads/86f7..1843/0002_yy.part", 11, 6),
	)
	assert.Equal(t, []Change{
		{Kind: ChangeCreated, Bucket: "b", Key: "a/b", Size: 9, TsNs: 2},
	}, got)
}

func TestTranslateMultipartNormalKey448(t *testing.T) {
	// Measured seq ⑦ (bucket probe7): non-versioned multipart complete to a
	// key without children. Real 4.48 path (no rename involved): staging
	// directory and parts, final parent dir, ONE create of the final object
	// (size = sum of parts), then staging cleanup. .uploads root persists.
	got := translateAll(
		mkdirResp("b", ".uploads", 1),
		mkdirResp("b", ".uploads/1b59..ecca", 2),
		createdResp("b", ".uploads/1b59..ecca/0001_xx.part", 5242880, 3),
		createdResp("b", ".uploads/1b59..ecca/0002_yy.part", 11, 4),
		implicitDirCreateResp("b", "mp", 5),
		createdResp("b", "mp/big.bin", 5242891, 6),
		deletedResp("b", ".uploads/1b59..ecca/0001_xx.part", 7),
		deletedResp("b", ".uploads/1b59..ecca/0002_yy.part", 8),
		rmdirResp("b", ".uploads/1b59..ecca", 9),
	)
	assert.Equal(t, []Change{
		{Kind: ChangeCreated, Bucket: "b", Key: "mp/big.bin", Size: 5242891, TsNs: 6},
	}, got)
}

// ---------------------------------------------------------------- IsIdleHeartbeat

func TestIsIdleHeartbeat(t *testing.T) {
	ts, ok := IsIdleHeartbeat(heartbeatResp(1_760_000_000_000_000_000))
	assert.True(t, ok)
	assert.Equal(t, int64(1_760_000_000_000_000_000), ts)

	// Heartbeat on the local stream also carries flushed_ts_ns.
	hb := heartbeatResp(55)
	hb.FlushedTsNs = 44
	ts, ok = IsIdleHeartbeat(hb)
	assert.True(t, ok)
	assert.Equal(t, int64(55), ts)

	_, ok = IsIdleHeartbeat(createdResp("b", "k", 1, 1))
	assert.False(t, ok, "event response is not a heartbeat")

	_, ok = IsIdleHeartbeat(nil)
	assert.False(t, ok)

	_, ok = IsIdleHeartbeat(&filerpb.SubscribeMetadataResponse{})
	assert.False(t, ok, "empty response carries no usable ts_ns")

	_, ok = IsIdleHeartbeat(flushReportResp(9))
	assert.False(t, ok, "flush report keeps ts_ns zero and must not advance the cursor")

	batched := heartbeatResp(1)
	batched.Events = []*filerpb.SubscribeMetadataResponse{createdResp("b", "k", 1, 2)}
	_, ok = IsIdleHeartbeat(batched)
	assert.False(t, ok, "batched response is not a heartbeat")
}

// ---------------------------------------------------------------- ChangeKind.String

func TestChangeKindString(t *testing.T) {
	assert.Equal(t, "created", ChangeCreated.String())
	assert.Equal(t, "removed", ChangeRemoved.String())
	assert.Equal(t, "bucket_removed", ChangeBucketRemoved.String())
	assert.Equal(t, "unknown", ChangeKind(99).String())
}

// Guard against accidental helper drift: eventPaths must round-trip the same
// path shape the filer reports.
func TestEventPathsHelper(t *testing.T) {
	dir, name := eventPaths("b/a/c.txt")
	assert.Equal(t, "/buckets/b/a", dir)
	assert.Equal(t, "c.txt", name)
	assert.Equal(t, "/buckets/b/a/c.txt", path.Join(dir, name))
}
