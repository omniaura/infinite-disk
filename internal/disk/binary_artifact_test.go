package disk

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func binaryFixture(t *testing.T) (fixture, string) {
	t.Helper()
	f := newFixture(t)
	write(t, filepath.Join(f.repo, ".gitignore"), "worker\noutput/\n")
	p := filepath.Join(f.repo, "worker")
	b := make([]byte, 256)
	binary.LittleEndian.PutUint32(b[:4], 0xfeedfacf)
	binary.LittleEndian.PutUint32(b[12:16], 2)
	if err := os.WriteFile(p, b, 0700); err != nil {
		t.Fatal(err)
	}
	old(t, p)
	return f, p
}
func binaryPlan(t *testing.T, f fixture, action string) Plan {
	t.Helper()
	p, err := f.e.PlanBinaryArtifact(context.Background(), f.repo, "worker", action)
	if err != nil {
		t.Fatal(err)
	}
	eligible(t, p.Candidates[0])
	return p
}
func TestBinaryArtifactDispositionApprovalAndRestore(t *testing.T) {
	for _, choice := range []string{"keep", "archive", "discard"} {
		t.Run(choice, func(t *testing.T) {
			f, file := binaryFixture(t)
			p := binaryPlan(t, f, choice)
			c := p.Candidates[0]
			if _, err := f.e.ApplyCache(context.Background(), p, c.ID, ""); err == nil {
				t.Fatal("missing approval accepted")
			}
			exists(t, file)
			original := runGit(t, f.repo, "rev-parse", "HEAD")
			r, err := f.e.ApplyCache(context.Background(), p, c.ID, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			expected := map[string]string{"keep": "artifact_kept", "archive": "artifact_archived", "discard": "artifact_discarded"}[choice]
			if r.Result != expected {
				t.Fatal(r.Result)
			}
			if choice == "keep" {
				exists(t, file)
			} else if _, err = os.Lstat(file); !os.IsNotExist(err) {
				t.Fatal("artifact remains", err)
			}
			if choice == "archive" {
				if r.RetainedBytes != 256 || r.ArtifactArchivePath == "" {
					t.Fatal("archive bytes not disclosed")
				}
				if err = os.Remove(filepath.Join(f.e.CacheConfig().StateDir, "plans", p.ID+".json")); err != nil {
					t.Fatal(err)
				}
				loaded, err := f.e.LoadArtifactRestorePlan(p.ID, c.ID)
				if err != nil || loaded.ID != p.ID {
					t.Fatal("archive lost plan after report rotation", err)
				}
				if err = f.e.RestoreBinaryArtifact(context.Background(), p, c.ID, ""); err == nil {
					t.Fatal("restore missing consent")
				}
				if err = f.e.RestoreBinaryArtifact(context.Background(), p, c.ID, p.ID); err != nil {
					t.Fatal(err)
				}
				exists(t, file)
				exists(t, r.ArtifactArchivePath)
				if err = f.e.RestoreBinaryArtifact(context.Background(), p, c.ID, p.ID); err == nil {
					t.Fatal("restore overwrites")
				}
				s, _ := os.Stat(file)
				if s.Mode().Perm() != 0700 {
					t.Fatal("permissions changed")
				}
			}
			if runGit(t, f.repo, "rev-parse", "HEAD") != original {
				t.Fatal("source changed")
			}
			exists(t, filepath.Join(f.repo, "source.txt"))
			if _, err = f.e.ApplyCache(context.Background(), p, c.ID, p.ID); err == nil {
				t.Fatal("action replayed")
			}
		})
	}
}

func TestBinaryArtifactArchiveTamperAndBoundedSensitiveRead(t *testing.T) {
	for _, what := range []string{"bytes", "mode"} {
		t.Run(what, func(t *testing.T) {
			f, file := binaryFixture(t)
			p := binaryPlan(t, f, "archive")
			c := p.Candidates[0]
			r, err := f.e.ApplyCache(context.Background(), p, c.ID, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if what == "bytes" {
				os.WriteFile(r.ArtifactArchivePath, []byte("tampered"), 0700)
			} else {
				os.Chmod(r.ArtifactArchivePath, 0600)
			}
			if err = f.e.RestoreBinaryArtifact(context.Background(), p, c.ID, p.ID); err == nil {
				t.Fatal("tampered archive restored")
			}
			if _, err = os.Lstat(file); !os.IsNotExist(err) {
				t.Fatal("failed restore created original")
			}
		})
	}
	f, file := binaryFixture(t)
	b := make([]byte, 65536-2)
	copy(b, []byte{0xcf, 0xfa, 0xed, 0xfe})
	binary.LittleEndian.PutUint32(b[12:16], 2)
	b = append(b, []byte("ghp_"+strings.Repeat("a", 30))...)
	os.WriteFile(file, b, 0700)
	old(t, file)
	p, err := f.e.PlanBinaryArtifact(context.Background(), f.repo, "worker", "discard")
	if err != nil {
		t.Fatal(err)
	}
	if p.Candidates[0].Eligible {
		t.Fatal("sensitive value crossing read boundary accepted")
	}
	fd, err := os.OpenFile(file, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fd.Truncate(maxBinaryArtifact + 1)
	fd.Close()
	old(t, file)
	if _, err = binaryArtifactFormat(file); err == nil {
		t.Fatal("oversized artifact accepted")
	}
}

func TestKondoSelectedSubsetUsesCatalogWithoutWaivingChecks(t *testing.T) {
	f, _ := cacheFixture(t)
	base := t.TempDir()
	script := filepath.Join(base, "kondo")
	write(t, script, "#!/bin/sh\nprintf '%s Node project\\n  └─ node_modules (1.0MiB)\\nProjects cleaned: 0/1, Bytes deleted: 0.0B / 1.0MiB\\n' \"$INFINITE_DISK_FIXTURE_PROJECT\"\n")
	os.Chmod(script, 0700)
	t.Setenv("PATH", base+string(filepath.ListSeparator)+os.Getenv("PATH"))
	t.Setenv("INFINITE_DISK_FIXTURE_PROJECT", f.repo)
	dist := filepath.Join(f.repo, "node_modules", "large-package", "dist")
	write(t, filepath.Join(dist, "file.js"), "rebuildable")
	write(t, filepath.Join(f.repo, "node_modules", "other-package", ".env"), "private retained elsewhere")
	filepath.Walk(dist, func(p string, s os.FileInfo, err error) error {
		if err == nil {
			old(t, p)
		}
		return err
	})
	for _, action := range []string{"keep", "archive"} {
		if _, err := f.e.PlanBinaryArtifact(context.Background(), f.repo, "node_modules/large-package/dist", action); err == nil {
			t.Fatal("directory implicitly discarded")
		}
	}
	p, err := f.e.PlanBinaryArtifact(context.Background(), f.repo, "node_modules/large-package/dist", "discard")
	if err != nil {
		t.Fatal(err)
	}
	eligible(t, p.Candidates[0])
	if p.Candidates[0].Kind != "kondo-cache" {
		t.Fatal("directory catalog not reused")
	}
	if _, err = f.e.ApplyCache(context.Background(), p, p.Candidates[0].ID, p.ID); err != nil {
		t.Fatal(err)
	}
	exists(t, filepath.Join(f.repo, "node_modules", "other-package", ".env"))
	exists(t, filepath.Join(f.repo, "source.txt"))
	write(t, filepath.Join(f.repo, "uncataloged", "file"), "not a Kondo artifact")
	old(t, filepath.Join(f.repo, "uncataloged", "file"))
	old(t, filepath.Join(f.repo, "uncataloged"))
	if _, err = f.e.PlanBinaryArtifact(context.Background(), f.repo, "uncataloged", "discard"); err == nil {
		t.Fatal("uncataloged arbitrary directory accepted")
	}
	write(t, filepath.Join(f.repo, "node_modules", "session-package", ".claude", "session.jsonl"), "retained coding session")
	filepath.Walk(filepath.Join(f.repo, "node_modules", "session-package"), func(p string, s os.FileInfo, err error) error {
		if err == nil {
			old(t, p)
		}
		return err
	})
	p, err = f.e.PlanBinaryArtifact(context.Background(), f.repo, "node_modules/session-package", "discard")
	if err != nil {
		t.Fatal(err)
	}
	if p.Candidates[0].Eligible {
		t.Fatal("session namespace treated as cache")
	}
}
func TestBinaryArtifactBlockersAndMutation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(fixture, string)
	}{
		{"tracked", func(f fixture, p string) { runGit(t, f.repo, "add", "-f", "worker") }},
		{"unignored", func(f fixture, p string) { write(t, filepath.Join(f.repo, ".gitignore"), "") }},
		{"unknown", func(f fixture, p string) {
			write(t, p, strings.Repeat("not an executable ", 20))
			os.Chmod(p, 0700)
			old(t, p)
		}},
		{"library", func(f fixture, p string) {
			b, _ := os.ReadFile(p)
			binary.LittleEndian.PutUint32(b[12:16], 6)
			os.WriteFile(p, b, 0700)
			old(t, p)
		}},
		{"symlink", func(f fixture, p string) { os.Remove(p); os.Symlink(filepath.Join(f.repo, "source.txt"), p) }},
		{"hardlink", func(f fixture, p string) { os.Link(p, filepath.Join(f.repo, "other-worker")) }},
		{"private bytes", func(f fixture, p string) {
			fd, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
			fd.WriteString("\nghp_" + strings.Repeat("a", 30))
			fd.Close()
			old(t, p)
		}},
		{"recent", func(f fixture, p string) { os.Chtimes(p, f.e.Now(), f.e.Now()) }},
		{"busy", func(f fixture, p string) {
			f.e.Activity = func(context.Context) Activity { return Activity{Known: true, Paths: []string{p}} }
		}},
		{"unknown activity", func(f fixture, p string) { f.e.Activity = func(context.Context) Activity { return Activity{} } }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, file := binaryFixture(t)
			tc.mutate(f, file)
			p, err := f.e.PlanBinaryArtifact(context.Background(), f.repo, "worker", "discard")
			if err != nil {
				t.Fatal(err)
			}
			if p.Candidates[0].Eligible {
				t.Fatal("unsafe artifact eligible")
			}
			if _, err = f.e.ApplyCache(context.Background(), p, p.Candidates[0].ID, p.ID); err == nil {
				t.Fatal("unsafe artifact removed")
			}
			exists(t, file)
		})
	}
	for _, kind := range []string{"content", "replacement", "tracked", "disposition", "cancel"} {
		t.Run("changed_"+kind, func(t *testing.T) {
			f, file := binaryFixture(t)
			p := binaryPlan(t, f, "discard")
			ctx := context.Background()
			switch kind {
			case "content":
				s, _ := os.Stat(file)
				b, _ := os.ReadFile(file)
				b[len(b)-1] = 1
				os.WriteFile(file, b, 0700)
				os.Chtimes(file, s.ModTime(), s.ModTime())
			case "replacement":
				b, _ := os.ReadFile(file)
				os.Remove(file)
				os.WriteFile(file, b, 0700)
				old(t, file)
			case "tracked":
				runGit(t, f.repo, "add", "-f", "worker")
			case "disposition":
				p.Candidates[0].ArtifactDisposition = "archive"
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := f.e.ApplyCache(ctx, p, p.Candidates[0].ID, p.ID); err == nil {
				t.Fatal("changed artifact removed")
			}
			exists(t, file)
		})
	}
}
func TestBinaryArtifactPathsFormatsAndAnalysis(t *testing.T) {
	f, file := binaryFixture(t)
	for _, rel := range []string{"../worker", ".", "/worker", "fixtures/worker", "vendor/worker", ".claude/worker", "nested/worker"} {
		if !strings.Contains(rel, "..") && !filepath.IsAbs(rel) && rel != "." {
			b, _ := os.ReadFile(file)
			p := filepath.Join(f.repo, rel)
			os.MkdirAll(filepath.Dir(p), 0700)
			os.WriteFile(p, b, 0700)
			old(t, p)
			if strings.HasPrefix(rel, "nested/") {
				runGit(t, filepath.Dir(p), "init")
				write(t, filepath.Join(filepath.Dir(p), ".gitignore"), "worker\n")
			}
		}
		p, err := f.e.PlanBinaryArtifact(context.Background(), f.repo, rel, "discard")
		if err == nil && p.Candidates[0].Eligible {
			t.Fatal("unsafe path eligible", rel)
		}
	}
	if p, err := f.e.PlanBinaryArtifact(context.Background(), f.repo, "worker", "force"); err == nil && p.Candidates[0].Eligible {
		t.Fatal("unknown disposition accepted")
	}
	b := make([]byte, 128)
	copy(b, "\x7fELF")
	b[4] = 2
	b[5] = 1
	b[6] = 1
	binary.LittleEndian.PutUint16(b[16:18], 2)
	if err := os.WriteFile(file, b, 0700); err != nil {
		t.Fatal(err)
	}
	old(t, file)
	if got, err := binaryArtifactFormat(file); err != nil || got != "ELF executable" {
		t.Fatal(got, err)
	}
	binary.LittleEndian.PutUint16(b[16:18], 3)
	os.WriteFile(file, b, 0700)
	if _, err := binaryArtifactFormat(file); err == nil {
		t.Fatal("shared library accepted")
	}
	binary.LittleEndian.PutUint64(b[32:40], 64)
	binary.LittleEndian.PutUint16(b[54:56], 56)
	binary.LittleEndian.PutUint16(b[56:58], 1)
	binary.LittleEndian.PutUint32(b[64:68], 3)
	os.WriteFile(file, b, 0700)
	old(t, file)
	if got, err := binaryArtifactFormat(file); err != nil || got != "ELF PIE executable" {
		t.Fatal(got, err)
	}
}
