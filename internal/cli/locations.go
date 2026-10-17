package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/omniaura/infinite-disk/internal/disk"
	"github.com/spf13/cobra"
)

func locationsCommand(load func() (*disk.Engine, error), configPath *string, asJSON *bool) *cobra.Command {
	var add bool
	cmd := &cobra.Command{Use: "locations", Short: "Find coding-app worktree locations and show scan coverage", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		rows, warnings := disk.LocationsContext(cmd.Context(), e.Config, home, true)
		if err := cmd.Context().Err(); err != nil {
			return err
		}
		if add {
			e.Config, err = disk.AddLocations(e.Config, rows)
			if err != nil {
				return err
			}
			if err = disk.SaveConfig(*configPath, e.Config); err != nil {
				return err
			}
			rows, _ = disk.LocationsContext(cmd.Context(), e.Config, home, false)
		}
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				Locations []disk.Location `json:"locations"`
				Warnings  []string        `json:"warnings"`
				Saved     bool            `json:"saved"`
			}{rows, warnings, add})
		}
		fmt.Fprintln(styled(cmd.OutOrStdout(), toneHeading), "\nCODING-APP WORKTREE LOCATIONS")
		for _, row := range rows {
			color := toneWarning
			if row.Status == "included" {
				color = toneReady
			}
			fmt.Fprintf(styled(cmd.OutOrStdout(), color), "\n  %-19s %s\n", plain(row.Provider), plain(row.Status))
			wrapped(cmd.OutOrStdout(), "    ", row.Path)
		}
		for _, warning := range warnings {
			wrapped(styled(cmd.OutOrStdout(), toneWarning), "! ", warning)
		}
		if add {
			fmt.Fprintln(cmd.OutOrStdout(), "\nSaved existing locations. Old approvals are invalidated; run scan again.")
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "\nNothing changed. Add existing missing locations: infinite-disk locations --add")
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Not present is not the same as empty. Custom locations need explicit roots.")
		return nil
	}}
	cmd.Flags().BoolVar(&add, "add", false, "Save discovered existing roots to config; invalidates previous approval plans")
	return cmd
}
