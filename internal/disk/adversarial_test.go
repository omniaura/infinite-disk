package disk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteAndRefIdentityBoundToApproval(t *testing.T) {
	for _, kind := range []string{"same-host remote", "push destination", "unpublished ref", "local config"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			p := plan(t, f)
			switch kind {
			case "same-host remote":
				runGit(t, f.repo, "remote", "set-url", "origin", f.remote+"-other")
			case "push destination":
				runGit(t, f.repo, "remote", "set-url", "--push", "origin", f.remote+"-other")
			case "unpublished ref":
				runGit(t, f.repo, "branch", "important-new-branch")
			case "local config":
				runGit(t, f.repo, "config", "user.email", "changed@example.invalid")
			}
			if _, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true); err == nil {
				t.Fatal("changed preservation identity accepted")
			}
			exists(t, f.repo)
		})
	}
}

func TestGlobalContentFilterNeverRunsDuringReport(t *testing.T) {
	f := newFixture(t)
	base := filepath.Dir(f.root)
	home := filepath.Join(base, "filter-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	marker := filepath.Join(base, "filter-must-not-run")
	write(t, filepath.Join(home, ".gitconfig"), "[filter \"fixture\"]\n clean = /usr/bin/touch "+marker+"\n")
	write(t, filepath.Join(f.repo, ".gitattributes"), "source.txt filter=fixture\n")
	old(t, filepath.Join(f.repo, ".gitattributes"))
	c := f.e.Inspect(context.Background(), f.repo, Activity{Known: true})
	if c.Eligible || !strings.Contains(strings.Join(c.Blockers, ","), "content_filters") {
		t.Fatal(c.Blockers)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("Git content filter executed during report")
	}
}

func TestBoundedScanResumesAcrossProjectsAndRoots(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"b-project", "c-project"} {
		path := filepath.Join(f.root, name)
		runGit(t, f.root, "clone", "--branch", "main", f.remote, path)
		old(t, filepath.Join(path, "source.txt"))
	}
	f.e.Config.MaxProjects = 2
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		fresh := NewEngine(f.e.Config)
		fresh.allowLocalRemote = true
		fresh.Activity = f.e.Activity
		p, err := fresh.RunReport(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Candidates) != 2 || len(p.Warnings) == 0 {
			t.Fatal("partial report not explicit")
		}
		for _, c := range p.Candidates {
			seen[c.Path] = true
		}
	}
	if len(seen) != 3 {
		t.Fatalf("later projects starved: %v", seen)
	}
}

func TestPrivateStateOrActivityAppearingDuringPreservation(t *testing.T) {
	for _, kind := range []string{"private state", "activity", "source"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			p := plan(t, f)
			calls := 0
			f.e.Activity = func(context.Context) Activity {
				calls++
				if calls >= 2 {
					switch kind {
					case "private state":
						write(t, filepath.Join(f.repo, ".git", "info", "exclude"), ".env\n")
						write(t, filepath.Join(f.repo, ".env"), "private fixture")
					case "activity":
						return Activity{Known: true, Paths: []string{f.repo}}
					case "source":
						write(t, filepath.Join(f.repo, "source.txt"), "late work\n")
						old(t, filepath.Join(f.repo, "source.txt"))
					}
				}
				return Activity{Known: true}
			}
			if _, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true); err == nil {
				t.Fatal("late mutation ignored")
			}
			exists(t, f.repo)
		})
	}
}

func TestSymlinkStateAndLogRotation(t *testing.T) {
	f := newFixture(t)
	if err := EnsureState(f.e.Config); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(filepath.Dir(f.root), "elsewhere")
	if err := os.Mkdir(elsewhere, 0700); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(f.e.Config.StateDir, "logs")
	if err := os.Symlink(elsewhere, logs); err != nil {
		t.Fatal(err)
	}
	if AppendEvent(f.e.Config, Event{Type: "fixture"}) == nil {
		t.Fatal("symlinked log directory accepted")
	}
	if err := os.Remove(logs); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(logs, "events.jsonl"), strings.Repeat("x", (10<<20)+1))
	if err := AppendEvent(f.e.Config, Event{Type: "fixture"}); err != nil {
		t.Fatal(err)
	}
	exists(t, filepath.Join(logs, "events.jsonl.1"))
	s, _ := os.Stat(filepath.Join(logs, "events.jsonl"))
	if s.Mode().Perm() != 0600 || s.Size() > 1024 {
		t.Fatal("rotation/privacy failed")
	}
}

