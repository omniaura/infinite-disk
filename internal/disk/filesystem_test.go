package disk

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type deviceInfo struct {
	os.FileInfo
	device *syscall.Stat_t
}

func (d deviceInfo) Sys() any { return d.device }
func TestCleanupDeviceBoundary(t *testing.T) {
	dir := t.TempDir()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	stat := *(info.Sys().(*syscall.Stat_t))
	other := stat
	other.Dev++
	if !sameDevice(info, info) || sameDevice(info, deviceInfo{info, &other}) || sameDevice(info, nil) {
		t.Fatal("device boundary did not fail closed")
	}
}
func TestCacheCaseVariantGitMarkerRetained(t *testing.T) {
	f, c := cacheFixture(t)
	write(t, filepath.Join(c.Path, ".GIT"), "gitdir: private")
	old(t, filepath.Join(c.Path, ".GIT"))
	old(t, c.Path)
	got := f.e.inspectCache(context.Background(), c, Activity{Known: true})
	if got.Eligible {
		t.Fatal("case-variant Git marker accepted")
	}
	exists(t, c.Path)
}

func TestCheckoutCaseVariantNestedGitMarkerRetained(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.repo, "node_modules/private/.GIT"), "gitdir: private")
	c := f.e.Inspect(context.Background(), f.repo, Activity{Known: true})
	if c.Eligible {
		t.Fatal("nested case-variant Git marker accepted")
	}
	exists(t, f.repo)
}
