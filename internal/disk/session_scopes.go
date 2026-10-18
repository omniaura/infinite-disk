package disk

import (
	"bufio"
	"context"
	"crypto/md5" // Cursor's native namespace, not a security/integrity primitive.
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var cursorNamespace = regexp.MustCompile(`^[0-9a-f]{32}$`)

func cursorHash(path string) string { h := md5.Sum([]byte(path)); return hex.EncodeToString(h[:]) }

// Cursor CLI keys chats by md5(resolve(cwd)). Walk EVERY directory, including
// ignored dependencies, to cover descendant cwd values without reversing a
// lossy hash. Missing cwd values are already orphaned, not invalidated here.
// An incomplete walk is a local candidate blocker, never a false empty match.
func cursorScopeHashes(ctx context.Context, path string) (map[string]bool, error) {
	hashes := map[string]bool{}
	for parent := filepath.Dir(path); parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
		if enclosingWorktree(parent, path) {
			hashes[cursorHash(parent)] = true
		}
	}
	count := 0
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		count++
		if err != nil || ctx.Err() != nil || count > 250000 {
			return errors.New("Cursor cwd scope could not be completely inspected")
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		} // A symlink isn't a canonical cwd.
		if d.IsDir() {
			hashes[cursorHash(p)] = true
		}
		return nil
	})
	return hashes, err
}

func readAdditionalSessionScopes(ctx context.Context, home string, result *SessionInventory, warn func(string), walk func(string, string, func(string, fs.DirEntry) error)) {
	root := filepath.Join(home, ".cursor", "chats")
	walk("Cursor CLI", root, func(path string, d fs.DirEntry) error {
		if path == root {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) == 1 {
			if !d.IsDir() || !cursorNamespace.MatchString(d.Name()) {
				return errors.New("unknown Cursor CLI scope")
			}
			return nil
		}
		if len(parts) == 2 {
			if !d.IsDir() || !sessionID.MatchString(d.Name()) {
				return errors.New("unknown Cursor CLI session")
			}
			store := filepath.Join(path, "store.db")
			info, err := os.Lstat(store)
			if err != nil || !info.Mode().IsRegular() {
				return errors.New("Cursor CLI store is unavailable")
			}
			result.Sessions = append(result.Sessions, Session{Provider: "Cursor CLI", ID: d.Name(), Store: store, WorkspaceHash: parts[0]})
			return filepath.SkipDir
		}
		return errors.New("unknown Cursor CLI layout")
	})
	pi := filepath.Join(home, ".pi", "agent", "sessions")
	walk("Pi", pi, func(path string, d fs.DirEntry) error {
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return errors.New("unknown Pi session file")
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		scanner := bufio.NewScanner(io.LimitReader(f, 64<<10))
		var row struct {
			Type    string `json:"type"`
			Version int    `json:"version"`
			ID      string `json:"id"`
			CWD     string `json:"cwd"`
		}
		if !scanner.Scan() || json.Unmarshal(scanner.Bytes(), &row) != nil || row.Type != "session" || row.Version < 1 || row.Version > 3 || !sessionID.MatchString(row.ID) || !sessionPath(row.CWD) {
			return errors.New("unknown Pi session header")
		}
		result.Sessions = append(result.Sessions, Session{Provider: "Pi", ID: row.ID, Checkout: row.CWD, Store: path})
		return nil
	})
	opencode := filepath.Join(home, ".local", "share", "opencode")
	legacy := filepath.Join(opencode, "storage", "session")
	walk("OpenCode", legacy, func(path string, d fs.DirEntry) error {
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			return errors.New("unknown OpenCode session metadata")
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		var row struct {
			ID        string `json:"id"`
			Directory string `json:"directory"`
		}
		if json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&row) != nil || !sessionID.MatchString(row.ID) || !sessionPath(row.Directory) {
			return errors.New("unknown OpenCode session scope")
		}
		result.Sessions = append(result.Sessions, Session{Provider: "OpenCode", ID: row.ID, Checkout: row.Directory, Store: path})
		return nil
	})
	for _, source := range []struct{ provider, file, query string }{
		{"OpenCode", filepath.Join(opencode, "opencode.db"), "SELECT id,directory AS checkout FROM session LIMIT 20001"},
		{"Emdash", filepath.Join(home, "Library", "Application Support", "emdash", "emdash4.db"), "SELECT id,path AS checkout FROM workspaces WHERE type='local' UNION ALL SELECT id,path AS checkout FROM projects LIMIT 20001"},
	} {
		if _, err := os.Lstat(source.file); errors.Is(err, os.ErrNotExist) {
			continue
		}
		rows, err := readSessionRows(ctx, source.file, source.query)
		if err != nil && source.provider == "Emdash" {
			rows, err = readClosedSessionRows(ctx, source.file, source.query)
		}
		if err != nil {
			warn(source.provider)
			continue
		}
		for _, row := range rows {
			if !sessionID.MatchString(row.ID) || !sessionPath(row.Checkout) {
				warn(source.provider)
				continue
			}
			result.Sessions = append(result.Sessions, Session{Provider: source.provider, ID: row.ID, Checkout: row.Checkout, Store: source.file})
		}
	}
	// Unknown databases are never silently assumed to use the supported schema.
	if files, err := os.ReadDir(opencode); err == nil {
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".db") && f.Name() != "opencode.db" {
				warn("OpenCode unknown index")
			}
		}
	}
}

// Some closed Electron databases cannot create the read-only WAL shared-memory
// sidecar. Immutable mode is allowed ONLY for a quiescent, single-file store:
// no WAL/journal, known no-open-files activity, and identical full readback hash.
// Never use immutable=1 on a live SQLite/WAL database.
func readClosedSessionRows(ctx context.Context, file, query string) ([]sessionRow, error) {
	if noSymlinkAncestors(file) != nil {
		return nil, errors.New("unsafe closed database")
	}
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("closed database is not a regular file")
	}
	activity := ReadActivity(ctx)
	if !activity.Known || activity.Busy(file) {
		return nil, errors.New("database may be active")
	}
	for _, suffix := range []string{"-wal", "-journal"} {
		if _, err := os.Lstat(file + suffix); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("database has a live journal")
		}
	}
	before, err := digestFile(file)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: file, RawQuery: "mode=ro&immutable=1"}
	cmd := exec.CommandContext(ctx, "sqlite3", "-init", os.DevNull, "-batch", "-readonly", "-json", u.String(), query)
	out := &cappedBuffer{limit: 8 << 20}
	cmd.Stdout = out
	if cmd.Run() != nil || out.overflow {
		return nil, errors.New("closed database unavailable")
	}
	var rows []sessionRow
	if strings.TrimSpace(out.String()) != "" && json.Unmarshal(out.Bytes(), &rows) != nil {
		return nil, errors.New("unknown closed database schema")
	}
	after, err := digestFile(file)
	if err != nil || before != after || len(rows) > 20000 {
		return nil, errors.New("closed database changed")
	}
	for _, suffix := range []string{"-wal", "-journal"} {
		if _, err := os.Lstat(file + suffix); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("database journal appeared")
		}
	}
	return rows, nil
}
