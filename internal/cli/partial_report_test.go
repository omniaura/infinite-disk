package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniaura/infinite-disk/internal/disk"
)

func TestTriageFailureStillReturnsReadablePlan(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := disk.DefaultConfig()
	c.StateDir = filepath.Join(base, "state")
	c.Roots = []string{filepath.Join(base, "projects")}
	config := filepath.Join(base, "config.json")
	if err = disk.SaveConfig(config, c); err != nil {
		t.Fatal(err)
	}
	p, err := disk.SavePlan(c, disk.Plan{Version: 1, CreatedAt: time.Now(), ConfigDigest: disk.ConfigDigest(c), Candidates: []disk.Candidate{{ID: "aaaaaaaaaaaaaaaa", Path: filepath.Join(c.Roots[0], "fixture"), Eligible: false, Blockers: []string{"project_in_use"}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"triage", "--json"}, {"triage", "--json", "--project", "aaaaaaaaaaaaaaaa"}} {
		cmd := New("test")
		var out, diagnostic bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&diagnostic)
		cmd.SetArgs(append([]string{"--config", config}, args...))
		if cmd.Execute() == nil {
			t.Fatal("inference failure reported as success")
		}
		var retained disk.Plan
		if json.Unmarshal(out.Bytes(), &retained) != nil || retained.ID != p.ID {
			t.Fatal("usable report discarded", out.String())
		}
	}
	for _, selection := range []string{"unknown", strings.Repeat("aaaaaaaaaaaaaaaa,", 20) + "aaaaaaaaaaaaaaaa"} {
		cmd := New("test")
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--config", config, "triage", "--project", selection})
		if cmd.Execute() == nil {
			t.Fatal("invalid/oversize project selection accepted")
		}
	}
}

func TestRollingScanStreamsEachBatchWithoutDuplicates(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", base)
	c := disk.DefaultConfig()
	c.StateDir = filepath.Join(base, "state")
	c.Roots = []string{filepath.Join(base, "projects")}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := os.MkdirAll(filepath.Join(c.Roots[0], name, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(base, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "lsof"), []byte("#!/bin/sh\nprintf 'n/tmp/fixture-open\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	config := filepath.Join(base, "config.json")
	if err := disk.SaveConfig(config, c); err != nil {
		t.Fatal(err)
	}
	cmd := New("test")
	var out, diagnostic bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostic)
	cmd.SetArgs([]string{"--config", config, "scan", "--batch-size", "2", "--batches", "3", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err, diagnostic.String())
	}
	decoder := json.NewDecoder(&out)
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		var p disk.Plan
		if err := decoder.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if i == 1 && (!p.DiscoveryCached || len(p.Candidates) != 1) {
			t.Fatal("not a rolling tail batch", p)
		}
		for _, c := range p.Candidates {
			if seen[c.ID] {
				t.Fatal("duplicate inspection in same rolling pass")
			}
			seen[c.ID] = true
		}
	}
	if err := decoder.Decode(new(any)); err != io.EOF || len(seen) != 3 {
		t.Fatal("extra/missing batch", err, len(seen))
	}
}
