package disk

import (
	"context"
	"os"
	"strings"
	"testing"
)

func orphanBlob(t *testing.T, repo string) string {
	t.Helper()
	oid, err := gitInput(context.Background(), repo, nil, []byte("fixture forgotten intermediate version\n"), "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	return oid
}

func TestCloneRemovalRetainsUnreachableHistoryBeforeWrites(t *testing.T) {
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
			localRefs := runGit(t, f.repo, "show-ref")
			remoteRefs := runGit(t, f.remote, "show-ref")
			_, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true)
			if err == nil || !strings.Contains(err.Error(), "unreachable-only Git history") {
				t.Fatal("unreachable work was not retained", err)
			}
			exists(t, f.repo)
			runGit(t, f.repo, "cat-file", "-e", oid)
			if localRefs != runGit(t, f.repo, "show-ref") || remoteRefs != runGit(t, f.remote, "show-ref") {
				t.Fatal("history rejection must happen before recovery writes")
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
