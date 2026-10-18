package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/omniaura/infinite-disk/internal/disk"
	"github.com/spf13/cobra"
)

type actionItem struct {
	ID         string                `json:"id"`
	PlanID     string                `json:"plan_id"`
	ObservedAt time.Time             `json:"observed_at"`
	Path       string                `json:"path"`
	SizeBytes  uint64                `json:"size_bytes"`
	Eligible   bool                  `json:"eligible"`
	Blockers   []string              `json:"blockers"`
	Analysis   *disk.ProjectAnalysis `json:"analysis,omitempty"`
	PiAdvice   *disk.Recommendation  `json:"pi_advice,omitempty"`
	Commands   map[string][]string   `json:"commands,omitempty"`
}
type actionReport struct {
	Version         int            `json:"version"`
	Mode            string         `json:"mode"`
	ApprovalGranted bool           `json:"approval_granted"`
	PlanID          string         `json:"plan_id,omitempty"`
	SavedBatches    int            `json:"saved_batches"`
	Total           int            `json:"total"`
	Matching        int            `json:"matching"`
	Returned        int            `json:"returned"`
	Warnings        []string       `json:"warnings"`
	Projects        []actionItem   `json:"projects"`
	AgentRun        *disk.AgentRun `json:"agent_run,omitempty"`
}

func projectActions(p disk.Plan, config, project, filter string, limit int) (actionReport, error) {
	r := actionReport{Version: 1, Mode: "suggestions-only", PlanID: p.ID, SavedBatches: 1, Total: len(p.Candidates), Projects: []actionItem{}, Warnings: append(append([]string{}, p.Warnings...), p.SessionWarnings...)}
	r.AgentRun = p.AgentRun
	if limit < 1 || limit > 5000 {
		return r, errors.New("--limit must be between 1 and 5000")
	}
	if filter != "all" && filter != "ready" && filter != "needs-analysis" {
		return r, errors.New("--filter must be all, ready or needs-analysis")
	}
	rows := append([]disk.Candidate{}, p.Candidates...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].SizeBytes > rows[j].SizeBytes })
	found := project == ""
	for _, c := range rows {
		if project != "" && project != c.ID {
			continue
		}
		found = true
		if filter == "ready" && !c.Eligible || filter == "needs-analysis" && c.Eligible {
			continue
		}
		r.Matching++
		if len(r.Projects) >= limit {
			continue
		}
		observed := p.CreatedAt
		if c.Analysis != nil {
			observed = c.Analysis.ObservedAt
		}
		item := actionItem{ID: c.ID, PlanID: p.ID, ObservedAt: observed, Path: c.Path, SizeBytes: c.SizeBytes, Eligible: c.Eligible, Blockers: c.Blockers, Analysis: c.Analysis, Commands: map[string][]string{}}
		for i := range p.Recommendations {
			if p.Recommendations[i].CandidateID == c.ID {
				r := p.Recommendations[i]
				item.PiAdvice = &r
				break
			}
		}
		// Construct argv ourselves. Never evaluate shell text or model output.
		base := []string{"infinite-disk", "--config", config, "--json"}
		if c.Eligible || disk.CanPreserveSource(c) {
			args := append(append([]string{}, base...), "apply", "--plan", p.ID, "--project", c.ID, "--approve-plan", p.ID, "--non-interactive")
			if c.Eligible {
				item.Commands["remove_checkout"] = args
			}
			item.Commands["preserve_source"] = append(append([]string{}, args...), "--preserve-only")
		}
		item.Commands["analyze"] = append(append([]string{}, base...), "analyze", "--plan", p.ID, "--project", c.ID)
		if disk.CanArchivePrivate(c) {
			item.Commands["private_plan"] = append(append([]string{}, base...), "private", "plan", "--plan", p.ID, "--project", c.ID)
		}
		item.Commands["inspect_artifacts"] = append(append([]string{}, base...), "caches", "scan")
		if c.Analysis != nil {
			for i, entry := range c.Analysis.Ignored {
				if entry.Category == "binary_candidate" {
					item.Commands[fmt.Sprintf("review_binary_%d", i)] = append(append([]string{}, base...), "caches", "plan", "--project", c.Path, "--path", entry.Path, "--disposition", "keep")
				}
			}
		}
		r.Projects = append(r.Projects, item)
	}
	if !found {
		return r, errors.New("project ID is not in this plan")
	}
	r.Returned = len(r.Projects)
	return r, nil
}