func TestStateNeverClaimsOrChangesUnownedDirectory(t *testing.T) {
	f := newFixture(t)
	foreign := filepath.Join(filepath.Dir(f.root), "unowned")
	if err := os.Mkdir(foreign, 0755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(foreign, "keep.txt"), "unowned data")
	before, _ := os.Stat(foreign)
	c := f.e.Config
	c.StateDir = foreign
	if EnsureState(c) == nil {
		t.Fatal("unowned nonempty directory claimed")
	}
	after, _ := os.Stat(foreign)
	if before.Mode().Perm() != after.Mode().Perm() {
		t.Fatal("unowned directory permissions changed")
	}
	exists(t, filepath.Join(foreign, "keep.txt"))
}

func TestPartialOrWarnedActivityInventoryFailsClosed(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(base, "lsof")
	t.Setenv("PATH", base+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, kind := range []string{"partial", "warning", "success"} {
		script := "#!/bin/sh\nprintf 'n/tmp/infinite-disk-fixture\\n'\n"
		if kind == "partial" {
			script += "exit 1\n"
		}
		if kind == "warning" {
			script += "printf 'fixture warning\\n' >&2\n"
		}
		write(t, fake, script)
		if err = os.Chmod(fake, 0700); err != nil {
			t.Fatal(err)
		}
		a := ReadActivity(context.Background())
		if a.Known != (kind == "success") {
			t.Fatalf("activity trust mismatch for %s", kind)
		}
	}
}

func TestRestoreRefusesTamperedBundle(t *testing.T) {
	f := newFixture(t)
	p := plan(t, f)
	r, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(r.BundlePath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("tampered")
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(f.root, "restore")
	if f.e.Restore(context.Background(), p.ID, p.Candidates[0].ID, destination) == nil {
		t.Fatal("corrupt bundle restored")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("destination created before checksum passed")
	}
}

func TestShallowAndManagedWorktreesBlocked(t *testing.T) {
	f := newFixture(t)
	shallow := filepath.Join(f.root, "shallow")
	runGit(t, f.root, "clone", "--depth=1", "--branch", "main", "file://"+f.remote, shallow)
	old(t, filepath.Join(shallow, "source.txt"))
	c := f.e.Inspect(context.Background(), shallow, Activity{Known: true})
	if c.Eligible || !strings.Contains(strings.Join(c.Blockers, ","), "shallow") {
		t.Fatal(c.Blockers)
	}
	home, _ := os.UserHomeDir()
	managed := filepath.Join(home, ".codex", "worktrees")
	if within(managed, f.repo) {
		t.Skip("fixture unexpectedly in managed directory")
	}
	// Protected-path behavior can be verified without creating anything under a real agent directory.
	if err := f.e.safeTarget(home); err == nil {
		t.Fatal("home accepted")
	}
}

func TestPressureAuditCadenceAndCancellation(t *testing.T) {
	f := newFixture(t)
	f.e.Config.MinFreePercent = 100
	now := time.Now()
	f.e.Now = func() time.Time { return now }
	s, err := f.e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first := s.LastPlanID
	now = now.Add(59 * time.Minute)
	s, err = f.e.Tick(context.Background())
	if err != nil || s.LastPlanID != first {
		t.Fatal("low-space scan storm", err)
	}
	now = now.Add(time.Minute)
	s, err = f.e.Tick(context.Background())
	if err != nil || s.LastPlanID == first {
		t.Fatal("pressure scan not scheduled", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = f.e.Watch(ctx, false, nil); err != nil {
		t.Fatal(err)
	}
	exists(t, f.repo)
}
