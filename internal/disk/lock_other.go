//go:build !darwin && !linux

package disk

import "errors"

func acquireLock(Config) (func(), error) {
	return nil, errors.New("safe operation locking requires macOS or Linux")
}
