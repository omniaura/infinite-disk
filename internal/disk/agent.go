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
	"sort"
	"strings"
	"time"

	worker "github.com/omniaura/infinite-disk/agent"
)

// Keep both the inventory and final response comfortably inside the fixed
// four-request / 4096-output-token budget. More projects need an explicit triage.
const agentProjectLimit = 20

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
	return e.triage(ctx, p, nil)
}

func (e *Engine) TriageSelected(ctx context.Context, p Plan, selected map[string]bool) (Plan, error) {
	return e.triage(ctx, p, selected)
}

func (e *Engine) triage(ctx context.Context, p Plan, selected map[string]bool) (Plan, error) {
	return e.triageInventory(ctx, p, selected, "projects", true)
}
func (e *Engine) triageInventory(ctx context.Context, p Plan, selected map[string]bool, domain string, persist bool) (Plan, error) {
	if err := EnsureState(e.Config); err != nil {
		return p, err
	}
	script := filepath.Join(e.Config.StateDir, "agent", "bridge.mjs")
	if _, err := os.Stat(script); err != nil {
		return p, errors.New("Pi worker is not installed; run agent setup")
	}
	if domain == "media" {
		installed, err := readMediaPrivate(script, 2<<20)
		embedded, embedErr := worker.Files.ReadFile("bridge.mjs")
		if err != nil || embedErr != nil || !bytes.Equal(installed, embedded) {
			return p, errors.New("installed Pi worker lacks this media contract; run agent setup to update it")
		}
	}
	type summary struct {
		ID           string         `json:"id"`
		Eligible     bool           `json:"eligible"`
		AgeDays      int            `json:"age_days"`
		SizeBytes    uint64         `json:"size_bytes"`
		SizeComplete bool           `json:"size_complete"`
		DirtyFiles   int            `json:"dirty_files"`
		Blockers     []string       `json:"blockers"`
		Analysis     *agentAnalysis `json:"analysis,omitempty"`
	}
	items := []summary{}
	candidates := append([]Candidate{}, p.Candidates...)
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].SizeBytes > candidates[j].SizeBytes })
	for _, c := range candidates {
		if selected != nil && !selected[c.ID] {
			continue
		}
		if len(items) >= agentProjectLimit {
			break
		}
		var analysis *agentAnalysis
		if c.Analysis != nil {
			a := c.Analysis
			analysis = &agentAnalysis{SourceChecked: a.SourceChecked, ChangedFiles: a.ChangedFiles, Commits: a.CommitsNotInOriginTracking, IgnoredCount: a.IgnoredCount, IgnoredComplete: a.IgnoredComplete, Categories: map[string]int{}, Actions: []string{}, Media: a.Media}
			for _, entry := range a.Ignored {
				analysis.Categories[entry.Category]++
			}
			for _, action := range a.Actions {
				analysis.Actions = append(analysis.Actions, action.Kind)
			}
		}
		age := 0
		if !c.LastModified.IsZero() {
			age = max(0, int(e.Now().Sub(c.LastModified).Hours()/24))
		}
		items = append(items, summary{c.ID, c.Eligible, age, c.SizeBytes, c.SizeComplete, c.DirtyFiles, c.Blockers, analysis})
	}
	if len(items) == 0 {
		return p, nil
	}
	input, _ := json.Marshal(struct {
		Version    int       `json:"version"`
		Model      string    `json:"model"`
		Provider   string    `json:"provider,omitempty"`
		BaseURL    string    `json:"base_url,omitempty"`
		Candidates []summary `json:"candidates"`
		Domain     string    `json:"domain,omitempty"`
	}{1, e.Config.AgentModel, e.Config.AgentProvider, e.Config.AgentBaseURL, items, domain})
	ctx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", script)
	cmd.Dir = e.Config.StateDir
	cmd.Stdin = bytes.NewReader(input)
	// The worker receives only the requested provider key, not all provider credentials.
	var err error
	cmd.Env, err = AgentEnvironment(e.Config)
	if err != nil {
		return p, err
	}
	output := &cappedBuffer{limit: 1 << 20}
	cmd.Stdout = output
	started := time.Now().UTC()
	if err := cmd.Run(); err != nil {
		stage := "worker execution"
		if ctx.Err() != nil {
			stage = "time budget"
		} else if exit, ok := err.(*exec.ExitError); ok {
			switch exit.ExitCode() {
			case 65:
				stage = "response validation"
			case 69:
				stage = "provider request"
			case 75:
				stage = "agent time budget"
			case 81:
				stage = "inventory evidence"
			case 82:
				stage = "model JSON parsing"
			case 83:
				stage = "recommendation validation"
			case 84:
				stage = "suggestion evidence"
			}
		}
		return p, errors.New("Pi triage failed at " + stage + "; original report retained. Retry a smaller set with triage --project ID")
	}
	var result struct {
		Version         int              `json:"version"`
		Recommendations []Recommendation `json:"recommendations"`
		Steps           []Evidence       `json:"steps"`
		Capabilities    []string         `json:"capabilities"`
		Warnings        []string         `json:"warnings,omitempty"`
		Requests        int              `json:"requests"`
		Usage           struct {
			Input    uint64 `json:"input_tokens"`
			Output   uint64 `json:"output_tokens"`
			Reported bool   `json:"reported"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.Version != 1 {
		return p, errors.New("invalid Pi response")
	}
	if result.Requests < 1 || result.Requests > 4 || result.Usage.Input > 1000000 || result.Usage.Output > 4*4096 || result.Usage.Reported != (result.Usage.Input+result.Usage.Output > 0) {
		return p, errors.New("invalid Pi inference evidence; run agent setup for the current worker")
	}
	slices.Sort(result.Capabilities)
	if !slices.Equal(result.Capabilities, []string{"inspect_project", "inventory_summary", "suggest_action"}) {
		return p, errors.New("unexpected or outdated Pi capabilities; run agent setup to install the current suggestion-only worker")
	}
	seen := map[string]bool{}
	for _, r := range result.Recommendations {
		if !slices.ContainsFunc(items, func(item summary) bool { return item.ID == r.CandidateID }) {
			return p, errors.New("agent invented a project ID")
		}
		if seen[r.CandidateID] || r.Priority < 1 || r.Priority > 3 || !slices.Contains([]string{"review", "keep", "blocked", "analyze"}, r.Decision) || len(r.Reason) > 2000 || len(r.ProposedSteps) > 10 || len(r.SuggestedActions) > 10 {
			return p, errors.New("invalid Pi recommendation")
		}
		found := false
		for _, c := range p.Candidates {
			if c.ID == r.CandidateID {
				found = true
				if !c.Eligible && r.Decision != "blocked" && r.Decision != "analyze" {
					return p, errors.New("agent attempted to override a safety blocker")
				}
				for _, kind := range r.SuggestedActions {
					if c.Analysis == nil || !slices.ContainsFunc(c.Analysis.Actions, func(a SuggestedAction) bool { return a.Kind == kind }) {
						return p, errors.New("agent invented an action")
					}
					if !slices.ContainsFunc(result.Steps, func(step Evidence) bool {
						return step.Check == "suggest_action" && step.Outcome == "performed" && step.Detail == r.CandidateID+":"+kind
					}) {
						return p, errors.New("agent action lacks suggestion-tool evidence")
					}
				}
				break
			}
		}
		if !found {
			return p, errors.New("agent invented a project ID")
		}
		seen[r.CandidateID] = true
		for _, step := range r.ProposedSteps {
			if len(step) > 500 {
				return p, errors.New("invalid Pi proposed step")
			}
		}
	}
	if len(result.Steps) > 1000 {
		return p, errors.New("Pi step budget exceeded")
	}
	inspected := false
	for _, step := range result.Steps {
		if !slices.Contains(result.Capabilities, step.Check) || step.Outcome != "performed" || len(step.Detail) > 200 {
			return p, errors.New("invalid Pi evidence")
		}
		switch step.Check {
		case "inventory_summary":
			if step.Detail != "" {
				return p, errors.New("invalid Pi inventory evidence")
			}
			inspected = true
		case "inspect_project":
			if !slices.ContainsFunc(items, func(item summary) bool { return item.ID == step.Detail }) {
				return p, errors.New("invalid Pi inspection evidence")
			}
		case "suggest_action":
			id, kind, _ := strings.Cut(step.Detail, ":")
			if !slices.ContainsFunc(items, func(item summary) bool {
				return item.ID == id && item.Analysis != nil && slices.Contains(item.Analysis.Actions, kind)
			}) {
				return p, errors.New("invalid Pi suggestion evidence")
			}
		}
	}
	if !inspected {
		return p, errors.New("Pi did not inspect the metadata inventory")
	}
	if len(result.Warnings) > 1 {
		return p, errors.New("invalid Pi warning")
	}
	for _, warning := range result.Warnings {
		if warning != "omitted_unrecorded_suggestions" {
			return p, errors.New("invalid Pi warning")
		}
	}
	p.Recommendations = result.Recommendations
	p.AgentSteps = result.Steps
	p.AgentRun = &AgentRun{Provider: e.Config.AgentProvider, Model: e.Config.AgentModel, StartedAt: started, DurationMS: time.Since(started).Milliseconds(), Requests: result.Requests, Projects: len(items), Advised: len(result.Recommendations), InputTokens: result.Usage.Input, OutputTokens: result.Usage.Output, UsageReported: result.Usage.Reported}
	if p.AgentRun.Provider == "" {
		p.AgentRun.Provider = "openrouter"
	}
	for range result.Warnings {
		p.AgentSteps = append(p.AgentSteps, Evidence{"pi_response", "constrained", "Unrecorded model action suggestions were omitted; no tool evidence or approval was invented."})
	}
	p.AgentSteps = append(p.AgentSteps, Evidence{"pi_coverage", "checked", itoa(len(result.Recommendations)) + " projects advised from " + itoa(len(items)) + " largest metadata records; " + itoa(len(p.Candidates)) + " projects in saved report. Advice is not approval."})
	if !persist {
		return p, nil
	}
	p, err = SavePlan(e.Config, p)
	if err != nil {
		return p, err
	}
	return p, AppendEvent(e.Config, Event{Type: "pi_triage_completed", PlanID: p.ID, Outcome: "metadata_only_report"})
}

// Explicit provider projection: no paths, names, source, diffs, session text,
// branches, remote URLs or ignored-file names cross the model boundary.
type agentAnalysis struct {
	SourceChecked   bool           `json:"source_checked"`
	ChangedFiles    *int           `json:"changed_files,omitempty"`
	Commits         *int           `json:"commits_not_in_origin_tracking,omitempty"`
	IgnoredCount    int            `json:"ignored_count"`
	IgnoredComplete bool           `json:"ignored_complete"`
	Categories      map[string]int `json:"ignored_example_categories"`
	Actions         []string       `json:"available_suggestions"`
	Media           *MediaFacts    `json:"media,omitempty"`
}
