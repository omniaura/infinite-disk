package disk

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	e                  *Engine
	root, repo, remote string
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	cmd.Env = gitEnvironment()
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}
func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func old(t *testing.T, path string) {
	t.Helper()
	when := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}
func newFixture(t *testing.T) fixture {
	t.Helper()
	return newFixtureFormat(t, "sha1")
}

func newFixtureFormat(t *testing.T, format string) fixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{root: filepath.Join(base, "projects"), repo: filepath.Join(base, "projects", "old-project"), remote: filepath.Join(base, "remote.git")}
	if err := os.MkdirAll(f.repo, 0700); err != nil {
		t.Fatal(err)
	}
	runGit(t, base, "init", "--bare", "--object-format="+format, f.remote)
	runGit(t, f.repo, "init", "--object-format="+format, "-b", "main")
	runGit(t, f.repo, "config", "user.name", "Infinite Disk Test")
	runGit(t, f.repo, "config", "user.email", "fixtures@example.invalid")
	write(t, filepath.Join(f.repo, "source.txt"), "original source\n")
	runGit(t, f.repo, "add", "source.txt")
	runGit(t, f.repo, "commit", "-m", "fixture")
	runGit(t, f.repo, "remote", "add", "origin", f.remote)
	runGit(t, f.repo, "push", "origin", "main")
	old(t, filepath.Join(f.repo, "source.txt"))
	c := DefaultConfig()
	c.Roots = []string{f.root}
	c.StateDir = filepath.Join(base, "state")
	c.MinFreeGiB = 0
	c.MinFreePercent = 0
	f.e = NewEngine(c)
	f.e.Sessions = func(context.Context) SessionInventory { return SessionInventory{Complete: true} }
	f.e.allowLocalRemote = true
	f.e.Activity = func(context.Context) Activity { return Activity{Known: true} }
	return f
}
func plan(t *testing.T, f fixture) Plan {
	t.Helper()
	p, err := f.e.RunReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Candidates) != 1 {
		t.Fatalf("candidates: %+v", p.Candidates)
	}
	return p
}
func eligible(t *testing.T, c Candidate) {
	t.Helper()
	if !c.Eligible {
		t.Fatalf("unexpected blockers: %v", c.Blockers)
	}
}
func exists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected retained %s: %v", path, err)
	}
}

func TestReportNeverMutatesRepository(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.repo, "new.txt"), "unfinished work\n")
	old(t, filepath.Join(f.repo, "new.txt"))
	before := runGit(t, f.repo, "status", "--porcelain")
	head := runGit(t, f.repo, "rev-parse", "HEAD")
	refs := runGit(t, f.repo, "show-ref")
	index, _ := digestFile(filepath.Join(f.repo, ".git", "index"))
	p := plan(t, f)
	eligible(t, p.Candidates[0])
	if p.Mode != "report-only" {
		t.Fatal(p.Mode)
	}
	if before != runGit(t, f.repo, "status", "--porcelain") || head != runGit(t, f.repo, "rev-parse", "HEAD") || refs != runGit(t, f.repo, "show-ref") {
		t.Fatal("scan mutated Git")
	}
	after, _ := digestFile(filepath.Join(f.repo, ".git", "index"))
	if after != index {
		t.Fatal("scan mutated index")
	}
	saved, err := LoadPlan(f.e.Config, "latest")
	if err != nil || saved.ID != p.ID {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(f.e.Config.StateDir, "plans", p.ID+".json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("non-private plan")
	}
}

