//go:build !darwin && !linux

package disk

import "errors"

func DiskSpace(string) (Space, error) {
	return Space{}, errors.New("disk monitoring is currently supported on macOS and Linux")
}
