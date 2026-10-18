package disk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Session discovery deliberately excludes titles, prompts, messages, and model
// output. These records stay local and are never included in Pi's inventory.
type Session struct {
	Provider   string `json:"provider"`
	ID         string `json:"id"`
	Checkout   string `json:"checkout,omitempty"`
	Store      string `json:"store"`
	ProjectKey string `json:"project_key,omitempty"`
	Primary    string `json:"primary,omitempty"`
	Source     string `json:"source,omitempty"`
	Originator string `json:"originator,omitempty"`
}

type SessionInventory struct {
	Sessions []Session `json:"sessions"`
	Warnings []string  `json:"warnings"`
	Complete bool      `json:"complete"`
}

var sessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func sessionPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n\x1b")
}

// Claude and Cursor project namespaces are lossy encodings. They are useful
// for conservative positive matches, never for reconstructing a missing path.
func projectKey(path string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, path)
}

func (s Session) Matches(path string) bool {
	if s.Checkout != "" && (within(path, s.Checkout) || within(s.Checkout, path)) {
		return true
	}
	if s.ProjectKey != "" {
		key := strings.TrimLeft(projectKey(path), "-")
		stored := strings.TrimLeft(s.ProjectKey, "-")
		return stored == key || strings.HasPrefix(stored, key+"-") || strings.HasPrefix(key, stored+"-")
	}
	return false
}

func (s Session) Orphaned() bool {
	if !sessionPath(s.Checkout) {
		return false // Unknown is not the same as orphaned.
	}
	_, err := os.Lstat(s.Checkout)
	return errors.Is(err, os.ErrNotExist)
}

