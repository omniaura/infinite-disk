package disk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const disposableNodeModules = "discard_node_modules"

func nodeDependencyPath(relative string) bool {
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		if part == "node_modules" {
			return true
		}
	}
	return false
}

// Never move a mixed private directory wholesale into a backup if it contains
// dependencies. Its nondependency data stays put until a partitioned plan or
// an independently approved dependency cleanup makes preservation possible.
// Legacy private archives still use privateTree unchanged and remain restorable.
func rejectDependencyArchive(ctx context.Context, root, relative string) error {
	if nodeDependencyPath(relative) {
		return errors.New("node_modules is disposable and must not enter a private archive")
	}
	count := 0
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		if count > 250000 || ctx.Err() != nil {
			return errors.New("private dependency-boundary inspection incomplete")
		}
		if d.Name() == "node_modules" {
			return errors.New("mixed private entry contains node_modules; clean dependencies separately and rescan")
		}
		return nil
	})
}

// Only a complete dependency install with retained, tracked regeneration inputs
// gets this policy. Arbitrary cache subsets retain their existing age/private
// checks. Installed package bytes are disposable, not a preservation archive.
func nodeDependencyInputs(ctx context.Context, project, relative string) (string, bool) {
	if relative != "node_modules" {
		return "", false
	}
	h := sha256.New()
	h.Write([]byte(disposableNodeModules))
	var total int64
	read := func(relative string) ([]byte, error) {
		if !safeRelative(relative) || relative == "." {
			return nil, errors.New("unsafe regeneration input")
		}
		file := filepath.Join(project, relative)
		if noSymlinkAncestors(file) != nil {
			return nil, errors.New("symlinked regeneration input")
		}
		before, err := os.Lstat(file)
		if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > 16<<20 {
			return nil, errors.New("regeneration input unavailable")
		}
		tracked, err := gitCommand(ctx, project, nil, "ls-files", "-z", "--", ":(literal)"+filepath.ToSlash(relative))
		if err != nil || string(tracked) != filepath.ToSlash(relative)+"\x00" {
			return nil, errors.New("regeneration input is not tracked")
		}
		fd, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		opened, openErr := fd.Stat()
		data, readErr := io.ReadAll(io.LimitReader(fd, (16<<20)+1))
		closeErr := fd.Close()
		after, statErr := os.Lstat(file)
		total += int64(len(data))
		if openErr != nil || readErr != nil || closeErr != nil || statErr != nil || total > 32<<20 || int64(len(data)) != before.Size() || !os.SameFile(before, opened) || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
			return nil, errors.New("regeneration input changed")
		}
		digest := sha256.Sum256(data)
		fmt.Fprintf(h, "%q %d %x\n", relative, before.Mode(), digest)
		return data, nil
	}
	data, err := read("package.json")
	if err != nil {
		return "", false
	}
	var manifest struct {
		PatchedDependencies map[string]string `json:"patchedDependencies"`
		PNPM                struct {
			PatchedDependencies map[string]string `json:"patchedDependencies"`
		} `json:"pnpm"`
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &manifest) != nil || json.Unmarshal(data, &object) != nil || object == nil {
		return "", false
	}
	locks := 0
	for _, name := range []string{"bun.lock", "bun.lockb", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml"} {
		if _, err := os.Lstat(filepath.Join(project, name)); os.IsNotExist(err) {
			continue
		}
		if _, err := read(name); err != nil {
			return "", false
		}
		locks++
	}
	if locks == 0 {
		return "", false
	}
	patches := map[string]bool{}
	for _, entries := range []map[string]string{manifest.PatchedDependencies, manifest.PNPM.PatchedDependencies} {
		for _, name := range entries {
			patches[name] = true
		}
	}
	if len(patches) > 128 {
		return "", false
	}
	names := make([]string, 0, len(patches))
	for name := range patches {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := read(name); err != nil {
			return "", false
		}
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func artifactLinkInside(root, file, link string, disposable bool) bool {
	resolved, err := filepath.EvalSymlinks(file)
	if err == nil {
		return within(root, resolved) && resolved != root
	}
	// A partial install may have dangling .bin links. They are deleted as links,
	// never followed. Only relative, lexically internal links whose nearest
	// existing ancestor also resolves inside the artifact may be discarded.
	if !disposable || !errors.Is(err, os.ErrNotExist) || filepath.IsAbs(link) {
		return false
	}
	lexical := filepath.Clean(filepath.Join(filepath.Dir(file), link))
	if lexical == root || !within(root, lexical) {
		return false
	}
	for current := lexical; within(root, current); current = filepath.Dir(current) {
		_, statErr := os.Lstat(current)
		if statErr == nil {
			parent, resolveErr := filepath.EvalSymlinks(current)
			return resolveErr == nil && within(root, parent)
		}
		if !errors.Is(statErr, os.ErrNotExist) || current == root || strings.EqualFold(filepath.Base(current), ".git") {
			return false
		}
	}
	return false
}
