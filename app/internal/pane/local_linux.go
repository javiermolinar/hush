package pane

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func localBirth(path string, st *syscall.Stat_t) (int64, bool) {
	var x unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_INO|unix.STATX_BTIME, &x); err != nil ||
		x.Ino != st.Ino || unix.Mkdev(x.Dev_major, x.Dev_minor) != st.Dev {
		return 0, false
	}
	if x.Mask&unix.STATX_BTIME == 0 {
		return 0, true // no birth timestamp on this filesystem
	}
	return x.Btime.Sec*1e9 + int64(x.Btime.Nsec), true
}
