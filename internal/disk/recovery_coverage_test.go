package disk

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type coverageFixture struct {
	e                   *Engine
	work, remote, state string
}

func newCoverageFixture(t *testing.T) coverageFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := coverageFixture{work: filepath.Join(root, "work"), remote: filepath.Join(root, "remote.git"), state: filepath.Join(root, "state")}
	runGit(t, root, "init", "--bare", "-b", "main", f.remote)
	runGit(t, root, "init", "-b", "main", f.work)
	runGit(t, f.work, "config", "user.email", "t@example.com")
	runGit(t, f.work, "config", "user.name", "t")
	write(t, filepath.Join(f.work, "a.txt"), "a")
	runGit(t, f.work, "add", ".")
	runGit(t, f.work, "commit", "-m", "a")
	runGit(t, f.work, "remote", "add", "origin", f.remote)
	runGit(t, f.work, "push", "-q", "origin", "main")
	c := DefaultConfig()
	c.StateDir = f.state
	f.e = NewEngine(c)
	f.e.allowLocalRemote = true
	if err := EnsureState(c); err != nil {
		t.Fatal(err)
	}
	return f
}

// bundle writes a recovery bundle of refs under the state dir, plus the
// receipt that maps it to the fixture remote.
func (f coverageFixture) bundle(t *testing.T, name string, refs ...string) string {
	t.Helper()
	dir := filepath.Join(f.state, "batch", "state")
	path := filepath.Join(dir, "recovery", name+".bundle")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.work, append([]string{"bundle", "create", "-q", path}, refs...)...)
	b, _ := json.Marshal(Receipt{BundlePath: path, RemoteURL: f.remote})
	write(t, filepath.Join(dir, "receipts", name+".json"), string(b))
	return path
}

func statusOf(t *testing.T, r CoverageReport, path string) BundleCoverage {
	t.Helper()
	for _, b := range r.Bundles {
		if b.Path == path {
			return b
		}
	}
	t.Fatalf("bundle %s missing from report", path)
	return BundleCoverage{}
}

func TestRecoveryCoverageProvesOnlyDurableHistory(t *testing.T) {
	f := newCoverageFixture(t)
	onMain := f.bundle(t, "main", "main")

	runGit(t, f.work, "checkout", "-q", "-b", "feature")
	write(t, filepath.Join(f.work, "b.txt"), "b")
	runGit(t, f.work, "add", ".")
	runGit(t, f.work, "commit", "-m", "b")
	runGit(t, f.work, "push", "-q", "origin", "feature")
	branchOnly := f.bundle(t, "feature", "feature")

	write(t, filepath.Join(f.work, "c.txt"), "c")
	runGit(t, f.work, "add", ".")
	runGit(t, f.work, "commit", "-m", "c")
	localOnly := f.bundle(t, "local", "feature")

	runGit(t, f.work, "checkout", "-q", "-b", "release", "main")
	write(t, filepath.Join(f.work, "d.txt"), "d")
	runGit(t, f.work, "add", ".")
	runGit(t, f.work, "commit", "-m", "d")
	runGit(t, f.work, "tag", "-a", "-m", "v1", "v1")
	runGit(t, f.work, "push", "-q", "origin", "v1")
	tagged := f.bundle(t, "tagged", "release")

	write(t, filepath.Join(f.state, "orphan", "x.bundle"), "not a bundle")

	r, err := f.e.VerifyRecoveryCoverage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, r, onMain); got.Status != CoverageCovered {
		t.Fatalf("main bundle: %+v", got)
	}
	if got := statusOf(t, r, tagged); got.Status != CoverageCovered {
		t.Fatalf("tag bundle: %+v", got)
	}
	if got := statusOf(t, r, branchOnly); got.Status != CoverageRetained || got.Reason != "1 commit(s) are only on deletable remote branches" {
		t.Fatalf("branch-only bundle must be retained: %+v", got)
	}
	if got := statusOf(t, r, localOnly); got.Status != CoverageRetained {
		t.Fatalf("local-only bundle must be retained: %+v", got)
	}
	if got := statusOf(t, r, filepath.Join(f.state, "orphan", "x.bundle")); got.Status != CoverageRetained || got.Reason != "no receipt maps this bundle to a remote" {
		t.Fatalf("orphan bundle: %+v", got)
	}
	if _, err := os.Stat(f.e.coverageReportPath(r.ID)); err != nil {
		t.Fatalf("report not saved: %v", err)
	}
}

func TestRecoveryPruneRequiresApprovalAndReproof(t *testing.T) {
	f := newCoverageFixture(t)
	keep := f.bundle(t, "keep", "main")
	tampered := f.bundle(t, "tampered", "main")
	r, err := f.e.VerifyRecoveryCoverage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.PruneCoveredBundles(context.Background(), r.ID, ""); err == nil {
		t.Fatal("prune without approval must fail")
	}
	if _, err := f.e.PruneCoveredBundles(context.Background(), r.ID, r.ID+"0"); err == nil {
		t.Fatal("prune with mismatched approval must fail")
	}
	// Rewriting a bundle after the report must keep it.
	runGit(t, f.work, "bundle", "create", "-q", tampered, "main", "--all")
	res, err := f.e.PruneCoveredBundles(context.Background(), r.ID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); !os.IsNotExist(err) {
		t.Fatalf("covered bundle not pruned: %v", err)
	}
	if _, err := os.Stat(tampered); err != nil {
		t.Fatalf("changed bundle must be kept: %v", err)
	}
	if res.KeptBundles != 1 || res.FreedBytes == 0 {
		t.Fatalf("result: %+v", res)
	}
	for _, b := range res.Bundles {
		if b.Result == "pruned" && b.Restore == "" {
			t.Fatalf("pruned bundle lacks restore instructions: %+v", b)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(f.state, coverageDir, r.ID+"-prune-*.json"))
	if len(matches) != 1 {
		t.Fatalf("prune receipt: %v", matches)
	}
}

func TestRecoveryCoverageRejectsDeletedDefaultBranchHistory(t *testing.T) {
	f := newCoverageFixture(t)
	runGit(t, f.work, "checkout", "-q", "-b", "gone")
	write(t, filepath.Join(f.work, "g.txt"), "g")
	runGit(t, f.work, "add", ".")
	runGit(t, f.work, "commit", "-m", "g")
	runGit(t, f.work, "push", "-q", "origin", "gone")
	b := f.bundle(t, "gone", "gone")
	runGit(t, f.work, "push", "-q", "origin", "--delete", "gone")
	r, err := f.e.VerifyRecoveryCoverage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, r, b); got.Status != CoverageRetained {
		t.Fatalf("history from a deleted branch must be retained: %+v", got)
	}
}
