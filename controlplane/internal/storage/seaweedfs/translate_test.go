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
	// Multipart finalize / FUSE move out of the staging area: only the final
	// object survives the filters.
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

	// File replaced by a directory on the same path: the resulting entry is a
	// directory, and directory entries never produce object changes.
	resp2 := &filerpb.SubscribeMetadataResponse{
		Directory:         "/buckets/b",
		TsNs:              7,
		EventNotification: &filerpb.EventNotification{OldEntry: fileEntry("y", 3), NewEntry: dirEntry("y")},
	}
	assert.Empty(t, Translate(resp2))
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
	// server and not expanded here.
	inner := createdResp("b", "k", 1, 10)
	resp := &filerpb.SubscribeMetadataResponse{Events: []*filerpb.SubscribeMetadataResponse{inner}}
	got := Translate(resp)
	require.Len(t, got, 1)
	assert.Equal(t, "k", got[0].Key)
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
