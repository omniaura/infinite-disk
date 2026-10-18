package disk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Activity struct {
	Paths []string
	Known bool
}
type Engine struct {
	Config            Config
	Activity          func(context.Context) Activity
	Now               func() time.Time
	Sessions          func(context.Context) SessionInventory
	claudeRunner      func(context.Context, ClaudeHandoff) error // Fixture-only substitution for native CLI.
	allowLocalRemote  bool                                       // Fixture tests only; never configurable by CLI or Pi.
	scanAfter         string                                     // Private discovery cursor; never changes approval scope.
	Progress          func(ScanProgress)                         // Counts/phases only; never source or credentials.
	ScanLimit         int                                        // Report batch size, not approval scope; zero uses configuration.
	RefreshDiscovery  bool
	SkipProjects      map[string]bool // Invocation-local rolling coverage, never approval.
	discovery         []string
	discoveredAt      time.Time
	discoveryComplete bool
}

type ScanProgress struct {
	Phase               string
	Checked, Discovered int
}

func (e *Engine) progress(phase string, checked, discovered int) {
	if e.Progress != nil {
		e.Progress(ScanProgress{phase, checked, discovered})
	}
}

func NewEngine(c Config) *Engine { return &Engine{Config: c, Activity: ReadActivity, Now: time.Now} }

func ReadActivity(ctx context.Context) Activity {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", "-nP", "-u", strconv.Itoa(os.Getuid()), "-Fn")
	output := &cappedBuffer{limit: 16 << 20}
	cmd.Stdout = output
	warnings := &cappedBuffer{limit: 64 << 10}
	cmd.Stderr = warnings
	err := cmd.Run()
	b := output.Bytes()
	// Output truncation cannot be treated as a known activity inventory.
	if err != nil || ctx.Err() != nil || output.overflow || warnings.Len() > 0 || warnings.overflow {
		return Activity{}
	}
	var paths []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "n/") {
			p := strings.TrimPrefix(line, "n")
			if canonical, e := filepath.EvalSymlinks(p); e == nil {
				p = canonical
			}
			paths = append(paths, p)
		}
	}
	return Activity{Paths: paths, Known: len(paths) > 0}
}

func (a Activity) Busy(path string) bool {
	for _, p := range a.Paths {
		if within(path, p) {
			return true
		}
	}
	return false
}

func pathID(path string) string { h := sha256.Sum256([]byte(path)); return hex.EncodeToString(h[:8]) }

func (e *Engine) safeTarget(path string) error {
	clean := filepath.Clean(path)
	canonical, err := filepath.EvalSymlinks(clean)
	if err != nil || !filepath.IsAbs(path) || clean != canonical {
		return errors.New("symlinked or unresolved project path")
	}
	home, _ := os.UserHomeDir()
	if clean == home || clean == string(filepath.Separator) || within(e.Config.StateDir, clean) {
		return errors.New("protected path")
	}
	for _, p := range []string{"Documents", "Pictures", "Movies", "Music", "Downloads", "Library", ".ssh", ".gnupg", ".aws", ".config", ".local", ".claude"} {
		if within(filepath.Join(home, p), clean) {
			return errors.New("protected user-data path")
		}
	}
	if within(filepath.Join(home, ".codex", "worktrees"), clean) {
		return errors.New("Codex-managed worktree requires its native archive lifecycle")
	}
	for _, root := range e.Config.Roots {
		r, err := filepath.EvalSymlinks(root)
		if err == nil && clean != r && within(r, clean) {
			return nil
		}
	}
	return errors.New("project is outside configured roots or equals a root")
}

func block(c *Candidate, reason string) {
	if !slices.Contains(c.Blockers, reason) {
		c.Blockers = append(c.Blockers, reason)
	}
	c.Eligible = false
}

