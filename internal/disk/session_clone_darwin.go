package disk

import (
	"errors"
	"golang.org/x/sys/unix"
)

const sessionClonePlatform = true

func cloneSessionPayload(from, to string) (bool, error) {
	err := unix.Clonefile(from, to, unix.CLONE_NOFOLLOW)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOSYS) {
		return false, nil
	}
	return false, err
}
