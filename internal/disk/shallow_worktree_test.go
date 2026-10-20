package disk

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func shallowLinkedFixture(t *testing.T) (fixture, string, string) {
	t.Helper()
	f := newFixture(t)
	write(t, filepath.Join(f.repo, "source.txt"), "second fixture revision\n")
	runGit(t, f.repo, "commit", "-am", "second fixture revision")
	runGit(t, f.repo, "push", "origin", "main")
	primary := filepath.Join(f.root, "shallow-primary")
	runGit(t, f.root, "clone", "--no-local", "--depth", "1", "--branch", "main", f.remote, primary)
	runGit(t, primary, "config", "user.name", "Infinite Disk Test")
	runGit(t, primary, "config", "user.email", "fixtures@example.invalid")
	linked := filepath.Join(f.root, "shallow-linked")
	runGit(t, primary, "worktree", "add", "-b", "fixture-shallow-linked", linked, "main")
	old(t, filepath.Join(primary, "source.txt"))
	old(t, filepath.Join(linked, "source.txt"))
	if runGit(t, linked, "rev-parse", "--is-shallow-repository") != "true" {
		t.Fatal("fixture did not create a shallow linked worktree")
	}
	return f, primary, linked
}

func shallowLinkedPlan(t *testing.T, f fixture, linked string) (Plan, Candidate) {
	t.Helper()
	p, err := f.e.RunReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range p.Candidates {
		if c.Path == linked {
			return p, c
		}
	}
	t.Fatal("linked fixture missing from report")
	return Plan{}, Candidate{}
}

func TestShallowLinkedWorktreeRetainsCommonHistoryAndVerifiesRecovery(t *testing.T) {
	f, primary, linked := shallowLinkedFixture(t)
	write(t, filepath.Join(linked, "committed.txt"), "local unpushed fixture commit\n")
	runGit(t, linked, "add", "committed.txt")
	runGit(t, linked, "commit", "-m", "unpublished fixture work")
	write(t, filepath.Join(linked, "unfinished.txt"), "unfinished fixture work\n")
	old(t, filepath.Join(linked, "committed.txt"))
	old(t, filepath.Join(linked, "unfinished.txt"))
	orphan := orphanBlob(t, primary)
	common := runGit(t, linked, "rev-parse", "--path-format=absolute", "--git-common-dir")
	boundary, err := os.ReadFile(filepath.Join(common, "shallow"))
	if err != nil {
		t.Fatal(err)
	}
	primaryHead := runGit(t, primary, "rev-parse", "HEAD")
	refs := runGit(t, primary, "show-ref")
	p, c := shallowLinkedPlan(t, f, linked)
	eligible(t, c)
	if c.Kind != "worktree" || !slices.ContainsFunc(c.Evidence, func(v Evidence) bool { return v.Check == "shallow_history" && v.Outcome == "retained" }) {
		t.Fatal("missing explicit shared shallow-history evidence", c.Kind)
	}
	if refs != runGit(t, primary, "show-ref") {
		t.Fatal("report wrote Git refs")
	}
	r, err := f.e.Apply(context.Background(), p, c.ID, p.ID, true)
	if err != nil || r.Result != "removed" || r.BundlePath != "" {
		t.Fatal("linked shallow recovery/removal failed", r.Result, err)
	}
	if _, err := os.Lstat(linked); !os.IsNotExist(err) {
		t.Fatal("approved linked worktree still exists", err)
	}
	exists(t, primary)
	gotBoundary, err := os.ReadFile(filepath.Join(common, "shallow"))
	if err != nil || string(gotBoundary) != string(boundary) || runGit(t, primary, "rev-parse", "HEAD") != primaryHead {
		t.Fatal("shared shallow history or primary branch changed", err)
	}
	runGit(t, primary, "cat-file", "-e", orphan)
	for _, file := range []string{"committed.txt", "unfinished.txt"} {
		if runGit(t, f.remote, "show", r.RecoveryCommit+":"+file) == "" {
			t.Fatal("working or unpublished committed source missing from remote recovery")
		}
	}
	if runGit(t, f.remote, "rev-parse", r.RecoveryRef) != r.RecoveryCommit {
		t.Fatal("exact remote recovery verification missing")
	}
}

