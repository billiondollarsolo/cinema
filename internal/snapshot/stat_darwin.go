package snapshot

import "syscall"

func statTimes(st *syscall.Stat_t) (mtime, ctime int64) {
	return st.Mtimespec.Nano(), st.Ctimespec.Nano()
}
