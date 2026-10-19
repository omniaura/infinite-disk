package disk

// Shared snapshots contain immutable private bytes only. Per-checkout receipts
// retain their own recovery commit, status and session destination. Old receipts
// still point at their original layout; neither layout is garbage-collected.
import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

type sessionSnapshot struct {
	Version   int           `json:"version"`
	Provider  string        `json:"provider"`
	SessionID string        `json:"session_id"`
	Files     []SessionFile `json:"files"`
}

func snapshotIdentity(h ClaudeHandoff) (string, []byte, error) {
	if h.Session.Provider != "Claude Code" || !sessionID.MatchString(h.Session.ID) || len(h.Files) == 0 || len(h.Files) > 5000 {
		return "", nil, errors.New("invalid session snapshot inventory")
	}
	var total int64
	names := map[string]bool{}
	store := false
	for i, f := range h.Files {
		if !sessionPath(f.Path) || !validPlanID.MatchString(f.SHA256) || f.Bytes < 0 || f.Bytes > 512<<20 || total > (512<<20)-f.Bytes || i > 0 && h.Files[i-1].Path >= f.Path || names[pathID(f.Path)] {
			return "", nil, errors.New("invalid session snapshot file")
		}
		total += f.Bytes
		names[pathID(f.Path)] = true
		store = store || f.Path == h.Session.Store
	}
	if !store {
		return "", nil, errors.New("session transcript missing from snapshot")
	}
	s := sessionSnapshot{Version: 1, Provider: h.Session.Provider, SessionID: h.Session.ID, Files: h.Files}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil || len(b) > 8<<20 {
		return "", nil, errors.New("session snapshot manifest exceeds budget")
	}
	id := sha256.Sum256(b)
	return hex.EncodeToString(id[:]), b, nil
}

func privateSnapshotPath(name string, directory bool) error {
	if noSymlinkAncestors(name) != nil {
		return errors.New("unsafe session snapshot path")
	}
	s, err := os.Lstat(name)
	if err != nil {
		return errors.New("session snapshot entry unavailable")
	}
	st, ok := s.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || s.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || directory && (!s.IsDir() || s.Mode().Perm() != 0700) || !directory && (!s.Mode().IsRegular() || s.Mode().Perm() != 0600 || st.Nlink != 1) {
		return errors.New("session snapshot entry is not private regular owned storage")
	}
	return nil
}

func verifySnapshotPayload(name string, expected SessionFile) error {
	if privateSnapshotPath(name, false) != nil {
		return errors.New("unsafe session snapshot payload")
	}
	before, err := os.Lstat(name)
	if err != nil || before.Size() != expected.Bytes {
		return errors.New("session snapshot size changed")
	}
	fd, err := syscall.Open(name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("cannot open session snapshot payload")
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return errors.New("session snapshot payload replaced")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, expected.Bytes+1))
	after, statErr := os.Lstat(name)
	if err != nil || statErr != nil || n != expected.Bytes || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || privateSnapshotPath(name, false) != nil || hex.EncodeToString(h.Sum(nil)) != expected.SHA256 {
		return errors.New("session snapshot readback failed")
	}
	return nil
}

func verifySessionSnapshot(dir string, manifest []byte, files []SessionFile) error {
	if privateSnapshotPath(dir, true) != nil || privateSnapshotPath(filepath.Join(dir, "manifest.json"), false) != nil {
		return errors.New("unsafe or incomplete session snapshot")
	}
	b, err := readPrivateFile(filepath.Join(dir, "manifest.json"), 8<<20)
	if err != nil || !bytes.Equal(b, manifest) {
		return errors.New("session snapshot manifest changed")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != len(files)+1 {
		return errors.New("session snapshot inventory changed")
	}
	for _, f := range files {
		name := filepath.Join(dir, pathID(f.Path))
		if err := verifySnapshotPayload(name, f); err != nil {
			return err
		}
	}
	return nil
}

// Called only inside the approved Apply operation's state lock. Reuse rehashes
// both the live inventory and every backup byte: an existing snapshot is not a
// shortcut around session-change or integrity checks. A crash leaves an invalid
// partial snapshot that blocks reuse, never an overwrite/repair of recovery.
func (e *Engine) sharedClaudeSnapshot(h ClaudeHandoff) (string, error) {
	id, manifest, err := snapshotIdentity(h)
	if err != nil {
		return "", err
	}
	current, err := claudeFiles(h.Session)
	if err != nil || !slices.Equal(current, h.Files) {
		return "", errors.New("live session inventory changed before backup")
	}
	root := filepath.Join(e.Config.StateDir, "session-backups", "shared-v1")
	if noSymlinkAncestors(root) != nil || os.MkdirAll(root, 0700) != nil || privateSnapshotPath(root, true) != nil {
		return "", errors.New("unsafe shared session backup root")
	}
	dir := filepath.Join(root, id)
	if err = os.Mkdir(dir, 0700); errors.Is(err, os.ErrExist) {
		if err = verifySessionSnapshot(dir, manifest, h.Files); err != nil {
			return "", err
		}
		return dir, nil
	} else if err != nil {
		return "", errors.New("cannot create private session snapshot")
	}
	for _, f := range h.Files {
		if err = copySessionFile(f.Path, filepath.Join(dir, pathID(f.Path)), f); err != nil {
			return "", err
		}
	}
	if err = atomicPrivate(filepath.Join(dir, "manifest.json"), manifest); err != nil {
		return "", err
	}
	for _, name := range []string{dir, root} {
		f, err := os.Open(name)
		if err != nil {
			return "", errors.New("cannot sync session snapshot directory")
		}
		syncErr := f.Sync()
		closeErr := f.Close()
		if syncErr != nil || closeErr != nil {
			return "", errors.New("cannot sync session snapshot directory")
		}
	}
	if err = verifySessionSnapshot(dir, manifest, h.Files); err != nil {
		return "", err
	}
	return dir, nil
}