func TestBlockers(t *testing.T) {
	cases := []struct {
		name, block string
		change      func(*testing.T, fixture)
	}{
		{"recent", "recent_source_activity", func(t *testing.T, f fixture) { write(t, filepath.Join(f.repo, "source.txt"), "new\n") }},
		{"active", "project_in_use", func(t *testing.T, f fixture) {
			f.e.Activity = func(context.Context) Activity {
				return Activity{Known: true, Paths: []string{filepath.Join(f.repo, "source.txt")}}
			}
		}},
		{"unknown activity", "activity_check_unavailable", func(t *testing.T, f fixture) { f.e.Activity = func(context.Context) Activity { return Activity{} } }},
		{"ignored env", "private_or_unknown_ignored_state", func(t *testing.T, f fixture) {
			write(t, filepath.Join(f.repo, ".git", "info", "exclude"), ".env\n")
			write(t, filepath.Join(f.repo, ".env"), "private")
		}},
		{"secret", "source_or_secret_check_requires_review", func(t *testing.T, f fixture) {
			write(t, filepath.Join(f.repo, "new.txt"), "-----BEGIN PRIVATE KEY-----\nfixture only")
			old(t, filepath.Join(f.repo, "new.txt"))
		}},
		{"sensitive name", "source_or_secret_check_requires_review", func(t *testing.T, f fixture) {
			write(t, filepath.Join(f.repo, "auth.json"), "fixture")
			old(t, filepath.Join(f.repo, "auth.json"))
		}},
		{"submodules", "submodules_require_manual_preservation", func(t *testing.T, f fixture) { write(t, filepath.Join(f.repo, ".gitmodules"), "fixture") }},
		{"LFS", "special_or_locked_git_storage_requires_manual_preservation", func(t *testing.T, f fixture) {
			if err := os.Mkdir(filepath.Join(f.repo, ".git", "lfs"), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"execution override", "repository_execution_or_credential_overrides", func(t *testing.T, f fixture) { runGit(t, f.repo, "config", "core.sshCommand", "false") }},
		{"two versions", "staged_and_unstaged_versions_require_manual_preservation", func(t *testing.T, f fixture) {
			write(t, filepath.Join(f.repo, "source.txt"), "staged\n")
			runGit(t, f.repo, "add", "source.txt")
			write(t, filepath.Join(f.repo, "source.txt"), "unstaged\n")
			old(t, filepath.Join(f.repo, "source.txt"))
		}},
		{"nested ignored repo", "nested_repository_or_uninspectable_tree", func(t *testing.T, f fixture) {
			write(t, filepath.Join(f.repo, ".git", "info", "exclude"), "node_modules/\n")
			nested := filepath.Join(f.repo, "node_modules", "work")
			if err := os.MkdirAll(nested, 0700); err != nil {
				t.Fatal(err)
			}
			runGit(t, nested, "init")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(t, f)
			c := f.e.Inspect(context.Background(), f.repo, f.e.Activity(context.Background()))
			if c.Eligible || !slices.Contains(c.Blockers, tc.block) {
				t.Fatalf("want %s, got %+v", tc.block, c.Blockers)
			}
			exists(t, f.repo)
		})
	}
}

func TestApprovalAndRevalidation(t *testing.T) {
	for _, kind := range []string{"missing approval", "tampered plan", "expired", "source changed", "now active", "origin changed"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			p := plan(t, f)
			approved := p.ID
			switch kind {
			case "missing approval":
				approved = ""
			case "tampered plan":
				p.Candidates[0].SizeBytes++
			case "expired":
				f.e.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
			case "source changed":
				write(t, filepath.Join(f.repo, "source.txt"), "changed\n")
				old(t, filepath.Join(f.repo, "source.txt"))
			case "now active":
				f.e.Activity = func(context.Context) Activity { return Activity{Known: true, Paths: []string{f.repo}} }
			case "origin changed":
				runGit(t, f.repo, "remote", "set-url", "origin", "https://example.invalid/other.git")
			}
			if _, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, approved, true); err == nil {
				t.Fatal("unsafe approval succeeded")
			}
			exists(t, f.repo)
		})
	}
}

