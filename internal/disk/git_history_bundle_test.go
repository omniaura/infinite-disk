package disk

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Git bundles retain blob/tree refs too, without introducing tag objects or
// mutating any user branch. Use only temporary fixture repositories here.
func TestBundleRetainsPrivateNonCommitRefs(t *testing.T) {
	f := newFixture(t)
	oid := orphanBlob(t, f.repo)
	ref := "refs/infinite-disk/recovery/objects/" + oid
	runGit(t, f.repo, "update-ref", ref, oid)
	bundle := filepath.Join(filepath.Dir(f.root), "objects.bundle")
	runGit(t, f.repo, "bundle", "create", bundle, "--all", "HEAD")
	restored := filepath.Join(f.root, "restored")
	runGit(t, f.root, "init", "--bare", restored)
	if _, err := gitInput(context.Background(), restored, nil, nil, "fetch", "--no-tags", "--", bundle, "refs/*:refs/*"); err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, restored, "cat-file", "-t", oid); got != "blob" {
		t.Fatal("private object not restored", got)
	}
	if got := runGit(t, restored, "rev-parse", ref); got != oid {
		t.Fatal("private recovery ref not restored")
	}
}

func TestCompleteHistoryPackedObjectsReflogAndPrivateRestore(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	input := func(data, command string, args ...string) string {
		t.Helper()
		oid, err := gitInput(ctx, f.repo, nil, []byte(data), append([]string{command}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return oid
	}
	blob := input("fixture historical private content, not an upload\n", "hash-object", "-w", "--stdin")
	tree := input("100644 blob "+blob+"\trecovered.txt\n", "mktree")
	head := runGit(t, f.repo, "rev-parse", "HEAD")
	commit := input("fixture abandoned version\n", "commit-tree", tree, "-p", head)
	tag := input(fmt.Sprintf("object %s\ntype commit\ntag lost-fixture\ntagger Fixture <fixture@example.invalid> 1700000000 +0000\n\nprivate historical tag\n", commit), "mktag")
	// Only reflogs, not a current ref, remember this abandoned commit.
	runGit(t, f.repo, "update-ref", "--create-reflog", "-m", "fixture temporary version", "HEAD", commit)
	runGit(t, f.repo, "update-ref", "-m", "fixture reset", "HEAD", head)
	// Include a name that the empty restore uses as its first placeholder.
	runGit(t, f.repo, "branch", "infinite-disk-restore-unborn-0")
	oids := []string{blob, tree, commit, tag}
	input(strings.Join(oids, "\n")+"\n", "pack-objects", filepath.Join(f.repo, ".git", "objects", "pack", "pack"))
	for _, oid := range oids {
		if err := os.Remove(filepath.Join(f.repo, ".git", "objects", oid[:2], oid[2:])); err != nil {
			t.Fatal(err)
		}
	}
	p := plan(t, f)
	eligible(t, p.Candidates[0])
	indexBefore, _ := digestFile(filepath.Join(f.repo, ".git", "index"))
	r, err := f.e.Apply(ctx, p, p.Candidates[0].ID, p.ID, false)
	if err != nil || r.GitHistory == nil || r.GitHistory.PrivateRefs != len(oids) || r.GitHistory.Scope != completeGitHistoryScope {
		t.Fatal("complete private preservation failed", r, err)
	}
	indexAfter, _ := digestFile(filepath.Join(f.repo, ".git", "index"))
	if indexBefore != indexAfter || runGit(t, f.repo, "rev-parse", "HEAD") != head || runGit(t, f.repo, "status", "--porcelain") != "" {
		t.Fatal("preservation changed user branch, index or source")
	}
	for _, oid := range oids {
		if _, err := gitText(ctx, f.remote, "cat-file", "-e", oid); err == nil {
			t.Fatal("historical private object was uploaded")
		}
	}
	dest := filepath.Join(f.root, "restored-complete")
	if err := f.e.Restore(ctx, p.ID, p.Candidates[0].ID, dest); err != nil {
		t.Fatal(err)
	}
	for _, oid := range oids {
		runGit(t, dest, "cat-file", "-e", oid)
		if runGit(t, dest, "rev-parse", privateHistoryRefs+oid) != oid {
			t.Fatal("private object retention ref missing")
		}
	}
	if runGit(t, dest, "show", commit+":recovered.txt") != "fixture historical private content, not an upload" {
		t.Fatal("historical working version lost")
	}
	if runGit(t, dest, "rev-parse", "infinite-disk-restore-unborn-0") != head {
		t.Fatal("original branch colliding with placeholder was not restored")
	}
}

func TestPrivateHistoryRefCollisionNeverOverwritesOrPushes(t *testing.T) {
	f := newFixture(t)
	first := orphanBlob(t, f.repo)
	second, err := gitInput(context.Background(), f.repo, nil, []byte("second private fixture version\n"), "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	ref := privateHistoryRefs + first
	head := runGit(t, f.repo, "rev-parse", "HEAD")
	runGit(t, f.repo, "update-ref", ref, head)
	before := runGit(t, f.repo, "show-ref")
	remote := runGit(t, f.remote, "show-ref")
	p := plan(t, f)
	if _, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true); err == nil {
		t.Fatal("collision accepted")
	}
	exists(t, f.repo)
	if runGit(t, f.repo, "show-ref") != before || runGit(t, f.remote, "show-ref") != remote {
		t.Fatal("collision overwrote history or pushed work")
	}
	if _, err := gitText(context.Background(), f.repo, "rev-parse", "--verify", privateHistoryRefs+second); err == nil {
		t.Fatal("failed multi-ref transaction partially committed")
	}
}

func TestCompleteRecoveryRejectsLateRefAndCorruptedBundle(t *testing.T) {
	for _, kind := range []string{"late_internal_ref", "corrupt_bundle"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			orphanBlob(t, f.repo)
			p := plan(t, f)
			calls := 0
			f.e.Activity = func(context.Context) Activity {
				calls++
				if calls == 2 {
					if kind == "late_internal_ref" {
						runGit(t, f.repo, "update-ref", "refs/infinite-disk/recovery/unexpected", p.Candidates[0].Head)
					} else {
						bundle := filepath.Join(f.e.Config.StateDir, "recovery", p.Candidates[0].ID, p.ID[:12]+".bundle")
						file, err := os.OpenFile(bundle, os.O_WRONLY|os.O_APPEND, 0600)
						if err != nil {
							t.Fatal(err)
						}
						_, err = file.WriteString("fixture corruption")
						file.Close()
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				return Activity{Known: true}
			}
			if _, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true); err == nil {
				t.Fatal("late recovery mutation accepted")
			}
			exists(t, f.repo)
		})
	}
}

func TestRestoreOldBundleReceiptRemainsSupported(t *testing.T) {
	f := newFixture(t)
	p := plan(t, f)
	r, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	r.GitHistory = nil // Pre-complete-history receipt schema.
	if err := saveReceipt(f.e.Config, r); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Restore(context.Background(), p.ID, p.Candidates[0].ID, filepath.Join(f.root, "legacy-restore")); err != nil {
		t.Fatal("old recovery receipts no longer restore", err)
	}
}

func TestCompleteRecoverySHA256Objects(t *testing.T) {
	f := newFixtureFormat(t, "sha256")
	oid := orphanBlob(t, f.repo)
	p := plan(t, f)
	eligible(t, p.Candidates[0])
	r, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true)
	if err != nil || r.GitHistory == nil || r.GitHistory.ObjectFormat != "sha256" || len(oid) != 64 {
		t.Fatal("SHA256 object preservation failed", r, err)
	}
	dest := filepath.Join(f.root, "sha256-restored")
	if err := f.e.Restore(context.Background(), p.ID, p.Candidates[0].ID, dest); err != nil {
		t.Fatal(err)
	}
	runGit(t, dest, "cat-file", "-e", oid)
}

func TestRestoreRejectsInvalidCompleteHistoryProofBeforeWrites(t *testing.T) {
	for _, kind := range []string{"scope", "format", "count", "digest"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			p := plan(t, f)
			r, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "scope":
				r.GitHistory.Scope = "unknown"
			case "format":
				r.GitHistory.ObjectFormat = "unknown"
			case "count":
				r.GitHistory.Objects = 0
			case "digest":
				r.GitHistory.ObjectsSHA256 = "invalid"
			}
			if err := saveReceipt(f.e.Config, r); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(f.root, "invalid-restore")
			if err := f.e.Restore(context.Background(), p.ID, p.Candidates[0].ID, dest); err == nil {
				t.Fatal("invalid recovery proof accepted")
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("invalid proof wrote a restore destination")
			}
		})
	}
}
