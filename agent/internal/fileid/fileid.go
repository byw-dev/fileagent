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

// Stat stats path and returns its identity.
func Stat(path string) (Identity, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Identity{}, err
	}
	return FromInfo(fi, path), nil
}

// TimeFromNs converts a Unix-nanosecond timestamp back to time.Time (UTC).
// Only used by tests and ledger bookkeeping.
func TimeFromNs(ns int64) time.Time {
	return time.Unix(0, ns).UTC()
}