func (e *Engine) Inspect(ctx context.Context, path string, activity Activity) Candidate {
	c := e.inspectSource(ctx, path, activity)
	attachSessionsContext(ctx, &c, e.SessionInventory(ctx))
	e.prepareClaudeHandoffs(ctx, &c, activity)
	return c
}

func (e *Engine) inspectSource(ctx context.Context, path string, activity Activity) Candidate {
	c := Candidate{ID: pathID(path), Path: path, Name: filepath.Base(path), Kind: "clone", Eligible: true, Blockers: []string{}, Evidence: []Evidence{}}
	if err := e.safeTarget(path); err != nil {
		block(&c, err.Error())
		c.Evidence = append(c.Evidence, Evidence{"scope", "blocked", "Protected or unsupported target; source inspection skipped"})
		return measureCandidate(ctx, c)
	}
	if !activity.Known {
		block(&c, "activity_check_unavailable")
	} else if activity.Busy(path) {
		block(&c, "project_in_use")
	}
	c.Evidence = append(c.Evidence, Evidence{"activity", "checked", "Current user open files and working directories"})
	if !c.Eligible {
		c.Evidence[0].Outcome = "blocked"
		return measureCandidate(ctx, c)
	}
	head, err := gitText(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		block(&c, "unreadable_git_repository")
		return measureCandidate(ctx, c)
	}
	c.Head = head
	c.Branch, _ = gitText(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	common, err := gitText(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		block(&c, "git_common_directory_unknown")
		return c
	}
	c.CommonDir = common
	if canonical, err := filepath.EvalSymlinks(common); err != nil || canonical != common {
		block(&c, "symlinked_git_metadata")
	}
	if !within(path, common) {
		c.Kind = "worktree"
	}
	if info, err := os.Lstat(filepath.Join(path, ".gitmodules")); err == nil && info.Mode().IsRegular() {
		block(&c, "submodules_require_manual_preservation")
	}
	indexEntries, indexErr := gitCommand(ctx, path, nil, "ls-files", "--stage", "-z")
	var indexedPaths []byte
	if indexErr != nil {
		block(&c, "index_state_unknown")
	} else {
		for _, entry := range bytes.Split(indexEntries, []byte{0}) {
			metadata, relative, _ := strings.Cut(string(entry), "\t")
			fields := strings.Fields(metadata)
			if len(fields) == 3 {
				indexedPaths = append(indexedPaths, []byte(relative)...)
				indexedPaths = append(indexedPaths, 0)
				if fields[0] == "160000" {
					block(&c, "submodules_require_manual_preservation")
				}
				if fields[2] != "0" {
					block(&c, "unmerged_index_requires_manual_preservation")
				}
			}
		}
	}
	// Git status/diff can invoke globally configured clean filters when a
	// repository requests them through attributes. Detect that before either
	// source command runs; check-attr itself only reads attribute metadata.
	if len(indexedPaths) > 0 {
		attrs, attrErr := gitInput(ctx, path, nil, indexedPaths, "check-attr", "-z", "--stdin", "filter")
		if attrErr != nil {
			block(&c, "content_filter_state_unknown")
		} else {
			fields := strings.Split(attrs, "\x00")
			for i := 2; i < len(fields); i += 3 {
				if fields[i] != "unspecified" && fields[i] != "unset" {
					block(&c, "content_filters_require_manual_preservation")
				}
			}
		}
	}
	if _, err := gitText(ctx, path, "config", "--local", "--name-only", "--get-regexp", `^(core\.sshcommand|credential\.|filter\.|include(if)?\.)`); err == nil {
		block(&c, "repository_execution_or_credential_overrides")
	}
	if shallow, err := gitText(ctx, path, "rev-parse", "--is-shallow-repository"); err != nil || shallow != "false" {
		block(&c, "shallow_repository_requires_manual_preservation")
	}
	// Standalone clones need a self-contained bundle before their Git database
	// is deleted. Linked worktrees keep the already-validated common database;
	// they must not be blocked just because that surviving database is filtered.
	if c.Kind == "clone" {
		if _, err := gitText(ctx, path, "config", "--local", "--name-only", "--get-regexp", `^(extensions\.partialclone|remote\..*\.(promisor|partialclonefilter))$`); err == nil {
			block(&c, "partial_clone_requires_materialized_recovery")
		}
		if entries, err := os.ReadDir(filepath.Join(common, "objects", "pack")); err == nil {
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".promisor") {
					block(&c, "partial_clone_requires_materialized_recovery")
					break
				}
			}
		}
	}
	for _, special := range []string{"lfs", "annex", "objects/info/alternates", "index.lock", "shallow.lock"} {
		if _, err := os.Lstat(filepath.Join(common, special)); err == nil {
			block(&c, "special_or_locked_git_storage_requires_manual_preservation")
		}
	}
	remote, err := gitText(ctx, path, "remote", "get-url", "origin")
	if err != nil {
		block(&c, "no_origin_remote")
	} else {
		host, err := remoteHost(remote)
		if e.allowLocalRemote && filepath.IsAbs(remote) {
			host = "local-fixture"
			err = nil
		}
		c.RemoteHost = host
		c.RemoteDigest = PlanDigest([]byte(remote))
		if err != nil || (!e.allowLocalRemote && !slices.Contains(e.Config.AllowedRemoteHosts, host)) {
			block(&c, "unapproved_or_credential_bearing_remote")
		}
		pushURL, pushErr := gitText(ctx, path, "remote", "get-url", "--push", "--all", "origin")
		fetchURLs, fetchErr := gitText(ctx, path, "remote", "get-url", "--all", "origin")
		if pushErr != nil || fetchErr != nil || pushURL != remote || fetchURLs != remote {
			block(&c, "ambiguous_or_different_push_remote")
		}
	}
	// An already-blocked project needs a size estimate, not an expensive source
	// fingerprint. No later stage can promote it or approve it.
	if !c.Eligible {
		c.Evidence = append(c.Evidence, Evidence{"private_state_and_git", "blocked", "Source fingerprint skipped after mechanical blockers"})
		return measureCandidate(ctx, c)
	}
	// Even ignored dependency caches must not hide another repository.
	count := 0
	projectInfo, projectErr := os.Stat(path)
	if projectErr != nil {
		block(&c, "project_filesystem_unknown")
		return c
	}
	errNested := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		if count > 250000 || ctx.Err() != nil {
			return errors.New("nested inspection budget exceeded")
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !sameDevice(projectInfo, info) {
			return errors.New("project crosses a mount or device metadata is unknown")
		}
		if p != path && strings.EqualFold(d.Name(), ".git") {
			if filepath.Dir(p) != path {
				return errors.New("nested repository")
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	if errNested != nil {
		block(&c, "nested_repository_or_uninspectable_tree")
		return measureCandidate(ctx, c)
	}
	ignored, err := gitCommand(ctx, path, nil, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "--no-empty-directory", "-z")
	if err != nil {
		block(&c, "ignored_state_unknown")
	} else {
		for _, p := range bytes.Split(ignored, []byte{0}) {
			if len(p) > 0 && !allowedIgnored(string(p)) {
				block(&c, "private_or_unknown_ignored_state")
				break
			}
		}
	}
	fp, _, dirty, newest, err := fingerprint(ctx, path)
	if err != nil {
		block(&c, "source_or_secret_check_requires_review")
	} else {
		c.Fingerprint = fp
		c.DirtyFiles = dirty
		c.LastModified = newest
		if newest.IsZero() || e.Now().Sub(newest) < time.Duration(e.Config.IdleDays)*24*time.Hour {
			block(&c, "recent_source_activity")
		}
	}
	// A staged intermediate version and a newer worktree version are two pieces of work.
	staged, stagedErr := gitCommand(ctx, path, nil, "diff", "--no-ext-diff", "--no-textconv", "--cached", "--name-only", "-z")
	// Name-only/quiet diff can report stat-dirty index entries without comparing
	// their bytes when refresh is disabled. Patch generation performs that
	// comparison without refreshing the user's index.
	unstaged, unstagedErr := gitCommand(ctx, path, nil, "diff", "--no-ext-diff", "--no-textconv", "--binary")
	if stagedErr != nil || unstagedErr != nil {
		block(&c, "index_comparison_unknown")
	}
	if len(staged) > 0 && len(unstaged) > 0 {
		block(&c, "staged_and_unstaged_versions_require_manual_preservation")
	}
	c.Evidence = append(c.Evidence, Evidence{"git_source", "checked", fmt.Sprintf("HEAD %s; %d changed files", head, dirty)}, Evidence{"private_state", "checked", "Unknown ignored files and sensitive changed content block removal"})
	return measureCandidate(ctx, c)
}

func measureCandidate(ctx context.Context, c Candidate) Candidate {
	ctxSize, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctxSize, "du", "-sk", c.Path).Output()
	if err == nil {
		fields := strings.Fields(string(output))
		if len(fields) > 0 {
			n, err := strconv.ParseUint(fields[0], 10, 64)
			if err == nil {
				c.SizeBytes = n * 1024
				c.SizeComplete = true
			}
		}
	}
	return c
}

var skipDirs = map[string]bool{".git": true, "node_modules": true, ".venv": true, "__pycache__": true, "target": true, "dist": true, ".terraform": true, ".next": true, ".infinite-disk-quarantine": true}

func (e *Engine) Scan(ctx context.Context) (Plan, error) {
	if err := e.Config.Validate(); err != nil {
		return Plan{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	p := Plan{Version: ProtocolVersion, CreatedAt: e.Now().UTC(), Mode: "report-only", ConfigDigest: ConfigDigest(e.Config), Candidates: []Candidate{}, Warnings: []string{}}
	spacePath, _ := os.UserHomeDir()
	var err error
	p.Space, err = DiskSpace(spacePath)
	if err != nil {
		return p, errors.New("cannot read free disk space")
	}
	e.progress("Checking running processes and coding-session links", 0, 0)
	activity := e.Activity(ctx)
	sessions := e.SessionInventory(ctx)
	p.SessionWarnings = sessions.Warnings
	home, _ := os.UserHomeDir()
	p.Locations, _ = Locations(e.Config, home, false)
	// Discovery must not consume the inspection budget. Breadth-first traversal
	// starts every root before descending through a large early repository.
	discovered, complete := e.discovery, e.discoveryComplete
	var warnings []string
	if e.RefreshDiscovery || (!complete && len(discovered) == 0) || e.discoveredAt.IsZero() || e.Now().Sub(e.discoveredAt) < 0 || e.Now().Sub(e.discoveredAt) >= 15*time.Minute {
		e.progress("Discovering project folders (up to 45 seconds)", 0, 0)
		discoveryCtx, stopDiscovery := context.WithTimeout(ctx, 45*time.Second)
		discovered, complete, warnings = discoverProjects(discoveryCtx, e.Config.Roots, e.Config.MaxDepth, os.ReadDir)
		stopDiscovery()
		e.discovery, e.discoveryComplete, e.discoveredAt = discovered, complete, e.Now().UTC()
	} else {
		p.DiscoveryCached = true
		e.progress("Reusing recent discovery index; rechecking each selected project", 0, len(discovered))
		if !complete {
			warnings = append(warnings, "Reusing a partial discovery index; more project folders may remain undiscovered. Use scan --refresh to refresh discovery.")
		}
	}
	discoveredAt := e.discoveredAt
	p.DiscoveredAt = &discoveredAt
	p.DiscoveryComplete = complete
	p.Warnings = append(p.Warnings, warnings...)
	p.DiscoveredProjects = len(discovered)
	// Discover first, then inspect a rotating bounded window. A large early
	// root cannot permanently starve every later root on periodic audits.
	slices.Sort(discovered)
	start, exact := slices.BinarySearch(discovered, e.scanAfter)
	if exact {
		start++
	}
	if start >= len(discovered) {
		start = 0
	}
	limit := e.Config.MaxProjects
	if e.ScanLimit > 0 {
		limit = min(limit, e.ScanLimit)
	}
	for i := 0; i < len(discovered); i++ {
		if ctx.Err() != nil || len(p.Candidates) >= limit {
			p.Warnings = append(p.Warnings, "Partial inspection; the next audit resumes after this window")
			break
		}
		path := discovered[(start+i)%len(discovered)]
		if e.SkipProjects[path] {
			continue
		}
		e.progress("Inspecting source, recovery prerequisites and sizes", len(p.Candidates), len(discovered))
		projectCtx, stopProject := context.WithTimeout(ctx, 15*time.Second)
		candidate := e.inspectSource(projectCtx, path, activity)
		attachSessionsContext(projectCtx, &candidate, sessions)
		e.prepareClaudeHandoffs(projectCtx, &candidate, activity)
		candidate.Analysis = e.analyzeCandidate(projectCtx, candidate)
		if projectCtx.Err() != nil {
			block(&candidate, "project_inspection_incomplete")
			candidate.Evidence = append(candidate.Evidence, Evidence{"inspection", "incomplete", "Per-project time budget reached; not cleanup-ready"})
		}
		stopProject()
		p.Candidates = append(p.Candidates, candidate)
		e.scanAfter = path
	}
	slices.SortStableFunc(p.Candidates, func(a, b Candidate) int {
		if a.SizeBytes > b.SizeBytes {
			return -1
		}
		if a.SizeBytes < b.SizeBytes {
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	p.ID = planID(p)
	e.progress("Inspection complete; saving report", len(p.Candidates), len(discovered))
	return p, nil
}

// readDir is supplied explicitly so tests can exercise a slow filesystem without
// walking user directories. Symlinks are never enqueued; only canonical roots
// and actual child directories are visited. Safety checks still run at inspect/apply.
func discoverProjects(ctx context.Context, roots []string, maxDepth int, readDir func(string) ([]os.DirEntry, error)) ([]string, bool, []string) {
	type directory struct {
		path  string
		depth int
	}
	var queue []directory
	var found []string
	var warnings []string
	complete := true
	for _, configured := range minimalRoots(roots) {
		root, err := filepath.EvalSymlinks(configured)
		if err != nil {
			complete = false
			warnings = append(warnings, "A configured discovery root is unavailable")
			continue
		}
		queue = append(queue, directory{root, -1})
	}
	seen := map[string]bool{}
	for next := 0; next < len(queue); next++ {
		if ctx.Err() != nil || len(found) >= 10000 {
			return found, false, append(warnings, "Discovery stopped at its 45-second or 10000-project budget; inspection uses the remaining scan time")
		}
		dir := queue[next]
		if dir.depth >= maxDepth {
			continue
		}
		entries, err := readDir(dir.path)
		if err != nil {
			complete = false
			warnings = append(warnings, "A directory could not be inspected")
			continue
		}
		// Inspect the directory entry itself instead of stat-ing .git in every
		// folder. Most source folders are not repositories.
		for _, entry := range entries {
			if entry.Name() == ".git" && dir.depth >= 0 && !seen[dir.path] {
				seen[dir.path] = true
				found = append(found, dir.path)
			}
			if entry.IsDir() && !skipDirs[entry.Name()] && dir.depth+1 < maxDepth {
				queue = append(queue, directory{filepath.Join(dir.path, entry.Name()), dir.depth + 1})
			}
		}
	}
	return found, complete, warnings
}

func planID(p Plan) string {
	p.ID = ""
	b, _ := jsonBytes(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func jsonBytes(v any) ([]byte, error) { return marshalJSON(v) }
