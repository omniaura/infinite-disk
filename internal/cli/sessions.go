package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/omniaura/infinite-disk/internal/disk"
	"github.com/spf13/cobra"
)

func sessionsCommand(load func() (*disk.Engine, error), asJSON *bool) *cobra.Command {
	root := &cobra.Command{Use: "sessions", Short: "Inspect checkout-linked sessions and recover orphaned native resume routes"}
	var orphaned, details bool
	var project string
	list := &cobra.Command{Use: "list", Short: "Read session metadata only; never start, move or delete a session", RunE: func(cmd *cobra.Command, _ []string) error {
		if project != "" && (!filepath.IsAbs(project) || filepath.Clean(project) != project) {
			return errors.New("--project must be an absolute, clean checkout path")
		}
		e, err := load()
		if err != nil {
			return err
		}
		inventory := e.SessionInventory(cmd.Context())
		if cmd.Context().Err() != nil {
			return cmd.Context().Err()
		}
		selected := []disk.Session{}
		for _, s := range inventory.Sessions {
			if orphaned && !s.Orphaned() || project != "" && !s.Matches(project) {
				continue
			}
			selected = append(selected, s)
		}
		inventory.Sessions = selected
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(inventory)
		}
		w := cmd.OutOrStdout()
		fmt.Fprintln(styled(w, toneHeading), "\nCODING SESSIONS  |  Metadata only")
		fmt.Fprintf(w, "%d matching sessions. No conversation contents included.\n", len(selected))
		limit := len(selected)
		if !details && limit > 30 {
			limit = 30
		}
		for _, s := range selected[:limit] {
			status := "CHECKOUT PRESENT"
			if s.Checkout == "" {
				status = "CHECKOUT UNKNOWN"
			} else if s.Orphaned() {
				status = "MISSING CHECKOUT"
			}
			fmt.Fprintf(styled(w, toneHeading), "\n  %s  %s\n", plain(s.Provider), plain(s.ID))
			wrapped(w, "    ", status+": "+s.Checkout)
			if details {
				wrapped(w, "    ", "Native storage: "+s.Store)
			}
		}
		if limit < len(selected) {
			fmt.Fprintf(w, "\n+ %d more; use --details or --project /absolute/checkout.\n", len(selected)-limit)
		}
		for _, warning := range inventory.Warnings {
			wrapped(styled(w, toneWarning), "! ", warning)
		}
		fmt.Fprintln(w, "\nNo history was deleted. Missing checkout does not prove history is missing.")
		return nil
	}}
	list.Flags().BoolVar(&orphaned, "orphans", false, "Only sessions whose recorded checkout is missing (unknown is not orphaned)")
	list.Flags().BoolVar(&details, "details", false, "Show all matching session IDs and native storage paths")
	list.Flags().StringVar(&project, "project", "", "Show sessions linked to this absolute checkout path")
	var provider, id, target string
	var launch bool
	reattach := &cobra.Command{Use: "reattach", Aliases: []string{"resume"}, Short: "Approve an orphan's reopening destination; optionally launch its native resume command", RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := load()
		if err != nil {
			return err
		}
		switch strings.ToLower(provider) {
		case "claude", "claude code":
			provider = "Claude Code"
		case "codex":
			provider = "Codex"
		}
		if target == "" {
			saved, err := e.LoadSessionRoute(provider, id)
			if err != nil {
				return err
			}
			target = saved.Destination
		}
		route, err := e.PrepareSessionResume(cmd.Context(), provider, id, target)
		if err != nil {
			return err
		}
		if *asJSON {
			if launch {
				return errors.New("JSON resume planning never launches a provider; use interactive review")
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(route)
		}
		if launch {
			if err := verifyNativeResume(cmd.Context(), route); err != nil {
				return err
			}
		}
		w := cmd.ErrOrStderr()
		fmt.Fprintf(styled(w, toneHeading), "\nReopen %s session %s\n", plain(route.Provider), plain(route.SessionID))
		wrapped(w, "  ", "Missing checkout: "+route.Original)
		wrapped(w, "  ", "Chosen project: "+route.Destination)
		wrapped(styled(w, toneWarning), "  ", "This resumes conversation history, NOT the old code. No branch switch, merge, or source restore. Confirm this is the correct repository; an orphan's original repository identity may no longer be verifiable.")
		wrapped(w, "  ", "Only a private Infinite Disk reopening route is saved. The agent owns its history and rebinding; app databases are never rewritten.")
		if launch {
			wrapped(styled(w, toneWarning), "  ", "Approval also starts the native agent here with its normal permissions and configuration; provider costs and native hooks may apply.")
		}
		fmt.Fprint(styled(w, tonePrompt), "Save this session's reopening destination? [y/N] (q quits, Ctrl+C cancels) ")
		answer, err := readLine(cmd.Context(), bufio.NewReader(oneByteInput{cmd.InOrStdin()}))
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if answer = strings.TrimSpace(answer); !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			fmt.Fprintln(w, "Skipped; no changes.")
			return nil
		}
		if err := e.SaveSessionRoute(cmd.Context(), route); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Saved native reopening route; session history and code were not moved.")
		if !launch {
			fmt.Fprintln(cmd.OutOrStdout(), "Add --launch to this command to start the native agent after review.")
			return nil
		}
		if err := e.ValidateSessionResume(cmd.Context(), route); err != nil {
			return err
		}
		// No shell interpolation, model prompt, bypass flag, or hidden tool grant.
		native := exec.CommandContext(cmd.Context(), route.Executable, route.Args...)
		native.Dir = route.Destination
		native.Stdin = cmd.InOrStdin()
		native.Stdout = unstyled(cmd.OutOrStdout())
		native.Stderr = unstyled(cmd.ErrOrStderr())
		if err := native.Run(); err != nil {
			return errors.New("native session exited unsuccessfully; history was not modified by Infinite Disk")
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Native session exited. Rebinding is owned by the provider, not verified by the saved route.")
		return nil
	}}
	reattach.Flags().StringVar(&provider, "provider", "", "Native provider: claude or codex (CLI sessions only)")
	reattach.Flags().StringVar(&id, "session", "", "Exact native session ID")
	reattach.Flags().StringVar(&target, "to", "", "Existing project checkout root; omit to reuse an approved saved route")
	reattach.Flags().BoolVar(&launch, "launch", false, "After per-session approval, start the native CLI in the chosen project")
	for _, flag := range []string{"provider", "session"} {
		_ = reattach.MarkFlagRequired(flag)
	}
	root.AddCommand(list, reattach)
	return root
}

