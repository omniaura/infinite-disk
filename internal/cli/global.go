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

func globalCommand(load func() (*disk.Engine, error), asJSON *bool) *cobra.Command {
	root := &cobra.Command{Use: "global", Short: "Report curated macOS caches and old installers; delete only after exact approval"}
	scan := &cobra.Command{Use: "scan", Short: "Save a report of global caches and Downloads installers; delete nothing", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		if !*asJSON {
			fmt.Fprintln(cmd.ErrOrStderr(), "Scanning known global caches and old installers (report only; up to five minutes).")
		}
		p, err := e.GlobalScan(cmd.Context())
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(p)
		}
		renderGlobal(cmd.OutOrStdout(), p)
		return nil
	}}
	var reviewPlan string
	review := &cobra.Command{Use: "review", Short: "Review a saved global cleanup report; apply nothing", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := disk.LoadPlan(e.GlobalConfig(), reviewPlan)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(p)
		}
		renderGlobal(cmd.OutOrStdout(), p)
		return nil
	}}
	review.Flags().StringVar(&reviewPlan, "plan", "latest", "Exact global report digest")
	var target, applyPlan, approval string
	var nonInteractive bool
	apply := &cobra.Command{Use: "apply", Short: "Approve one exact cache or installer target; preserve new or changed files", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if target == "" {
			return errors.New("select one --target from global review; there is no approve-all")
		}
		e, err := load()
		if err != nil {
			return err
		}
		p, err := disk.LoadPlan(e.GlobalConfig(), applyPlan)
		if err != nil {
			return err
		}
		if approval != "" && approval != p.ID {
			return errors.New("global approval must match the exact saved report digest")
		}
		if (nonInteractive || *asJSON) && approval == "" {
			return errors.New("non-interactive/JSON global apply requires one --target and exact --approve-plan digest")
		}
		var selected *disk.Candidate
		for _, c := range p.Candidates {
			if c.ID == target {
				copy := c
				selected = &copy
			}
		}
		if selected == nil || !selected.Eligible {
			return errors.New("selected global target is missing or blocked; rescan")
		}
		if approval == "" {
			wrapped(styled(cmd.ErrOrStderr(), toneWarning), "", fmt.Sprintf("Permanently remove approved files in %s (%s allocated estimate)?", selected.Path, size(selected.SizeBytes)))
			fmt.Fprintln(cmd.ErrOrStderr(), "No backup is created. Cache directories, newly generated files, worktrees and app data stay.")
			if selected.Kind == "global-installer" {
				fmt.Fprintln(cmd.ErrOrStderr(), "Confirm this downloaded image/package is disposable; its contents are not inspected.")
			}
			fmt.Fprint(styled(cmd.ErrOrStderr(), tonePrompt), "Approve this exact target? [y/N] ")
			line, err := readLine(cmd.Context(), bufio.NewReader(cmd.InOrStdin()))
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			answer := strings.ToLower(strings.TrimSpace(line))
			if answer != "y" && answer != "yes" {
				return nil
			}
		}
		r, err := e.ApplyGlobal(cmd.Context(), p, target, p.ID)
		if *asJSON {
			if writeErr := json.NewEncoder(cmd.OutOrStdout()).Encode(r); writeErr != nil {
				return writeErr
			}
		} else if err == nil {
			fmt.Fprintf(cmd.OutOrStdout(), "Approved files removed. Disk free now: %s. Rebuild/download caches when needed.\n", size(r.AfterSpace.FreeBytes))
		}
		return err
	}}
	apply.Flags().StringVar(&applyPlan, "plan", "latest", "Exact saved global report digest")
	apply.Flags().StringVar(&target, "target", "", "One target ID from the report (required)")
	apply.Flags().StringVar(&approval, "approve-plan", "", "Explicit approval of this exact report and selected target")
	apply.Flags().BoolVar(&nonInteractive, "non-interactive", false, "Never prompt; require an explicit target and exact approval digest")
	root.AddCommand(scan, review, apply)
	return root
}

func renderGlobal(w io.Writer, p disk.Plan) {
	fmt.Fprintln(styled(w, toneHeading), "\nGLOBAL CACHES / INSTALLERS | report only")
	var ready uint64
	for _, c := range p.Candidates {
		if c.Eligible {
			ready += c.SizeBytes
		}
	}
	fmt.Fprintf(w, "Inspected %d targets. Ready for approval: %s allocated estimate (not guaranteed free space).\n", len(p.Candidates), size(ready))
	candidates := append([]disk.Candidate{}, p.Candidates...)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].SizeBytes > candidates[j].SizeBytes })
	for _, c := range candidates {
		status := "REVIEW"
		tone := toneWarning
		if c.Eligible {
			status = "READY"
			tone = toneReady
		}
		fmt.Fprintf(styled(w, tone), "\n%s %s %s\n", status, size(c.SizeBytes), plain(c.Name))
		wrapped(w, "  ", c.Path)
		for _, b := range c.Blockers {
			wrapped(w, "  ", blockerText(b))
		}
		if c.Eligible {
			fmt.Fprintf(w, "  Approve: infinite-disk global apply --plan %s --target %s\n", p.ID, c.ID)
		}
	}
	for _, warning := range p.Warnings {
		wrapped(styled(w, toneWarning), "! ", warning)
	}
	fmt.Fprintln(w, "\nNothing deleted. Each target needs separate approval; there is no approve-all.")
}
