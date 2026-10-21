package disk

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func orphanBlob(t *testing.T, repo string) string {
	t.Helper()
	oid, err := gitInput(context.Background(), repo, nil, []byte("fixture forgotten intermediate version\n"), "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	return oid
}

func TestCloneRemovalPreservesUnreachableHistoryPrivately(t *testing.T) {
	for _, kind := range []string{"blob", "commit"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			p := plan(t, f)
			eligible(t, p.Candidates[0])
			// Create history after the report: fingerprints do not prove that
			// every unreferenced object is disposable.
			oid := ""
			if kind == "blob" {
				oid = orphanBlob(t, f.repo)
			} else {
				tree := runGit(t, f.repo, "rev-parse", "HEAD^{tree}")
				var err error
				oid, err = gitInput(context.Background(), f.repo, nil, []byte("fixture abandoned commit\n"), "commit-tree", tree, "-p", p.Candidates[0].Head)
				if err != nil {
					t.Fatal(err)
				}
			}
			r, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true)
			if err != nil || r.Result != "removed" || r.GitHistory == nil || r.GitHistory.PrivateRefs != 1 {
				t.Fatal("unreachable work was not privately preserved", r, err)
			}
			if _, err := os.Stat(f.repo); !os.IsNotExist(err) {
				t.Fatal("verified clone not removed")
			}
			if _, err := gitText(context.Background(), f.remote, "cat-file", "-e", oid); err == nil {
				t.Fatal("private unreachable history was pushed to origin")
			}
			destination := filepath.Join(f.root, "restored")
			if err := f.e.Restore(context.Background(), p.ID, p.Candidates[0].ID, destination); err != nil {
				t.Fatal(err)
			}
			runGit(t, destination, "cat-file", "-e", oid)
			if got := runGit(t, destination, "rev-parse", privateHistoryRefs+oid); got != oid {
				t.Fatal("private recovery ref not restored")
			}
		})
	}
}

func TestCloneHistoryAppearingDuringRemovalRetainsCheckout(t *testing.T) {
	for _, at := range []int{2, 3} {
		f := newFixture(t)
		p := plan(t, f)
		calls, oid := 0, ""
		f.e.Activity = func(context.Context) Activity {
			calls++
			if calls == at {
				repo := f.repo
				if _, err := os.Stat(repo); os.IsNotExist(err) {
					repo = f.root + "/.infinite-disk-quarantine/" + p.Candidates[0].ID + "-" + p.ID[:12]
				}
				oid = orphanBlob(t, repo)
			}
			return Activity{Known: true}
		}
		r, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true)
		if err == nil || oid == "" || r.QuarantinePath != "" {
			t.Fatal("late Git work was not retained/rolled back", at, r, err)
		}
		exists(t, f.repo)
		runGit(t, f.repo, "cat-file", "-e", oid)
	}
}

func TestCloneHistoryUnknownFailsClosedAndSharedDatabaseStays(t *testing.T) {
	if requireCompleteCloneHistory(context.Background(), Candidate{Path: t.TempDir(), Kind: "clone"}) == nil {
		t.Fatal("unknown history accepted")
	}
	f := newFixture(t)
	orphanBlob(t, f.repo)
	if err := requireCompleteCloneHistory(context.Background(), Candidate{Path: f.repo, Kind: "worktree"}); err != nil {
		t.Fatal("shared database is retained, not deleted", err)
	}
}

func fakeHistoryGit(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process fixture")
	}
	dir := t.TempDir()
	// Refuse a command other than the complete history check. Nothing here
	// reads a real checkout, invokes real Git, writes refs, or reaches a remote.
	script := "#!/bin/sh\ncase \"$*\" in\n  *'fsck --unreachable --no-reflogs --no-progress') ;;\n  *) exit 97 ;;\nesac\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestCloneHistoryAllowsExpensiveFsckWithoutSkippingVerification(t *testing.T) {
	dir := fakeHistoryGit(t, "exec sleep 21")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	// A real-host fsck passed in 24.8 seconds but the old 20-second reader
	// rolled the checkout back. This command must actually finish successfully.
	if err := requireCompleteCloneHistory(ctx, Candidate{Path: dir, Kind: "clone"}); err != nil {
		t.Fatal("completed expensive history check rejected", err)
	}
}

func TestCloneHistoryLongRunnerStillHonorsCancellation(t *testing.T) {
	dir := fakeHistoryGit(t, "exec sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := requireCompleteCloneHistory(ctx, Candidate{Path: dir, Kind: "clone"}); err == nil {
		t.Fatal("canceled verification accepted")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("caller cancellation was not honored promptly")
	}
}

func TestCloneHistoryLongRunnerRetainsErrorsAndUnexpectedOutput(t *testing.T) {
	for _, body := range []string{"exit 1", "printf 'unreachable blob fixture-object\\n'", "printf 'unexpected diagnostic\\n'"} {
		t.Run(body, func(t *testing.T) {
			dir := fakeHistoryGit(t, body)
			if err := requireCompleteCloneHistory(context.Background(), Candidate{Path: dir, Kind: "clone"}); err == nil {
				t.Fatal("failed or nonempty history verification accepted")
			}
		})
	}
}
