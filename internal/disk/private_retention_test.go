package disk

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func legacyRetentionFixture(t *testing.T, compiled ...bool) (fixture, PrivatePlan) {
	t.Helper()
	f := newFixture(t)
	write(t, filepath.Join(f.repo, ".git/info/exclude"), ".env\nlocal-data/\ndist/\n")
	write(t, filepath.Join(f.repo, ".env"), "fixture-private-value\n")
	write(t, filepath.Join(f.repo, "local-data/work.txt"), "fixture unique work\n")
	write(t, filepath.Join(f.repo, "dist/index.html"), "fixture generated html\n")
	namesToArchive := []string{".env", "dist", "local-data"}
	if len(compiled) > 0 && compiled[0] {
		b := make([]byte, 256)
		binary.LittleEndian.PutUint32(b[:4], 0xfeedfacf)
		binary.LittleEndian.PutUint32(b[12:16], 2)
		if err := os.MkdirAll(filepath.Join(f.repo, "tmp"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.repo, "tmp/main"), b, 0700); err != nil {
			t.Fatal(err)
		}
		namesToArchive = append(namesToArchive, "tmp")
	}
	return legacyRetentionArchive(t, f, namesToArchive)
}

func legacyRetentionArchive(t *testing.T, f fixture, namesToArchive []string) (fixture, PrivatePlan) {
	t.Helper()
	p := plan(t, f)
	a := PrivatePlan{Version: 1, CreatedAt: time.Now().UTC(), ConfigDigest: ConfigDigest(f.e.Config), SourcePlan: p.ID, Candidate: p.Candidates[0]}
	for _, name := range namesToArchive {
		e, err := privateTree(context.Background(), filepath.Join(f.repo, name))
		if err != nil {
			t.Fatal(err)
		}
		e.Path = name
		a.Entries = append(a.Entries, e)
		a.Bytes += e.Bytes
	}
	a.ID = privateID(a)
	if err := os.MkdirAll(filepath.Join(f.e.privateDir(a.ID), "payload"), 0700); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range a.Entries {
		names = append(names, entry.Path)
		if err := os.MkdirAll(filepath.Dir(filepath.Join(f.e.privateDir(a.ID), "payload", entry.Path)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(f.repo, entry.Path), filepath.Join(f.e.privateDir(a.ID), "payload", entry.Path)); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.MarshalIndent(a, "", "  ")
	if err := atomicPrivate(filepath.Join(f.e.privateDir(a.ID), "plan.json"), b); err != nil {
		t.Fatal(err)
	}
	if err := f.e.saveArchive(PrivateArchive{Version: 1, PlanID: a.ID, Status: "verified_local_archive", Moved: names, Verified: names}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.linkPrivate(a); err != nil {
		t.Fatal(err)
	}
	return f, a
}

func TestPrivatePruneRetainsSensitiveEntryRootNames(t *testing.T) {
	for _, name := range []string{"dist/.env", ".venv/.env", "dist/credentials", "dist/private.key", "dist/state.sqlite"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			write(t, filepath.Join(f.repo, name), "fixture private work")
			f, a := legacyRetentionArchive(t, f, []string{name})
			if _, err := f.e.PlanPrivatePrune(context.Background(), a.ID, []string{name}); err == nil {
				t.Fatal("sensitive archive entry root accepted")
			}
			if _, err := f.e.PrunePrivate(context.Background(), a.ID, []string{name}, strings.Repeat("0", 64)); err == nil {
				t.Fatal("sensitive archive entry removed")
			}
			exists(t, filepath.Join(f.e.privateDir(a.ID), "payload", name))
			if _, err := discardedPrivate(a, PrivateArchive{Discarded: []PrivateDiscard{{Entry: a.Entries[0], Policy: "reviewed_regenerable"}}}); err == nil {
				t.Fatal("sensitive discard receipt accepted")
			}
		})
	}
}

func TestPrivatePruneRetainsEmbeddedSecrets(t *testing.T) {
	for _, compiled := range []bool{false, true} {
		for _, offset := range []int{256, (64 << 10) - 8} {
			f := newFixture(t)
			name := "dist/index.html"
			data := make([]byte, offset)
			if compiled {
				name = "tmp/main"
				binary.LittleEndian.PutUint32(data[:4], 0xfeedfacf)
				binary.LittleEndian.PutUint32(data[12:16], 2)
			}
			data = append(data, []byte("sk-or-v1-"+strings.Repeat("x", 24))...)
			write(t, filepath.Join(f.repo, name), string(data))
			if err := os.Chmod(filepath.Join(f.repo, name), 0700); err != nil {
				t.Fatal(err)
			}
			f, a := legacyRetentionArchive(t, f, []string{name})
			if _, err := f.e.PlanPrivatePrune(context.Background(), a.ID, []string{name}); err == nil {
				t.Fatal("secret-bearing generated/compiled output accepted")
			}
			exists(t, filepath.Join(f.e.privateDir(a.ID), "payload", name))
		}
	}
}

func TestLegacyInstalledPackageDiscardCompatibility(t *testing.T) {
	for _, name := range []string{"node_modules/bottleneck/.env", "node_modules/@scope/auth/credentials.py", "node_modules/.bun/auth@1/node_modules/@scope/auth/credentials.py", ".venv/lib/python3.11/site-packages/botocore/cacert.pem"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			write(t, filepath.Join(f.repo, name), "fixture installed dependency")
			write(t, filepath.Join(f.repo, "local-data/work.txt"), "fixture unique work")
			f, a := legacyRetentionArchive(t, f, []string{name, "local-data"})
			ctx := context.Background()
			if _, err := f.e.PlanPrivatePrune(ctx, a.ID, []string{name}); err == nil {
				t.Fatal("legacy compatibility approved a new sensitive-name deletion")
			}
			r, err := f.e.PrivateReceipt(a.ID)
			if err != nil {
				t.Fatal(err)
			}
			r.Discarded = []PrivateDiscard{{Entry: a.Entries[0], Policy: "reviewed_regenerable"}}
			if err := f.e.saveArchive(r); err != nil {
				t.Fatal(err)
			}
			if err := f.e.RestorePrivate(ctx, a.ID, a.ID); err == nil {
				t.Fatal("legacy receipt suppressed a remaining package payload")
			}
			if err := os.RemoveAll(filepath.Join(f.e.privateDir(a.ID), "payload", name)); err != nil {
				t.Fatal(err)
			}
			if err := f.e.RestorePrivate(ctx, a.ID, a.ID); err != nil {
				t.Fatal("completed legacy dependency discard blocked unique work restore", err)
			}
			exists(t, filepath.Join(f.repo, "local-data/work.txt"))
		})
	}
	for _, name := range []string{".venv/.env", "node_modules/.env", "dist/.env", ".venv/lib/python3.11/site-packages/.env", ".env/project/node_modules/pkg/credentials.py", ".claude/node_modules/pkg/credentials.py", "node_modules/@scope/.env", "node_modules/pkg/.claude/sessions/session.jsonl", ".venv/lib/python3.11/site-packages/pkg/credentials.py/child", "node_modules/pkg/credentials/data.key", "node_modules/pkg/ordinary-file.js"} {
		if legacyInstalledPackageEntry(name) {
			t.Fatalf("private/session path qualified for legacy exception: %s", name)
		}
	}
}

