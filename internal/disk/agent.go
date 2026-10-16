package disk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	worker "github.com/omniaura/infinite-disk/agent"
)

func SetupAgent(ctx context.Context, c Config) error {
	if err := EnsureState(c); err != nil {
		return err
	}
	dir := filepath.Join(c.StateDir, "agent")
	for _, name := range []string{"bridge.mjs", "package.json", "package-lock.json"} {
		b, err := worker.Files.ReadFile(name)
		if err != nil {
			return err
		}
		if err = atomicPrivate(filepath.Join(dir, name), b); err != nil {
			return err
		}
	}
	cmd := exec.CommandContext(ctx, "npm", "ci", "--ignore-scripts", "--no-fund", "--no-audit", "--prefix", dir)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Env = gitEnvironment()
	if err := cmd.Run(); err != nil {
		return errors.New("Pi dependency setup failed; no package lifecycle scripts were run")
	}
	return nil
}

func (e *Engine) Triage(ctx context.Context, p Plan) (Plan, error) {
	if err := EnsureState(e.Config); err != nil {
		return p, err
	}
	script := filepath.Join(e.Config.StateDir, "agent", "bridge.mjs")
	if _, err := os.Stat(script); err != nil {
		return p, errors.New("Pi worker is not installed; run agent setup")
	}
	type summary struct {
		ID         string   `json:"id"`
		Eligible   bool     `json:"eligible"`
		AgeDays    int      `json:"age_days"`
		SizeBytes  uint64   `json:"size_bytes"`
		DirtyFiles int      `json:"dirty_files"`
		Blockers   []string `json:"blockers"`
	}
	items := []summary{}
	for _, c := range p.Candidates {
		if len(items) >= 200 {
			break
		}
		items = append(items, summary{c.ID, c.Eligible, int(e.Now().Sub(c.LastModified).Hours() / 24), c.SizeBytes, c.DirtyFiles, c.Blockers})
	}
	input, _ := json.Marshal(struct {
		Version    int       `json:"version"`
		Model      string    `json:"model"`
		Candidates []summary `json:"candidates"`
	}{1, e.Config.AgentModel, items})
	ctx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", script)
	cmd.Dir = e.Config.StateDir
	cmd.Stdin = bytes.NewReader(input)
	// The worker receives only the requested provider key, not all provider credentials.
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "SYSTEMROOT", "OPENROUTER_API_KEY"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	output := &cappedBuffer{limit: 1 << 20}
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		return p, errors.New("Pi triage failed or exceeded its budget; original report retained")
	}
	var result struct {
		Version         int              `json:"version"`
		Recommendations []Recommendation `json:"recommendations"`
		Steps           []Evidence       `json:"steps"`
		Capabilities    []string         `json:"capabilities"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.Version != 1 {
		return p, errors.New("invalid Pi response")
	}
	slices.Sort(result.Capabilities)
	if !slices.Equal(result.Capabilities, []string{"inspect_project", "inventory_summary"}) {
		return p, errors.New("unexpected Pi capabilities")
	}
	seen := map[string]bool{}
	for _, r := range result.Recommendations {
		if seen[r.CandidateID] || r.Priority < 1 || r.Priority > 3 || !slices.Contains([]string{"review", "keep", "blocked"}, r.Decision) || len(r.Reason) > 2000 || len(r.ProposedSteps) > 10 {
			return p, errors.New("invalid Pi recommendation")
		}
		found := false
		for _, c := range p.Candidates {
			if c.ID == r.CandidateID {
				found = true
				if !c.Eligible && r.Decision != "blocked" {
					return p, errors.New("agent attempted to override a safety blocker")
				}
				break
			}
		}
		if !found {
			return p, errors.New("agent invented a project ID")
		}
		seen[r.CandidateID] = true
	}
	p.Recommendations = result.Recommendations
	p.AgentSteps = result.Steps
	p, err := SavePlan(e.Config, p)
	if err != nil {
		return p, err
	}
	return p, AppendEvent(e.Config, Event{Type: "pi_triage_completed", PlanID: p.ID, Outcome: "metadata_only_report"})
}