func TestPreserveRemoveAndIndependentRestore(t *testing.T) {
	f := newFixture(t)
	runGit(t, f.repo, "branch", "unpublished")
	write(t, filepath.Join(f.repo, "source.txt"), "stashed work\n")
	runGit(t, f.repo, "stash", "push", "-m", "fixture stash")
	old(t, filepath.Join(f.repo, "source.txt"))
	stash := runGit(t, f.repo, "rev-parse", "refs/stash")
	runGit(t, f.repo, "notes", "add", "-m", "fixture note")
	notes := runGit(t, f.repo, "rev-parse", "refs/notes/commits")
	write(t, filepath.Join(f.repo, "new.txt"), "unfinished work\n")
	old(t, filepath.Join(f.repo, "new.txt"))
	p := plan(t, f)
	eligible(t, p.Candidates[0])
	r, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Result != "removed" || r.BundleSHA256 == "" || r.RecoveryCommit == "" {
		t.Fatalf("incomplete receipt: %+v", r)
	}
	if _, err := os.Stat(f.repo); !os.IsNotExist(err) {
		t.Fatal("checkout not removed")
	}
	if got := runGit(t, f.remote, "rev-parse", r.RecoveryRef); got != r.RecoveryCommit {
		t.Fatal("remote commit mismatch")
	}
	restore := filepath.Join(f.root, "restored")
	if err := f.e.Restore(context.Background(), p.ID, p.Candidates[0].ID, restore); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Restore(context.Background(), p.ID, p.Candidates[0].ID, restore); err == nil {
		t.Fatal("restore overwrote destination")
	}
	b, err := os.ReadFile(filepath.Join(restore, "new.txt"))
	if err != nil || string(b) != "unfinished work\n" {
		t.Fatal("unfinished work did not restore", err)
	}
	if !strings.Contains(runGit(t, restore, "branch", "-a"), "unpublished") {
		t.Fatal("unpublished ref lost")
	}
	if runGit(t, restore, "rev-parse", "refs/stash") != stash || runGit(t, restore, "rev-parse", "refs/notes/commits") != notes {
		t.Fatal("stash or notes ref not restored")
	}
	for _, path := range []string{r.BundlePath, filepath.Join(f.e.Config.StateDir, "receipts", p.ID+"-"+p.Candidates[0].ID+".json")} {
		s, err := os.Stat(path)
		if err != nil || s.Mode().Perm() != 0600 {
			t.Fatal("recovery is not private", err)
		}
	}
}

func TestPreserveOnlyDoesNotChangeIndexOrSource(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.repo, "source.txt"), "staged work\n")
	runGit(t, f.repo, "add", "source.txt")
	old(t, filepath.Join(f.repo, "source.txt"))
	p := plan(t, f)
	eligible(t, p.Candidates[0])
	before, _ := digestFile(filepath.Join(f.repo, ".git", "index"))
	head := runGit(t, f.repo, "rev-parse", "HEAD")
	r, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := digestFile(filepath.Join(f.repo, ".git", "index"))
	if before != after || head != runGit(t, f.repo, "rev-parse", "HEAD") || r.Result != "preserved" {
		t.Fatal("user state changed")
	}
	exists(t, f.repo)
}

func TestRemoteFailureNeverDeletes(t *testing.T) {
	f := newFixture(t)
	p := plan(t, f)
	if err := os.Rename(f.remote, f.remote+"-offline"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true); err == nil {
		t.Fatal("push should fail")
	}
	exists(t, f.repo)
}

