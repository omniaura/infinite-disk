//go:build darwin || linux

package disk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func globalDirectoryMatches(fd int, info os.FileInfo) bool {
	var a unix.Stat_t
	b, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.IsDir() && unix.Fstat(fd, &a) == nil && a.Dev == b.Dev && a.Ino == b.Ino && a.Uid == b.Uid && a.Mode == b.Mode
}

// Resolve each parent relative to an already-open approved root. Directory
// identities must match the manifest and no symlink is followed during removal.
func globalParentFD(root int, relative string, entries map[string]globalEntry) (int, error) {
	fd, err := unix.Dup(root)
	if err != nil {
		return -1, err
	}
	if relative == "." {
		return fd, nil
	}
	current := ""
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			unix.Close(fd)
			return -1, errors.New("invalid manifest parent")
		}
		current = filepath.Join(current, part)
		expected, ok := entries[current]
		if !ok {
			unix.Close(fd)
			return -1, errors.New("manifest parent unknown")
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		if !globalDirectoryMatches(next, expected.info) {
			unix.Close(next)
			return -1, errors.New("manifest parent replaced")
		}
		fd = next
	}
	return fd, nil
}

func removeGlobalManifest(ctx context.Context, c Candidate, s globalSnapshot, guard func() error) (int, int, error) {
	removed, retained := 0, 0
	if len(s.entries) == 0 {
		return 0, 0, errors.New("empty manifest")
	}
	root := c.Path
	rootInfo := s.entries[0].info
	installer := c.Kind == "global-installer"
	if installer {
		root = filepath.Dir(root)
		var err error
		rootInfo, err = os.Lstat(root)
		if err != nil || !globalOwned(rootInfo) {
			return 0, 0, errors.New("installer parent unknown")
		}
	}
	if err := noSymlinkAncestors(root); err != nil {
		return 0, 0, err
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, 0, err
	}
	defer unix.Close(fd)
	if !globalDirectoryMatches(fd, rootInfo) {
		return 0, 0, errors.New("approved root replaced")
	}
	entries := map[string]globalEntry{}
	for _, entry := range s.entries {
		entries[entry.rel] = entry
	}
	lastGuard := time.Time{}
	updated := map[string]os.FileInfo{}
	for _, entry := range s.entries {
		if entry.info.IsDir() {
			continue
		}
		if ctx.Err() != nil {
			return removed, retained, ctx.Err()
		}
		if time.Since(lastGuard) >= time.Second {
			if err := guard(); err != nil {
				return removed, retained, err
			}
			lastGuard = time.Now()
		}
		relative := entry.rel
		if installer {
			relative = filepath.Base(c.Path)
		}
		parent, err := globalParentFD(fd, filepath.Dir(relative), entries)
		if err != nil {
			retained++
			continue
		}
		var actual unix.Stat_t
		key := fmt.Sprintf("%d:%d", entry.info.Sys().(*syscall.Stat_t).Dev, entry.info.Sys().(*syscall.Stat_t).Ino)
		expected := entry.info
		if info, ok := updated[key]; ok {
			expected = info
		}
		err = unix.Fstatat(parent, filepath.Base(relative), &actual, unix.AT_SYMLINK_NOFOLLOW)
		if err != nil || !globalStatUnchanged(&actual, expected) {
			retained++
			unix.Close(parent)
			continue
		}
		var opened *os.File
		if expected.Mode().IsRegular() {
			fileFD, openErr := unix.Openat(parent, filepath.Base(relative), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if openErr != nil {
				retained++
				unix.Close(parent)
				continue
			}
			opened = os.NewFile(uintptr(fileFD), relative)
			info, statErr := opened.Stat()
			if statErr != nil || globalIdentity(info) != globalIdentity(expected) {
				opened.Close()
				retained++
				unix.Close(parent)
				continue
			}
		}
		err = unix.Unlinkat(parent, filepath.Base(relative), 0)
		unix.Close(parent)
		if opened != nil {
			if err == nil {
				if info, statErr := opened.Stat(); statErr == nil {
					updated[key] = info
				}
			}
			opened.Close()
		}
		if err != nil {
			retained++
		} else {
			removed++
		}
	}
	return removed, retained, nil
}
