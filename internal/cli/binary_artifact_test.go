package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniaura/infinite-disk/internal/disk"
)

func TestBinaryArtifactPromptAndJSONNeverImplicitlyApprove(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := disk.DefaultConfig()
	c.Roots = []string{filepath.Join(base, "projects")}
	c.StateDir = filepath.Join(base, "state")
	config := filepath.Join(base, "config.json")
	if err = disk.SaveConfig(config, c); err != nil {
		t.Fatal(err)
	}
	cache := disk.NewEngine(c).CacheConfig()
	file := filepath.Join(c.Roots[0], "fixture", "worker")
	os.MkdirAll(filepath.Dir(file), 0700)
	os.WriteFile(file, []byte("fixture must remain"), 0700)
	for _, choice := range []string{"keep", "archive", "discard"} {
		p, err := disk.SavePlan(cache, disk.Plan{Version: 1, CreatedAt: time.Now(), ConfigDigest: disk.ConfigDigest(cache), Mode: "report-only", Candidates: []disk.Candidate{{ID: "aaaaaaaaaaaaaaaa", Name: "fixture", Path: file, CommonDir: filepath.Dir(file), Kind: "binary-artifact", ArtifactDisposition: choice, Eligible: true, SizeBytes: 200 << 20}}})
		if err != nil {
			t.Fatal(err)
		}
		for _, input := range []string{"", "\n", "no\n", "q\n", "yes"} {
			cmd := New("test")
			var out, diagnostic bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&diagnostic)
			cmd.SetIn(strings.NewReader(input))
			cmd.SetArgs([]string{"--config", config, "--json", "caches", "apply", "--plan", p.ID, "--project", "aaaaaaaaaaaaaaaa"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			var receipts []disk.Receipt
			if json.Unmarshal(out.Bytes(), &receipts) != nil || len(receipts) != 0 {
				t.Fatal("unapproved receipt", out.String())
			}
			if !strings.Contains(diagnostic.String(), "Planned choice: "+choice) || !strings.Contains(diagnostic.String(), "[y/N]") {
				t.Fatal("choice not disclosed", diagnostic.String())
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatal("unapproved target changed")
			}
		}
		cmd := New("test")
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--config", config, "caches", "apply", "--plan", p.ID, "--project", "aaaaaaaaaaaaaaaa", "--non-interactive"})
		if cmd.Execute() == nil {
			t.Fatal("noninteractive missing approval accepted")
		}
		cmd = New("test")
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cmd.SetContext(ctx)
		cmd.SetArgs([]string{"--config", config, "caches", "apply", "--plan", p.ID, "--project", "aaaaaaaaaaaaaaaa"})
		if cmd.Execute() == nil {
			t.Fatal("cancellation ignored")
		}
	}
}
func TestBinaryArchiveAndKeepAreNotCountedAsSavings(t *testing.T) {
	for _, choice := range []string{"archive", "keep"} {
		var out bytes.Buffer
		renderCaches(&out, disk.Plan{Candidates: []disk.Candidate{{Kind: "binary-artifact", ArtifactDisposition: choice, SizeBytes: 200 << 20, Eligible: true}}})
		if !strings.Contains(out.String(), "Ready for approval: 0 MiB.") {
			t.Fatal("retained bytes counted as reclaimable", out.String())
		}
	}
}
func TestBinaryReviewArgvCannotGrantDeletion(t *testing.T) {
	r, err := projectActions(disk.Plan{ID: strings.Repeat("a", 64), Candidates: []disk.Candidate{{ID: "fixture", Path: "/projects/with spaces", Analysis: &disk.ProjectAnalysis{Ignored: []disk.IgnoredEntry{{Path: "worker", Category: "binary_candidate"}}}}}}, "/private/config", "", "all", 10)
	if err != nil {
		t.Fatal(err)
	}
	argv := r.Projects[0].Commands["review_binary_0"]
	if strings.Join(argv, "|") != "infinite-disk|--config|/private/config|--json|caches|plan|--project|/projects/with spaces|--path|worker|--disposition|keep" {
		t.Fatal(argv)
	}
}
