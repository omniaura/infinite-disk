package cli

import (
	"encoding/json"
	"fmt"

	"github.com/omniaura/infinite-disk/internal/disk"
	"github.com/spf13/cobra"
)

func recoveryCommand(load func() (*disk.Engine, error), asJSON *bool) *cobra.Command {
	root := &cobra.Command{Use: "recovery", Short: "Prove which recovery bundles a remote already covers; prune only proven ones"}
	verify := &cobra.Command{Use: "verify", Short: "Prove each recovery bundle's history is on a durable remote ref; delete nothing", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		r, err := e.VerifyRecoveryCoverage(cmd.Context())
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(r)
		}
		renderCoverage(cmd, r)
		return nil
	}}
	var report, approval string
	prune := &cobra.Command{Use: "prune", Short: "Re-prove and delete the covered bundles of one approved coverage report", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		res, err := e.PruneCoveredBundles(cmd.Context(), report, approval)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
		}
		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "Pruned %d bundle(s), freed %s; kept %d.\n", len(res.Bundles)-res.KeptBundles, size(res.FreedBytes), res.KeptBundles)
		for _, b := range res.Bundles {
			if b.Result == "kept" {
				wrapped(w, "  kept: ", b.Path+" — "+b.Reason)
			}
		}
		fmt.Fprintln(w, "Restore commands are in the prune receipt under recovery-coverage/.")
		return nil
	}}
	prune.Flags().StringVar(&report, "report", "", "Coverage report ID from recovery verify (required)")
	prune.Flags().StringVar(&approval, "approve-report", "", "Repeat the exact coverage report ID to approve deletion")
	_ = prune.MarkFlagRequired("report")
	_ = prune.MarkFlagRequired("approve-report")
	consolidate := &cobra.Command{Use: "consolidate", Short: "Fold unproven bundles into one restore-tested thin bundle per remote; delete nothing", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		res, err := e.ConsolidateRecovery(cmd.Context())
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
		}
		w := cmd.OutOrStdout()
		fmt.Fprintln(styled(w, toneHeading), "\nRECOVERY CONSOLIDATION  |  Nothing deleted")
		for _, b := range res.Bundles {
			wrapped(w, "  ", fmt.Sprintf("%s: %d bundle(s), %s -> %s thin bundle (restore-tested)", b.Remote, len(b.Sources), size(b.SourceBytes), size(b.Bytes)))
		}
		if len(res.Skipped) > 0 {
			fmt.Fprintf(w, "  %d bundle(s) skipped; see --json for reasons.\n", len(res.Skipped))
		}
		if len(res.Bundles) > 0 {
			fmt.Fprintln(w, "  Next: infinite-disk recovery verify, then prune the originals it reports covered.")
		}
		return nil
	}}
	root.AddCommand(verify, prune, consolidate)
	return root
}

func renderCoverage(cmd *cobra.Command, r disk.CoverageReport) {
	w := cmd.OutOrStdout()
	covered := 0
	reasons := map[string]int{}
	for _, b := range r.Bundles {
		if b.Status == disk.CoverageCovered {
			covered++
		} else {
			reasons[b.Reason]++
		}
	}
	fmt.Fprintln(styled(w, toneHeading), "\nRECOVERY COVERAGE  |  Nothing deleted")
	fmt.Fprintf(w, "  %d bundle(s): %d covered (%s), %d retained (%s).\n", len(r.Bundles), covered, size(r.CoveredBytes), len(r.Bundles)-covered, size(r.RetainedBytes))
	fmt.Fprintln(w, "  Covered means every bundle tip is reachable from the remote's default branch,")
	fmt.Fprintln(w, "  tags, pull-request heads or infinite-disk/recovery branches.")
	for reason, n := range reasons {
		wrapped(w, "    ", fmt.Sprintf("%d retained: %s", n, reason))
	}
	if covered > 0 {
		fmt.Fprintf(w, "  Coverage report: %s\n", r.ID)
		fmt.Fprintf(w, "  Approve: infinite-disk recovery prune --report %s --approve-report %s\n", r.ID, r.ID)
	}
}