func unstyled(w io.Writer) io.Writer {
	if terminal, ok := w.(*terminalWriter); ok {
		return terminal.Writer
	}
	return w
}

// Don't prefetch pasted lines intended for the native agent after approval.
// Pass the original file descriptor to the child so its TUI still gets a TTY.
type oneByteInput struct{ io.Reader }

func (r oneByteInput) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}

func verifyNativeResume(ctx context.Context, route disk.SessionResume) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	args := []string{"--version"}
	if route.Provider == "Codex" {
		args = []string{"resume", "--help"}
	}
	cmd := exec.CommandContext(ctx, route.Executable, args...)
	var out limitedCapabilityOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil || out.overflow {
		return errors.New("native resume capability check failed; no session was launched")
	}
	if route.Provider == "Claude Code" {
		match := regexp.MustCompile(`^2\.1\.([0-9]+)(?:\s|$)`).FindStringSubmatch(strings.TrimSpace(out.String()))
		if len(match) != 2 {
			return errors.New("orphan resume adapter requires verified Claude Code 2.1.302+ in the 2.1 series")
		}
		patch, _ := strconv.Atoi(match[1])
		if patch < 302 {
			return errors.New("update Claude Code to 2.1.302+ before using orphan resume")
		}
	} else if !strings.Contains(out.String(), "--cd") {
		return errors.New("installed Codex CLI does not advertise resume --cd; update it or use native handoff")
	}
	return nil
}

type limitedCapabilityOutput struct {
	strings.Builder
	overflow bool
}

func (w *limitedCapabilityOutput) Write(p []byte) (int, error) {
	if w.Len()+len(p) > 64<<10 {
		w.overflow = true
		return 0, errors.New("native help output exceeded its limit")
	}
	return w.Builder.Write(p)
}
