//go:build darwin || linux

package disk

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func acquireLock(c Config) (func(), error) {
	if err := EnsureState(c); err != nil {
		return nil, err
	}
	path := filepath.Join(c.StateDir, "operation.lock")
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, errors.New("cannot open operation lock")
	}
	f := os.NewFile(uintptr(fd), path)
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrOperationInProgress
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = f.Close() }, nil
}
