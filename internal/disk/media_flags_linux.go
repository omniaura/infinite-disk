//go:build linux

package disk

import (
	"os"
	"syscall"
)

func mediaOffline(info os.FileInfo) bool { return info == nil }

// Unknown/network/FUSE filesystems are metadata-only, never implicitly hydrated.
func mediaFilesystemLocal(path string) bool {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return false
	}
	switch uint64(st.Type) {
	case 0xEF53, 0x58465342, 0x9123683E, 0x01021994, 0x794c7630:
		return true
	}
	return false
}
