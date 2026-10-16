//go:build darwin || linux

package disk

import "syscall"

func DiskSpace(path string) (Space, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return Space{}, err
	}
	total := uint64(s.Blocks) * uint64(s.Bsize)
	free := uint64(s.Bavail) * uint64(s.Bsize)
	p := float64(0)
	if total > 0 {
		p = float64(free) * 100 / float64(total)
	}
	return Space{TotalBytes: total, FreeBytes: free, FreePercent: p}, nil
}
