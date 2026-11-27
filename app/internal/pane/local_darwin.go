package pane

import "syscall"

func localBirth(_ string, st *syscall.Stat_t) (int64, bool) {
	return st.Birthtimespec.Nano(), true
}