func TestLinkedWorktreeUsesNativeRemoval(t *testing.T) {
	f := newFixture(t)
	wt := filepath.Join(f.root, "linked")
	runGit(t, f.repo, "worktree", "add", "-b", "linked", wt)
	old(t, filepath.Join(wt, "source.txt"))
	c := f.e.Inspect(context.Background(), wt, Activity{Known: true})
	eligible(t, c)
	if c.Kind != "worktree" {
		t.Fatal(c.Kind)
	}
	p := Plan{Version: 1, CreatedAt: time.Now(), Mode: "report-only", ConfigDigest: ConfigDigest(f.e.Config), Candidates: []Candidate{c}}
	p.ID = planID(p)
	r, err := f.e.Apply(context.Background(), p, c.ID, p.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Result != "removed" || r.BundlePath != "" {
		t.Fatal(r)
	}
	exists(t, f.repo)
	if strings.Contains(runGit(t, f.repo, "worktree", "list", "--porcelain"), wt) {
		t.Fatal("worktree registration leaked")
	}
	runGit(t, f.repo, "rev-parse", "refs/heads/linked")
}

func TestPlanTamperingAndBoundedPrivateState(t *testing.T) {
	f := newFixture(t)
	p := plan(t, f)
	p.Candidates[0].Eligible = false
	b, _ := json.Marshal(p)
	write(t, filepath.Join(f.e.Config.StateDir, "plans", p.ID+".json"), string(b))
	if _, err := LoadPlan(f.e.Config, p.ID); err == nil {
		t.Fatal("tampering accepted")
	}
	write(t, filepath.Join(f.e.Config.StateDir, "receipts", "keep.json"), "fixture")
	for i := 0; i < 55; i++ {
		p.CreatedAt = time.Now().Add(time.Duration(i) * time.Second)
		if _, err := SavePlan(f.e.Config, p); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(f.e.Config.StateDir, "plans"))
	if len(entries) > 50 {
		t.Fatalf("unbounded plans %d", len(entries))
	}
	exists(t, filepath.Join(f.e.Config.StateDir, "receipts", "keep.json"))
	release, err := acquireLock(f.e.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := acquireLock(f.e.Config); err == nil {
		t.Fatal("concurrent lock accepted")
	}
}

func TestWatcherNeverAppliesAndThrottles(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	f.e.Now = func() time.Time { return now }
	s, err := f.e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	exists(t, f.repo)
	if s.Mode != "report-only" || s.LastPlanID == "" {
		t.Fatal(s)
	}
	first := s.LastPlanID
	now = now.Add(time.Minute)
	s, err = f.e.Tick(context.Background())
	if err != nil || s.LastPlanID != first {
		t.Fatal("audit not throttled", err)
	}
	now = now.Add(24 * time.Hour)
	s, err = f.e.Tick(context.Background())
	if err != nil || s.LastPlanID == first {
		t.Fatal("periodic audit missing", err)
	}
	exists(t, f.repo)
	if !UnderPressure(DefaultConfig(), Space{FreeBytes: 1, FreePercent: 1}) {
		t.Fatal("pressure missing")
	}
}

func TestPathConfigAndEnvironmentSafety(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{f.root, filepath.Dir(f.root), "/"} {
		if f.e.safeTarget(p) == nil {
			t.Fatal("unsafe target", p)
		}
	}
	link := filepath.Join(f.root, "link")
	if err := os.Symlink(f.repo, link); err != nil {
		t.Fatal(err)
	}
	if f.e.safeTarget(link) == nil {
		t.Fatal("symlink accepted")
	}
	c := f.e.Config
	home, _ := os.UserHomeDir()
	c.StateDir = home
	if c.Validate() == nil {
		t.Fatal("home state accepted")
	}
	config := filepath.Join(filepath.Dir(f.root), "config.json")
	write(t, config, `{"version":1} {}`)
	if _, err := LoadConfig(config); err == nil {
		t.Fatal("trailing config accepted")
	}
	for _, p := range []string{"../escape", "a/../../escape", "/absolute", "a/../b"} {
		if safeRelative(p) {
			t.Fatal("unsafe relative path", p)
		}
	}
	for _, remote := range []string{"https://user:token@github.com/o/r", "ext::echo hacked", "-oCommand=x", "https://github.com/o/r?secret=x"} {
		if _, err := remoteHost(remote); err == nil {
			t.Fatal("unsafe remote", remote)
		}
	}
	t.Setenv("OPENROUTER_API_KEY", "fixture-secret")
	t.Setenv("GIT_DIR", "/bad")
	for _, s := range gitEnvironment() {
		if strings.HasPrefix(s, "OPENROUTER_API_KEY=") || strings.HasPrefix(s, "GIT_DIR=") {
			t.Fatal("ambient override leaked")
		}
	}
}

func TestServiceGeneration(t *testing.T) {
	for _, platform := range []string{"darwin", "linux", "cron"} {
		s, err := ServiceContent(platform, "/opt/a & b/bin", "/opt/a'quoted/config", "/opt/state")
		if err != nil || !strings.Contains(s, "report-only") || strings.Contains(s, " apply") {
			t.Fatal(platform, s, err)
		}
	}
	for _, p := range []string{"relative", "/bad\ncommand"} {
		if _, err := ServiceContent("cron", p, "/cfg", "/state"); err == nil {
			t.Fatal("unsafe service path")
		}
	}
	if _, err := ServiceContent("windows", "/bin", "/cfg", "/state"); err == nil {
		t.Fatal("unsupported service accepted")
	}
}
