package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/omniaura/infinite-disk/internal/disk"
	"github.com/spf13/cobra"
)

func cacheCommand(load func() (*disk.Engine, error), asJSON *bool) *cobra.Command {
	root := &cobra.Command{Use: "caches", Short: "Kondo-powered artifact cleanup; keep source and checkouts"}
	var artifactRoot, artifactPath, disposition string
	planArtifact := &cobra.Command{Use: "plan", Short: "Review one ignored binary or Kondo directory subset; never delete", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := e.PlanBinaryArtifact(cmd.Context(), artifactRoot, artifactPath, disposition)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(p)
		}
		renderCaches(cmd.OutOrStdout(), p)
		fmt.Fprintf(cmd.OutOrStdout(), "Exact plan: %s\n", p.ID)
		return nil
	}}
	planArtifact.Flags().StringVar(&artifactRoot, "project", "", "Absolute Git checkout root (required)")
	planArtifact.Flags().StringVar(&artifactPath, "path", "", "Exact project-relative binary or Kondo artifact subdirectory (required)")
	planArtifact.Flags().StringVar(&disposition, "disposition", "keep", "Intended action: keep, archive or discard; bound to the plan")
	_ = planArtifact.MarkFlagRequired("project")
	_ = planArtifact.MarkFlagRequired("path")
	scan := &cobra.Command{Use: "scan", Short: "Use Kondo dry-run to discover build/dependency artifacts; delete nothing", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		if !*asJSON {
			fmt.Fprintln(cmd.ErrOrStderr(), "Scanning build artifacts with Kondo (report only; up to five minutes). Ctrl+C cancels.")
		}
		p, err := e.CacheScan(cmd.Context())
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(p)
		}
		renderCaches(cmd.OutOrStdout(), p)
		return nil
	}}
	var reviewPlan string
	review := &cobra.Command{Use: "review", Short: "Show an exact saved artifact report", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := disk.LoadPlan(e.CacheConfig(), reviewPlan)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(p)
		}
		renderCaches(cmd.OutOrStdout(), p)
		return nil
	}}
	review.Flags().StringVar(&reviewPlan, "plan", "latest", "Exact saved artifact plan ID")
	var project, planID, approval string
	var nonInteractive bool
	apply := &cobra.Command{Use: "apply", Short: "Approve one exact artifact choice; never remove source checkouts", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := disk.LoadPlan(e.CacheConfig(), planID)
		if err != nil {
			return err
		}
		if approval != "" && (project == "" || approval != p.ID) {
			return errors.New("non-interactive approval requires one --project and the exact cache --approve-plan digest")
		}
		if nonInteractive && approval == "" {
			return errors.New("--non-interactive requires --project and exact cache --approve-plan; inspect caches review --json first")
		}
		if project != "" {
			found := false
			for _, c := range p.Candidates {
				found = found || c.ID == project
				if c.ID == project && !c.Eligible {
					return errors.New("selected artifact is blocked; run caches scan for fresh evidence")
				}
			}
			if !found {
				return errors.New("artifact ID is not in this report")
			}
		}
		reader := bufio.NewReader(cmd.InOrStdin())
		results := []disk.Receipt{}
		for _, c := range p.Candidates {
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			if !c.Eligible || (project != "" && c.ID != project) {
				continue
			}
			if approval != p.ID {
				fmt.Fprintln(cmd.ErrOrStderr())
				if c.Kind == "binary-artifact" {
					wrapped(styled(cmd.ErrOrStderr(), toneHeading), "  ", fmt.Sprintf("Likely rebuildable binary: %s (%s)", c.Path, size(c.SizeBytes)))
					fmt.Fprintf(cmd.ErrOrStderr(), "Planned choice: %s. Classification is a hint, not proof this file is disposable.\n", c.ArtifactDisposition)
					if c.ArtifactDisposition == "archive" {
						fmt.Fprintln(cmd.ErrOrStderr(), "A private verified local copy stays; this does not reclaim the file's bytes.")
					}
					if c.ArtifactDisposition == "discard" {
						fmt.Fprintln(styled(cmd.ErrOrStderr(), toneDanger), "Permanently delete this exact file, without a backup. Rebuild it later.")
					}
					fmt.Fprint(styled(cmd.ErrOrStderr(), tonePrompt), "Approve this exact choice? [y/N] (q quits, Ctrl+C cancels) ")
				} else {
					wrapped(styled(cmd.ErrOrStderr(), toneHeading), "  ", fmt.Sprintf("Remove %s of regenerable artifacts: %s", size(c.SizeBytes), c.Path))
					if c.ArtifactPolicy == "discard_node_modules" {
						fmt.Fprintln(cmd.ErrOrStderr(), "Disposable node_modules: delete without a backup, including installed package fixtures. Tracked reinstall inputs stay.")
					}
					fmt.Fprintln(cmd.ErrOrStderr(), "Source and checkout stay. No commits or uploads. Reinstall/rebuild later.")
					fmt.Fprint(styled(cmd.ErrOrStderr(), tonePrompt), "Delete this artifact directory? [y/N] (q quits, Ctrl+C cancels) ")
				}
				line, err := readLine(cmd.Context(), reader)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return err
				}
				answer := strings.ToLower(strings.TrimSpace(line))
				if answer == "q" || answer == "quit" {
					break
				}
				if answer != "y" && answer != "yes" {
					continue
				}
			}
			receipt, err := e.ApplyCache(cmd.Context(), p, c.ID, p.ID)
			results = append(results, receipt)
			if err != nil {
				if *asJSON {
					_ = json.NewEncoder(cmd.OutOrStdout()).Encode(results)
				}
				return err
			}
			if !*asJSON {
				fmt.Fprintf(styled(cmd.OutOrStdout(), toneReady), "%s. Disk free: %s\n", map[string]string{"artifact_kept": "Kept artifact unchanged", "artifact_archived": "Archived artifact locally; zero file bytes reclaimed", "artifact_discarded": "Discarded exact approved binary", "cache_removed": "Removed artifact directory"}[receipt.Result], size(receipt.AfterSpace.FreeBytes))
			}
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(results)
		}
		return nil
	}}
	apply.Flags().StringVar(&project, "project", "", "Review just one artifact ID")
	apply.Flags().StringVar(&planID, "plan", "latest", "Exact saved cache plan ID")
	apply.Flags().StringVar(&approval, "approve-plan", "", "Explicit per-artifact approval of the exact cache plan; requires --project")
	apply.Flags().BoolVar(&nonInteractive, "non-interactive", false, "Never prompt; require exact per-artifact approval")
	var restorePlan, restoreID, restoreApproval string
	restore := &cobra.Command{Use: "restore", Short: "Restore an archived build binary without overwriting; retain archive", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := e.LoadArtifactRestorePlan(restorePlan, restoreID)
		if err != nil {
			return err
		}
		if err = e.RestoreBinaryArtifact(cmd.Context(), p, restoreID, restoreApproval); err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"version": 1, "result": "artifact_restored", "archive_retained": true})
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Artifact restored and verified. Archive retained; no existing path overwritten.")
		return nil
	}}
	restore.Flags().StringVar(&restorePlan, "plan", "", "Exact saved artifact plan (required)")
	_ = restore.MarkFlagRequired("plan")
	restore.Flags().StringVar(&restoreID, "project", "", "Exact artifact ID (required)")
	_ = restore.MarkFlagRequired("project")
	restore.Flags().StringVar(&restoreApproval, "approve-plan", "", "Explicit approval of this exact restore plan")
	root.AddCommand(scan, review, apply, planArtifact, restore)
	return root
}
func renderCaches(w io.Writer, p disk.Plan) {
	fmt.Fprintln(styled(w, toneHeading), "\nBUILD / DEPENDENCY ARTIFACTS  |  Report only")
	var ready, blocked uint64
	for _, c := range p.Candidates {
		if c.Eligible && (c.Kind != "binary-artifact" || c.ArtifactDisposition == "discard") {
			ready += c.SizeBytes
		} else if !c.Eligible {
			blocked += c.SizeBytes
		}
	}
	fmt.Fprintf(w, "Ready for approval: %s. Needs review: %s (not approved savings).\n", size(ready), size(blocked))
	candidates := append([]disk.Candidate{}, p.Candidates...)
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].SizeBytes > candidates[j].SizeBytes })
	for _, c := range candidates {
		status := "REVIEW"
		color := toneWarning
		if c.Eligible {
			status = "READY"
			color = toneReady
		}
		fmt.Fprintf(styled(w, color), "\n  %9s  %-6s  %s\n", size(c.SizeBytes), status, plain(c.Name))
		wrapped(w, "    ", c.Path)
		if c.ArtifactPolicy == "discard_node_modules" {
			wrapped(w, "    ", "Disposable dependencies — no backup. Install age is informational; live-use and preservation boundaries still checked.")
		}
		if c.Kind == "binary-artifact" {
			wrapped(w, "    ", "Likely rebuildable binary — planned choice: "+c.ArtifactDisposition+". Requires exact approval; source and sessions stay.")
			if c.ArtifactDisposition == "archive" {
				fmt.Fprintf(w, "    %s retained locally; zero file bytes reclaimed.\n", size(c.SizeBytes))
			}
			if c.ArtifactDisposition == "keep" {
				fmt.Fprintln(w, "    File stays unchanged; zero bytes reclaimed.")
			}
		}
		for _, b := range c.Blockers {
			wrapped(styled(w, toneWarning), "    ", blockerText(b))
		}
		if c.Eligible {
			fmt.Fprintf(w, "    Select: --project %s\n", c.ID)
		}
	}
	for _, warning := range p.Warnings {
		wrapped(styled(w, toneWarning), "! ", warning)
	}
	fmt.Fprintln(w, "\nNothing deleted. Source, Git history and checkouts will stay.")
	fmt.Fprintln(w, "Review exact directories individually: infinite-disk caches apply")
}
