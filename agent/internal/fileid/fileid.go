// Package fileid provides the kernel-backed content-identity metadata for
// files: size, mtime (nanoseconds), ctime (status-change time, nanoseconds)
// and the file identity (device + inode on Unix; volume serial number + file
// index on Windows). It exists for IC-BUG-48: deduplication must verify
// content identity with metadata a writer cannot arbitrarily forge — ctime
// cannot be set by utimes/touch, and a delete-and-recreate always changes the
// inode — instead of trusting the forgeable mtime+size pair.
package fileid

import (
	"os"
	"time"
)

// Identity is the kernel metadata snapshot of one file version. All fields
// come from the OS; a writer cannot set CtimeNs (utimes/touch only set
// atime/mtime) and cannot keep Dev/Ino across a delete-and-recreate.
//
// A zero field means "unknown": the platform or the filesystem does not
// provide it. Zero fields never compare equal in PrefilterVerdict — an
// unknown must lead to hash confirmation (fail-closed), never to "same".
type Identity struct {
	MtimeNs int64
	CtimeNs int64
	Dev     uint64
	Ino     uint64
	Size    int64

	// SnapAtNs is the wall clock (Unix ns) at the moment this identity was
	// stat'ed — the racy anchor. Not part of equality comparisons.
	SnapAtNs int64
}

// FromInfo extracts the identity from an os.FileInfo. Fields the platform
// cannot provide are left zero — callers must treat them as unknown, and the
// verdict logic does. path is the file's path, needed by platforms whose
// FileInfo does not carry every identity field (Windows re-opens the file to
// read the ChangeTime and the file index); pass "" only in tests that do not
// exercise those platforms.
func FromInfo(fi os.FileInfo, path string) Identity {
	id := Identity{
		MtimeNs: fi.ModTime().UnixNano(),
		Size:    fi.Size(),
	}
	fillPlatform(&id, fi, path)
	return id
}

// PostStatMatches reports whether the post-read re-stat describes the same
// file state as the pre-read snapshot. SnapAtNs is deliberately EXCLUDED
// (it is the wall clock of each stat call, never equal); every kernel field
// must match. Used by the uploader's post-read verification (IC-BUG-48).
func PostStatMatches(pre, post Identity) bool {
	return pre.MtimeNs == post.MtimeNs && pre.CtimeNs == post.CtimeNs &&
		pre.Dev == post.Dev && pre.Ino == post.Ino && pre.Size == post.Size
}

// Stat stats path and returns its identity. Rework-4 blocker 2: it opens
// the path ONCE and takes every field from that handle (StatHandle) — on
// Windows the previous implementation os.Stat'ed the path for size/mtime and
// then re-opened the path for ChangeTime/volume/index, so a path re-pointed
// between the two queries produced a hybrid identity of two files. The
// anchor is taken BEFORE the open (rework-2 fix 5, conservative direction —
// see the comment above StatHandle).
func Stat(path string) (Identity, error) {
	anchor := time.Now().UnixNano()
	f, err := os.Open(path)
	if err != nil {
		return Identity{}, err
	}
	defer f.Close()
	id, err := StatHandle(f)
	if err != nil {
		return Identity{}, err
	}
	id.SnapAtNs = anchor
	return id, nil
}

// StatHandle stats an ALREADY-OPEN file handle and returns its identity.
// Reading the identity from the same fd that will be read for upload (or
// hashing) is the only way to guarantee that "the stat" and "the read" see
// the same file version (IC-BUG-48: the snapshot and the bytes must come from
// one file description — rework-2 blocker 4: on Windows this queries
// GetFileInformationByHandle(Ex) on THIS handle, never re-opens by path).
// SnapAtNs anchors the racy window BEFORE the stat (see Stat).
func StatHandle(f *os.File) (Identity, error) {
	anchor := time.Now().UnixNano()
	fi, err := f.Stat()
	if err != nil {
		return Identity{}, err
	}
	id := statHandleIdentity(f, fi)
	id.SnapAtNs = anchor
	return id, nil
}

// On Windows StatHandle routes through statHandleIdentity (defined below):
// size/mtime from f.Stat, ChangeTime/volume/index from THIS handle's
// GetFileInformationByHandle(Ex) — every field from the same handle, never a
// path re-open (rework-2 blocker 4).
// TimeFromNs converts a Unix-nanosecond timestamp back to time.Time (UTC).
// Only used by tests and ledger bookkeeping.
func TimeFromNs(ns int64) time.Time {
	return time.Unix(0, ns).UTC()
}
