package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/omniaura/infinite-disk/internal/disk"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivateCLIExactApprovalAndJSONRecovery(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "projects", "fixture")
	if err = os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture git failed: %s %v", out, err)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.invalid")
	source := filepath.Join(repo, "source.txt")
	if err = os.WriteFile(source, []byte("fixture source"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-m", "fixture")
	git("remote", "add", "origin", "https://github.com/example/infinite-disk-fixture.git")
	when := time.Now().Add(-30 * 24 * time.Hour)
	if err = os.Chtimes(source, when, when); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(repo, ".git/info/exclude"), []byte(".env\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(repo, ".env"), []byte("private fixture bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	config := disk.DefaultConfig()
	config.Roots = []string{filepath.Dir(repo)}
	config.StateDir = filepath.Join(base, "state")
	engine := disk.NewEngine(config)
	engine.Activity = func(context.Context) disk.Activity { return disk.Activity{Known: true} }
	engine.Sessions = func(context.Context) disk.SessionInventory { return disk.SessionInventory{Complete: true} }
	p, err := engine.RunReport(context.Background())
	if err != nil || len(p.Candidates) != 1 {
		t.Fatal(err)
	}
	invoke := func(asJSON bool, args ...string) (string, error) {
		var out, diag bytes.Buffer
		cmd := privateCommand(func() (*disk.Engine, error) { return engine, nil }, &asJSON)
		cmd.SetOut(&out)
		cmd.SetErr(&diag)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	out, err := invoke(true, "plan", "--plan", p.ID, "--project", p.Candidates[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var archive disk.PrivatePlan
	if json.Unmarshal([]byte(out), &archive) != nil || archive.ID == "" {
		t.Fatal(out)
	}
	if strings.Contains(out, "private fixture bytes") {
		t.Fatal("private contents in report")
	}
	for _, approval := range []string{"", strings.Repeat("0", 64)} {
		if _, err = invoke(true, "apply", "--plan", archive.ID, "--approve-plan", approval); err == nil {
			t.Fatal("approval not required")
		}
		if _, err = os.Stat(filepath.Join(repo, ".env")); err != nil {
			t.Fatal("unapproved move")
		}
	}
	out, err = invoke(true, "apply", "--plan", archive.ID, "--approve-plan", archive.ID)
	if err != nil || !strings.Contains(out, "verified_local_archive") {
		t.Fatal(out, err)
	}
	out, err = invoke(false, "review", "--plan", archive.ID)
	if err != nil || !strings.Contains(out, "not off-device backup") {
		t.Fatal(out, err)
	}
	out, err = invoke(true, "restore", "--plan", archive.ID, "--approve-plan", archive.ID)
	if err != nil || !strings.Contains(out, `"archive_retained":true`) {
		t.Fatal(out, err)
	}
	data, err := os.ReadFile(filepath.Join(repo, ".env"))
	if err != nil || string(data) != "private fixture bytes" {
		t.Fatal("restore failed", err)
	}
}
func TestAgentEvidenceVisibleWithoutOpaqueToolIDsOrFakeFreeUsage(t *testing.T) {
	p := disk.Plan{CreatedAt: time.Now(), AgentRun: &disk.AgentRun{Provider: "ditto", Model: "gm/gpt-6-luna", Requests: 3, Projects: 20, Advised: 1, DurationMS: 1234}, Candidates: []disk.Candidate{{ID: "abcdef0123456789", Path: "/fixture/old-project", Eligible: false}}, AgentSteps: []disk.Evidence{{Check: "inventory_summary", Outcome: "performed"}, {Check: "suggest_action", Outcome: "performed", Detail: "abcdef0123456789:private_backup"}}, Recommendations: []disk.Recommendation{{CandidateID: "abcdef0123456789", Reason: "Private data needs local recovery first."}}}
	var out bytes.Buffer
	renderPlan(&out, p, false)
	for _, text := range []string{"AGENT RUN", "gm/gpt-6-luna", "3 model requests", "20 metadata records", "usage unavailable", "suggest_action × 1", "Private data needs local recovery first."} {
		if !strings.Contains(out.String(), text) {
			t.Fatal("missing agent proof", text)
		}
	}
	if strings.Contains(out.String(), "abcdef0123456789") {
		t.Fatal("opaque ID in default human agent panel")
	}
	p.AgentRun.UsageReported = true
	p.AgentRun.InputTokens = 123
	p.AgentRun.OutputTokens = 45
	out.Reset()
	renderPlan(&out, p, false)
	if !strings.Contains(out.String(), "123 input") {
		t.Fatal("reported usage missing")
	}
}
