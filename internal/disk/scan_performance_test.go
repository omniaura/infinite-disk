package disk

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDiscoveryBreadthFirstAndSkipsGeneratedTreesAndSymlinks(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	for _, p := range []string{filepath.Join(a, "project/.git"), filepath.Join(a, "project/workspaces/nested/.git"), filepath.Join(a, "node_modules/hidden/.git"), filepath.Join(b, "project/.git")} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(b, filepath.Join(a, "linked")); err != nil {
		t.Fatal(err)
	}
	var visited []string
	rows, complete, warnings := discoverProjects(context.Background(), []string{a, b}, 7, func(p string) ([]os.DirEntry, error) { visited = append(visited, p); return os.ReadDir(p) })
	if !complete || len(warnings) > 0 || len(rows) != 3 {
		t.Fatal(rows, complete, warnings)
	}
	if visited[0] != a || visited[1] != b {
		t.Fatal("later root starved", visited)
	}
	if slices.Index(rows, filepath.Join(b, "project")) > slices.Index(rows, filepath.Join(a, "project/workspaces/nested")) {
		t.Fatal("not breadth first", rows)
	}
}

func TestCachedPartialDiscoveryNeverClaimsCompleteCoverage(t *testing.T) {
	f := newFixture(t)
	if _, err := f.e.RunReport(context.Background()); err != nil {
		t.Fatal(err)
	}
	cursor := filepath.Join(f.e.Config.StateDir, "discovery-cursor.json")
	b, err := os.ReadFile(cursor)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err = json.Unmarshal(b, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot["complete"] = false
	b, err = json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	write(t, cursor, string(b))
	p, err := f.e.RunReport(context.Background())
	if err != nil || !p.DiscoveryCached || p.DiscoveryComplete || !strings.Contains(strings.Join(p.Warnings, " "), "partial discovery") {
		t.Fatal("partial coverage lost", p.Warnings, err)
	}
}

func TestRollingBatchesCacheDiscoveryButRecheckSourceAndRefresh(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"a-project", "b-project"} {
		p := filepath.Join(f.root, name)
		runGit(t, f.root, "clone", f.remote, p)
	}
	f.e.ScanLimit = 2
	f.e.SkipProjects = map[string]bool{}
	first, err := f.e.RunReport(context.Background())
	if err != nil || len(first.Candidates) != 2 || first.DiscoveryCached {
		t.Fatal(first, err)
	}
	for _, c := range first.Candidates {
		f.e.SkipProjects[c.Path] = true
	}
	write(t, filepath.Join(f.repo, "source.txt"), "fresh unfinished change\n")
	second, err := f.e.RunReport(context.Background())
	if err != nil || !second.DiscoveryCached || len(second.Candidates) != 1 {
		t.Fatal("batch wrapped or rediscovered", second, err)
	}
	if second.Candidates[0].Eligible || second.Candidates[0].DirtyFiles != 1 {
		t.Fatal("cached discovery reused stale safety checks")
	}
	newPath := filepath.Join(f.root, "new-project")
	runGit(t, f.root, "clone", f.remote, newPath)
	fresh := NewEngine(f.e.Config)
	fresh.Activity, fresh.Sessions = f.e.Activity, f.e.Sessions
	fresh.allowLocalRemote = true
	fresh.ScanLimit = 1
	third, err := fresh.RunReport(context.Background())
	if err != nil || !third.DiscoveryCached || third.DiscoveredProjects != 3 {
		t.Fatal("cross-process index not reused", err)
	}
	fresh.RefreshDiscovery = true
	fourth, err := fresh.RunReport(context.Background())
	if err != nil || fourth.DiscoveryCached || fourth.DiscoveredProjects != 4 {
		t.Fatal("explicit refresh missed new project", err)
	}
	fresh.RefreshDiscovery = false
	fresh.Now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	fifth, err := fresh.RunReport(context.Background())
	if err != nil || fifth.DiscoveryCached {
		t.Fatal("stale index never expires", err)
	}
}