func TestShallowStandaloneAndActiveLinkedWorktreesRemainBlocked(t *testing.T) {
	f, primary, linked := shallowLinkedFixture(t)
	p, _ := shallowLinkedPlan(t, f, linked)
	for _, c := range p.Candidates {
		if c.Path == primary && (!slices.Contains(c.Blockers, "shallow_repository_requires_manual_preservation") || c.Eligible || CanPreserveSource(c)) {
			t.Fatal("standalone shallow clone became removable", c.Blockers)
		}
	}
	f.e.Activity = func(context.Context) Activity {
		return Activity{Known: true, Paths: []string{filepath.Join(linked, "source.txt")}}
	}
	p, c := shallowLinkedPlan(t, f, linked)
	if c.Eligible || !slices.Contains(c.Blockers, "project_in_use") {
		t.Fatal("active linked worktree promoted", c.Blockers)
	}
	before := runGit(t, f.remote, "show-ref")
	if _, err := f.e.Apply(context.Background(), p, c.ID, p.ID, true); err == nil {
		t.Fatal("active shallow linked worktree removed")
	}
	exists(t, primary)
	exists(t, linked)
	if before != runGit(t, f.remote, "show-ref") {
		t.Fatal("rejected active worktree wrote remote refs")
	}
}

func TestShallowLinkedFailedRecoveryKeepsCheckoutAndCommonDatabase(t *testing.T) {
	f, primary, linked := shallowLinkedFixture(t)
	// A receive hook in the isolated fixture proves rejected recovery is not
	// bypassed. No hooks from a user checkout run in this test.
	write(t, filepath.Join(f.remote, "hooks", "pre-receive"), "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(filepath.Join(f.remote, "hooks", "pre-receive"), 0700); err != nil {
		t.Fatal(err)
	}
	p, c := shallowLinkedPlan(t, f, linked)
	eligible(t, c)
	before := runGit(t, f.remote, "show-ref")
	if _, err := f.e.Apply(context.Background(), p, c.ID, p.ID, true); err == nil {
		t.Fatal("failed remote recovery allowed deletion")
	}
	exists(t, primary)
	exists(t, linked)
	if before != runGit(t, f.remote, "show-ref") {
		t.Fatal("rejected push changed remote refs")
	}
}

func TestShallowLinkedClaudeHandoffKeepsHistoryAndSessionBytes(t *testing.T) {
	f, primary, s := claudeFixture(t)
	common := runGit(t, f.repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	boundary := runGit(t, primary, "rev-parse", "HEAD") + "\n"
	write(t, filepath.Join(common, "shallow"), boundary)
	if runGit(t, f.repo, "rev-parse", "--is-shallow-repository") != "true" {
		t.Fatal("Claude fixture is not shallow")
	}
	original, err := os.ReadFile(s.Store)
	if err != nil {
		t.Fatal(err)
	}
	p := plan(t, f)
	eligible(t, p.Candidates[0])
	r, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true)
	if err != nil || r.Result != "removed" || len(r.SessionHandoffs) != 1 || r.SessionHandoffs[0].Status != "native_verified" {
		t.Fatal("shallow Claude handoff failed", r.Result, err)
	}
	saved, err := os.ReadFile(filepath.Join(r.SessionHandoffs[0].BackupDir, pathID(s.Store)))
	if err != nil || string(saved) != string(original) {
		t.Fatal("session recovery bytes changed", err)
	}
	if err := verifyOriginalPrefix(r.SessionHandoffs[0]); err != nil {
		t.Fatal("native handoff lost original conversation", err)
	}
	got, err := os.ReadFile(filepath.Join(common, "shallow"))
	if err != nil || string(got) != boundary {
		t.Fatal("shared shallow boundary lost", err)
	}
	exists(t, primary)
}
