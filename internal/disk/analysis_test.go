package disk

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestAnalysisFindsWorkAndIgnoredDataWithoutMutation(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.repo, "source.txt"), "unpublished commit\n")
	runGit(t, f.repo, "add", "source.txt")
	runGit(t, f.repo, "commit", "-m", "local only")
	write(t, filepath.Join(f.repo, "pending.txt"), "unfinished\n")
	old(t, filepath.Join(f.repo, "pending.txt"))
	old(t, filepath.Join(f.repo, "source.txt"))
	write(t, filepath.Join(f.repo, ".git", "info", "exclude"), ".next/\n.env\n")
	write(t, filepath.Join(f.repo, ".next", "cache"), "build\n")
	write(t, filepath.Join(f.repo, ".env"), "PRIVATE FIXTURE VALUE")
	head := runGit(t, f.repo, "rev-parse", "HEAD")
	refs := runGit(t, f.repo, "show-ref")
	status := runGit(t, f.repo, "status", "--porcelain")
	index, _ := digestFile(filepath.Join(f.repo, ".git", "index"))
	p := plan(t, f)
	c := p.Candidates[0]
	a := c.Analysis
	if c.Eligible || a == nil || a.ChangedFiles == nil || *a.ChangedFiles != 1 || a.CommitsNotInOriginTracking == nil || *a.CommitsNotInOriginTracking != 1 || !a.IgnoredComplete || a.IgnoredCount != 2 {
		t.Fatalf("wrong analysis: %+v blockers %v", a, c.Blockers)
	}
	for _, category := range []string{"build_candidate", "private_data"} {
		if !slices.ContainsFunc(a.Ignored, func(entry IgnoredEntry) bool { return entry.Category == category }) {
			t.Fatal("missing ignored category", category)
		}
	}
	for _, action := range a.Actions {
		if action.Kind == "remove_checkout" {
			t.Fatal("blocked project gained removal action")
		}
	}
	if !CanPreserveSource(c) {
		t.Fatal("safe source-only preservation not offered")
	}
	if _, err := f.e.Apply(context.Background(), p, c.ID, p.ID, true); err == nil {
		t.Fatal("analysis bypassed safety")
	}
	if runGit(t, f.repo, "rev-parse", "HEAD") != head || runGit(t, f.repo, "show-ref") != refs || runGit(t, f.repo, "status", "--porcelain") != status {
		t.Fatal("analysis mutated source")
	}
	after, _ := digestFile(filepath.Join(f.repo, ".git", "index"))
	if after != index {
		t.Fatal("analysis changed index")
	}
	if runGit(t, f.remote, "for-each-ref", "--format=%(refname)") != "refs/heads/main" {
		t.Fatal("analysis pushed")
	}
	if _, err := os.Stat(filepath.Join(f.e.Config.StateDir, "receipts")); !os.IsNotExist(err) {
		t.Fatal("analysis created approval receipt")
	}
}

func TestSourceOnlyPreservationNeverUploadsIgnoredDataOrRemovesSessions(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.repo, ".git", "info", "exclude"), "private/\n")
	write(t, filepath.Join(f.repo, "private", "data"), "private ignored fixture")
	write(t, filepath.Join(f.repo, "pending.txt"), "unfinished source\n")
	old(t, filepath.Join(f.repo, "pending.txt"))
	p := plan(t, f)
	c := p.Candidates[0]
	if c.Eligible || !CanPreserveSource(c) {
		t.Fatal(c.Blockers)
	}
	head := runGit(t, f.repo, "rev-parse", "HEAD")
	index, _ := digestFile(filepath.Join(f.repo, ".git", "index"))
	if _, err := f.e.Apply(context.Background(), p, c.ID, "", false); err == nil {
		t.Fatal("unapproved push permitted")
	}
	r, err := f.e.Apply(context.Background(), p, c.ID, p.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Result != "preserved" || r.RecoveryCommit == "" || len(r.SessionHandoffs) != 0 {
		t.Fatal(r)
	}
	exists(t, f.repo)
	exists(t, filepath.Join(f.repo, "private", "data"))
	exists(t, filepath.Join(f.repo, "pending.txt"))
	if runGit(t, f.remote, "ls-tree", "-r", "--name-only", r.RecoveryRef) != "pending.txt\nsource.txt" {
		t.Fatal("wrong snapshot or private ignored data uploaded")
	}
	if head != runGit(t, f.repo, "rev-parse", "HEAD") {
		t.Fatal("user branch moved")
	}
	after, _ := digestFile(filepath.Join(f.repo, ".git", "index"))
	if index != after {
		t.Fatal("user index changed")
	}
}

func TestAnalysisUnknownDoesNotMeanCleanAndSelectedRefreshIsBounded(t *testing.T) {
	f := newFixture(t)
	p := plan(t, f)
	id := p.Candidates[0].ID
	f.e.Activity = func(context.Context) Activity { return Activity{Known: true, Paths: []string{f.repo}} }
	updated, err := f.e.AnalyzeSelected(context.Background(), p, map[string]bool{id: true})
	if err != nil {
		t.Fatal(err)
	}
	c := updated.Candidates[0]
	if updated.ID == p.ID || c.Eligible || c.Analysis.SourceChecked || c.Analysis.ChangedFiles != nil || c.Analysis.CommitsNotInOriginTracking != nil {
		t.Fatal("active candidate treated as clean", c.Analysis)
	}
	if _, err = f.e.AnalyzeSelected(context.Background(), p, map[string]bool{"invented": true}); err == nil {
		t.Fatal("unknown selection accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = f.e.AnalyzeSelected(ctx, p, map[string]bool{id: true}); err == nil {
		t.Fatal("cancelled analysis succeeded")
	}
	exists(t, f.repo)
}

func TestAnalysisDoesNotExecuteConfiguredFilters(t *testing.T) {
	f := newFixture(t)
	marker := filepath.Join(filepath.Dir(f.root), "filter-executed")
	write(t, filepath.Join(f.repo, ".gitattributes"), "*.txt filter=evil\n")
	runGit(t, f.repo, "config", "filter.evil.clean", "touch "+marker)
	p := plan(t, f)
	if p.Candidates[0].Eligible || p.Candidates[0].Analysis.SourceChecked {
		t.Fatal("filter project promoted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("analysis executed filter")
	}
}

func TestSourcePreservationAllowlistDoesNotAcceptUnsafeBlockers(t *testing.T) {
	base := Candidate{Fingerprint: "checked", Head: "head", SessionsChecked: true}
	for _, reason := range []string{"project_in_use", "activity_check_unavailable", "source_or_secret_check_requires_review", "special_or_locked_git_storage_requires_manual_preservation", "repository_execution_or_credential_overrides", "coding_session_in_use", "unknown_blocker"} {
		c := base
		c.Blockers = []string{reason}
		if CanPreserveSource(c) {
			t.Fatal("unsafe source-only preservation", reason)
		}
	}
	for _, reason := range []string{"private_or_unknown_ignored_state", "recent_source_activity", "coding_sessions_require_native_handoff", "coding_session_coverage_incomplete"} {
		c := base
		c.Blockers = []string{reason}
		if !CanPreserveSource(c) {
			t.Fatal("removal-only prerequisite blocked source preservation", reason)
		}
	}
	base.SessionsChecked = false
	if CanPreserveSource(base) {
		t.Fatal("legacy report accepted")
	}
}
