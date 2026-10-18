package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniaura/infinite-disk/internal/disk"
)

func globalCLIPlan(t *testing.T) (string, disk.Plan, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := disk.DefaultConfig()
	c.StateDir = filepath.Join(base, "state")
	c.Roots = []string{filepath.Join(base, "projects")}
	config := filepath.Join(base, "config.json")
	if err := disk.SaveConfig(config, c); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "keep")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	c.StateDir = filepath.Join(c.StateDir, "global")
	p, err := disk.SavePlan(c, disk.Plan{Version: 1, CreatedAt: time.Now(), ConfigDigest: disk.ConfigDigest(c), Candidates: []disk.Candidate{{ID: "fixture", Path: target, Name: "fixture cache", Kind: "global-cache/go-build", Eligible: true, SizeBytes: 1024}}})
	if err != nil {
		t.Fatal(err)
	}
	return config, p, target
}

func TestGlobalCLIRejectsImplicitOrBroadApprovals(t *testing.T) {
	config, p, target := globalCLIPlan(t)
	for _, args := range [][]string{
		{"global", "apply"},
		{"global", "apply", "--non-interactive"},
		{"global", "apply", "--target", "fixture", "--non-interactive"},
		{"global", "apply", "--target", "fixture", "--json"},
		{"global", "apply", "--target", "fixture", "--approve-plan", strings.Repeat("0", 64)},
		{"global", "apply", "--target", "unknown", "--approve-plan", p.ID},
		{"global", "apply", "--target", "fixture", "--approve-plan", p.ID, "--yes-all"},
		{"global", "apply", "--target", "fixture", target},
	} {
		cmd := New("test")
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs(append([]string{"--config", config}, args...))
		if err := cmd.Execute(); err == nil {
			t.Fatal("accepted missing/wrong approval", args)
		}
		if strings.Contains(output.String(), "[y/N]") {
			t.Fatal("non-interactive or invalid command prompted", args)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatal("missing approval removed a file", args)
		}
	}
}

func TestGlobalCLIInteractiveNoAndEOFDoNotApprove(t *testing.T) {
	config, p, target := globalCLIPlan(t)
	for _, input := range []string{"", "yes", "\n", "n\n", "quit\n"} {
		cmd := New("test")
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetIn(strings.NewReader(input))
		cmd.SetArgs([]string{"--config", config, "global", "apply", "--plan", p.ID, "--target", "fixture"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "No backup") || !strings.Contains(strings.Join(strings.Fields(output.String()), ""), target) {
			t.Fatal("approval scope/consequence not displayed")
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatal("EOF or negative reply approved deletion", input)
		}
	}
}

func TestGlobalCLIReviewReadsExactNamespaceWithoutApproval(t *testing.T) {
	config, p, target := globalCLIPlan(t)
	cmd := New("test")
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--config", config, "--color", "always", "global", "review", "--plan", p.ID, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var got disk.Plan
	if err := json.Unmarshal(output.Bytes(), &got); err != nil || got.ID != p.ID || len(got.Candidates) != 1 || strings.Contains(output.String(), "\x1b[") {
		t.Fatal(output.String(), err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("review mutated target")
	}
	output.Reset()
	cmd = New("test")
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--config", config, "global", "review"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Nothing deleted") || !strings.Contains(output.String(), "--plan "+p.ID+" --target fixture") {
		t.Fatal("missing report-only scope or exact suggestion", output.String())
	}
}
