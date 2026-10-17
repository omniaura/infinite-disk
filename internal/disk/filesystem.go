package disk

import (
	"os"
	"syscall"
)

// A cleanup target may not traverse a nested mount, even when its lexical path
// is inside the approved directory. Unknown device metadata fails closed.
func sameDevice(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return false
	}
	left, lok := a.Sys().(*syscall.Stat_t)
	right, rok := b.Sys().(*syscall.Stat_t)
	return lok && rok && left != nil && right != nil && left.Dev == right.Dev
}
