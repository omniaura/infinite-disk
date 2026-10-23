//go:build darwin

package disk

import (
	"golang.org/x/sys/unix"
	"os"
	"strings"
	"syscall"
)

func mediaFilesystemLocal(path string) bool {
	var st unix.Statfs_t
	if unix.Statfs(path, &st) != nil {
		return false
	}
	b := make([]byte, 0, len(st.Fstypename))
	for _, v := range st.Fstypename {
		if v == 0 {
			break
		}
		b = append(b, byte(v))
	}
	return strings.EqualFold(string(b), "apfs") || strings.EqualFold(string(b), "hfs")
}

func mediaOffline(info os.FileInfo) bool {
	if info == nil {
		return true
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	// UF_OFFLINE (0x80) and SF_DATALESS are checked before opening any data fork.
	return !ok || st.Flags&(0x80|unix.SF_DATALESS) != 0
}
