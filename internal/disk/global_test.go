package disk

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

type globalFixture struct {
	e     *Engine
	host  globalHost
	use   globalUse
	cache string
}

func newGlobalFixture(t *testing.T) *globalFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &globalFixture{host: globalHost{home: filepath.Join(base, "home"), temp: filepath.Join(base, "temp"), sharedTemp: filepath.Join(base, "shared"), platform: "darwin"}, use: globalUse{activity: Activity{Known: true}, processesKnown: true, mounts: Activity{Known: true}}}
	for _, p := range []string{f.host.home, f.host.temp, f.host.sharedTemp} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	c := DefaultConfig()
	c.StateDir = filepath.Join(base, "state")
	c.Roots = []string{filepath.Join(f.host.home, "projects")}
	f.e = NewEngine(c)
	f.e.globalHost = func() (globalHost, error) { return f.host, nil }
	f.e.globalUse = func(context.Context) globalUse { return f.use }
	now := time.Now().Add(30 * 24 * time.Hour)
	f.e.Now = func() time.Time { return now }
	f.cache = globalCaches(f.host)["go-build"]
	write(t, filepath.Join(f.cache, "aa", "compiled"), "generated object")
	return f
}
func (f *globalFixture) scan(t *testing.T) Plan {
	t.Helper()
	p, err := f.e.GlobalScan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func globalCandidate(t *testing.T, p Plan, target string) Candidate {
	t.Helper()
	for _, c := range p.Candidates {
		if c.Path == target {
			return c
		}
	}
	t.Fatal("missing target", target)
	return Candidate{}
}
func installer(t *testing.T, home, name string) string {
	t.Helper()
	target := filepath.Join(home, "Downloads", name)
	data := "xar!package"
	if strings.HasSuffix(strings.ToLower(name), ".dmg") {
		data = "payload" + "koly" + strings.Repeat("\x00", 508)
	}
	write(t, target, data)
	return target
}

func TestGlobalCatalogReportAndNamespace(t *testing.T) {
	f := newGlobalFixture(t)
	for _, p := range globalCaches(f.host) {
		write(t, filepath.Join(p, "payload"), "cached")
	}
	for _, root := range globalXgoRoots(f.host) {
		write(t, filepath.Join(root, "go1.27.1_fixture", "build-cache", "object"), "compiled")
	}
	dmg := installer(t, f.host.home, "old.dmg")
	pkg := installer(t, f.host.home, "old.pkg")
	for _, p := range []string{filepath.Join(f.host.home, ".bun/install/global/app"), filepath.Join(f.host.home, "go/pkg/mod/module"), filepath.Join(f.host.home, "Library/pnpm/store/package"), filepath.Join(f.host.home, "Library/Application Support/App/state"), filepath.Join(f.host.home, "Downloads/nested/do-not-touch.dmg")} {
		write(t, p, "preserve")
	}
	p := f.scan(t)
	if len(p.Candidates) != 10 {
		t.Fatalf("catalog targets=%d", len(p.Candidates))
	}
	for _, c := range p.Candidates {
		eligible(t, c)
		exists(t, c.Path)
	}
	if _, err := LoadPlan(f.e.GlobalConfig(), p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPlan(f.e.Config, "latest"); err == nil {
		t.Fatal("overwrote checkout report")
	}
	if _, err := LoadPlan(f.e.CacheConfig(), "latest"); err == nil {
		t.Fatal("overwrote Kondo report")
	}
	exists(t, dmg)
	exists(t, pkg)
}

func TestGlobalExplicitApprovalOnlyAndScopeForgery(t *testing.T) {
	f := newGlobalFixture(t)
	p := f.scan(t)
	c := globalCandidate(t, p, f.cache)
	for _, approval := range []string{"", strings.Repeat("0", 64)} {
		if _, err := f.e.ApplyGlobal(context.Background(), p, c.ID, approval); err == nil {
			t.Fatal("unapproved deletion")
		}
		exists(t, filepath.Join(f.cache, "aa/compiled"))
	}
	for _, target := range []string{f.host.home, filepath.Join(f.host.home, "projects"), filepath.Join(f.host.home, ".bun/install/global"), filepath.Join(f.host.home, "Downloads/nested/other.dmg"), filepath.Join(f.host.sharedTemp, "xgo/go-instrument/not-go/build-cache")} {
		write(t, filepath.Join(target, "keep"), "private")
		bad := c
		bad.Path = target
		bad.ID = pathID(target)
		bad.Eligible = true
		for _, kind := range []string{"clone", "global-cache/unknown", "global-cache/go-build", "global-cache/xgo", "global-installer"} {
			bad.Kind = kind
			q := p
			q.Candidates = []Candidate{bad}
			q.ID = planID(q)
			if _, err := f.e.ApplyGlobal(context.Background(), q, bad.ID, q.ID); err == nil {
				t.Fatal("forged out-of-catalog target", target, kind)
			}
			exists(t, filepath.Join(target, "keep"))
		}
	}
}

func TestGlobalApplyPreservesDirectoriesSourceAndSharedHardlinks(t *testing.T) {
	f := newGlobalFixture(t)
	outside := filepath.Join(f.host.home, "source/keep")
	write(t, outside, "source")
	for _, name := range []string{"link-one", "link-two"} {
		if err := os.Link(outside, filepath.Join(f.cache, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("aa/compiled", filepath.Join(f.cache, "internal-link")); err != nil {
		t.Fatal(err)
	}
	p := f.scan(t)
	c := globalCandidate(t, p, f.cache)
	eligible(t, c)
	r, err := f.e.ApplyGlobal(context.Background(), p, c.ID, p.ID)
	if err != nil || r.Result != "global_files_removed" {
		t.Fatal(r, err)
	}
	exists(t, f.cache)
	exists(t, filepath.Join(f.cache, "aa"))
	exists(t, outside)
	for _, name := range []string{"aa/compiled", "link-one", "link-two", "internal-link"} {
		if _, err := os.Lstat(filepath.Join(f.cache, name)); !os.IsNotExist(err) {
			t.Fatal("approved file remained", name, err)
		}
	}
	file := filepath.Join(f.e.GlobalConfig().StateDir, "receipts", p.ID+"-"+c.ID+".json")
	var saved Receipt
	b, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(b, &saved) != nil || saved.Result != r.Result || saved.BeforeSpace.TotalBytes == 0 || saved.AfterSpace.TotalBytes == 0 {
		t.Fatal("missing real-space receipt", err)
	}
}

func TestGlobalScanSafetyBlockers(t *testing.T) {
	cases := []struct {
		name, reason string
		mutate       func(*globalFixture)
	}{
		{"unknown activity", "activity_check_unavailable", func(f *globalFixture) { f.use.activity.Known = false }},
		{"unknown producers", "producer_check_unavailable", func(f *globalFixture) { f.use.processesKnown = false }},
		{"producer without open cache file", "global_cache_producer_running", func(f *globalFixture) { f.use.processes = []string{"gopls"} }},
		{"open cache file", "global_target_in_use", func(f *globalFixture) { f.use.activity.Paths = []string{filepath.Join(f.cache, "aa/compiled")} }},
		{"private state", "global_tree_unverified_or_private", func(f *globalFixture) { write(t, filepath.Join(f.cache, ".env"), "private") }},
		{"nested Git", "global_tree_unverified_or_private", func(f *globalFixture) { write(t, filepath.Join(f.cache, ".GIT"), "private") }},
		{"special file", "global_tree_unverified_or_private", func(f *globalFixture) {
			if err := syscall.Mkfifo(filepath.Join(f.cache, "pipe"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"escaping symlink", "global_tree_unverified_or_private", func(f *globalFixture) {
			outside := filepath.Join(f.host.home, "private")
			write(t, outside, "keep")
			if err := os.Symlink(outside, filepath.Join(f.cache, "link")); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlinked cache root", "symlinked_global_target", func(f *globalFixture) {
			if err := os.Rename(f.cache, f.cache+"-saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.cache+"-saved", f.cache); err != nil {
				t.Fatal(err)
			}
		}},
		{"state overlaps cache", "outside_global_catalog", func(f *globalFixture) { f.e.Config.StateDir = filepath.Join(f.cache, "private-state") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGlobalFixture(t)
			tc.mutate(f)
			p := f.scan(t)
			c := globalCandidate(t, p, f.cache)
			if c.Eligible || !slices.Contains(c.Blockers, tc.reason) {
				t.Fatal(c.Blockers)
			}
			exists(t, f.cache)
		})
	}
}

func TestGlobalRevalidationRejectsChangesAndExpiredApproval(t *testing.T) {
	for _, name := range []string{"rewrite same size restored mtime", "new file", "replacement root", "expired", "future", "tampered", "new producer", "new mounted image"} {
		t.Run(name, func(t *testing.T) {
			f := newGlobalFixture(t)
			target := f.cache
			if name == "new mounted image" {
				target = installer(t, f.host.home, "old.dmg")
			}
			p := f.scan(t)
			c := globalCandidate(t, p, target)
			eligible(t, c)
			switch name {
			case "rewrite same size restored mtime":
				q := filepath.Join(f.cache, "aa/compiled")
				s, _ := os.Stat(q)
				write(t, q, "different object")
				if err := os.Chtimes(q, s.ModTime(), s.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "new file":
				write(t, filepath.Join(f.cache, "new"), "keep")
			case "replacement root":
				if err := os.Rename(f.cache, f.cache+"-saved"); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(f.cache, "aa/compiled"), "new private data")
			case "expired":
				f.e.Now = func() time.Time { return p.CreatedAt.Add(25 * time.Hour) }
			case "future":
				p.CreatedAt = p.CreatedAt.Add(2 * time.Minute)
				p.ID = planID(p)
			case "tampered":
				p.Candidates[0].SizeBytes++
			case "new producer":
				f.use.processes = []string{"go"}
			case "new mounted image":
				f.use.mounts.Paths = []string{target}
			}
			if _, err := f.e.ApplyGlobal(context.Background(), p, c.ID, p.ID); err == nil {
				t.Fatal("accepted changed/expired target")
			}
			exists(t, target)
		})
	}
}

func TestGlobalManifestKeepsConcurrentNewAndRewrittenFiles(t *testing.T) {
	for _, mutation := range []string{"new", "rewrite", "directory swap", "root swap", "producer starts", "cancelled"} {
		t.Run(mutation, func(t *testing.T) {
			f := newGlobalFixture(t)
			p := f.scan(t)
			c := globalCandidate(t, p, f.cache)
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			outside := filepath.Join(f.host.home, "outside")
			write(t, filepath.Join(outside, "compiled"), "private")
			f.e.globalUse = func(context.Context) globalUse {
				calls++
				if calls == 3 {
					switch mutation {
					case "new":
						write(t, filepath.Join(f.cache, "new-after-approval"), "preserve")
					case "rewrite":
						write(t, filepath.Join(f.cache, "aa/compiled"), "newly generated")
					case "producer starts":
						f.use.processes = []string{"go"}
					case "cancelled":
						cancel()
					case "root swap":
						if err := os.Rename(f.cache, f.cache+"-saved"); err != nil {
							t.Fatal(err)
						}
						write(t, filepath.Join(f.cache, "aa/compiled"), "new private data")
					case "directory swap":
						if err := os.Rename(filepath.Join(f.cache, "aa"), filepath.Join(f.host.home, "saved")); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(outside, filepath.Join(f.cache, "aa")); err != nil {
							t.Fatal(err)
						}
					}
				}
				return f.use
			}
			r, err := f.e.ApplyGlobal(ctx, p, c.ID, p.ID)
			if mutation == "new" {
				if err != nil {
					t.Fatal(err)
				}
				exists(t, filepath.Join(f.cache, "new-after-approval"))
			} else if err == nil || r.Result != "global_removal_partial" {
				t.Fatal("changed file not retained", r, err)
			}
			exists(t, filepath.Join(outside, "compiled"))
			exists(t, f.cache)
			if mutation == "root swap" {
				exists(t, filepath.Join(f.cache+"-saved", "aa/compiled"))
				exists(t, filepath.Join(f.cache, "aa/compiled"))
			}
		})
	}
}

func TestGlobalInstallersAgeFormatsMountsAndApproval(t *testing.T) {
	for _, name := range []string{"valid dmg", "valid pkg", "recent", "old mtime new ctime", "mounted alias", "unknown mounts", "unknown format", "open installer", "symlink"} {
		t.Run(name, func(t *testing.T) {
			f := newGlobalFixture(t)
			extension := ".dmg"
			if name == "valid pkg" {
				extension = ".pkg"
			}
			target := installer(t, f.host.home, "installer"+extension)
			switch name {
			case "recent":
				now := time.Now()
				f.e.Now = func() time.Time { return now }
			case "old mtime new ctime":
				old(t, target)
				now := time.Now()
				f.e.Now = func() time.Time { return now }
			case "mounted alias":
				alias := filepath.Join(f.host.home, "mounted.dmg")
				if err := os.Link(target, alias); err != nil {
					t.Fatal(err)
				}
				f.use.mounts.Paths = []string{alias}
			case "unknown mounts":
				f.use.mounts.Known = false
			case "unknown format":
				write(t, target, "not a disk image")
			case "open installer":
				f.use.activity.Paths = []string{target}
			case "symlink":
				if err := os.Rename(target, target+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target+".saved", target); err != nil {
					t.Fatal(err)
				}
			}
			p := f.scan(t)
			c := globalCandidate(t, p, target)
			if name == "valid dmg" || name == "valid pkg" {
				eligible(t, c)
				if _, err := f.e.ApplyGlobal(context.Background(), p, c.ID, p.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(target); !os.IsNotExist(err) {
					t.Fatal("installer not removed")
				}
				exists(t, filepath.Dir(target))
			} else {
				if c.Eligible {
					t.Fatal("unsafe installer accepted", name)
				}
				exists(t, target)
			}
		})
	}
}

func TestGlobalMountInventoryFailsClosed(t *testing.T) {
	good := `<?xml version="1.0"?><plist version="1.0"><dict><key>images</key><array><dict><key>image-path</key><string>/fixture/mounted.dmg</string></dict></array></dict></plist>`
	a, err := parseGlobalMounts([]byte(good))
	if err != nil || !a.Known || len(a.Paths) != 1 {
		t.Fatal(a, err)
	}
	for _, bad := range []string{"", good + "garbage", good + "<plist/>", `<plist><dict/></plist>`, strings.ReplaceAll(good, "/fixture/mounted.dmg", "relative.dmg"), strings.ReplaceAll(good, "<string>/fixture/mounted.dmg</string>", "<false/>"), good[:len(good)-5], strings.ReplaceAll(good, "<key>images</key>", "<key>images</key><array/><key>images</key>")} {
		if a, err := parseGlobalMounts([]byte(bad)); err == nil || a.Known {
			t.Fatal("unsafe mounted inventory accepted", bad)
		}
	}
	empty := `<plist><dict><key>images</key><array/></dict></plist>`
	if a, err := parseGlobalMounts([]byte(empty)); err != nil || !a.Known {
		t.Fatal(a, err)
	}
}

func TestGlobalUnsupportedHostAndUnknownOwnership(t *testing.T) {
	f := newGlobalFixture(t)
	f.host.platform = "linux"
	if _, err := f.e.GlobalScan(context.Background()); err == nil {
		t.Fatal("unsupported production host accepted")
	}
	s, _ := os.Stat(f.cache)
	native := *(s.Sys().(*syscall.Stat_t))
	native.Uid++
	if globalOwned(deviceInfo{s, &native}) || globalOwned(nil) {
		t.Fatal("unknown/foreign owner accepted")
	}
}
