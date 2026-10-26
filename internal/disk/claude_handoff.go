package disk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Claude owns transcript mutation. Infinite Disk only makes byte-exact private
// copies, stages a reversible checkout move, and asks Claude to resume locally.
// A native init in Destination plus an on-disk cleared binding is required;
// neither a saved argv nor a successful process exit is sufficient.
type ClaudeHandoff struct {
	Session             Session       `json:"session"`
	Destination         string        `json:"destination"`
	TargetIdentity      string        `json:"target_identity"`
	Executable          string        `json:"executable,omitempty"`
	NativeIdentity      string        `json:"native_identity,omitempty"`
	Files               []SessionFile `json:"files"`
	BackupDir           string        `json:"backup_dir,omitempty"`
	Status              string        `json:"status"`
	RecoveryCommit      string        `json:"recovery_commit,omitempty"`
	VerifiedStoreSHA256 string        `json:"verified_store_sha256,omitempty"`
	byID                bool          // Second native verification uses normal global ID lookup.
}

type SessionFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type claudeMetadata struct {
	CWD           string
	Binding       string
	BindingSeen   bool
	RelocatedSeen bool
	Messages      int
	References    []string
}

var claudeCWDField = regexp.MustCompile(`"cwd"\s*:\s*("(?:[^"\\]|\\.)*")`)

