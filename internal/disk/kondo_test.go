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

func codexCacheFixture(t *testing.T) (fixture, Candidate) {
	t.Helper()
	f, c := cacheFixture(t)
	home := filepath.Join(filepath.Dir(f.root), "home")
	managed := filepath.Join(home, ".codex", "worktrees")
	repo := filepath.Join(managed, "fixture", "stack", "workspaces", "task", "app")
	if err := os.MkdirAll(filepath.Dir(repo), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.repo, repo); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	f.repo = repo
	f.root = managed
	f.e.Config.Roots = []string{managed}
	c.Path = filepath.Join(repo, "node_modules")
	c.CommonDir = repo
	c.ID = pathID(c.Path)
	return f, c
}

func TestCodexCacheApprovalRetainsCheckoutAndPrivateState(t *testing.T) {
	f, c := codexCacheFixture(t)
	write(t, filepath.Join(f.repo, ".env"), "fixture private configuration")
	write(t, filepath.Join(f.repo, "patches", "package.patch"), "fixture patch")
	write(t, filepath.Join(f.root, "fixture", ".codex", "session.jsonl"), "fixture session")
	head := runGit(t, f.repo, "rev-parse", "HEAD")
	c = f.e.inspectCache(context.Background(), c, Activity{Known: true})
	eligible(t, c)
	p, err := SavePlan(f.e.CacheConfig(), Plan{Version: 1, CreatedAt: f.e.Now(), ConfigDigest: ConfigDigest(f.e.CacheConfig()), Candidates: []Candidate{c}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.ApplyCache(context.Background(), p, c.ID, ""); err == nil {
		t.Fatal("cache accepted without exact approval")
	}
	if _, err = f.e.ApplyCache(context.Background(), p, c.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(c.Path); !os.IsNotExist(err) {
		t.Fatal("approved cache still exists", err)
	}
	for _, path := range []string{".git", "source.txt", ".env", "patches/package.patch"} {
		exists(t, filepath.Join(f.repo, path))
	}
	exists(t, filepath.Join(f.root, "fixture", ".codex", "session.jsonl"))
	if runGit(t, f.repo, "rev-parse", "HEAD") != head || f.e.safeTarget(f.repo) == nil {
		t.Fatal("checkout changed or managed checkout removal became permitted")
	}
}

func TestCodexCacheProjectScopeStillFailsClosed(t *testing.T) {
	for _, mode := range []string{"unknown activity", "active", "private", "tracked", "recent", "outside roots", "root itself", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			f, c := codexCacheFixture(t)
			activity := Activity{Known: true}
			switch mode {
			case "unknown activity":
				activity = Activity{}
			case "active":
				activity.Paths = []string{c.Path}
			case "private":
				write(t, filepath.Join(c.Path, ".env"), "fixture")
			case "tracked":
				runGit(t, f.repo, "add", "-f", "node_modules/package/index.js")
			case "recent":
				write(t, filepath.Join(c.Path, "fresh.js"), "fixture")
			case "outside roots":
				f.e.Config.Roots = []string{filepath.Dir(f.root) + "-other"}
			case "root itself":
				f.e.Config.Roots = []string{f.repo}
			case "symlink":
				alias := f.repo + "-alias"
				if err := os.Symlink(f.repo, alias); err != nil {
					t.Fatal(err)
				}
				c.CommonDir = alias
				c.Path = filepath.Join(alias, "node_modules")
			}
			if got := f.e.inspectCache(context.Background(), c, activity); got.Eligible {
				t.Fatal("unsafe managed cache accepted", mode)
			}
			exists(t, filepath.Join(f.repo, "node_modules"))
		})
	}
}

func TestCodexCachePlanUsesKondoButBinaryAndCheckoutStayBlocked(t *testing.T) {
	f, c := codexCacheFixture(t)
	bin := t.TempDir()
	write(t, filepath.Join(bin, "kondo"), "#!/bin/sh\nprintf '%s Node project\\n  └─ node_modules (1.0MiB)\\nProjects cleaned: 0/1, Bytes deleted: 0.0B / 1.0MiB\\n' \"$INFINITE_DISK_FIXTURE_PROJECT\"\n")
	if err := os.Chmod(filepath.Join(bin, "kondo"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(filepath.ListSeparator)+os.Getenv("PATH"))
	t.Setenv("INFINITE_DISK_FIXTURE_PROJECT", f.repo)
	p, err := f.e.PlanBinaryArtifact(context.Background(), f.repo, "node_modules", "discard")
	if err != nil || len(p.Candidates) != 1 {
		t.Fatal("managed cache plan failed", err)
	}
	eligible(t, p.Candidates[0])
	if p.Candidates[0].Path != c.Path || p.Candidates[0].Kind != "kondo-cache" {
		t.Fatal("unexpected managed cache scope")
	}
	binaryPlan, err := f.e.PlanBinaryArtifact(context.Background(), f.repo, "source.txt", "discard")
	if err != nil {
		t.Fatal(err)
	}
	if len(binaryPlan.Candidates) != 1 || binaryPlan.Candidates[0].Eligible || f.e.safeTarget(f.repo) == nil {
		t.Fatal("native checkout lifecycle guard bypassed")
	}
	exists(t, filepath.Join(f.repo, "source.txt"))
	exists(t, c.Path)
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

func TestCacheInternalPackageLinksAreHashedAndNeverFollowedForRemoval(t *testing.T) {
	f, c := cacheFixture(t)
	link := filepath.Join(c.Path, "bin-link")
	if err := os.Symlink("package/index.js", link); err != nil {
		t.Fatal(err)
	}
	f.e.Now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	c = f.e.inspectCache(context.Background(), c, Activity{Known: true})
	eligible(t, c)
	p, err := SavePlan(f.e.CacheConfig(), Plan{Version: 1, CreatedAt: f.e.Now(), ConfigDigest: ConfigDigest(f.e.CacheConfig()), Candidates: []Candidate{c}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(f.repo, "source.txt"), link); err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.ApplyCache(context.Background(), p, c.ID, p.ID); err == nil {
		t.Fatal("changed escaping link was removed")
	}
	exists(t, filepath.Join(f.repo, "source.txt"))
	exists(t, c.Path)
	if err = os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("package/index.js", link); err != nil {
		t.Fatal(err)
	}
	c = f.e.inspectCache(context.Background(), c, Activity{Known: true})
	eligible(t, c)
	p, err = SavePlan(f.e.CacheConfig(), Plan{Version: 1, CreatedAt: f.e.Now(), ConfigDigest: ConfigDigest(f.e.CacheConfig()), Candidates: []Candidate{c}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.ApplyCache(context.Background(), p, c.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	exists(t, filepath.Join(f.repo, "source.txt"))
	if _, err = os.Lstat(c.Path); !os.IsNotExist(err) {
		t.Fatal("artifact not removed")
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
