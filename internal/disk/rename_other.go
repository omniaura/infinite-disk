//go:build !darwin && !linux

package disk

import "errors"

func renameExclusive(from, to string) error {
	return errors.New("exclusive private relocation is not supported on this OS")
}
