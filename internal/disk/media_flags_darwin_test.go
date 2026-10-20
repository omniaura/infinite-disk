//go:build darwin

package disk

import (
	"os"
	"syscall"
	"testing"
	"time"
)

type offlineFixtureInfo struct{ flags uint32 }

func (s offlineFixtureInfo) Name() string       { return "placeholder.png" }
func (s offlineFixtureInfo) Size() int64        { return 100 }
func (s offlineFixtureInfo) Mode() os.FileMode  { return 0600 }
func (s offlineFixtureInfo) ModTime() time.Time { return time.Time{} }
func (s offlineFixtureInfo) IsDir() bool        { return false }
func (s offlineFixtureInfo) Sys() any           { return &syscall.Stat_t{Flags: s.flags} }
func TestMediaCloudPlaceholderFlags(t *testing.T) {
	for _, flags := range []uint32{0x80, 0x40000000} {
		if !mediaOffline(offlineFixtureInfo{flags}) {
			t.Fatal("cloud placeholder not held")
		}
	}
}