// ReadSessions reads bounded native metadata, never transcript text into the
// report. A failed/unsupported store is explicit, not an empty session list.
func ReadSessions(ctx context.Context, home string) SessionInventory {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	result := SessionInventory{Sessions: []Session{}, Warnings: []string{}, Complete: true}
	warn := func(provider string) {
		message := provider + " session coverage is incomplete; native handoff is required before checkout removal"
		for _, existing := range result.Warnings {
			if existing == message {
				return
			}
		}
		result.Warnings = append(result.Warnings, message)
		result.Complete = false
	}
	count := 0
	walk := func(provider, root string, visit func(string, fs.DirEntry) error) {
		if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
			return
		}
		if err := noSymlinkAncestors(root); err != nil {
			warn(provider)
			return
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			count++
			if err != nil || ctx.Err() != nil || count > 50000 {
				return errors.New("session discovery budget or metadata unavailable")
			}
			if d.Type()&os.ModeSymlink != 0 {
				return errors.New("session store contains a symlink")
			}
			return visit(path, d)
		})
		if err != nil {
			warn(provider)
		}
	}
	claude := filepath.Join(home, ".claude", "projects")
	walk("Claude Code", claude, func(path string, d fs.DirEntry) error {
		if d.IsDir() {
			rel, _ := filepath.Rel(claude, path)
			if strings.Count(rel, string(filepath.Separator)) > 0 {
				return filepath.SkipDir // Top-level sessions own their subagents.
			}
			return nil
		}
		id := strings.TrimSuffix(d.Name(), ".jsonl")
		if !strings.HasSuffix(d.Name(), ".jsonl") || !sessionID.MatchString(id) {
			return nil
		}
		if info, err := d.Info(); err != nil || !info.Mode().IsRegular() {
			return errors.New("session transcript is not a regular file")
		}
		s := Session{Provider: "Claude Code", ID: id, Store: path, ProjectKey: filepath.Base(filepath.Dir(path))}
		// A header is an advisory cwd, not proof the complete transcript never
		// moved. The encoded native project namespace supplies an additional
		// conservative match. Linked sessions always require native handoff.
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		scanner := bufio.NewScanner(io.LimitReader(file, 256<<10))
		scanner.Buffer(make([]byte, 4096), 256<<10)
		for scanner.Scan() {
			var header struct {
				CWD string `json:"cwd"`
			}
			if json.Unmarshal(scanner.Bytes(), &header) == nil && sessionPath(header.CWD) {
				s.Checkout = header.CWD
				break
			}
		}
		if scanner.Err() != nil {
			warn("Claude Code header")
		}
		if s.Checkout != "" && projectKey(s.Checkout) != s.ProjectKey {
			// The transcript may have moved natively. A historical header must
			// not be presented as its current cwd or called an orphan.
			s.Checkout = ""
		}
		result.Sessions = append(result.Sessions, s)
		return nil
	})
	// Grok's native session namespace records the URL-encoded absolute cwd.
	grok := filepath.Join(home, ".grok", "sessions")
	walk("Grok", grok, func(path string, d fs.DirEntry) error {
		if !d.IsDir() || path == grok {
			return nil
		}
		rel, _ := filepath.Rel(grok, path)
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) == 2 {
			cwd, err := url.PathUnescape(parts[0])
			if err != nil || !sessionPath(cwd) || !sessionID.MatchString(parts[1]) {
				return errors.New("unsupported session namespace")
			}
			result.Sessions = append(result.Sessions, Session{Provider: "Grok", ID: parts[1], Checkout: cwd, Store: path})
			return filepath.SkipDir
		}
		return nil
	})
	cursor := filepath.Join(home, ".cursor", "projects")
	walk("Cursor", cursor, func(path string, d fs.DirEntry) error {
		if d.IsDir() {
			if filepath.Base(filepath.Dir(path)) == "agent-transcripts" {
				warn("Cursor nested transcript")
				return filepath.SkipDir
			}
			if filepath.Dir(path) == cursor || path == cursor || d.Name() == "agent-transcripts" {
				return nil
			}
			return filepath.SkipDir
		}
		if filepath.Base(filepath.Dir(path)) != "agent-transcripts" {
			return nil
		}
		id := strings.TrimSuffix(strings.TrimSuffix(d.Name(), ".txt"), ".jsonl")
		if !sessionID.MatchString(id) {
			return nil
		}
		result.Sessions = append(result.Sessions, Session{Provider: "Cursor", ID: id, Store: path, ProjectKey: filepath.Base(filepath.Dir(filepath.Dir(path)))})
		return nil
	})
	// These app-owned indexes are queried read-only, with fixed SQL and only
	// identity/path columns. No titles, messages, credentials, or prompts.
	for _, source := range []struct{ provider, file, query string }{
		{"Codex", filepath.Join(home, ".codex", "state_5.sqlite"), "SELECT id, cwd AS checkout, rollout_path AS store, source, originator FROM threads LIMIT 20001"},
		{"T3 Code", filepath.Join(home, ".t3", "userdata", "state.sqlite"), "SELECT t.thread_id AS id, coalesce(t.worktree_path,p.workspace_root) AS checkout,p.workspace_root AS primary_path FROM projection_threads t JOIN projection_projects p ON p.project_id=t.project_id WHERE t.deleted_at IS NULL LIMIT 20001"},
	} {
		if _, err := os.Lstat(source.file); errors.Is(err, os.ErrNotExist) {
			continue
		}
		rows, err := readSessionRows(ctx, source.file, source.query)
		if err != nil {
			warn(source.provider)
			continue
		}
		for _, row := range rows {
			store := row.Store
			if store == "" {
				store = source.file
			}
			if !sessionID.MatchString(row.ID) || !sessionPath(row.Checkout) || !sessionPath(store) {
				warn(source.provider)
				continue
			}
			result.Sessions = append(result.Sessions, Session{Provider: source.provider, ID: row.ID, Checkout: row.Checkout, Store: store, Primary: row.Primary, Source: row.Source, Originator: row.Originator})
		}
	}
	if _, err := os.Lstat(filepath.Join(home, ".codex", "state_5.sqlite")); errors.Is(err, os.ErrNotExist) {
		if entries, err := os.ReadDir(filepath.Join(home, ".codex", "sessions")); err == nil && len(entries) > 0 || err != nil && !errors.Is(err, os.ErrNotExist) {
			warn("Codex unindexed")
		}
	}
	if files, _ := filepath.Glob(filepath.Join(home, ".codex", "state_*.sqlite")); len(files) > 0 {
		for _, file := range files {
			if filepath.Base(file) != "state_5.sqlite" {
				warn("Codex unknown index version")
			}
		}
	}
	// Don't misrepresent opaque/untested stores as having no linked sessions.
	for _, store := range []struct{ provider, path string }{
		{"Cursor CLI", filepath.Join(home, ".cursor", "chats")},
		{"Emdash", filepath.Join(home, "Library", "Application Support", "emdash", "emdash4.db")},
		{"Emdash", filepath.Join(home, ".config", "emdash")},
		{"Pi", filepath.Join(home, ".pi", "agent", "sessions")},
		{"Grok Bot", filepath.Join(home, ".grokbot", "sessions")},
		{"OpenCode", filepath.Join(home, ".local", "share", "opencode")},
	} {
		if info, err := os.Lstat(store.path); err == nil {
			if info.IsDir() {
				entries, readErr := os.ReadDir(store.path)
				if readErr == nil && len(entries) == 0 {
					continue
				}
			}
			warn(store.provider)
		} else if !errors.Is(err, os.ErrNotExist) {
			warn(store.provider)
		}
	}
	if ctx.Err() != nil {
		warn("Coding-agent")
	}
	sort.Slice(result.Sessions, func(i, j int) bool {
		a, b := result.Sessions[i], result.Sessions[j]
		return a.Provider+"/"+a.ID+"/"+a.Store < b.Provider+"/"+b.ID+"/"+b.Store
	})
	return result
}

