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
	scan := &cobra.Command{Use: "scan", Short: "Use Kondo dry-run to discover build/dependency artifacts; delete nothing", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		if !*asJSON {
			fmt.Fprintln(cmd.ErrOrStderr(), "Scanning build artifacts with Kondo (report only). Ctrl+C cancels.")
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
	review := &cobra.Command{Use: "review", Short: "Show the latest Kondo artifact report", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := disk.LoadPlan(e.CacheConfig(), "latest")
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(p)
		}
		renderCaches(cmd.OutOrStdout(), p)
		return nil
	}}
	var project string
	apply := &cobra.Command{Use: "apply", Short: "Approve artifact directories one at a time; never remove source checkouts", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := disk.LoadPlan(e.CacheConfig(), "latest")
		if err != nil {
			return err
		}
		if project != "" {
			found := false
			for _, c := range p.Candidates {
				found = found || c.ID == project
			}
			if !found {
				return errors.New("artifact ID is not in this report")
			}
		}
		reader := bufio.NewReader(cmd.InOrStdin())
		results := []disk.Receipt{}
		for _, c := range p.Candidates {
			if !c.Eligible || (project != "" && c.ID != project) {
				continue
			}
			fmt.Fprintln(cmd.ErrOrStderr())
			wrapped(cmd.ErrOrStderr(), "  ", fmt.Sprintf("Remove %s of regenerable artifacts: %s", size(c.SizeBytes), c.Path))
			fmt.Fprintln(cmd.ErrOrStderr(), "Source and checkout stay. No commits or uploads. Reinstall/rebuild later.")
			fmt.Fprint(cmd.ErrOrStderr(), "Delete this artifact directory? [y/N] (q quits, Ctrl+C cancels) ")
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
			receipt, err := e.ApplyCache(cmd.Context(), p, c.ID, p.ID)
			results = append(results, receipt)
			if err != nil {
				return err
			}
			if !*asJSON {
				fmt.Fprintf(cmd.OutOrStdout(), "Removed artifact. Disk free: %s\n", size(receipt.AfterSpace.FreeBytes))
			}
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(results)
		}
		return nil
	}}
	apply.Flags().StringVar(&project, "project", "", "Review just one artifact ID")
	root.AddCommand(scan, review, apply)
	return root
}
func renderCaches(w io.Writer, p disk.Plan) {
	fmt.Fprintln(w, "\nBUILD / DEPENDENCY ARTIFACTS  |  Kondo dry-run")
	var ready, blocked uint64
	for _, c := range p.Candidates {
		if c.Eligible {
			ready += c.SizeBytes
		} else {
			blocked += c.SizeBytes
		}
	}
	fmt.Fprintf(w, "Ready for approval: %s. Needs review: %s (not approved savings).\n", size(ready), size(blocked))
	candidates := append([]disk.Candidate{}, p.Candidates...)
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].SizeBytes > candidates[j].SizeBytes })
	for _, c := range candidates {
		status := "REVIEW"
		if c.Eligible {
			status = "READY"
		}
		fmt.Fprintf(w, "\n  %9s  %-6s  %s\n", size(c.SizeBytes), status, plain(c.Name))
		wrapped(w, "    ", c.Path)
		for _, b := range c.Blockers {
			wrapped(w, "    ", blockerText(b))
		}
		if c.Eligible {
			fmt.Fprintf(w, "    Select: --project %s\n", c.ID)
		}
	}
	for _, warning := range p.Warnings {
		wrapped(w, "! ", warning)
	}
	fmt.Fprintln(w, "\nNothing deleted. Source, Git history and checkouts will stay.")
	fmt.Fprintln(w, "Review exact directories individually: infinite-disk caches apply")
}
