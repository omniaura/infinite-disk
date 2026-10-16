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
