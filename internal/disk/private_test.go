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

func privateFixture(t *testing.T) (fixture, Plan, PrivatePlan) {
	t.Helper()
	f := newFixture(t)
	write(t, filepath.Join(f.repo, ".git/info/exclude"), ".env\nlocal-data/\nnode_modules/\n")
	write(t, filepath.Join(f.repo, ".env"), "fixture-private-value\n")
	write(t, filepath.Join(f.repo, "local-data/state.txt"), "fixture-local-work\n")
	write(t, filepath.Join(f.repo, "node_modules/pkg/credentials.js"), "fixture public dependency\n")
	write(t, filepath.Join(f.repo, "node_modules/pkg/rebuildable.js"), "regenerable\n")
	p := plan(t, f)
	if !CanArchivePrivate(p.Candidates[0]) {
		t.Fatal(p.Candidates[0].Blockers)
	}
	a, err := f.e.PlanPrivate(context.Background(), p, p.Candidates[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, p, a
}
func TestPrivateArchivePlanApplyRestoreAndSourceRecovery(t *testing.T) {
	f, p, a := privateFixture(t)
	ctx := context.Background()
	before := runGit(t, f.repo, "status", "--porcelain")
	refs := runGit(t, f.repo, "show-ref")
	index, _ := digestFile(filepath.Join(f.repo, ".git/index"))
	if len(a.Entries) != 3 {
		t.Fatalf("unexpected roots: %+v", a.Entries)
	}
	exists(t, filepath.Join(f.repo, ".env"))
	if refs != runGit(t, f.repo, "show-ref") {
		t.Fatal("planning wrote refs")
	}
	r, err := f.e.ArchivePrivate(ctx, a.ID, a.ID)
	if err != nil || r.Status != "verified_local_archive" {
		t.Fatal(r, err)
	}
	if before != runGit(t, f.repo, "status", "--porcelain") || refs != runGit(t, f.repo, "show-ref") {
		t.Fatal("private archive changed source or refs")
	}
	after, _ := digestFile(filepath.Join(f.repo, ".git/index"))
	if after != index {
		t.Fatal("index changed")
	}
	exists(t, filepath.Join(f.repo, "node_modules/pkg/rebuildable.js"))
	if _, err = os.Stat(filepath.Join(f.repo, ".env")); !os.IsNotExist(err) {
		t.Fatal("private file was not relocated")
	}
	fresh := plan(t, f)
	eligible(t, fresh.Candidates[0])
	if !slices.Equal(fresh.Candidates[0].PrivateArchives, []string{a.ID}) {
		t.Fatal("archive not bound to approval")
	}
	removed, err := f.e.Apply(ctx, fresh, fresh.Candidates[0].ID, fresh.ID, true)
	if err != nil || removed.Result != "removed" {
		t.Fatal(removed, err)
	}
	if err = f.e.Restore(ctx, fresh.ID, fresh.Candidates[0].ID, f.repo); err != nil {
		t.Fatal(err)
	}
	if err = f.e.RestorePrivate(ctx, a.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	for _, entry := range a.Entries {
		original, err := privateTree(ctx, filepath.Join(f.repo, entry.Path))
		if err != nil || original.SHA256 != entry.SHA256 {
			t.Fatal("restore differs", entry.Path, err)
		}
		exists(t, filepath.Join(f.e.privateDir(a.ID), "payload", entry.Path))
	}
	if err = f.e.RestorePrivate(ctx, a.ID, a.ID); err != nil {
		t.Fatal("identical copy restore should be idempotent", err)
	}
	_ = p
}
func TestPrivateArchiveRevalidation(t *testing.T) {
	for _, kind := range []string{"no approval", "expired", "future", "source changed", "private changed", "new private root", "active", "unknown activity", "source symlink"} {
		t.Run(kind, func(t *testing.T) {
			f, _, a := privateFixture(t)
			approve := a.ID
			switch kind {
			case "no approval":
				approve = ""
			case "expired":
				f.e.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
			case "future":
				f.e.Now = func() time.Time { return time.Now().Add(-time.Hour) }
			case "source changed":
				write(t, filepath.Join(f.repo, "source.txt"), "new work")
			case "private changed":
				write(t, filepath.Join(f.repo, ".env"), "new private work")
			case "new private root":
				write(t, filepath.Join(f.repo, ".git/info/exclude"), ".env\nlocal-data/\nnode_modules/\nnew-private/\n")
				write(t, filepath.Join(f.repo, "new-private/keep"), "new")
			case "active":
				f.e.Activity = func(context.Context) Activity { return Activity{Known: true, Paths: []string{f.repo}} }
			case "unknown activity":
				f.e.Activity = func(context.Context) Activity { return Activity{} }
			case "source symlink":
				if err := os.Remove(filepath.Join(f.repo, ".env")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(f.repo, "source.txt"), filepath.Join(f.repo, ".env")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.e.ArchivePrivate(context.Background(), a.ID, approve); err == nil {
				t.Fatal("unsafe private archive accepted")
			}
			exists(t, f.repo)
			if kind != "source symlink" {
				exists(t, filepath.Join(f.repo, ".env"))
			}
		})
	}
}
func TestPrivateArchiveRejectsUnsupportedEntriesAndNativeState(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "native agent", "nested Git"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			write(t, filepath.Join(f.repo, ".git/info/exclude"), "private/\n.claude/\n")
			switch kind {
			case "symlink":
				if err := os.Mkdir(filepath.Join(f.repo, "private"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.root, filepath.Join(f.repo, "private/escape")); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Mkdir(filepath.Join(f.repo, "private"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(filepath.Join(f.repo, "source.txt"), filepath.Join(f.repo, "private/shared")); err != nil {
					t.Fatal(err)
				}
			case "native agent":
				write(t, filepath.Join(f.repo, ".claude/settings.local.json"), "{}")
			case "nested Git":
				runGit(t, f.repo, "init", filepath.Join(f.repo, "private/repo"))
			}
			p, scanErr := f.e.RunReport(context.Background())
			if scanErr != nil {
				t.Fatal(scanErr)
			}
			if _, err := f.e.PlanPrivate(context.Background(), p, pathID(f.repo)); err == nil {
				t.Fatal("unsupported data archived")
			}
			exists(t, f.repo)
		})
	}
}
func TestPrivateArchiveDamagedBackupBlocksRemoval(t *testing.T) {
	for _, kind := range []string{"payload changed", "receipt missing", "receipt incomplete", "index malformed", "backup active", "archive symlink"} {
		t.Run(kind, func(t *testing.T) {
			f, _, a := privateFixture(t)
			ctx := context.Background()
			if _, err := f.e.ArchivePrivate(ctx, a.ID, a.ID); err != nil {
				t.Fatal(err)
			}
			p := plan(t, f)
			eligible(t, p.Candidates[0])
			dir := f.e.privateDir(a.ID)
			switch kind {
			case "payload changed":
				write(t, filepath.Join(dir, "payload/.env"), "changed")
			case "receipt missing":
				if err := os.Remove(filepath.Join(dir, "receipt.json")); err != nil {
					t.Fatal(err)
				}
			case "receipt incomplete":
				if err := f.e.saveArchive(PrivateArchive{Version: 1, PlanID: a.ID, Status: "in_progress"}); err != nil {
					t.Fatal(err)
				}
			case "index malformed":
				write(t, filepath.Join(f.e.Config.StateDir, "private-archives/projects", a.Candidate.ID+".json"), "[\"../bad\"]")
			case "backup active":
				f.e.Activity = func(context.Context) Activity {
					return Activity{Known: true, Paths: []string{filepath.Join(dir, "payload/.env")}}
				}
			case "archive symlink":
				file := filepath.Join(dir, "payload/.env")
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(f.repo, "source.txt"), file); err != nil {
					t.Fatal(err)
				}
			}
			c := f.e.Inspect(ctx, f.repo, f.e.Activity(ctx))
			if c.Eligible || !slices.Contains(c.Blockers, "private_archive_requires_recovery") {
				t.Fatal(c.Blockers)
			}
			if _, err := f.e.Apply(ctx, p, c.ID, p.ID, true); err == nil {
				t.Fatal("removed checkout with invalid private archive")
			}
			exists(t, f.repo)
		})
	}
}
func TestPrivateArchivePartialRecoveryNoOverwrite(t *testing.T) {
	f, _, a := privateFixture(t)
	ctx := context.Background()
	calls := 0
	f.e.Activity = func(context.Context) Activity {
		calls++
		if calls >= 3 {
			return Activity{Known: true, Paths: []string{f.repo}}
		}
		return Activity{Known: true}
	}
	r, err := f.e.ArchivePrivate(ctx, a.ID, a.ID)
	if err == nil || r.Status != "needs_recovery" {
		t.Fatal(r, err)
	}
	f.e.Activity = func(context.Context) Activity { return Activity{Known: true} }
	if err = f.e.RestorePrivate(ctx, a.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	for _, entry := range a.Entries {
		exists(t, filepath.Join(f.repo, entry.Path))
	}
	// A different file must not be overwritten even if the backup is sound.
	f2, _, a2 := privateFixture(t)
	if _, err = f2.e.ArchivePrivate(ctx, a2.ID, a2.ID); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f2.repo, ".env"), "brand new local work")
	if err = f2.e.RestorePrivate(ctx, a2.ID, a2.ID); err == nil {
		t.Fatal("restore overwrote existing work")
	}
	data, _ := os.ReadFile(filepath.Join(f2.repo, ".env"))
	if string(data) != "brand new local work" {
		t.Fatal("original work overwritten")
	}
}
func TestPrivatePlanTamperingAndExclusiveRename(t *testing.T) {
	f, _, a := privateFixture(t)
	file := filepath.Join(f.e.privateDir(a.ID), "plan.json")
	b, _ := os.ReadFile(file)
	var p PrivatePlan
	_ = json.Unmarshal(b, &p)
	p.Entries[0].Path = "../escape"
	tampered, _ := json.Marshal(p)
	write(t, file, string(tampered))
	if _, err := f.e.LoadPrivate(a.ID); err == nil {
		t.Fatal("tampered plan accepted")
	}
	src, dst := filepath.Join(f.root, "move-src"), filepath.Join(f.root, "move-dst")
	write(t, src, "source")
	write(t, dst, "existing")
	if renameExclusive(src, dst) == nil {
		t.Fatal("exclusive rename overwrote destination")
	}
	data, _ := os.ReadFile(dst)
	if !strings.Contains(string(data), "existing") {
		t.Fatal("destination overwritten")
	}
	exists(t, src)
}

func TestPrivateArchiveWithClaudeHandoffAndUnfinishedSource(t *testing.T) {
	f, _, s := claudeFixture(t)
	ctx := context.Background()
	write(t, filepath.Join(f.repo, ".gitignore"), ".env\n")
	old(t, filepath.Join(f.repo, ".gitignore"))
	write(t, filepath.Join(f.repo, ".env"), "private fixture value")
	p := plan(t, f)
	if p.Candidates[0].Eligible {
		t.Fatal("private data must block cleanup")
	}
	archive, err := f.e.PlanPrivate(ctx, p, p.Candidates[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.ArchivePrivate(ctx, archive.ID, archive.ID); err != nil {
		t.Fatal(err)
	}
	fresh := plan(t, f)
	eligible(t, fresh.Candidates[0])
	if len(fresh.Candidates[0].SessionHandoffs) != 1 {
		t.Fatal("handoff not prepared after private archive")
	}
	r, err := f.e.Apply(ctx, fresh, fresh.Candidates[0].ID, fresh.ID, true)
	if err != nil || r.Result != "removed" {
		t.Fatal(r, err)
	}
	if runGit(t, f.remote, "show", r.RecoveryCommit+":unfinished.txt") != "unfinished fixture work" {
		t.Fatal("unfinished work missing")
	}
	exists(t, s.Store)
	exists(t, filepath.Join(f.e.privateDir(archive.ID), "payload/.env"))
}
