package disk

import (
	"golang.org/x/sys/unix"
	"os"
	"syscall"
	"time"
)

func globalChangeTime(info os.FileInfo) time.Time {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}
	}
	return time.Unix(s.Ctimespec.Sec, s.Ctimespec.Nsec)
}

func globalStatUnchanged(a *unix.Stat_t, info os.FileInfo) bool {
	b, ok := info.Sys().(*syscall.Stat_t)
	return ok && a.Dev == b.Dev && a.Ino == b.Ino && a.Uid == b.Uid && a.Mode == b.Mode && a.Size == b.Size && a.Nlink == b.Nlink && a.Mtim.Sec == b.Mtimespec.Sec && a.Mtim.Nsec == b.Mtimespec.Nsec && a.Ctim.Sec == b.Ctimespec.Sec && a.Ctim.Nsec == b.Ctimespec.Nsec
}
