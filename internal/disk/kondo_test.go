package disk

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKondoParserFailsClosed(t *testing.T) {
	valid := "/projects/old repo Node project (8 days ago)\n  └─ node_modules (313.6MiB)\nProjects cleaned: 0/1, Bytes deleted: 0.0B / 313.6MiB\n"
	parsed, err := parseKondo([]byte(valid))
	if err != nil || len(parsed) != 1 || parsed[0].Path != "/projects/old repo/node_modules" {
		t.Fatal(parsed, err)
	}
	parsed, err = parseKondo([]byte(strings.ReplaceAll(valid, "Node project", "Node (React Native) project")))
	if err != nil || parsed[0].CommonDir != "/projects/old repo" {
		t.Fatal("multiword project type", parsed, err)
	}
	for _, output := range []string{strings.ReplaceAll(valid, "node_modules", "../source"), strings.ReplaceAll(valid, "node_modules", "/source"), strings.ReplaceAll(valid, "Projects cleaned: 0/1", "Projects cleaned: 1/1"), strings.Split(valid, "Projects cleaned")[0], valid + "oops\n", strings.ReplaceAll(valid, "  └─ node_modules (313.6MiB)", "  └─ node_modules (313.6MiB)\n  └─ node_modules/sub (10.0MiB)")} {
		if _, err := parseKondo([]byte(output)); err == nil {
			t.Fatal("accepted invalid output", output)
		}
	}
}

func TestKondoRealDryRunIntegration(t *testing.T) {
	if _, err := exec.LookPath("kondo"); err != nil {
		t.Skip("optional installed Kondo integration")
	}
	f, c := cacheFixture(t)
	write(t, filepath.Join(f.repo, "package.json"), "{}\n")
	if err := filepath.Walk(f.repo, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		old(t, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p, err := f.e.CacheScan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Candidates) != 1 || p.Candidates[0].Path != c.Path {
		t.Fatal("unexpected Kondo discovery", p.Candidates)
	}
	eligible(t, p.Candidates[0])
	exists(t, c.Path)
	exists(t, filepath.Join(f.repo, "source.txt"))
	// CacheScan did not overwrite the checkout report namespace.
	if _, err := LoadPlan(f.e.Config, "latest"); err == nil {
		t.Fatal("cache report overwrote checkout report")
	}
}
func cacheFixture(t *testing.T) (fixture, Candidate) {
	t.Helper()
	f := newFixture(t)
	if err := EnsureState(f.e.Config); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.repo, ".gitignore"), "node_modules/\n")
	write(t, filepath.Join(f.repo, "node_modules", "package", "index.js"), "regenerable\n")
	for _, p := range []string{filepath.Join(f.repo, "node_modules/package/index.js"), filepath.Join(f.repo, "node_modules/package"), filepath.Join(f.repo, "node_modules")} {
		old(t, p)
	}
	return f, Candidate{ID: pathID(filepath.Join(f.repo, "node_modules")), Path: filepath.Join(f.repo, "node_modules"), CommonDir: f.repo, Kind: "kondo-cache", Name: "fixture"}
}
func TestCacheApprovalRemovesOnlyArtifact(t *testing.T) {
	f, c := cacheFixture(t)
	ctx := context.Background()
	c = f.e.inspectCache(ctx, c, Activity{Known: true})
	eligible(t, c)
	p, err := SavePlan(f.e.CacheConfig(), Plan{Version: 1, CreatedAt: time.Now(), ConfigDigest: ConfigDigest(f.e.CacheConfig()), Candidates: []Candidate{c}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.ApplyCache(ctx, p, c.ID, ""); err == nil {
		t.Fatal("accepted missing approval")
	}
	exists(t, c.Path)
	head := runGit(t, f.repo, "rev-parse", "HEAD")
	r, err := f.e.ApplyCache(ctx, p, c.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Result != "cache_removed" {
		t.Fatal(r)
	}
	if _, err = os.Stat(c.Path); !os.IsNotExist(err) {
		t.Fatal("artifact retained", err)
	}
	exists(t, filepath.Join(f.repo, "source.txt"))
	exists(t, filepath.Join(f.repo, ".git"))
	if runGit(t, f.repo, "rev-parse", "HEAD") != head {
		t.Fatal("changed source HEAD")
	}
}

func TestCacheMonorepoComponentAndRootActivity(t *testing.T) {
	f, c := cacheFixture(t)
	component := filepath.Join(f.repo, "component")
	if err := os.MkdirAll(component, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(c.Path, filepath.Join(component, "node_modules")); err != nil {
		t.Fatal(err)
	}
	c.Path = filepath.Join(component, "node_modules")
	c.CommonDir = component
	c.ID = pathID(c.Path)
	got := f.e.inspectCache(context.Background(), c, Activity{Known: true})
	eligible(t, got)
	got = f.e.inspectCache(context.Background(), c, Activity{Known: true, Paths: []string{f.repo}})
	if got.Eligible {
		t.Fatal("ignored monorepo root activity")
	}
}
func TestCacheSafetyBlockers(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(fixture, Candidate)
	}{
		{"tracked", func(f fixture, c Candidate) { runGit(t, f.repo, "add", "-f", "node_modules/package/index.js") }},
		{"private", func(f fixture, c Candidate) {
			write(t, filepath.Join(c.Path, ".env"), "private")
			old(t, filepath.Join(c.Path, ".env"))
			old(t, c.Path)
		}},
		{"nested git", func(f fixture, c Candidate) {
			write(t, filepath.Join(c.Path, ".git"), "gitdir: elsewhere")
			old(t, filepath.Join(c.Path, ".git"))
			old(t, c.Path)
		}},
		{"escaping symlink", func(f fixture, c Candidate) {
			if err := os.Symlink(filepath.Join(f.repo, "source.txt"), filepath.Join(c.Path, "link")); err != nil {
				t.Fatal(err)
			}
			old(t, c.Path)
		}},
		{"unignored", func(f fixture, c Candidate) { write(t, filepath.Join(f.repo, ".gitignore"), "") }},
		{"recent", func(f fixture, c Candidate) { write(t, filepath.Join(c.Path, "fresh"), "regenerable") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := cacheFixture(t)
			tc.mutate(f, c)
			got := f.e.inspectCache(context.Background(), c, Activity{Known: true})
			if got.Eligible {
				t.Fatal("unsafe artifact eligible", tc.name)
			}
			exists(t, c.Path)
		})
	}
}
func TestCacheChangedOrActiveAfterReportRetained(t *testing.T) {
	for _, mode := range []string{"changed", "active", "unknown", "expired", "config"} {
		t.Run(mode, func(t *testing.T) {
			f, c := cacheFixture(t)
			ctx := context.Background()
			c = f.e.inspectCache(ctx, c, Activity{Known: true})
			eligible(t, c)
			p, err := SavePlan(f.e.CacheConfig(), Plan{Version: 1, CreatedAt: time.Now(), ConfigDigest: ConfigDigest(f.e.CacheConfig()), Candidates: []Candidate{c}})
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "changed":
				write(t, filepath.Join(c.Path, "new"), "new")
			case "active":
				f.e.Activity = func(context.Context) Activity { return Activity{Known: true, Paths: []string{c.Path}} }
			case "unknown":
				f.e.Activity = func(context.Context) Activity { return Activity{} }
			case "expired":
				f.e.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
			case "config":
				f.e.Config.IdleDays++
			}
			if _, err = f.e.ApplyCache(ctx, p, c.ID, p.ID); err == nil {
				t.Fatal("unsafe apply accepted")
			}
			exists(t, c.Path)
		})
	}
}
