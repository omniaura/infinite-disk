package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/omniaura/infinite-disk/internal/disk"
	"github.com/spf13/cobra"
	"os"
)

func privateCommand(load func() (*disk.Engine, error), asJSON *bool) *cobra.Command {
	root := &cobra.Command{Use: "private", Short: "Plan, explicitly archive, or restore ignored/private files locally; never push them"}
	var source, project string
	plan := &cobra.Command{Use: "plan", Short: "Inventory exact private paths and hashes; move nothing", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := disk.LoadPlan(e.Config, source)
		if err != nil {
			return err
		}
		private, err := e.PlanPrivate(cmd.Context(), p, project)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(private)
		}
		renderPrivate(cmd, private)
		return nil
	}}
	plan.Flags().StringVar(&source, "plan", "latest", "Saved source report ID")
	plan.Flags().StringVar(&project, "project", "", "Exactly one project ID (required)")
	_ = plan.MarkFlagRequired("project")
	for _, name := range []string{"review", "apply", "restore"} {
		var id, approval string
		action := name
		command := &cobra.Command{Use: action, Short: map[string]string{"review": "Review an exact private archive plan", "apply": "Approve relocation of exact ignored files into verified local recovery; keep checkout", "restore": "Copy private files back to original checkout; retain archive and never overwrite"}[action], RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load()
			if err != nil {
				return err
			}
			p, err := e.LoadPrivate(id)
			if err != nil {
				return err
			}
			if action == "review" {
				r, receiptErr := e.PrivateReceipt(id)
				var receipt *disk.PrivateArchive
				if receiptErr == nil {
					receipt = &r
				} else if !errors.Is(receiptErr, os.ErrNotExist) {
					return receiptErr
				}
				if *asJSON {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
						Version int                  `json:"version"`
						Plan    disk.PrivatePlan     `json:"plan"`
						Receipt *disk.PrivateArchive `json:"receipt,omitempty"`
					}{1, p, receipt})
				}
				renderPrivate(cmd, p)
				if receipt != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "  Archive status: %s; %d roots verified.\n", plain(receipt.Status), len(receipt.Verified))
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "  Archive status: not attempted; source paths unchanged.")
				}
				return nil
			}
			if action == "restore" {
				if err = e.RestorePrivate(cmd.Context(), id, approval); err != nil {
					return err
				}
				if *asJSON {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"version": 1, "plan_id": id, "result": "restored_without_overwrite", "archive_retained": true})
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Private files restored or already identical. Original archive retained.")
				return nil
			}
			receipt, err := e.ArchivePrivate(cmd.Context(), id, approval)
			if *asJSON && receipt.Version == 1 {
				_ = json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
			}
			if err != nil {
				return err
			}
			if !*asJSON {
				fmt.Fprintln(cmd.OutOrStdout(), "Private archive verified locally. No source pushed or checkout removed.")
				fmt.Fprintln(cmd.OutOrStdout(), "Next: analyze this project again; checkout removal still needs separate exact approval.")
			}
			return nil
		}}
		command.Flags().StringVar(&id, "plan", "", "Exact private archive plan ID (required)")
		_ = command.MarkFlagRequired("plan")
		if action != "review" {
			command.Flags().StringVar(&approval, "approve-plan", "", "Explicit approval of this exact one-project private archive plan")
		}
		root.AddCommand(command)
	}
	root.AddCommand(plan)
	var pruneArchive, pruneApproval string
	var pruneEntries []string
	prune := &cobra.Command{Use: "prune", Short: "Review or explicitly discard exact regenerable entries from a legacy private archive", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		var p disk.PrivatePrunePlan
		if pruneApproval == "" {
			p, err = e.PlanPrivatePrune(cmd.Context(), pruneArchive, pruneEntries)
		} else {
			p, err = e.PrunePrivate(cmd.Context(), pruneArchive, pruneEntries, pruneApproval)
		}
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(p)
		}
		if pruneApproval == "" {
			fmt.Fprintln(cmd.OutOrStdout(), "REPORT ONLY — exact archive entries proposed for permanent discard:")
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "Approved regenerable entries removed. Unique work and sessions were not targeted.")
		}
		for _, entry := range p.Entries {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s\n", size(entry.Entry.Bytes), plain(entry.Entry.Path))
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Retention plan: %s\n", p.ID)
		fmt.Fprintln(cmd.OutOrStdout(), "Recovery metadata retains the original plan and explicit discard receipt. Other private entries remain restorable.")
		if pruneApproval == "" {
			fmt.Fprintln(cmd.OutOrStdout(), "Review these paths before repeating with --approve-plan and the exact retention plan ID.")
		}
		return nil
	}}
	prune.Flags().StringVar(&pruneArchive, "plan", "", "Exact existing private archive ID")
	prune.Flags().StringArrayVar(&pruneEntries, "entry", nil, "Exact archived regenerable entry; repeat to select multiple roots")
	prune.Flags().StringVar(&pruneApproval, "approve-plan", "", "Explicit approval of the exact retention plan ID; omitted means report-only")
	_ = prune.MarkFlagRequired("plan")
	_ = prune.MarkFlagRequired("entry")
	root.AddCommand(prune)
	return root
}
func renderPrivate(cmd *cobra.Command, p disk.PrivatePlan) {
	w := cmd.OutOrStdout()
	fmt.Fprintln(styled(w, toneHeading), "\nPRIVATE RECOVERY  |  Separate approval required")
	wrapped(w, "  ", p.Candidate.Path)
	fmt.Fprintf(w, "  %d exact roots; %s retained locally (not off-device backup).\n", len(p.Entries), size(p.Bytes))
	for _, entry := range p.Entries {
		wrapped(w, "    ", entry.Path+" — "+size(entry.Bytes))
	}
	fmt.Fprintln(w, "  Apply moves only these ignored/private paths to a verified local archive.")
	fmt.Fprintln(w, "  Source, Git history and external sessions stay; no files are uploaded.")
	fmt.Fprintln(w, "  Do not remove the archive wholesale. Use private prune to review regenerable entries.")
	fmt.Fprintln(w, "  Restore copies retained private work back without overwriting.")
	fmt.Fprintf(w, "  Private plan: %s\n", p.ID)
	fmt.Fprintf(w, "  Approve: infinite-disk private apply --plan %s --approve-plan %s\n", p.ID, p.ID)
	fmt.Fprintln(w, "  Use the same --config for every command. Reanalyze after archiving.")
}
