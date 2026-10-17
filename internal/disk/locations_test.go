package disk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProviderLocationDiscoveryAndExplicitAdd(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{".codex/worktrees", ".cursor/worktrees", ".t3/worktrees", "emdash/worktrees", "code/product/.claude/worktrees", "code/product/workspaces", "code/other/.worktrees"}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Join(home, path), 0700); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(home, path, "checkout/.git"), "gitdir: fixture")
	}
	c := DefaultConfig()
	c.Roots = []string{filepath.Join(home, ".codex/worktrees")}
	c.StateDir = filepath.Join(home, "state")
	rows, warnings := Locations(c, home, true)
	if len(warnings) > 0 {
		t.Fatal(warnings)
	}
	states := map[string]string{}
	for _, row := range rows {
		states[row.Path] = row.Status
	}
	for _, path := range paths {
		want := "not configured"
		if path == ".codex/worktrees" {
			want = "included"
		}
		if states[filepath.Join(home, path)] != want {
			t.Fatal(path, states)
		}
	}
	if states[filepath.Join(home, ".grok/worktrees")] != "not present" {
		t.Fatal("invented Grok worktrees")
	}
	updated, err := AddLocations(c, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Roots) != 1 {
		t.Fatal("read-only discovery modified config")
	}
	for _, path := range paths {
		if locationStatus(updated, filepath.Join(home, path)) != "included" {
			t.Fatal("did not include", path)
		}
	}
	for _, root := range updated.Roots {
		if root == filepath.Join(home, ".grok/worktrees") {
			t.Fatal("added nonexistent root")
		}
	}
}

func TestLocationDiscoveryDoesNotTreatFeatureFoldersAsWorktrees(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "code/product/src/features/workspaces")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig()
	c.Roots = nil
	rows, _ := Locations(c, home, true)
	for _, row := range rows {
		if row.Path == path {
			t.Fatal("classified feature folder as worktrees")
		}
	}
}
func TestLocationDiscoverySkipsDependenciesAndSymlinks(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(home, "code/project/node_modules/dependency/worktrees")
	if err := os.MkdirAll(hidden, 0700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(home, "elsewhere/worktrees")
	if err := os.MkdirAll(elsewhere, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "elsewhere"), filepath.Join(home, "code/linked")); err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig()
	c.Roots = nil
	rows, _ := Locations(c, home, true)
	for _, row := range rows {
		if row.Path == hidden || row.Path == elsewhere || row.Path == filepath.Join(home, "code/linked/worktrees") {
			t.Fatal("followed hidden or linked location", row)
		}
	}
}
func TestMinimalRootsAndDiscoveryCoverage(t *testing.T) {
	roots := minimalRoots([]string{"/test/projects/nested", "/test/other", "/test/projects", "/test/projects"})
	if len(roots) != 2 {
		t.Fatal(roots)
	}
	f := newFixture(t)
	f.e.Config.Roots = append(f.e.Config.Roots, f.repo)
	p, err := f.e.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Candidates) != 1 || p.DiscoveredProjects != 1 || !p.DiscoveryComplete {
		t.Fatal("incorrect coverage", p.DiscoveredProjects, p.DiscoveryComplete, len(p.Candidates))
	}
}
