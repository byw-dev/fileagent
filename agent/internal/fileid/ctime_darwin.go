//go:build darwin

package fileid

import "syscall"

// statCtimeNs returns st_ctime in Unix nanoseconds on macOS (the stat blob
// names the field Ctimespec).
func statCtimeNs(st *syscall.Stat_t) int64 {
	return st.Ctimespec.Sec*1e9 + st.Ctimespec.Nsec
}
