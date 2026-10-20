package disk

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func countedReportDigests(ctx context.Context) (*reportDigests, *int) {
	r := newReportDigests(ctx)
	n := new(int)
	r.read = func(name string) (string, error) { *n = *n + 1; return digestFile(name) }
	return r, n
}

func reportTempDir(t testing.TB) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestReportDigestsReuseOnlyUnchangedIdentities(t *testing.T) {
	root := reportTempDir(t)
	name := filepath.Join(root, "sidecar")
	write(t, name, "old fixture bytes")
	r, reads := countedReportDigests(context.Background())
	first, err := r.sum(name)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := r.sum(name); err != nil || first != second || *reads != 1 {
		t.Fatal("unchanged identity was not reused", *reads, err)
	}
	// Same size and restored mtime are insufficient: ctime must invalidate.
	s, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	write(t, name, "new fixture bytes")
	if err = os.Chtimes(name, s.ModTime(), s.ModTime()); err != nil {
		t.Fatal(err)
	}
	next, err := r.sum(name)
	if err != nil || next == first || *reads != 2 {
		t.Fatal("changed bytes with restored mtime reused stale digest", *reads, err)
	}
	write(t, filepath.Join(root, "replacement"), "new fixture bytes")
	if err := os.Rename(filepath.Join(root, "replacement"), name); err != nil {
		t.Fatal(err)
	}
	if _, err := r.sum(name); err != nil || *reads != 3 {
		t.Fatal("replacement inode reused prior entry", *reads, err)
	}
	if err := os.Chmod(name, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := r.sum(name); err != nil || *reads != 4 {
		t.Fatal("permission/ctime change reused prior entry", *reads, err)
	}
}

func TestReportDigestCacheIsLocalAndNeverUsedForMutationInventory(t *testing.T) {
	_, _, s := claudeFixture(t)
	r, reads := countedReportDigests(context.Background())
	a, err := claudeFilesWithDigest(s, r.sum)
	if err != nil {
		t.Fatal(err)
	}
	b, err := claudeFilesWithDigest(s, r.sum)
	if err != nil || !slices.Equal(a, b) || *reads != len(a) {
		t.Fatal("report inventory mismatch or repeated payload reads", *reads, len(a), err)
	}
	// A new inventory member still requires a full fresh walk, even after hits.
	extra := filepath.Join(filepath.Dir(s.Store), s.ID, "subagents", "new-sidecar")
	write(t, extra, "new fixture sidecar")
	b, err = claudeFilesWithDigest(s, r.sum)
	if err != nil || len(b) != len(a)+1 || *reads != len(a)+1 {
		t.Fatal("fresh inventory missed an added sidecar", *reads, err)
	}
	fresh, freshReads := countedReportDigests(context.Background())
	if _, err := claudeFilesWithDigest(s, fresh.sum); err != nil || *freshReads != len(b) {
		t.Fatal("another report reused the previous invocation", *freshReads, err)
	}
	// This cache never belongs to Engine. Public inspection/mutation paths use
	// the uncached wrapper, not a poisoned report entry from a prior scan.
	for k, v := range r.entries {
		v.sha = "poisoned-report-only-digest"
		r.entries[k] = v
	}
	uncached, err := claudeFiles(s)
	if err != nil || !slices.Equal(uncached, b) {
		t.Fatal("mutation inventory consumed report cache", err)
	}
}

func TestReportDigestsRejectCancellationSymlinksAndConcurrentChanges(t *testing.T) {
	root := reportTempDir(t)
	name := filepath.Join(root, "sidecar")
	write(t, name, "fixture content")
	ctx, cancel := context.WithCancel(context.Background())
	r, reads := countedReportDigests(ctx)
	if _, err := r.sum(name); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := r.sum(name); err == nil || *reads != 1 {
		t.Fatal("cancelled cache hit accepted")
	}
	link := filepath.Join(root, "symlink")
	if err := os.Symlink(name, link); err != nil {
		t.Fatal(err)
	}
	r, reads = countedReportDigests(context.Background())
	if _, err := r.sum(link); err == nil || *reads != 0 {
		t.Fatal("symlink followed by report digest cache")
	}
	r.read = func(p string) (string, error) {
		digest, err := digestFile(p)
		write(t, p, "changed during full read")
		return digest, err
	}
	if _, err := r.sum(name); err == nil || len(r.entries) != 0 {
		t.Fatal("concurrently changed read became reusable")
	}
}

func TestReportDigestFallbackAndBoundedMemory(t *testing.T) {
	root := reportTempDir(t)
	name := filepath.Join(root, "sidecar")
	write(t, name, "fixture bytes")
	if err := os.Link(name, filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	r, reads := countedReportDigests(context.Background())
	for i := 0; i < 2; i++ {
		if _, err := r.sum(name); err != nil {
			t.Fatal("unsupported cache identity should fall back to normal full read", err)
		}
	}
	if *reads != 2 || len(r.entries) != 0 {
		t.Fatal("hardlinked metadata was cached")
	}
	if err := os.Remove(filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		r.entries[fmt.Sprint(i)] = reportDigest{}
	}
	for i := 0; i < 2; i++ {
		if _, err := r.sum(name); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.entries) != 5000 || *reads != 4 {
		t.Fatal("full cache must fall back without growing or accepting stale data")
	}
}

func TestApplyRevalidatesSessionBytesAfterCachedReport(t *testing.T) {
	f, _, s := claudeFixture(t)
	p := plan(t, f)
	eligible(t, p.Candidates[0])
	sidecar := filepath.Join(filepath.Dir(s.Store), s.ID, "subagents", "agent-child.jsonl")
	before, err := os.Stat(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	write(t, sidecar, "CHANGED SUBAGENT\n")
	if err := os.Chtimes(sidecar, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	refs := runGit(t, f.remote, "show-ref")
	if _, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true); err == nil {
		t.Fatal("post-report session change was not revalidated at approval time")
	}
	exists(t, f.repo)
	if refs != runGit(t, f.remote, "show-ref") {
		t.Fatal("changed session allowed a recovery push before rejection")
	}
}

func BenchmarkRepeatedReportSidecarDigests(b *testing.B) {
	root := reportTempDir(b)
	name := filepath.Join(root, "sidecar")
	data := make([]byte, 4<<20)
	if err := os.WriteFile(name, data, 0600); err != nil {
		b.Fatal(err)
	}
	for _, cached := range []bool{false, true} {
		b.Run(fmt.Sprint("cached=", cached), func(b *testing.B) {
			for n := 0; n < b.N; n++ {
				digest := digestFile
				if cached {
					digest = newReportDigests(context.Background()).sum
				}
				for i := 0; i < 20; i++ {
					if _, err := digest(name); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
