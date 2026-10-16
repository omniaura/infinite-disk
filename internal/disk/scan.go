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
	allowLocalRemote bool // Fixture tests only; never configurable by CLI or Pi.
}

func NewEngine(c Config) *Engine { return &Engine{Config: c, Activity: ReadActivity, Now: time.Now} }

func ReadActivity(ctx context.Context) Activity {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", "-nP", "-u", strconv.Itoa(os.Getuid()), "-Fn")
	b, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return Activity{}
		}
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
	}
	if !activity.Known {
		block(&c, "activity_check_unavailable")
	} else if activity.Busy(path) {
		block(&c, "project_in_use")
	}
	c.Evidence = append(c.Evidence, Evidence{"activity", "checked", "Current user open files and working directories"})
	head, err := gitText(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		block(&c, "unreadable_git_repository")
		return c
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
	// Subdirectories, including ignored dependency caches, must not hide a second
	// repository. Bound this traversal and fail closed if it cannot finish.
	count := 0
	errNested := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		if count > 250000 || ctx.Err() != nil {
			return errors.New("nested repository inspection budget exceeded")
		}
		if p == path {
			return nil
		}
		if d.Name() == ".git" {
			if filepath.Dir(p) != path {
				return errors.New("nested Git repository")
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	if errNested != nil {
		block(&c, "nested_repository_or_uninspectable_tree")
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
		if err != nil || (!e.allowLocalRemote && !slices.Contains(e.Config.AllowedRemoteHosts, host)) {
			block(&c, "unapproved_or_credential_bearing_remote")
		}
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
	_, staged := gitText(ctx, path, "diff", "--cached", "--quiet")
	_, unstaged := gitText(ctx, path, "diff", "--quiet")
	if staged != nil && unstaged != nil {
		block(&c, "staged_and_unstaged_versions_require_manual_preservation")
	}
	c.Evidence = append(c.Evidence, Evidence{"git_source", "checked", fmt.Sprintf("HEAD %s; %d changed files", head, dirty)}, Evidence{"private_state", "checked", "Unknown ignored files and sensitive changed content block removal"})
	ctxSize, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctxSize, "du", "-sk", path).Output()
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
				if len(p.Candidates) >= e.Config.MaxProjects {
					return errors.New("project limit reached")
				}
				seen[path] = true
				p.Candidates = append(p.Candidates, e.Inspect(ctx, path, activity))
			}
			return nil
		})
		if err != nil {
			p.Warnings = append(p.Warnings, "Scan stopped at its time or project budget")
			break
		}
	}
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