// The internal metadata reader is version-gated at mutation time. Unknown,
// oversized, malformed, compressed, or ambiguous history cannot be promoted.
func readClaudeMetadataContext(ctx context.Context, file, id string, associationOnly bool) (claudeMetadata, error) {
	var m claudeMetadata
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 512<<20 {
		return m, errors.New("Claude transcript is unavailable or exceeds the inspection budget")
	}
	f, err := os.Open(file)
	if err != nil {
		return m, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(io.LimitReader(f, (512<<20)+1))
	scanner.Buffer(make([]byte, 4096), 16<<20)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return m, errors.New("Claude metadata inspection was cancelled")
		}
		line := scanner.Bytes()
		if associationOnly {
			for _, field := range claudeCWDField.FindAllSubmatch(line, 65) {
				if len(field[1]) > 16386 {
					return m, errors.New("Claude cwd metadata exceeds its budget")
				}
				var cwd string
				if json.Unmarshal(field[1], &cwd) == nil && sessionPath(cwd) && !slices.Contains(m.References, cwd) {
					m.References = append(m.References, cwd)
					if len(m.References) > 64 {
						return m, errors.New("Claude cwd reference budget exceeded")
					}
				}
			}
		}
		// Full-file association is essential: hook-created worktrees may keep
		// their transcript in the PRIMARY namespace, and the binding can be
		// far beyond any bounded header/tail. Avoid decoding conversation bodies
		// once a cwd is known; only routing metadata needs decoding here.
		if associationOnly && m.CWD != "" && !bytes.Contains(line, []byte(`"worktree-state"`)) && !bytes.Contains(line, []byte(`"worktreeSession"`)) && !bytes.Contains(line, []byte(`"relocated"`)) {
			continue
		}
		var row struct {
			Type      string          `json:"type"`
			SessionID string          `json:"sessionId"`
			CWD       string          `json:"cwd"`
			Relocated string          `json:"relocatedCwd"`
			Worktree  json.RawMessage `json:"worktreeSession"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			return m, errors.New("Claude transcript metadata is not readable")
		}
		if row.SessionID != "" && row.SessionID != id {
			return m, errors.New("Claude transcript has mixed session identities")
		}
		if sessionPath(row.CWD) && m.CWD == "" {
			m.CWD = row.CWD
		}
		if row.Type == "relocated" {
			m.RelocatedSeen = true
			if !sessionPath(row.Relocated) {
				return m, errors.New("Claude relocation metadata is unsupported")
			}
			m.CWD = row.Relocated
		}
		if row.Type == "user" || row.Type == "assistant" {
			m.Messages++
		}
		if row.Type == "worktree-state" {
			m.BindingSeen = true
			m.Binding = ""
			if !bytes.Equal(row.Worktree, []byte("null")) {
				var binding struct {
					Path string `json:"worktreePath"`
				}
				if json.Unmarshal(row.Worktree, &binding) != nil || !sessionPath(binding.Path) {
					return m, errors.New("Claude worktree binding is unsupported")
				}
				m.Binding = binding.Path
			}
		} else if row.Worktree != nil {
			return m, errors.New("Claude routing metadata version is unsupported")
		}
	}
	slices.Sort(m.References)
	if scanner.Err() != nil || !associationOnly && (m.Messages == 0 || !sessionPath(m.CWD)) || associationOnly && m.Messages > 0 && m.CWD == "" && m.Binding == "" {
		return m, errors.New("Claude transcript is incomplete or has no conversation")
	}
	return m, nil
}

func claudeFiles(s Session) ([]SessionFile, error) {
	return claudeFilesWithDigest(s, digestFile)
}

// A report may reuse a digest only through its invocation-local validated
// reader. Mutation, backup and handoff verification always call claudeFiles.
func claudeFilesWithDigest(s Session, digest func(string) (string, error)) ([]SessionFile, error) {
	if !sessionID.MatchString(s.ID) || filepath.Base(s.Store) != s.ID+".jsonl" || filepath.Base(filepath.Dir(filepath.Dir(s.Store))) != "projects" {
		return nil, errors.New("Claude session store is not a supported native layout")
	}
	config := filepath.Dir(filepath.Dir(filepath.Dir(s.Store)))
	roots := []string{s.Store, strings.TrimSuffix(s.Store, ".jsonl"), filepath.Join(config, "tasks", s.ID), filepath.Join(config, "file-history", s.ID), filepath.Join(config, "session-env", s.ID)}
	todos, err := filepath.Glob(filepath.Join(config, "todos", s.ID+"*.json"))
	if err != nil {
		return nil, err
	}
	roots = append(roots, todos...)
	var files []SessionFile
	var total int64
	for _, root := range roots {
		if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := noSymlinkAncestors(root); err != nil {
			return nil, err
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("unsafe Claude session sidecar")
			}
			if info.IsDir() {
				return nil
			}
			if !info.Mode().IsRegular() {
				return errors.New("unsupported Claude session sidecar")
			}
			total += info.Size()
			if total > 512<<20 || len(files) >= 5000 {
				return errors.New("Claude session backup budget exceeded")
			}
			var sha string
			if info.Size() < minimumReportDigestBytes {
				sha, err = digestFile(path)
			} else {
				sha, err = digest(path)
			}
			if err != nil {
				return err
			}
			files = append(files, SessionFile{path, sha, info.Size()})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(files, func(a, b SessionFile) int { return strings.Compare(a.Path, b.Path) })
	return files, nil
}

func nativeClaudeIdentity(ctx context.Context) (string, string, error) {
	parent := ctx
	bin, err := exec.LookPath("claude")
	if err != nil {
		return "", "", errors.New("Claude Code is not installed")
	}
	bin, err = filepath.EvalSymlinks(bin)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(bin)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", errors.New("Claude executable unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	out := &cappedBuffer{limit: 4096}
	cmd.Stdout = out
	if cmd.Run() != nil || out.overflow {
		return "", "", errors.New("Claude Code version check is unavailable; checkout and sessions retained")
	}
	output := strings.TrimSpace(out.String())
	version := strings.TrimSuffix(output, " (Claude Code)")
	if !claudeVersionOutput.MatchString(output) || version == output {
		return "", "", claudeCompatibilityError(output)
	}
	b, _ := json.Marshal([]any{bin, info.Size(), info.ModTime().UnixNano(), output})
	identity := PlanDigest(b)
	if err := verifiedClaudeExecutable(parent, bin, identity, version); err != nil {
		return "", "", err
	}
	return bin, identity, nil
}

func claudeWorkspacePrefix(path string) (string, bool) {
	for _, marker := range []string{string(filepath.Separator) + ".claude" + string(filepath.Separator) + "worktrees" + string(filepath.Separator), string(filepath.Separator) + ".worktrees" + string(filepath.Separator) + "claude-"} {
		if prefix, _, ok := strings.Cut(path, marker); ok {
			return prefix, true
		}
	}
	return "", false
}

func (e *Engine) claudeDestination(ctx context.Context, c Candidate) (string, string, error) {
	var choices []string
	if c.Kind == "worktree" {
		listing, err := gitText(ctx, c.Path, "worktree", "list", "--porcelain")
		if err != nil {
			return "", "", err
		}
		// Git lists the primary checkout first. Never select another temporary
		// linked checkout simply because its origin happens to match.
		line := strings.Split(listing, "\n")[0]
		if strings.HasPrefix(line, "worktree ") {
			choices = append(choices, strings.TrimPrefix(line, "worktree "))
		}
	} else if prefix, ok := claudeWorkspacePrefix(c.Path); ok {
		choices = []string{filepath.Join(prefix, filepath.Base(c.Path)), filepath.Join(filepath.Dir(prefix), filepath.Base(c.Path))}
	}
	var target, identity string
	for _, path := range choices {
		if !sessionPath(path) || within(c.Path, path) || within(path, e.Config.StateDir) || noSymlinkAncestors(path) != nil {
			continue
		}
		root, err := gitText(ctx, path, "rev-parse", "--show-toplevel")
		if err != nil || root != path {
			continue
		}
		remote, err := gitText(ctx, path, "remote", "get-url", "origin")
		if err != nil || PlanDigest([]byte(remote)) != c.RemoteDigest {
			continue
		}
		head, err := gitText(ctx, path, "rev-parse", "HEAD")
		if err != nil {
			continue
		}
		common, err := gitText(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			continue
		}
		common, err = filepath.EvalSymlinks(common)
		if err != nil || c.Kind == "worktree" && common != c.CommonDir {
			continue
		}
		if target != "" && target != path {
			return "", "", errors.New("surviving project is ambiguous")
		}
		target = path
		b, _ := json.Marshal([]string{path, common, head})
		identity = PlanDigest(b)
	}
	if target == "" {
		return "", "", errors.New("no verified surviving project")
	}
	return target, identity, nil
}

func (e *Engine) prepareClaudeHandoffs(ctx context.Context, c *Candidate, activity Activity) {
	e.prepareClaudeHandoffsWithDigest(ctx, c, activity, digestFile)
}

func (e *Engine) prepareClaudeHandoffsWithDigest(ctx context.Context, c *Candidate, activity Activity, digest func(string) (string, error)) {
	if c.SessionCount == 0 || c.SessionCount != len(c.Sessions) || !activity.Known {
		return
	}
	interrupted := func() bool {
		if ctx.Err() == nil {
			return false
		}
		block(c, "activity_check_unavailable")
		return true
	}
	lookup, err := indexActivity(ctx, activity)
	if err != nil || interrupted() {
		block(c, "activity_check_unavailable")
		return
	}
	// A child repo can be idle while Claude is still running in its enclosing
	// stack workspace. Surface that activity before private archival, not only
	// after ignored files have already been relocated for removal analysis.
	for _, s := range c.Sessions {
		if interrupted() {
			return
		}
		if s.Provider == "Claude Code" && (lookup.Busy(s.Store) || lookup.Busy(s.Checkout)) {
			block(c, "project_in_use")
			c.Evidence = append(c.Evidence, Evidence{"claude_sessions", "active", "An associated Claude conversation or its working directory is in use; keep this checkout and its private files"})
			return
		}
	}
	for _, blocker := range c.Blockers {
		if blocker != "coding_sessions_require_native_handoff" {
			return
		}
	}
	// Other apps' managed workspaces require their own lifecycle adapter.
	if _, ok := claudeWorkspacePrefix(c.Path); !ok {
		return
	}
	// A surviving parent conversation does not need a same-origin primary
	// checkout for the child repository. Resolve a different Git destination
	// only for conversations actually bound inside the removed checkout.
	var target, identity string
	var handoffs []ClaudeHandoff
	for _, s := range c.Sessions {
		if interrupted() {
			return
		}
		if s.Provider != "Claude Code" || within(c.Path, s.Store) || lookup.Busy(s.Store) || lookup.Busy(s.Checkout) {
			return
		}
		m, err := readClaudeMetadataContext(ctx, s.Store, s.ID, false)
		if err != nil || m.CWD != s.Checkout || !sessionPath(s.Checkout) {
			return
		}
		files, err := claudeFilesWithDigest(s, digest)
		if err != nil {
			return
		}
		for _, f := range files {
			if interrupted() {
				return
			}
			if within(c.Path, f.Path) || lookup.Busy(f.Path) {
				return
			}
		}
		h := ClaudeHandoff{Session: s, Files: files, Status: "planned"}
		if !within(c.Path, s.Checkout) {
			// A stack session in a surviving parent workspace is not orphaned by
			// deleting one child repo. Archive/link it, but don't steal its cwd.
			if !enclosingWorktree(s.Checkout, c.Path) {
				return
			}
			if noSymlinkAncestors(s.Checkout) != nil {
				return
			}
			parent, err := os.Lstat(s.Checkout)
			if err != nil || !parent.IsDir() {
				return
			}
			parentIdentity, err := privateIdentity(parent)
			if err != nil {
				return
			}
			h.Destination = s.Checkout
			h.TargetIdentity = PlanDigest([]byte(s.Checkout + "\x00" + parentIdentity))
			h.Status = "retain_parent"
		} else {
			if target == "" {
				target, identity, err = e.claudeDestination(ctx, *c)
				if err != nil {
					return
				}
			}
			h.Destination, h.TargetIdentity = target, identity
			if m.Binding != "" && !within(c.Path, m.Binding) {
				return
			}
			if e.claudeRunner == nil {
				h.Executable, h.NativeIdentity, err = nativeClaudeIdentity(ctx)
				if err != nil {
					return
				}
			}
		}
		handoffs = append(handoffs, h)
	}
	encoded, _ := json.Marshal(handoffs)
	if len(encoded) > 8<<20 || interrupted() {
		return
	}
	c.SessionHandoffs = handoffs
	c.Blockers = slices.DeleteFunc(c.Blockers, func(s string) bool { return s == "coding_sessions_require_native_handoff" })
	c.Eligible = len(c.Blockers) == 0
	c.Evidence = append(c.Evidence, Evidence{"claude_sessions", "planned", "Private byte-exact session backup and native local handoff will be verified before permanent removal; surviving parent sessions stay in place"})
}

func sameHandoffs(a, b []ClaudeHandoff) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func copySessionFile(from, to string, expected SessionFile) error {
	if err := noSymlinkAncestors(from); err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, io.LimitReader(in, expected.Bytes+1))
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("private session backup write failed")
	}
	sha, err := digestFile(to)
	if err != nil || sha != expected.SHA256 {
		return errors.New("private session backup readback failed")
	}
	return nil
}

func (e *Engine) backupClaudeSessions(c Candidate, r *Receipt) error {
	for _, h := range c.SessionHandoffs {
		backup, err := e.sharedClaudeSnapshot(h)
		if err != nil {
			return err
		}
		h.BackupDir = backup
		h.RecoveryCommit = r.RecoveryCommit
		h.Status = "backed_up"
		r.SessionHandoffs = append(r.SessionHandoffs, h)
		if err := saveReceipt(e.Config, *r); err != nil {
			return err
		}
	}
	return nil
}

// An empty-tools, offline, bare invocation processes a fixed local command.
// It cannot execute hooks/MCP/tools, read user credentials, or send a model
// prompt. A loopback-only dummy endpoint is defense in depth against CLI drift.
func runClaudeHandoff(ctx context.Context, h ClaudeHandoff) error {
	bin, identity, err := nativeClaudeIdentity(ctx)
	if err != nil || bin != h.Executable || identity != h.NativeIdentity {
		return errors.New("Claude executable changed; checkout retained")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := claudeLocalCommand(ctx, bin, h)
	out := &cappedBuffer{limit: 1 << 20}
	cmd.Stdout = out
	if cmd.Run() != nil || out.overflow {
		return errors.New("native Claude local handoff failed; checkout retained")
	}
	return verifyClaudeOutput(out.Bytes(), h)
}

func claudeLocalCommand(ctx context.Context, bin string, h ClaudeHandoff) *exec.Cmd {
	resume := h.Session.Store
	if h.byID {
		resume = h.Session.ID
	}
	cmd := exec.CommandContext(ctx, bin, "--bare", "--setting-sources", "", "--settings", `{"disableAllHooks":true,"cleanupPeriodDays":3650000,"autoMemoryEnabled":false}`, "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--tools", "", "--disable-slash-commands", "-p", "--resume", resume, "--output-format", "stream-json", "--verbose", "--max-turns", "0", "/status")
	cmd.Dir = h.Destination
	for _, key := range []string{"HOME", "PATH", "TMPDIR", "USER", "SYSTEMROOT"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	config := filepath.Dir(filepath.Dir(filepath.Dir(h.Session.Store)))
	cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+config, "ANTHROPIC_API_KEY=infinite-disk-offline-local-handoff", "ANTHROPIC_BASE_URL=http://127.0.0.1:1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	return cmd
}

func verifyClaudeOutput(output []byte, h ClaudeHandoff) error {
	init, result := false, false
	initializations, results := 0, 0
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var row struct {
			Type       string                     `json:"type"`
			Subtype    string                     `json:"subtype"`
			CWD        string                     `json:"cwd"`
			ID         string                     `json:"session_id"`
			Error      *bool                      `json:"is_error"`
			Turns      *int                       `json:"num_turns"`
			Cost       *float64                   `json:"total_cost_usd"`
			Tools      []json.RawMessage          `json:"tools"`
			MCP        []json.RawMessage          `json:"mcp_servers"`
			APIMillis  *int                       `json:"duration_api_ms"`
			ModelUsage map[string]json.RawMessage `json:"modelUsage"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			return errors.New("unsupported native Claude response")
		}
		if row.Type == "system" && row.Subtype == "init" {
			initializations++
			init = row.ID == h.Session.ID && row.CWD == h.Destination && row.Tools != nil && row.MCP != nil && len(row.Tools) == 0 && len(row.MCP) == 0
		}
		if row.Type == "result" {
			results++
			result = row.ID == h.Session.ID && row.Subtype == "success" && row.Error != nil && !*row.Error && row.Turns != nil && row.Cost != nil && *row.Turns == 0 && *row.Cost == 0 && row.APIMillis != nil && *row.APIMillis == 0 && row.ModelUsage != nil && len(row.ModelUsage) == 0
		}
	}
	if scanner.Err() != nil || !init || !result || initializations != 1 || results != 1 {
		return errors.New("native Claude identity/cwd/zero-turn verification failed")
	}
	return nil
}

