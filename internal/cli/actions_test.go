package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniaura/infinite-disk/internal/disk"
)

func TestCompactActionsBindExactPlanAndNeverOfferBlockedExecution(t *testing.T) {
	p := disk.Plan{ID: strings.Repeat("a", 64), Candidates: []disk.Candidate{
		{ID: "ready", Path: "/test/small", Eligible: true, SizeBytes: 1},
		{ID: "blocked", Path: "/test/big", SizeBytes: 2, Blockers: []string{"private_or_unknown_ignored_state"}},
	}}
	r, err := projectActions(p, "/private/config with spaces.json", "", "all", 1)
	if err != nil || r.Matching != 2 || r.Returned != 1 || r.Projects[0].ID != "blocked" || r.Projects[0].Commands["remove_checkout"] != nil {
		t.Fatal(r, err)
	}
	r, err = projectActions(p, "/private/config with spaces.json", "", "ready", 20)
	if err != nil || r.Returned != 1 {
		t.Fatal(r, err)
	}
	args := r.Projects[0].Commands["preserve_source"]
	if !strings.Contains(strings.Join(args, "|"), "--plan|"+p.ID+"|--project|ready|--approve-plan|"+p.ID+"|--non-interactive|--preserve-only") || args[2] != "/private/config with spaces.json" {
		t.Fatal(args)
	}
	for _, bad := range []struct {
		filter string
		limit  int
	}{{"bad", 20}, {"all", 0}, {"all", 5001}} {
		if _, err := projectActions(p, "config", "", bad.filter, bad.limit); err == nil {
			t.Fatal("invalid filter accepted")
		}
	}
	if _, err := projectActions(p, "config", "missing", "all", 20); err == nil {
		t.Fatal("unknown project accepted")
	}
}

func TestBlockedSourceSafeProjectOffersPreserveOnlyNotRemoval(t *testing.T) {
	c := disk.Candidate{ID: "source-safe", Path: "/fixture/blocked", Head: "head", Fingerprint: "checked", SessionsChecked: true, Blockers: []string{"private_or_unknown_ignored_state"}}
	r, err := projectActions(disk.Plan{ID: strings.Repeat("a", 64), Candidates: []disk.Candidate{c}}, "config", "", "all", 20)
	if err != nil || r.ApprovalGranted || r.Projects[0].Commands["preserve_source"] == nil || r.Projects[0].Commands["remove_checkout"] != nil {
		t.Fatal(r, err)
	}
}

func TestAllBatchesKeepRealApprovalDigestsAndNewestBlocker(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := disk.DefaultConfig()
	c.Roots = []string{filepath.Join(base, "projects")}
	c.StateDir = filepath.Join(base, "state")
	before := time.Now().Add(-time.Hour)
	old := disk.Plan{Version: 1, CreatedAt: before, ConfigDigest: disk.ConfigDigest(c), Candidates: []disk.Candidate{{ID: "same", Path: "/fixture/same", Eligible: true, SizeBytes: 100}, {ID: "ready", Path: "/fixture/ready", Eligible: true, SizeBytes: 50}}}
	old, err = disk.SavePlan(c, old)
	if err != nil {
		t.Fatal(err)
	}
	newer := disk.Plan{Version: 1, CreatedAt: time.Now(), ConfigDigest: disk.ConfigDigest(c), Candidates: []disk.Candidate{{ID: "same", Path: "/fixture/same", Eligible: false, SizeBytes: 100, Blockers: []string{"project_in_use"}}}}
	newer, err = disk.SavePlan(c, newer)
	if err != nil {
		t.Fatal(err)
	}
	r, err := allProjectActions(c, "config", "", "ready", 20)
	if err != nil || r.ApprovalGranted || r.SavedBatches != 2 || r.Total != 2 || r.Matching != 1 || r.PlanID != "" || r.Projects[0].ID != "ready" || r.Projects[0].PlanID != old.ID {
		t.Fatal(r, err)
	}
	if !strings.Contains(strings.Join(r.Projects[0].Commands["remove_checkout"], "|"), old.ID) {
		t.Fatal("synthetic approval digest")
	}
	r, err = allProjectActions(c, "config", "same", "all", 20)
	if err != nil || len(r.Projects) != 1 || r.Projects[0].Eligible || r.Projects[0].PlanID != newer.ID || r.Projects[0].Commands["remove_checkout"] != nil {
		t.Fatal("new blocker lost", r, err)
	}
}

func TestMachineErrorsAreVersionedAndActionable(t *testing.T) {
	for _, tc := range []struct {
		err       error
		cancelled bool
		code      string
	}{{disk.ErrOperationInProgress, false, "operation_in_progress"}, {errors.New("fixture failure"), false, "command_failed"}, {errors.New("interrupted"), true, "cancelled"}} {
		var out bytes.Buffer
		WriteError(&out, tc.err, true, tc.cancelled)
		var response struct {
			Version int `json:"version"`
			Error   struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(out.Bytes(), &response); err != nil || response.Version != 1 || response.Error.Code != tc.code || response.Error.Message == "" {
			t.Fatal(out.String(), err)
		}
	}
}

func TestAgentFriendlyCommandsNeverWaitForImplicitApproval(t *testing.T) {
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
	cacheConfig := c
	cacheConfig.StateDir = filepath.Join(c.StateDir, "kondo")
	for _, conf := range []disk.Config{c, cacheConfig} {
		if _, err = disk.SavePlan(conf, disk.Plan{Version: 1, CreatedAt: time.Now(), ConfigDigest: disk.ConfigDigest(conf), Candidates: []disk.Candidate{{ID: "blocked", Path: "/fixture/blocked", Eligible: false}}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"apply", "--non-interactive"}, {"caches", "apply", "--non-interactive"}, {"apply", "--project", "blocked"}, {"caches", "apply", "--project", "blocked"}, {"caches", "apply", "--approve-plan", strings.Repeat("0", 64)}} {
		cmd := New("test")
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--config", config, "--json"}, args...))
		if cmd.Execute() == nil {
			t.Fatal("missing approval or blocked project returned success", args)
		}
		if strings.Contains(out.String(), "[y/N]") {
			t.Fatal("noninteractive command prompted", out.String())
		}
	}
	cmd := New("test")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--config", config, "actions", "--json", "--limit", "1"})
	if err = cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var report actionReport
	if err = json.Unmarshal(out.Bytes(), &report); err != nil || report.Mode != "suggestions-only" || report.Total != 1 {
		t.Fatal(out.String(), err)
	}
}