type sessionRow struct {
	ID         string `json:"id"`
	Checkout   string `json:"checkout"`
	Store      string `json:"store"`
	Primary    string `json:"primary_path"`
	Source     string `json:"source"`
	Originator string `json:"originator"`
}

func readSessionRows(ctx context.Context, file, query string) ([]sessionRow, error) {
	if err := noSymlinkAncestors(file); err != nil {
		return nil, err
	}
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("session index unavailable")
	}
	cmd := exec.CommandContext(ctx, "sqlite3", "-init", os.DevNull, "-batch", "-readonly", "-json", file, query)
	output := &cappedBuffer{limit: 8 << 20}
	cmd.Stdout = output
	if err := cmd.Run(); err != nil || output.overflow {
		return nil, errors.New("session index read failed")
	}
	var rows []sessionRow
	if strings.TrimSpace(output.String()) != "" {
		if err := json.Unmarshal(output.Bytes(), &rows); err != nil {
			return nil, errors.New("unsupported session index")
		}
	}
	if len(rows) > 20000 {
		return nil, errors.New("session index budget exceeded")
	}
	return rows, nil
}

func (e *Engine) SessionInventory(ctx context.Context) SessionInventory {
	if e.Sessions != nil {
		return e.Sessions(ctx)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return SessionInventory{Warnings: []string{"Coding-session home is unavailable"}}
	}
	result := ReadSessions(ctx, home)
	for name, standard := range map[string]string{"CODEX_HOME": filepath.Join(home, ".codex"), "CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude")} {
		if value := os.Getenv(name); value != "" && filepath.Clean(value) != standard {
			result.Complete = false
			result.Warnings = append(result.Warnings, "Custom "+name+" session storage is not covered; native handoff is required")
		}
	}
	return result
}

func attachSessions(c *Candidate, inventory SessionInventory) {
	for _, session := range inventory.Sessions {
		if session.Matches(c.Path) {
			c.Sessions = append(c.Sessions, session)
		}
	}
	if len(c.Sessions) > 0 {
		block(c, "coding_sessions_require_native_handoff")
	}
	if !inventory.Complete {
		block(c, "coding_session_coverage_incomplete")
	}
	c.SessionsChecked = true
}
