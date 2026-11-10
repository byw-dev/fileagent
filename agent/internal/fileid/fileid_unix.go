//go:build unix

// fillPlatform fills the platform-provided identity fields from the stat
// result (IC-BUG-48): ctime is the status-change time (any write, chmod,
// rename or recreate bumps it; utimes/touch cannot set it), Dev+Ino is the
// file identity (a delete-and-recreate always changes it, even when cp -p
// preserves mtime and size).
package fileid

import (
	"os"
	"syscall"
)

// fillPlatform extracts st_ctime (nanoseconds), st_dev and st_ino from the
// Unix stat blob. The timespec field name differs per platform (Ctimespec on
// the BSDs/macOS, Ctim on Linux), so switch on GOOS at compile time. Fields
// are left zero when the underlying type does not carry them, which callers
// treat as unknown.
func fillPlatform(id *Identity, fi os.FileInfo, path string) {
	_ = path
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	id.CtimeNs = statCtimeNs(st)
	id.Dev = uint64(st.Dev)
	id.Ino = st.Ino
}

// statHandleIdentity fills the identity from an ALREADY-OPEN handle's stat
// blob. On Unix the syscall blob carries ctime/dev/inode directly, so every
// field comes from the same handle trivially (no path re-open).
func statHandleIdentity(f *os.File, fi os.FileInfo) Identity {
	id := Identity{MtimeNs: fi.ModTime().UnixNano(), Size: fi.Size()}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return id
	}
	id.CtimeNs = statCtimeNs(st)
	id.Dev = uint64(st.Dev)
	id.Ino = st.Ino
	return id
}
