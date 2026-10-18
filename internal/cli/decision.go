package cli

import (
	"encoding/json"
	"fmt"
	"github.com/omniaura/infinite-disk/internal/disk"
	"github.com/spf13/cobra"
)

func decisionCommand(load func() (*disk.Engine, error), asJSON *bool) *cobra.Command {
	var source, key string
	var projects []string
	cmd := &cobra.Command{Use: "decide", Short: "EXPERIMENT: one report-only Jev/OpenRouter typed decision request; never applies actions", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := disk.LoadPlan(e.Config, source)
		if err != nil {
			return err
		}
		var selected map[string]bool
		if len(projects) > 0 {
			selected = map[string]bool{}
			for _, id := range projects {
				selected[id] = true
			}
		}
		r, err := e.Decide(cmd.Context(), p, selected, key)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(r)
		}
		w := cmd.OutOrStdout()
		fmt.Fprintln(styled(w, toneHeading), "\nSYSTEM ONE EXPERIMENT | Suggestions only")
		fmt.Fprintf(w, "OpenRouter / %s · %d request · %.2fs · %d project decisions\n", r.ServedModel, r.Requests, float64(r.DurationMS)/1000, len(r.Answers))
		fmt.Fprintf(w, "Reported tokens: %d input, %d output.\n", r.InputTokens, r.OutputTokens)
		if r.CostUSD != nil {
			fmt.Fprintf(w, "Provider-reported cost: $%.8f (not an independent settlement audit).\n", *r.CostUSD)
		} else {
			fmt.Fprintln(w, "Cost unavailable; not assumed free.")
		}
		for _, a := range r.Answers {
			for _, c := range p.Candidates {
				if c.ID == a.ProjectID {
					wrapped(w, "  ", label(c)+": "+a.Suggested)
					break
				}
			}
			fmt.Fprintf(w, "    Model choice %s; confidence %.2f; choice probability %.2f.\n", a.Choice, a.Confidence, a.Probabilities[a.Choice])
		}
		fmt.Fprintln(w, "No explanations generated, tools invoked, approvals granted, or actions applied. Original approval plan unchanged.")
		return nil
	}}
	cmd.Flags().StringVar(&source, "plan", "latest", "Exact saved source report; approval plan stays unchanged")
	cmd.Flags().StringVar(&key, "key-file", "", "Explicit owner-only OpenRouter key file (required); never stored in reports")
	_ = cmd.MarkFlagRequired("key-file")
	cmd.Flags().StringSliceVar(&projects, "project", nil, "At most 20 exact project IDs; default 20 largest metadata records")
	return cmd
}
