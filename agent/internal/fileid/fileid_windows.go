//go:build windows

package fileid

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fillPlatform extracts the Windows identity fields (IC-BUG-48), from the
// GIVEN HANDLE (rework-2 blocker 4): StatHandle passes its *os.File here so
// size/mtime (f.Stat) and ChangeTime/volume/index (GetFileInformationByHandle
// + GetFileInformationByHandleEx on windows.Handle(f.Fd())) all describe the
// same open file — a path swap after the open cannot blend two files.
// Stat (by path) opens once and passes that handle.
//
//   - CtimeNs = the NTFS ChangeTime from FILE_BASIC_INFO — the equivalent of
//     Unix ctime (any write or metadata change bumps it). It is deliberately
//     NOT CreationTime: a delete-and-recreate can carry an arbitrary creation
//     time (cp -p style copies restore it), while ChangeTime always reflects
//     the last status change, exactly like Unix ctime.
//   - Dev/Ino = VolumeSerialNumber + FileIndex from
//     GetFileInformationByHandle — a delete-and-recreate changes the file
//     index even when every timestamp is preserved.
//
// Any field that cannot be obtained is left zero; callers treat zero fields
// as unknown → hash confirmation (fail-closed), never as "same".
func fillPlatform(id *Identity, fi os.FileInfo, path string) {
	h := openByPath(path) // Stat(path) path: open once, query the handle
	if h == windows.InvalidHandle {
		return
	}
	defer windows.CloseHandle(h)
	fillFromHandle(id, h)
}

// statHandleIdentity fills the identity from an ALREADY-OPEN handle (all
// fields from windows.Handle(f.Fd()) — same handle, no path query).
func statHandleIdentity(f *os.File, fi os.FileInfo) Identity {
	id := Identity{MtimeNs: fi.ModTime().UnixNano(), Size: fi.Size()}
	h := windows.Handle(f.Fd())
	fillFromHandle(&id, h)
	return id
}

// fillFromHandle queries ChangeTime, volume serial and file index from the
// given handle. Failures leave the fields zero (unknown → hash confirm).
func fillFromHandle(id *Identity, h windows.Handle) {
	var bi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &bi); err != nil {
		return
	}
	id.Dev = uint64(bi.VolumeSerialNumber)
	id.Ino = uint64(bi.FileIndexHigh)<<32 | uint64(bi.FileIndexLow)

	// FILE_BASIC_INFO (class FileBasicInfo) carries the ChangeTime. x/sys
	// exposes the class constant but not the struct, so define the documented
	// Win32 layout here: four 64-bit timestamps + a 32-bit attribute word.
	var basic struct {
		CreationTime   int64
		LastAccessTime int64
		LastWriteTime  int64
		ChangeTime     int64
		FileAttributes uint32
	}
	if err := windows.GetFileInformationByHandleEx(h, windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
		return
	}
	id.CtimeNs = filetimeTicksToUnixNs(basic.ChangeTime)
}

// openByPath opens path (metadata only) for the by-path Stat entry point.
// FILE_FLAG_BACKUP_SEMANTICS: required for directories, harmless for files.
// Returns windows.InvalidHandle on failure.
func openByPath(path string) windows.Handle {
	if path == "" {
		return windows.InvalidHandle
	}
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle
	}
	h, err := windows.CreateFile(p16, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return windows.InvalidHandle
	}
	return h
}

// filetimeTicksToUnixNs converts Windows FILETIME 100-ns ticks (since
// 1601-01-01) to Unix nanoseconds (since 1970-01-01). Zero input stays zero
// so "unknown" is preserved end to end.
func filetimeTicksToUnixNs(ticks int64) int64 {
	if ticks == 0 {
		return 0
	}
	return (ticks - 116444736000000000) * 100
}
