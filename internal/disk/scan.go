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
	Config           Config
	Activity         func(context.Context) Activity
	Now              func() time.Time
	allowLocalRemote bool   // Fixture tests only; never configurable by CLI or Pi.
	scanAfter        string // Private discovery cursor; never changes approval scope.
}

func NewEngine(c Config) *Engine { return &Engine{Config: c, Activity: ReadActivity, Now: time.Now} }

func ReadActivity(ctx context.Context) Activity {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", "-nP", "-u", strconv.Itoa(os.Getuid()), "-Fn")
	output := &cappedBuffer{limit: 16 << 20}
	cmd.Stdout = output
	err := cmd.Run()
	b := output.Bytes()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return Activity{}
		}
	}
	// Output truncation cannot be treated as a known activity inventory.
	if output.overflow {
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
	errNested := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		if count > 250000 || ctx.Err() != nil {
			return errors.New("nested inspection budget exceeded")
		}
		if p != path && d.Name() == ".git" {
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
	activity := e.Activity(ctx)
	seen := map[string]bool{}
	var discovered []string
	for _, configured := range e.Config.Roots {
		root, err := filepath.EvalSymlinks(configured)
		if err != nil {
			p.Warnings = append(p.Warnings, "A configured discovery root is unavailable")
			continue
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil {
				p.Warnings = append(p.Warnings, "A directory could not be inspected")
				return filepath.SkipDir
			}
			if !d.IsDir() {
				return nil
			}
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			rel, _ := filepath.Rel(root, path)
			if strings.Count(rel, string(filepath.Separator)) >= e.Config.MaxDepth {
				return filepath.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil && path != root && !seen[path] {
				if len(discovered) >= 10000 {
					return errors.New("discovery limit reached")
				}
				seen[path] = true
				discovered = append(discovered, path)
			}
			return nil
		})
		if err != nil {
			p.Warnings = append(p.Warnings, "Discovery stopped at its time or 10000-project budget")
			break
		}
	}
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
	for i := 0; i < len(discovered); i++ {
		if ctx.Err() != nil || len(p.Candidates) >= e.Config.MaxProjects {
			p.Warnings = append(p.Warnings, "Partial inspection; the next audit resumes after this window")
			break
		}
		path := discovered[(start+i)%len(discovered)]
		p.Candidates = append(p.Candidates, e.Inspect(ctx, path, activity))
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
	return p, nil
}

func planID(p Plan) string {
	p.ID = ""
	b, _ := jsonBytes(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func jsonBytes(v any) ([]byte, error) { return marshalJSON(v) }
