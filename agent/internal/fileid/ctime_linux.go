//go:build linux

package fileid

import "syscall"

// statCtimeNs returns st_ctime in Unix nanoseconds on Linux (the stat blob
// names the field Ctim).
func statCtimeNs(st *syscall.Stat_t) int64 {
	return st.Ctim.Sec*1e9 + st.Ctim.Nsec
}
