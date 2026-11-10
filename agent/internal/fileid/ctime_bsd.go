//go:build unix && !darwin && !linux

package fileid

import "syscall"

// statCtimeNs returns st_ctime in Unix nanoseconds on Unix platforms that
// name the field Ctimespec (the BSD family). When a new platform's stat blob
// differs, add a file for it — unknown stays zero (fail-closed).
func statCtimeNs(st *syscall.Stat_t) int64 {
	return st.Ctimespec.Sec*1e9 + st.Ctimespec.Nsec
}