func TestPrivatePruneCompiledOnlyRequiresWholeEntryAndExactApproval(t *testing.T) {
	f, a := legacyRetentionFixture(t, true)
	ctx := context.Background()
	p, err := f.e.PlanPrivatePrune(ctx, a.ID, []string{"tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Entries[0].Policy != "reviewed_compiled_only" {
		t.Fatal("compiled recognition not explicit")
	}
	if _, err = f.e.PrunePrivate(ctx, a.ID, []string{"tmp"}, p.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.e.RestorePrivate(ctx, a.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	exists(t, filepath.Join(f.repo, "local-data/work.txt"))
	f, a = legacyRetentionFixture(t, true)
	write(t, filepath.Join(f.e.privateDir(a.ID), "payload/tmp/unfinished.txt"), "fixture unique work")
	if _, err = f.e.PlanPrivatePrune(ctx, a.ID, []string{"tmp"}); err == nil {
		t.Fatal("mixed source and executable entry pruned")
	}
}

func TestPrivateCreationNeverArchivesRegenerableOutput(t *testing.T) {
	for _, name := range []string{"dist/index.html", "apps/site/dist/index.js", ".mypy_cache/3.11/cache.db", ".pytest_cache/v/cache/nodeids", ".ruff_cache/cache", ".xgo/gen/overlay/source.go", "mixed/dist/index.html"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			root := strings.Split(name, "/")[0]
			write(t, filepath.Join(f.repo, ".git/info/exclude"), root+"/\n")
			write(t, filepath.Join(f.repo, name), "fixture generated output\n")
			p := plan(t, f)
			if _, err := f.e.PlanPrivate(context.Background(), p, p.Candidates[0].ID); err == nil {
				t.Fatal("generated output entered private preservation")
			}
			exists(t, filepath.Join(f.repo, name))
		})
	}
}

func TestPrivateCreationRequiresSeparateCompiledArtifactDecision(t *testing.T) {
	f, file := binaryFixture(t)
	p := plan(t, f)
	if _, err := f.e.PlanPrivate(context.Background(), p, p.Candidates[0].ID); err == nil {
		t.Fatal("compiled executable silently entered private recovery")
	}
	exists(t, file)
}

func TestPrivatePrunePreservesOriginalPlanAndRestoresUniqueData(t *testing.T) {
	f, a := legacyRetentionFixture(t)
	ctx := context.Background()
	before, err := os.ReadFile(filepath.Join(f.e.privateDir(a.ID), "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.e.PlanPrivatePrune(ctx, a.ID, []string{"dist"})
	if err != nil {
		t.Fatal(err)
	}
	exists(t, filepath.Join(f.e.privateDir(a.ID), "payload/dist/index.html"))
	if _, err = f.e.PrunePrivate(ctx, a.ID, []string{"dist"}, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong approval accepted")
	}
	if _, err = f.e.PrunePrivate(ctx, a.ID, []string{"dist"}, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(f.e.privateDir(a.ID), "payload/dist")); !os.IsNotExist(err) {
		t.Fatal("generated output retained", err)
	}
	after, _ := os.ReadFile(filepath.Join(f.e.privateDir(a.ID), "plan.json"))
	if string(before) != string(after) {
		t.Fatal("immutable plan rewritten")
	}
	c := a.Candidate
	if err = f.e.verifyPrivateArchives(ctx, &c); err != nil {
		t.Fatal("remaining archive no longer verifies", err)
	}
	if err = f.e.RestorePrivate(ctx, a.ID, a.ID); err != nil {
		t.Fatal("unique private work cannot restore", err)
	}
	for _, name := range []string{".env", "local-data/work.txt"} {
		exists(t, filepath.Join(f.repo, name))
	}
	if _, err = os.Stat(filepath.Join(f.repo, "dist")); !os.IsNotExist(err) {
		t.Fatal("discarded build output unexpectedly restored")
	}
	if err = f.e.RestorePrivate(ctx, a.ID, a.ID); err != nil {
		t.Fatal("idempotent restore failed", err)
	}
}

func TestPrivatePruneFailsClosed(t *testing.T) {
	for _, kind := range []string{"private work", "duplicate entry", "changed bytes", "unknown activity", "active archive", "symlink", "session", "secret", "tampered plan", "forged discard"} {
		t.Run(kind, func(t *testing.T) {
			f, a := legacyRetentionFixture(t)
			ctx := context.Background()
			names := []string{"dist"}
			root := filepath.Join(f.e.privateDir(a.ID), "payload/dist")
			switch kind {
			case "private work":
				names = []string{".env"}
			case "duplicate entry":
				names = []string{"dist", "dist"}
			case "changed bytes":
				write(t, filepath.Join(root, "index.html"), "changed")
			case "unknown activity":
				f.e.Activity = func(context.Context) Activity { return Activity{} }
			case "active archive":
				f.e.Activity = func(context.Context) Activity { return Activity{Known: true, Paths: []string{root}} }
			case "symlink":
				if err := os.Symlink(f.repo, filepath.Join(root, "outside")); err != nil {
					t.Fatal(err)
				}
			case "session":
				write(t, filepath.Join(root, ".claude/projects/transcript.jsonl"), "fixture session")
			case "secret":
				write(t, filepath.Join(root, ".env"), "fixture private data")
			case "tampered plan":
				write(t, filepath.Join(f.e.privateDir(a.ID), "plan.json"), "{}")
			case "forged discard":
				r, err := f.e.PrivateReceipt(a.ID)
				if err != nil {
					t.Fatal(err)
				}
				r.Discarded = []PrivateDiscard{{Entry: a.Entries[0], Policy: "reviewed_regenerable"}}
				if err = f.e.saveArchive(r); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.e.PlanPrivatePrune(ctx, a.ID, names); err == nil {
				t.Fatal("unsafe retention plan accepted")
			}
			exists(t, filepath.Join(root, "index.html"))
			exists(t, filepath.Join(f.e.privateDir(a.ID), "payload/.env"))
		})
	}
}

func TestPrivatePruneRevalidatesAfterReview(t *testing.T) {
	f, a := legacyRetentionFixture(t)
	ctx := context.Background()
	p, err := f.e.PlanPrivatePrune(ctx, a.ID, []string{"dist"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.e.privateDir(a.ID), "payload/dist/new-work.txt"), "new work")
	if _, err = f.e.PrunePrivate(ctx, a.ID, []string{"dist"}, p.ID); err == nil {
		t.Fatal("changed archive removed")
	}
	exists(t, filepath.Join(f.e.privateDir(a.ID), "payload/dist/new-work.txt"))
}

func TestPrivateDiscardCannotSuppressRemainingPayload(t *testing.T) {
	for _, kind := range []string{"compiled receipt", "xgo receipt", "interrupted discard", "dangling symlink", "symlink ancestor"} {
		t.Run(kind, func(t *testing.T) {
			f, a := legacyRetentionFixture(t)
			name, policy := "local-data", "reviewed_compiled_only"
			if kind == "xgo receipt" {
				f = newFixture(t)
				write(t, filepath.Join(f.repo, ".xgo/gen/output.go"), "fixture generated output")
				write(t, filepath.Join(f.repo, ".xgo/unique-work.txt"), "fixture unique work")
				f, a = legacyRetentionArchive(t, f, []string{".xgo"})
				name, policy = ".xgo", "verified_xgo_gen_only"
			} else if kind != "compiled receipt" {
				name, policy = "dist", "reviewed_regenerable"
				root := filepath.Join(f.e.privateDir(a.ID), "payload", name)
				if kind == "interrupted discard" {
					write(t, filepath.Join(root, "remnant.txt"), "fixture remnant")
					if err := os.Remove(filepath.Join(root, "index.html")); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.RemoveAll(root); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(f.root, "missing"), root); err != nil {
						t.Fatal(err)
					}
					if kind == "symlink ancestor" {
						name = "dist/index.html"
						a.Entries[1].Path = name
						a.ID = privateID(a)
						// The lower-level validator also rejects an absent leaf
						// under a dangling ancestor without following that link.
						if err := os.MkdirAll(filepath.Dir(f.e.privateDir(a.ID)), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.MkdirAll(filepath.Join(f.e.privateDir(a.ID), "payload"), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(filepath.Join(f.root, "missing"), filepath.Join(f.e.privateDir(a.ID), "payload/dist")); err != nil {
							t.Fatal(err)
						}
						r := PrivateArchive{Discarded: []PrivateDiscard{{Entry: a.Entries[1], Policy: policy}}}
						if _, err := f.e.validatedPrivateDiscards(context.Background(), a, r); err == nil {
							t.Fatal("dangling ancestor treated as completed discard")
						}
						return
					}
				}
			}
			r, err := f.e.PrivateReceipt(a.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range a.Entries {
				if entry.Path == name {
					r.Discarded = []PrivateDiscard{{Entry: entry, Policy: policy}}
				}
			}
			if err := f.e.saveArchive(r); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if _, _, err := f.e.retentionArchive(ctx, a.ID); err == nil {
				t.Fatal("remaining payload accepted by retention loader")
			}
			c := a.Candidate
			if err := f.e.verifyPrivateArchives(ctx, &c); err == nil {
				t.Fatal("remaining payload silently skipped by verification")
			}
			if err := f.e.RestorePrivate(ctx, a.ID, a.ID); err == nil {
				t.Fatal("remaining payload silently skipped by restore")
			}
			if kind == "compiled receipt" {
				exists(t, filepath.Join(f.e.privateDir(a.ID), "payload/local-data/work.txt"))
			}
			if kind == "xgo receipt" {
				exists(t, filepath.Join(f.e.privateDir(a.ID), "payload/.xgo/unique-work.txt"))
			}
		})
	}
}

func TestPrivatePruneApprovalIsBoundToStateDirectory(t *testing.T) {
	f, a := legacyRetentionFixture(t)
	ctx := context.Background()
	p, err := f.e.PlanPrivatePrune(ctx, a.ID, []string{"dist"})
	if err != nil {
		t.Fatal(err)
	}
	c := f.e.Config
	c.StateDir = filepath.Join(filepath.Dir(f.root), "other-state")
	if err = EnsureState(c); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(c)
	e.Activity = func(context.Context) Activity { return Activity{Known: true} }
	if err = os.MkdirAll(filepath.Join(c.StateDir, "private-archives"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = copyPrivateTree(ctx, f.e.privateDir(a.ID), e.privateDir(a.ID)); err != nil {
		t.Fatal(err)
	}
	other, err := e.PlanPrivatePrune(ctx, a.ID, []string{"dist"})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == other.ID {
		t.Fatal("approval applies to a different recovery root")
	}
	if _, err = e.PrunePrivate(ctx, a.ID, []string{"dist"}, p.ID); err == nil {
		t.Fatal("cross-root approval accepted")
	}
	exists(t, filepath.Join(e.privateDir(a.ID), "payload/dist/index.html"))
}