func verifyOriginalPrefix(h ClaudeHandoff) error {
	current, err := claudeFiles(h.Session)
	if err != nil || len(current) != len(h.Files) {
		return errors.New("Claude session sidecar inventory changed")
	}
	for i, f := range current {
		if f.Path != h.Files[i].Path {
			return errors.New("Claude session sidecar inventory changed")
		}
	}
	for _, f := range h.Files {
		if noSymlinkAncestors(f.Path) != nil {
			return errors.New("session storage changed")
		}
		backup := filepath.Join(h.BackupDir, pathID(f.Path))
		sha, err := digestFile(backup)
		if err != nil || sha != f.SHA256 {
			return errors.New("session backup integrity changed")
		}
		// Claude may append metadata to the parent transcript, never replace
		// existing history or mutate subagent/checkpoint state in this mode.
		if f.Path != h.Session.Store {
			sha, err = digestFile(f.Path)
			if err != nil || sha != f.SHA256 {
				return errors.New("Claude sidecar changed during handoff")
			}
			continue
		}
		live, err := os.Open(f.Path)
		if err != nil {
			return err
		}
		original, err := os.Open(backup)
		if err != nil {
			live.Close()
			return err
		}
		a, b := make([]byte, 64<<10), make([]byte, 64<<10)
		var remaining = f.Bytes
		for remaining > 0 {
			n := int64(len(a))
			if n > remaining {
				n = remaining
			}
			_, ea := io.ReadFull(live, a[:n])
			_, eb := io.ReadFull(original, b[:n])
			if ea != nil || eb != nil || !bytes.Equal(a[:n], b[:n]) {
				live.Close()
				original.Close()
				return errors.New("native Claude changed original conversation history")
			}
			remaining -= n
		}
		live.Close()
		original.Close()
	}
	return nil
}

