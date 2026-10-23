package disk

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func legacyRetentionFixture(t *testing.T, compiled ...bool) (fixture, PrivatePlan) {
	t.Helper()
	f := newFixture(t)
	write(t, filepath.Join(f.repo, ".git/info/exclude"), ".env\nlocal-data/\ndist/\n")
	write(t, filepath.Join(f.repo, ".env"), "fixture-private-value\n")
	write(t, filepath.Join(f.repo, "local-data/work.txt"), "fixture unique work\n")
	write(t, filepath.Join(f.repo, "dist/index.html"), "fixture generated html\n")
	p := plan(t, f)
	a := PrivatePlan{Version: 1, CreatedAt: time.Now().UTC(), ConfigDigest: ConfigDigest(f.e.Config), SourcePlan: p.ID, Candidate: p.Candidates[0]}
	namesToArchive := []string{".env", "dist", "local-data"}
	if len(compiled) > 0 && compiled[0] {
		b := make([]byte, 256)
		binary.LittleEndian.PutUint32(b[:4], 0xfeedfacf)
		binary.LittleEndian.PutUint32(b[12:16], 2)
		if err := os.MkdirAll(filepath.Join(f.repo, "tmp"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.repo, "tmp/main"), b, 0700); err != nil {
			t.Fatal(err)
		}
		namesToArchive = append(namesToArchive, "tmp")
	}
	for _, name := range namesToArchive {
		e, err := privateTree(context.Background(), filepath.Join(f.repo, name))
		if err != nil {
			t.Fatal(err)
		}
		e.Path = name
		a.Entries = append(a.Entries, e)
		a.Bytes += e.Bytes
	}
	a.ID = privateID(a)
	if err := os.MkdirAll(filepath.Join(f.e.privateDir(a.ID), "payload"), 0700); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range a.Entries {
		names = append(names, entry.Path)
		if err := os.Rename(filepath.Join(f.repo, entry.Path), filepath.Join(f.e.privateDir(a.ID), "payload", entry.Path)); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.MarshalIndent(a, "", "  ")
	if err := atomicPrivate(filepath.Join(f.e.privateDir(a.ID), "plan.json"), b); err != nil {
		t.Fatal(err)
	}
	if err := f.e.saveArchive(PrivateArchive{Version: 1, PlanID: a.ID, Status: "verified_local_archive", Moved: names, Verified: names}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.linkPrivate(a); err != nil {
		t.Fatal(err)
	}
	return f, a
}

func TestPrivatePruneCompiledOnlyRequiresWholeEntryAndExactApproval(t *testing.T) {
	f, a := legacyRetentionFixture(t, true)
	ctx := context.Background()
	p, err := f.e.PlanPrivatePrune(ctx, a.ID, []string{"tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Entries[0].Policy != "reviewed_compiled_only" {
		t.Fatal("compiled recognition not explicit")
	}
	if _, err = f.e.PrunePrivate(ctx, a.ID, []string{"tmp"}, p.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.e.RestorePrivate(ctx, a.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	exists(t, filepath.Join(f.repo, "local-data/work.txt"))
	f, a = legacyRetentionFixture(t, true)
	write(t, filepath.Join(f.e.privateDir(a.ID), "payload/tmp/unfinished.txt"), "fixture unique work")
	if _, err = f.e.PlanPrivatePrune(ctx, a.ID, []string{"tmp"}); err == nil {
		t.Fatal("mixed source and executable entry pruned")
	}
}

func TestPrivateCreationNeverArchivesRegenerableOutput(t *testing.T) {
	for _, name := range []string{"dist/index.html", "apps/site/dist/index.js", ".mypy_cache/3.11/cache.db", ".pytest_cache/v/cache/nodeids", ".ruff_cache/cache", ".xgo/gen/overlay/source.go", "mixed/dist/index.html"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			root := strings.Split(name, "/")[0]
			write(t, filepath.Join(f.repo, ".git/info/exclude"), root+"/\n")
			write(t, filepath.Join(f.repo, name), "fixture generated output\n")
			p := plan(t, f)
			if _, err := f.e.PlanPrivate(context.Background(), p, p.Candidates[0].ID); err == nil {
				t.Fatal("generated output entered private preservation")
			}
			exists(t, filepath.Join(f.repo, name))
		})
	}
}

func TestPrivateCreationRequiresSeparateCompiledArtifactDecision(t *testing.T) {
	f, file := binaryFixture(t)
	p := plan(t, f)
	if _, err := f.e.PlanPrivate(context.Background(), p, p.Candidates[0].ID); err == nil {
		t.Fatal("compiled executable silently entered private recovery")
	}
	exists(t, file)
}

func TestPrivatePrunePreservesOriginalPlanAndRestoresUniqueData(t *testing.T) {
	f, a := legacyRetentionFixture(t)
	ctx := context.Background()
	before, err := os.ReadFile(filepath.Join(f.e.privateDir(a.ID), "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.e.PlanPrivatePrune(ctx, a.ID, []string{"dist"})
	if err != nil {
		t.Fatal(err)
	}
	exists(t, filepath.Join(f.e.privateDir(a.ID), "payload/dist/index.html"))
	if _, err = f.e.PrunePrivate(ctx, a.ID, []string{"dist"}, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong approval accepted")
	}
	if _, err = f.e.PrunePrivate(ctx, a.ID, []string{"dist"}, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(f.e.privateDir(a.ID), "payload/dist")); !os.IsNotExist(err) {
		t.Fatal("generated output retained", err)
	}
	after, _ := os.ReadFile(filepath.Join(f.e.privateDir(a.ID), "plan.json"))
	if string(before) != string(after) {
		t.Fatal("immutable plan rewritten")
	}
	c := a.Candidate
	if err = f.e.verifyPrivateArchives(ctx, &c); err != nil {
		t.Fatal("remaining archive no longer verifies", err)
	}
	if err = f.e.RestorePrivate(ctx, a.ID, a.ID); err != nil {
		t.Fatal("unique private work cannot restore", err)
	}
	for _, name := range []string{".env", "local-data/work.txt"} {
		exists(t, filepath.Join(f.repo, name))
	}
	if _, err = os.Stat(filepath.Join(f.repo, "dist")); !os.IsNotExist(err) {
		t.Fatal("discarded build output unexpectedly restored")
	}
	if err = f.e.RestorePrivate(ctx, a.ID, a.ID); err != nil {
		t.Fatal("idempotent restore failed", err)
	}
}

func TestPrivatePruneFailsClosed(t *testing.T) {
	for _, kind := range []string{"private work", "duplicate entry", "changed bytes", "unknown activity", "active archive", "symlink", "session", "secret", "tampered plan", "forged discard"} {
		t.Run(kind, func(t *testing.T) {
			f, a := legacyRetentionFixture(t)
			ctx := context.Background()
			names := []string{"dist"}
			root := filepath.Join(f.e.privateDir(a.ID), "payload/dist")
			switch kind {
			case "private work":
				names = []string{".env"}
			case "duplicate entry":
				names = []string{"dist", "dist"}
			case "changed bytes":
				write(t, filepath.Join(root, "index.html"), "changed")
			case "unknown activity":
				f.e.Activity = func(context.Context) Activity { return Activity{} }
			case "active archive":
				f.e.Activity = func(context.Context) Activity { return Activity{Known: true, Paths: []string{root}} }
			case "symlink":
				if err := os.Symlink(f.repo, filepath.Join(root, "outside")); err != nil {
					t.Fatal(err)
				}
			case "session":
				write(t, filepath.Join(root, ".claude/projects/transcript.jsonl"), "fixture session")
			case "secret":
				write(t, filepath.Join(root, ".env"), "fixture private data")
			case "tampered plan":
				write(t, filepath.Join(f.e.privateDir(a.ID), "plan.json"), "{}")
			case "forged discard":
				r, err := f.e.PrivateReceipt(a.ID)
				if err != nil {
					t.Fatal(err)
				}
				r.Discarded = []PrivateDiscard{{Entry: a.Entries[0], Policy: "reviewed_regenerable"}}
				if err = f.e.saveArchive(r); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.e.PlanPrivatePrune(ctx, a.ID, names); err == nil {
				t.Fatal("unsafe retention plan accepted")
			}
			exists(t, filepath.Join(root, "index.html"))
			exists(t, filepath.Join(f.e.privateDir(a.ID), "payload/.env"))
		})
	}
}

func TestPrivatePruneRevalidatesAfterReview(t *testing.T) {
	f, a := legacyRetentionFixture(t)
	ctx := context.Background()
	p, err := f.e.PlanPrivatePrune(ctx, a.ID, []string{"dist"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.e.privateDir(a.ID), "payload/dist/new-work.txt"), "new work")
	if _, err = f.e.PrunePrivate(ctx, a.ID, []string{"dist"}, p.ID); err == nil {
		t.Fatal("changed archive removed")
	}
	exists(t, filepath.Join(f.e.privateDir(a.ID), "payload/dist/new-work.txt"))
}

func TestPrivatePruneApprovalIsBoundToStateDirectory(t *testing.T) {
	f, a := legacyRetentionFixture(t)
	ctx := context.Background()
	p, err := f.e.PlanPrivatePrune(ctx, a.ID, []string{"dist"})
	if err != nil {
		t.Fatal(err)
	}
	c := f.e.Config
	c.StateDir = filepath.Join(f.root, "other-state")
	if err = EnsureState(c); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(c)
	e.Activity = func(context.Context) Activity { return Activity{Known: true} }
	if err = os.MkdirAll(filepath.Join(c.StateDir, "private-archives"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = copyPrivateTree(ctx, f.e.privateDir(a.ID), e.privateDir(a.ID)); err != nil {
		t.Fatal(err)
	}
	other, err := e.PlanPrivatePrune(ctx, a.ID, []string{"dist"})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == other.ID {
		t.Fatal("approval applies to a different recovery root")
	}
	if _, err = e.PrunePrivate(ctx, a.ID, []string{"dist"}, p.ID); err == nil {
		t.Fatal("cross-root approval accepted")
	}
	exists(t, filepath.Join(e.privateDir(a.ID), "payload/dist/index.html"))
}
