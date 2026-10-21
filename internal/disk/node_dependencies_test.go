package disk

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func disposableNodeFixture(t *testing.T) (fixture, Candidate) {
	t.Helper()
	f, c := cacheFixture(t)
	write(t, filepath.Join(f.repo, "package.json"), `{"name":"fixture","patchedDependencies":{"package@1":"patches/package.patch"}}`)
	write(t, filepath.Join(f.repo, "bun.lock"), `{"lockfileVersion":1}`)
	write(t, filepath.Join(f.repo, "patches/package.patch"), "fixture patch")
	runGit(t, f.repo, "add", "package.json", "bun.lock", "patches/package.patch")
	runGit(t, f.repo, "commit", "-m", "fixture reinstall inputs")
	write(t, filepath.Join(c.Path, "package/credential.d.ts"), "public library declaration")
	write(t, filepath.Join(c.Path, "package/.env.example"), "fixture configuration example")
	write(t, filepath.Join(c.Path, "package/.claude/settings.local.json"), `{"fixture":true}`)
	write(t, filepath.Join(c.Path, "package/.claude/skills/fixture/SKILL.md"), "package author fixture")
	return f, c
}

func TestDisposableNodeModulesNoAgeOrVendorFilenameBackup(t *testing.T) {
	f, c := disposableNodeFixture(t)
	ctx := context.Background()
	write(t, filepath.Join(f.repo, ".env"), "private configuration outside dependencies")
	before := runGit(t, f.repo, "rev-parse", "HEAD")
	c = f.e.inspectCache(ctx, c, Activity{Known: true})
	eligible(t, c)
	if c.ArtifactPolicy != disposableNodeModules {
		t.Fatal("missing explicit disposable-dependency policy")
	}
	p, err := SavePlan(f.e.CacheConfig(), Plan{Version: 1, CreatedAt: f.e.Now(), ConfigDigest: ConfigDigest(f.e.CacheConfig()), Candidates: []Candidate{c}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.ApplyCache(ctx, p, c.ID, ""); err == nil {
		t.Fatal("dependency policy bypassed exact approval")
	}
	if _, err = f.e.ApplyCache(ctx, p, c.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(c.Path); !os.IsNotExist(err) {
		t.Fatal("approved dependencies remain", err)
	}
	for _, name := range []string{"source.txt", ".git", ".env", "package.json", "bun.lock", "patches/package.patch"} {
		exists(t, filepath.Join(f.repo, name))
	}
	if before != runGit(t, f.repo, "rev-parse", "HEAD") {
		t.Fatal("source changed")
	}
	if _, err = os.Stat(filepath.Join(f.e.Config.StateDir, "private-archives")); !os.IsNotExist(err) {
		t.Fatal("dependency cleanup created private backup")
	}
}

func TestDisposableNodeModulesStillProtectsWorkAndActivity(t *testing.T) {
	for _, mode := range []string{"active", "unknown", "tracked", "git", "session", "external link", "untracked patch", "missing lock"} {
		t.Run(mode, func(t *testing.T) {
			f, c := disposableNodeFixture(t)
			activity := Activity{Known: true}
			switch mode {
			case "active":
				activity.Paths = []string{f.repo}
			case "unknown":
				activity = Activity{}
			case "tracked":
				runGit(t, f.repo, "add", "-f", "node_modules/package/index.js")
			case "git":
				write(t, filepath.Join(c.Path, "work/.git"), "fixture repository")
			case "session":
				write(t, filepath.Join(c.Path, ".claude/session.jsonl"), "fixture session")
			case "external link":
				if err := os.Symlink(filepath.Join(f.repo, "source.txt"), filepath.Join(c.Path, "linked-work")); err != nil {
					t.Fatal(err)
				}
			case "untracked patch":
				runGit(t, f.repo, "rm", "--cached", "patches/package.patch")
			case "missing lock":
				os.Remove(filepath.Join(f.repo, "bun.lock"))
			}
			if got := f.e.inspectCache(context.Background(), c, activity); got.Eligible {
				t.Fatal("unsafe dependency removal permitted", mode)
			}
			exists(t, c.Path)
		})
	}
}

func TestDisposableNodeModulesBindsReinstallInputs(t *testing.T) {
	for _, input := range []string{"package.json", "bun.lock", "patches/package.patch"} {
		t.Run(input, func(t *testing.T) {
			f, c := disposableNodeFixture(t)
			ctx := context.Background()
			c = f.e.inspectCache(ctx, c, Activity{Known: true})
			eligible(t, c)
			p, err := SavePlan(f.e.CacheConfig(), Plan{Version: 1, CreatedAt: f.e.Now(), ConfigDigest: ConfigDigest(f.e.CacheConfig()), Candidates: []Candidate{c}})
			if err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(f.repo, input), "changed regeneration input")
			if _, err = f.e.ApplyCache(ctx, p, c.ID, p.ID); err == nil {
				t.Fatal("changed regeneration inputs accepted")
			}
			exists(t, c.Path)
		})
	}
}

func TestDisposableNodeModulesDanglingInternalLinks(t *testing.T) {
	f, c := disposableNodeFixture(t)
	link := filepath.Join(c.Path, ".bin", "missing")
	os.MkdirAll(filepath.Dir(link), 0700)
	if err := os.Symlink("../removed-package/bin.js", link); err != nil {
		t.Fatal(err)
	}
	eligible(t, f.e.inspectCache(context.Background(), c, Activity{Known: true}))
	os.Remove(link)
	if err := os.Symlink("../../source.txt", link); err != nil {
		t.Fatal(err)
	}
	if got := f.e.inspectCache(context.Background(), c, Activity{Known: true}); got.Eligible {
		t.Fatal("escaping link accepted")
	}
}

func TestDependencyAuthorSettingsAreNotSessionStores(t *testing.T) {
	for _, name := range []string{"node_modules/pkg/.claude/settings.local.json", "node_modules/pkg/.codex/config.toml", "node_modules/pkg/.claude/skills/fixture/SKILL.md"} {
		if possibleCodingSession(name, true) || !possibleCodingSession(name, false) {
			t.Fatal("incorrect package settings classification", name)
		}
	}
	for _, name := range []string{"node_modules/pkg/.claude/session.jsonl", "node_modules/.codex/state_5.sqlite", "node_modules/pkg/.cursor/workspaceStorage/id/state.vscdb", "node_modules/pkg/.opencode/storage/session/id.json", "node_modules/pkg/.grok/history.json"} {
		if !possibleCodingSession(name, true) {
			t.Fatal("native session store not retained", name)
		}
	}
}

func TestPrivatePlansExcludeDependenciesButRetainActualPrivateFiles(t *testing.T) {
	f, _, p := privateFixture(t)
	for _, e := range p.Entries {
		if nodeDependencyPath(e.Path) {
			t.Fatal("dependency entered preservation plan")
		}
	}
	if len(p.Entries) != 2 {
		t.Fatal("actual private files not retained", p.Entries)
	}
	write(t, filepath.Join(f.repo, "local-data/node_modules/pkg/file.js"), "regenerable")
	if _, err := f.e.privateEntries(context.Background(), Candidate{Path: f.repo}); err == nil {
		t.Fatal("mixed directory would back up dependencies wholesale")
	}
	exists(t, filepath.Join(f.repo, "local-data/state.txt"))
	exists(t, filepath.Join(f.repo, "node_modules/pkg/credentials.js"))
}

func TestLegacyDependencyPrivateArchiveStillRestores(t *testing.T) {
	f, _, legacy := privateFixture(t)
	ctx := context.Background()
	name := "node_modules/pkg/credentials.js"
	entry, err := privateTree(ctx, filepath.Join(f.repo, name))
	if err != nil {
		t.Fatal(err)
	}
	entry.Path = name
	legacy.Entries = []PrivateEntry{entry}
	legacy.Bytes = entry.Bytes
	legacy.ID = privateID(legacy)
	data, err := json.Marshal(legacy)
	if err != nil || atomicPrivate(filepath.Join(f.e.privateDir(legacy.ID), "plan.json"), data) != nil {
		t.Fatal("legacy fixture plan unavailable", err)
	}
	dst := filepath.Join(f.e.privateDir(legacy.ID), "payload", name)
	if err = os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(f.repo, name), dst); err != nil {
		t.Fatal(err)
	}
	if err = f.e.saveArchive(PrivateArchive{Version: 1, PlanID: legacy.ID, Status: "verified_local_archive", Moved: []string{name}, Verified: []string{name}}); err != nil {
		t.Fatal(err)
	}
	if err = f.e.RestorePrivate(ctx, legacy.ID, legacy.ID); err != nil {
		t.Fatal("new policy broke legacy restore", err)
	}
	check, err := privateTree(ctx, filepath.Join(f.repo, name))
	if err != nil || check.SHA256 != entry.SHA256 {
		t.Fatal("legacy content did not restore", err)
	}
	exists(t, dst)
}