func renderActions(w io.Writer, r actionReport) {
	fmt.Fprintln(styled(w, toneHeading), "\nSUGGESTED ACTIONS  |  Nothing applied")
	fmt.Fprintf(w, "Showing %d of %d matching projects (%d unique paths; %d saved batches).\n", r.Returned, r.Matching, r.Total, r.SavedBatches)
	for _, c := range r.Projects {
		state := "NEEDS ANALYSIS"
		if c.Eligible {
			state = "READY FOR APPROVAL"
			if r.SavedBatches > 1 {
				state = "READY IN SAVED REPORT"
			}
		}
		fmt.Fprintf(styled(w, toneHeading), "\n%s  %s  [%s]\n", size(c.SizeBytes), state, c.ID)
		wrapped(w, "  ", c.Path)
		if r.SavedBatches > 1 {
			fmt.Fprintf(w, "  Observed: %s; approval rechecks current state.\n", c.ObservedAt.Local().Format("Jan 2, 15:04"))
		}
		for _, b := range c.Blockers {
			wrapped(styled(w, toneWarning), "  ", blockerText(b))
		}
		if c.Analysis == nil {
			wrapped(w, "  ", "No analysis in this saved report. Run analyze for fresh evidence and suggestions.")
			continue
		}
		a := c.Analysis
		if a.ChangedFiles != nil {
			fmt.Fprintf(w, "  Working changes: %d files (source/secret check completed).\n", *a.ChangedFiles)
		} else {
			fmt.Fprintln(w, "  Working changes: unknown, not assumed clean.")
		}
		if a.CommitsNotInOriginTracking != nil {
			fmt.Fprintf(w, "  Commits absent from local origin-tracking refs: %d (not live remote proof).\n", *a.CommitsNotInOriginTracking)
		}
		for _, entry := range a.Ignored {
			wrapped(w, "    ", entry.Category+": "+entry.Path)
		}
		for _, action := range a.Actions {
			status := "manual prerequisite"
			if action.Executable {
				status = "available in CLI"
			}
			if action.RequiresApproval {
				status += "; requires approval"
			}
			wrapped(w, "  → ", action.Summary+" ["+status+"]")
		}
		if c.PiAdvice != nil {
			wrapped(w, "  Pi advisory: ", c.PiAdvice.Reason)
		}
	}
	for _, warning := range r.Warnings {
		wrapped(styled(w, toneWarning), "! ", warning)
	}
	fmt.Fprintln(w, "\nAnalysis cannot approve, upload or delete. JSON includes exact argv for supported actions.")
}

func actionsCommand(load func() (*disk.Engine, error), config *string, asJSON *bool, analyze bool) *cobra.Command {
	var plan, project, filter string
	var limit int
	var withAgent bool
	var allPlans bool
	name := "actions"
	short := "List compact structured suggestions and exact approval commands; apply nothing"
	if analyze {
		name = "analyze"
		short = "Refresh project evidence and suggest how to resolve blockers; apply nothing"
	}
	cmd := &cobra.Command{Use: name, Short: short, RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		if allPlans {
			if cmd.Flags().Changed("plan") {
				return errors.New("--all-plans cannot be combined with --plan")
			}
			r, err := allProjectActions(e.Config, *config, project, filter, limit)
			if err != nil {
				return err
			}
			if *asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(r)
			}
			renderActions(cmd.OutOrStdout(), r)
			return nil
		}
		p, err := disk.LoadPlan(e.Config, plan)
		if err != nil {
			return err
		}
		if _, err = projectActions(p, *config, project, filter, limit); err != nil {
			return err
		}
		if analyze {
			// Refresh exactly the selected largest matching candidates. The full
			// immutable plan is retained, not a filtered substitute for it.
			r, _ := projectActions(p, *config, project, filter, limit)
			if !*asJSON {
				fmt.Fprintln(cmd.ErrOrStderr(), "Analyzing project metadata (suggestions only). Ctrl+C cancels.")
			}
			ids := map[string]bool{}
			for _, item := range r.Projects {
				ids[item.ID] = true
			}
			p, err = e.AnalyzeSelected(cmd.Context(), p, ids)
			if err != nil {
				return err
			}
			if withAgent {
				p, err = e.TriageSelected(cmd.Context(), p, ids)
				if err != nil {
					return err
				}
			}
		}
		r, err := projectActions(p, *config, project, filter, limit)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(r)
		}
		renderActions(cmd.OutOrStdout(), r)
		return nil
	}}
	cmd.Flags().StringVar(&plan, "plan", "latest", "Exact saved plan ID")
	cmd.Flags().StringVar(&project, "project", "", "Select exactly one project ID")
	cmd.Flags().StringVar(&filter, "filter", "all", "all, ready or needs-analysis")
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum projects, largest first (1-5000)")
	if analyze {
		cmd.Flags().BoolVar(&withAgent, "agent", false, "Ask Pi to prioritize the metadata-backed suggestions (explicit paid run)")
	}
	if !analyze {
		cmd.Flags().BoolVar(&allPlans, "all-plans", false, "Combine saved batches for review; newest per path, original approval digests retained")
	}
	return cmd
}
