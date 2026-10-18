package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/omniaura/infinite-disk/internal/disk"
	"github.com/spf13/cobra"
)

func New(version string) *cobra.Command {
	var configPath string
	var asJSON bool
	var colorMode string
	root := &cobra.Command{Use: "infinite-disk", Short: "Approval-first disk and developer-workspace steward", Version: version, SilenceUsage: true}
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if colorMode != "auto" && colorMode != "always" && colorMode != "never" {
			return errors.New("--color must be auto, always or never")
		}
		cmd.SetOut(terminalOutput(cmd.OutOrStdout(), colorMode, asJSON))
		cmd.SetErr(terminalOutput(cmd.ErrOrStderr(), colorMode, asJSON))
		return nil
	}
	root.PersistentFlags().StringVar(&configPath, "config", disk.DefaultConfigPath(), "Configuration file (never contains provider keys)")
	root.PersistentFlags().BoolVar(&asJSON, "json", false, "Emit versioned JSON for Desktop and automation")
	root.PersistentFlags().StringVar(&colorMode, "color", "auto", "Terminal colors: auto, always or never (JSON is always plain)")
	load := func() (*disk.Engine, error) {
		c, err := disk.LoadConfig(configPath)
		if err != nil {
			return nil, err
		}
		return disk.NewEngine(c), nil
	}
	print := func(cmd *cobra.Command, value any) error {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	}
	var withAgent bool
	scan := &cobra.Command{Use: "scan", Aliases: []string{"run"}, Short: "Report recommendations; never commit, push, or delete", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		if !asJSON {
			fmt.Fprintln(cmd.ErrOrStderr(), "Scanning checkouts (report only). May take up to five minutes; Ctrl+C cancels.")
		}
		p, err := e.RunReport(cmd.Context())
		if err != nil {
			return err
		}
		if withAgent {
			p, err = e.Triage(cmd.Context(), p)
			if err != nil {
				return err
			}
		}
		if asJSON {
			return print(cmd, p)
		}
		renderPlan(cmd.OutOrStdout(), p, false)
		return nil
	}}
	scan.Flags().BoolVar(&withAgent, "agent", false, "Use the bounded, metadata-only Pi worker (requires setup and OpenRouter key)")
	root.AddCommand(scan)
	var planID string
	var details bool
	var reviewProject string
	review := &cobra.Command{Use: "review", Short: "Review a saved report and the checks actually performed", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := disk.LoadPlan(e.Config, planID)
		if err != nil {
			return err
		}
		if reviewProject != "" {
			var selected []disk.Candidate
			for _, candidate := range p.Candidates {
				if candidate.ID == reviewProject {
					selected = append(selected, candidate)
				}
			}
			if len(selected) == 0 {
				return errors.New("project ID is not in this plan")
			}
			p.Candidates = selected
		}
		if asJSON {
			return print(cmd, p)
		}
		renderPlan(cmd.OutOrStdout(), p, details)
		if home, err := os.UserHomeDir(); err == nil {
			if current, err := disk.DiskSpace(home); err == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Current disk free: %s\n", size(current.FreeBytes))
			}
		}
		return nil
	}}
	review.Flags().StringVar(&planID, "plan", "latest", "Saved immutable plan ID")
	review.Flags().BoolVar(&details, "details", false, "Show evidence and proposed future steps")
	review.Flags().StringVar(&reviewProject, "project", "", "Show just one project ID from this report")
	root.AddCommand(review)
	var applyPlan, project, approval string
	var preserveOnly bool
	var reviewDetails bool
	apply := &cobra.Command{Use: "apply", Short: "Approve projects individually, preserve and verify, then optionally remove", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := disk.LoadPlan(e.Config, applyPlan)
		if err != nil {
			return err
		}
		if approval != "" && (project == "" || approval != p.ID) {
			return errors.New("non-interactive approval requires one --project and the exact --approve-plan digest")
		}
		if project != "" {
			found := false
			for _, c := range p.Candidates {
				if c.ID == project {
					found = true
				}
			}
			if !found {
				return errors.New("project ID is not in this plan")
			}
		}
		for _, c := range p.Candidates {
			if c.Eligible && !c.SessionsChecked && (project == "" || project == c.ID) {
				return errors.New("saved report predates coding-session checks; run scan before approving checkout removal")
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
			approved := approval == p.ID
			if !approved {
				renderApproval(cmd.ErrOrStderr(), c, preserveOnly)
				if reviewDetails {
					for _, v := range c.Evidence {
						renderEvidence(cmd.ErrOrStderr(), v)
					}
					for _, recommendation := range p.Recommendations {
						if recommendation.CandidateID == c.ID {
							wrapped(cmd.ErrOrStderr(), "  ", "Pi advisory: "+recommendation.Decision+". "+recommendation.Reason)
							for _, step := range recommendation.ProposedSteps {
								wrapped(cmd.ErrOrStderr(), "    ", "Proposed, not performed: "+step)
							}
						}
					}
				}
				action := "Create verified recovery only"
				if !preserveOnly {
					action = "Permanently remove this entire checkout"
				}
				fmt.Fprintf(styled(cmd.ErrOrStderr(), tonePrompt), "Approve %s? [y/N] (q quits, Ctrl+C cancels) ", action)
				line, readErr := readLine(cmd.Context(), reader)
				if readErr != nil && !errors.Is(readErr, io.EOF) {
					return readErr
				}
				if errors.Is(readErr, io.EOF) || strings.EqualFold(strings.TrimSpace(line), "q") || strings.EqualFold(strings.TrimSpace(line), "quit") {
					fmt.Fprintln(cmd.ErrOrStderr(), "Review ended. No further projects will be changed.")
					break
				}
				approved = strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes")
			}
			if !approved {
				fmt.Fprintln(cmd.ErrOrStderr(), "Skipped; no changes.")
				continue
			}
			r, err := e.Apply(cmd.Context(), p, c.ID, p.ID, !preserveOnly)
			results = append(results, r)
			if err != nil {
				if asJSON {
					_ = print(cmd, results)
				}
				return err
			}
			if !asJSON {
				fmt.Fprintf(styled(cmd.OutOrStdout(), toneReady), "%s: %s; verified recovery %s\n", c.ID, r.Result, r.RecoveryRef)
				if len(r.SessionHandoffs) > 0 {
					native := 0
					for _, h := range r.SessionHandoffs {
						if h.Status == "native_verified" {
							native++
						}
					}
					fmt.Fprintf(styled(cmd.OutOrStdout(), toneReady), "  %d session histories preserved; %d native handoffs verified. Parent-workspace sessions stay in place.\n", len(r.SessionHandoffs), native)
				}
			}
		}
		if asJSON {
			return print(cmd, results)
		}
		return nil
	}}
	apply.Flags().StringVar(&applyPlan, "plan", "latest", "Saved immutable plan ID")
	apply.Flags().StringVar(&project, "project", "", "Select exactly one project ID")
	apply.Flags().StringVar(&approval, "approve-plan", "", "Explicit non-interactive approval of this exact plan; requires --project")
	apply.Flags().BoolVar(&preserveOnly, "preserve-only", false, "Preserve recovery without deleting the checkout")
	apply.Flags().BoolVar(&reviewDetails, "review", false, "Show full evidence before each approval prompt")
	root.AddCommand(apply)
	root.AddCommand(cacheCommand(load, &asJSON))
	root.AddCommand(locationsCommand(load, &configPath, &asJSON))
	root.AddCommand(sessionsCommand(load, &asJSON))
	var restorePlan, restoreProject, restoreTo string
	restore := &cobra.Command{Use: "restore", Short: "Restore approved work from a checksum-verified private bundle; never overwrite", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		if err = e.Restore(cmd.Context(), restorePlan, restoreProject, restoreTo); err != nil {
			return err
		}
		return print(cmd, map[string]any{"version": 1, "result": "restored", "destination": restoreTo})
	}}
	restore.Flags().StringVar(&restorePlan, "plan", "", "Receipt plan ID")
	restore.Flags().StringVar(&restoreProject, "project", "", "Receipt project ID")
	restore.Flags().StringVar(&restoreTo, "to", "", "New absolute destination (must not exist)")
	_ = restore.MarkFlagRequired("plan")
	_ = restore.MarkFlagRequired("project")
	_ = restore.MarkFlagRequired("to")
	root.AddCommand(restore)
	var recoverPlan, recoverProject string
	recoverCommand := &cobra.Command{Use: "recover", Short: "Restore an interrupted quarantine to its original path; never overwrite", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		if err := e.RecoverQuarantine(cmd.Context(), recoverPlan, recoverProject); err != nil {
			return err
		}
		return print(cmd, map[string]any{"version": 1, "result": "quarantine_restored"})
	}}
	recoverCommand.Flags().StringVar(&recoverPlan, "plan", "", "Receipt plan ID")
	recoverCommand.Flags().StringVar(&recoverProject, "project", "", "Receipt project ID")
	_ = recoverCommand.MarkFlagRequired("plan")
	_ = recoverCommand.MarkFlagRequired("project")
	root.AddCommand(recoverCommand)
	configuration := &cobra.Command{Use: "config", Short: "Initialize or inspect explicit discovery and safety settings"}
	root.AddCommand(configuration)
	var roots []string
	var idleDays int
	initConfig := &cobra.Command{Use: "init", Short: "Create a private report-only configuration", RunE: func(cmd *cobra.Command, _ []string) error {
		if _, err := os.Lstat(configPath); err == nil {
			return errors.New("configuration already exists; edit it explicitly")
		}
		c := disk.DefaultConfig()
		if len(roots) > 0 {
			c.Roots = roots
		}
		c.IdleDays = idleDays
		if err := disk.SaveConfig(configPath, c); err != nil {
			return err
		}
		if asJSON {
			return print(cmd, c)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Saved report-only configuration: %s\n", configPath)
		return nil
	}}
	initConfig.Flags().StringArrayVar(&roots, "root", nil, "Explicit project-parent directory; repeat for multiple roots")
	initConfig.Flags().IntVar(&idleDays, "idle-days", 7, "Minimum source inactivity before a project may be reviewed for removal")
	configuration.AddCommand(initConfig, &cobra.Command{Use: "show", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		return print(cmd, e.Config)
	}})
	agent := &cobra.Command{Use: "agent", Short: "Install or run the read-only Pi worker"}
	root.AddCommand(agent)
	agent.AddCommand(&cobra.Command{Use: "setup", Short: "Install the locked Pi SDK once, without package lifecycle scripts", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		if err = disk.SetupAgent(cmd.Context(), e.Config); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Pi worker installed; no credential was stored and no scheduled paid runs were enabled.")
		return nil
	}})
	var triagePlan string
	triage := &cobra.Command{Use: "triage", Short: "Ask Pi to review a saved metadata-only inventory", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		p, err := disk.LoadPlan(e.Config, triagePlan)
		if err != nil {
			return err
		}
		p, err = e.Triage(cmd.Context(), p)
		if err != nil {
			return err
		}
		if asJSON {
			return print(cmd, p)
		}
		renderPlan(cmd.OutOrStdout(), p, true)
		return nil
	}}
	triage.Flags().StringVar(&triagePlan, "plan", "latest", "Saved immutable plan ID")
	root.AddCommand(triage)
	var once bool
	watch := &cobra.Command{Use: "watch", Short: "Check disk pressure and periodically save reports; NEVER apply plans", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		return e.Watch(cmd.Context(), once, func(s disk.WatchState) {
			if asJSON {
				_ = print(cmd, s)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s report-only: %.2f GiB free; pressure=%t; plan=%s\n", s.CheckedAt.Format(time.RFC3339), float64(s.Space.FreeBytes)/(1<<30), s.Pressure, s.LastPlanID)
			}
		})
	}}
	watch.Flags().BoolVar(&once, "once", false, "Perform one throttled pressure/periodic check (for cron)")
	root.AddCommand(watch)
	root.AddCommand(&cobra.Command{Use: "status", Short: "Show watcher health, free space, and saved plan location", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		s, err := disk.ReadWatchState(e.Config)
		if err != nil {
			return errors.New("no watcher state yet; run watch --once")
		}
		return print(cmd, s)
	}})
	root.AddCommand(&cobra.Command{Use: "doctor", Short: "Check local prerequisites without modifying projects", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		checks := map[string]any{"version": version, "platform": runtime.GOOS, "mode": "report-only", "state_dir": e.Config.StateDir, "logs": filepath.Join(e.Config.StateDir, "logs", "events.jsonl"), "openrouter_key_present": os.Getenv("OPENROUTER_API_KEY") != ""}
		for _, name := range []string{"git", "lsof", "du", "node", "npm"} {
			_, err := exec.LookPath(name)
			checks[name+"_available"] = err == nil
		}
		home, _ := os.UserHomeDir()
		checks["space"], _ = disk.DiskSpace(home)
		return print(cmd, checks)
	}})
	var platform string
	var start bool
	service := &cobra.Command{Use: "service", Short: "Manage only Infinite Disk's native report-only monitor"}
	root.AddCommand(service)
	service.PersistentFlags().StringVar(&platform, "platform", "auto", "auto, darwin, linux, or cron")
	install := &cobra.Command{Use: "install", Short: "Install a stable binary and native report-only monitor", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		file, err := disk.InstallService(cmd.Context(), e.Config, configPath, platform, start)
		if err != nil {
			return err
		}
		return print(cmd, map[string]any{"mode": "report-only", "service": file, "start_requested": start, "paid_agent_enabled": e.Config.AgentEnabled})
	}}
	install.Flags().BoolVar(&start, "start", false, "Start the native monitor immediately")
	service.AddCommand(install, &cobra.Command{Use: "uninstall", Short: "Stop and remove only the managed native service; keep logs and recovery data", RunE: func(cmd *cobra.Command, _ []string) error { return disk.UninstallService(cmd.Context(), platform) }})
	return root
}

// A signal cancels the command context, not a blocking terminal ReadString.
// Keep at most one pending read; cancellation ends the command immediately.
func readLine(ctx context.Context, reader *bufio.Reader) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() { line, err := reader.ReadString('\n'); ch <- result{line, err} }()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return r.line, r.err
	}
}