func TestDiscoveryDeadlineRetainsFoundProjectsForInspection(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(base, "project/.git"), "gitdir: fixture")
	if err := os.MkdirAll(filepath.Join(base, "project/deeper"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows, complete, warnings := discoverProjects(ctx, []string{base}, 7, func(p string) ([]os.DirEntry, error) {
		entries, err := os.ReadDir(p)
		if filepath.Base(p) == "project" {
			cancel()
		}
		return entries, err
	})
	if complete || len(warnings) == 0 || len(rows) != 1 {
		t.Fatal("lost partial inventory", rows, complete, warnings)
	}
	// Discovery cancellation is isolated from the caller's inspection context.
	if context.Background().Err() != nil {
		t.Fatal("inspection context canceled")
	}
}

func TestMeasuredFootprintDoesNotDoubleCountOrClaimUnknownSizes(t *testing.T) {
	rows := []Candidate{
		{Path: "/projects/parent/child", SizeBytes: 8, SizeComplete: true},
		{Path: "/projects/parent", SizeBytes: 10, SizeComplete: true},
		{Path: "/projects/parent-other", SizeBytes: 5, SizeComplete: true},
		{Path: "/unknown", SizeBytes: 100, SizeComplete: false},
	}
	n, overlaps := MeasuredFootprint(rows)
	if n != 15 || !overlaps {
		t.Fatal(n, overlaps)
	}
	if rows[0].Path != "/projects/parent/child" {
		t.Fatal("mutated immutable report")
	}
	rows[1].SizeComplete = false
	n, _ = MeasuredFootprint(rows)
	if n != 13 {
		t.Fatal("unknown parent hid measured child", n)
	}
}

func TestPartialCloneBlockedBeforeRecoveryPush(t *testing.T) {
	for _, kind := range []string{"config", "pack marker"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			if kind == "config" {
				runGit(t, f.repo, "config", "remote.origin.promisor", "true")
				runGit(t, f.repo, "config", "remote.origin.partialclonefilter", "blob:none")
			} else {
				write(t, filepath.Join(f.repo, ".git/objects/pack/fixture.promisor"), "")
			}
			p, err := f.e.RunReport(context.Background())
			if err != nil || len(p.Candidates) != 1 {
				t.Fatal(err)
			}
			c := p.Candidates[0]
			if c.Eligible || !slices.Contains(c.Blockers, "partial_clone_requires_materialized_recovery") || CanPreserveSource(c) {
				t.Fatal("partial clone advertised as recoverable", c.Blockers)
			}
			if _, err := f.e.Apply(context.Background(), p, c.ID, p.ID, true); err == nil {
				t.Fatal("partial clone removal allowed")
			}
			exists(t, f.repo)
			if runGit(t, f.remote, "for-each-ref", "--format=%(refname)") != "refs/heads/main" {
				t.Fatal("partial-clone preflight pushed anything")
			}
		})
	}
}

func TestFilteredLinkedWorktreeKeepsCommonHistoryAndVerifiesWorkingSource(t *testing.T) {
	f := newFixture(t)
	head := runGit(t, f.repo, "rev-parse", "HEAD")
	runGit(t, f.repo, "tag", "retained-local-history", head)
	linked := filepath.Join(f.root, "linked-worktree")
	runGit(t, f.repo, "worktree", "add", "-b", "fixture-linked", linked, "main")
	runGit(t, f.repo, "config", "remote.origin.promisor", "true")
	runGit(t, f.repo, "config", "remote.origin.partialclonefilter", "blob:none")
	write(t, filepath.Join(f.repo, ".git/objects/pack/fixture.promisor"), "")
	old(t, filepath.Join(linked, "source.txt"))
	write(t, filepath.Join(linked, "unfinished.txt"), "unfinished fixture work\n")
	old(t, filepath.Join(linked, "unfinished.txt"))
	p, err := f.e.RunReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var selected Candidate
	for _, c := range p.Candidates {
		if c.Path == linked {
			selected = c
		}
	}
	if !selected.Eligible || selected.Kind != "worktree" {
		t.Fatal("surviving shared database treated as deleted clone", selected.Blockers)
	}
	r, err := f.e.Apply(context.Background(), p, selected.ID, p.ID, true)
	if err != nil || r.Result != "removed" || r.BundlePath != "" {
		t.Fatal(r.Result, err)
	}
	exists(t, f.repo)
	if runGit(t, f.repo, "rev-parse", "HEAD") != head || runGit(t, f.repo, "rev-parse", "retained-local-history") != head {
		t.Fatal("shared history changed")
	}
	if got := runGit(t, f.remote, "show", r.RecoveryCommit+":unfinished.txt"); got != "unfinished fixture work" {
		t.Fatal("working source was not preserved", got)
	}
	if !slices.ContainsFunc(r.Steps, func(s Evidence) bool { return s.Check == "remote_recovery" && s.Outcome == "verified" }) {
		t.Fatal("remote recovery not verified")
	}
}

func TestSharedDatabaseCannotBeRemovedWhileLinkedWorktreeHasWork(t *testing.T) {
	f := newFixture(t)
	linked := filepath.Join(f.root, "linked-worktree")
	runGit(t, f.repo, "worktree", "add", "-b", "fixture-linked", linked, "main")
	write(t, filepath.Join(linked, "source.txt"), "staged fixture version\n")
	runGit(t, linked, "add", "source.txt")
	write(t, filepath.Join(linked, "source.txt"), "unstaged fixture version\n")
	p, err := f.e.RunReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var primary Candidate
	for _, c := range p.Candidates {
		if c.Path == f.repo {
			primary = c
		}
	}
	if primary.Eligible || !slices.Contains(primary.Blockers, "shared_git_database_has_worktrees") {
		t.Fatal("shared Git database offered for deletion", primary.Blockers)
	}
	if _, err := f.e.Apply(context.Background(), p, primary.ID, p.ID, true); err == nil {
		t.Fatal("shared database removed")
	}
	exists(t, f.repo)
	exists(t, linked)
	if runGit(t, linked, "show", ":source.txt") != "staged fixture version" {
		t.Fatal("linked staged work lost")
	}
	b, err := os.ReadFile(filepath.Join(linked, "source.txt"))
	if err != nil || string(b) != "unstaged fixture version\n" {
		t.Fatal("linked working changes lost", err)
	}
}
