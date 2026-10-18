package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/omniaura/infinite-disk/internal/disk"
	"github.com/spf13/cobra"
)

// Rotating audits save different batches. Listing exact digests keeps older
// candidates addressable instead of silently pretending latest is all projects.
func plansCommand(load func() (*disk.Engine, error), asJSON *bool) *cobra.Command {
	return &cobra.Command{Use: "plans", Short: "List saved audit batches and exact digests; latest is not a whole-disk report", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(filepath.Join(e.Config.StateDir, "plans"))
		if err != nil {
			return err
		}
		type summary struct {
			ID        string    `json:"id"`
			CreatedAt time.Time `json:"created_at"`
			Projects  int       `json:"projects"`
			Ready     int       `json:"ready"`
		}
		rows := []summary{}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			p, err := disk.LoadPlan(e.Config, strings.TrimSuffix(entry.Name(), ".json"))
			if err != nil {
				continue
			}
			r := summary{ID: p.ID, CreatedAt: p.CreatedAt, Projects: len(p.Candidates)}
			for _, c := range p.Candidates {
				if c.Eligible {
					r.Ready++
				}
			}
			rows = append(rows, r)
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt.After(rows[j].CreatedAt) })
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"version": 1, "plans": rows})
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Saved audit batches (may overlap; do not sum their savings):")
		for _, r := range rows {
			fmt.Fprintf(cmd.OutOrStdout(), "%s  %d projects, %d ready  %s\n", r.CreatedAt.Local().Format("Jan 2 15:04"), r.Projects, r.Ready, r.ID)
		}
		return nil
	}}
}