func (e *Engine) handoffClaudeSessions(ctx context.Context, r *Receipt) error {
	for i := range r.SessionHandoffs {
		h := &r.SessionHandoffs[i]
		if !within(r.Path, h.Session.Checkout) {
			h.Status = "retained_parent"
			for _, f := range h.Files {
				if f.Path == h.Session.Store {
					h.VerifiedStoreSHA256 = f.SHA256
				}
			}
			continue
		}
		runner := e.claudeRunner
		if runner == nil {
			runner = runClaudeHandoff
		}
		// A second independent resume checks that clearing persisted, not just
		// an in-memory cwd change during the first process.
		for attempt := range 2 {
			route, err := e.PrepareSessionResume(ctx, "Claude Code", h.Session.ID, h.Destination)
			if err != nil || route.TargetIdentity != h.TargetIdentity {
				return errors.New("session destination changed; checkout retained")
			}
			probe := *h
			probe.byID = attempt == 1
			if err := runner(ctx, probe); err != nil {
				return err
			}
			m, err := readClaudeMetadataContext(ctx, h.Session.Store, h.Session.ID, false)
			if err != nil || m.Binding != "" {
				return errors.New("native Claude binding was not cleared")
			}
			if err := verifyOriginalPrefix(*h); err != nil {
				return err
			}
		}
		h.Status = "native_verified"
		sha, err := digestFile(h.Session.Store)
		if err != nil {
			return err
		}
		h.VerifiedStoreSHA256 = sha
		// Approval already covers this exact immutable destination. Do not
		// reacquire the operation lock inside Apply.
		route, err := e.PrepareSessionResume(ctx, "Claude Code", h.Session.ID, h.Destination)
		if err != nil || route.TargetIdentity != h.TargetIdentity {
			if err == nil {
				err = errors.New("session destination changed; checkout retained")
			}
			return err
		}
		route.SavedAt = e.Now().UTC()
		b, _ := json.MarshalIndent(route, "", "  ")
		if err := atomicPrivate(filepath.Join(e.Config.StateDir, "session-routes", pathID("Claude Code/"+h.Session.ID)+".json"), b); err != nil {
			return err
		}
		if err := saveReceipt(e.Config, *r); err != nil {
			return err
		}
	}
	return nil
}
