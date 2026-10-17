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

func TestApprovalDefaultNoAndExactSelection(t *testing.T) {
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
	p, err := disk.SavePlan(c, disk.Plan{Version: 1, CreatedAt: time.Now(), ConfigDigest: disk.ConfigDigest(c), Candidates: []disk.Candidate{{ID: "aaaaaaaaaaaaaaaa", Path: filepath.Join(c.Roots[0], "fixture"), Name: "fixture", Eligible: true}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"", "\n", "no\n"} {
		cmd := New("test")
		var out, diagnostic bytes.Buffer
		cmd.SetIn(strings.NewReader(input))
		cmd.SetOut(&out)
		cmd.SetErr(&diagnostic)
		cmd.SetArgs([]string{"--config", config, "--json", "apply"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		var result []disk.Receipt
		if json.Unmarshal(out.Bytes(), &result) != nil || len(result) != 0 {
			t.Fatal("default approval not empty", out.String())
		}
		if !strings.Contains(diagnostic.String(), "[y/N]") {
			t.Fatal("no per-project prompt")
		}
	}
	for _, args := range [][]string{{"apply", "--approve-plan", p.ID}, {"apply", "--project", "aaaaaaaaaaaaaaaa", "--approve-plan", strings.Repeat("0", 64)}, {"apply", "--project", "unknown"}} {
		cmd := New("test")
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"--config", config}, args...))
		if cmd.Execute() == nil {
			t.Fatal("invalid selection approved", args)
		}
	}
	if _, err := os.Stat(filepath.Join(c.StateDir, "receipts")); !os.IsNotExist(err) {
		t.Fatal("unapproved operation created receipts")
	}
}

func TestHelpAndDoctorAreReadOnly(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"doctor", "--json"}, {"service", "--help"}, {"restore", "--help"}} {
		cmd := New("test")
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if out.Len() == 0 {
			t.Fatal("empty CLI response")
		}
	}
}
