//go:build windows

package fileid

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fillPlatform extracts the Windows identity fields (IC-BUG-48):
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
	_ = fi // re-opened by name; os.FileInfo on Windows carries neither ChangeTime nor the file index
	attrs, ok := byHandleInfo(path)
	if !ok {
		return
	}
	id.CtimeNs = filetimeTicksToUnixNs(attrs.changeTimeTicks)
	id.Dev = uint64(attrs.volumeSerial)
	id.Ino = uint64(attrs.fileIndexHigh)<<32 | uint64(attrs.fileIndexLow)
}

// byHandleInfo opens path (metadata only) and reads the volume serial, file
// index and basic-info block. It reports ok=false when any step fails — the
// caller then leaves the fields zero (unknown → hash confirmation).
func byHandleInfo(path string) (hInfo, bool) {
	var out hInfo
	if path == "" {
		return out, false
	}
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return out, false
	}
	// FILE_FLAG_BACKUP_SEMANTICS: required for directories, harmless for
	// files; we only ever read attributes through this handle.
	h, err := windows.CreateFile(p16, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return out, false
	}
	defer windows.CloseHandle(h)

	var bi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &bi); err != nil {
		return out, false
	}
	out.volumeSerial = bi.VolumeSerialNumber
	out.fileIndexHigh = bi.FileIndexHigh
	out.fileIndexLow = bi.FileIndexLow

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
		return out, false
	}
	out.changeTimeTicks = basic.ChangeTime
	return out, true
}

// hInfo is the raw Windows identity data before conversion.
type hInfo struct {
	volumeSerial    uint32
	fileIndexHigh   uint32
	fileIndexLow    uint32
	changeTimeTicks int64 // FILETIME: 100-ns ticks since 1601-01-01 UTC
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
