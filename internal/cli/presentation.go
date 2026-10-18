package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/omniaura/infinite-disk/internal/disk"
)

// Never allow a path or model advisory to inject terminal control sequences.
func plain(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return ' '
		}
		return r
	}, s)
}
func wrapped(w io.Writer, prefix, text string) {
	words := strings.Fields(plain(text))
	line := prefix
	for _, word := range words {
		// Paths can be one long token; wrap them on narrow terminals too.
		for len([]rune(word)) > 76-len([]rune(prefix)) {
			if line != prefix {
				fmt.Fprintln(w, line)
				line = prefix
			}
			runes := []rune(word)
			width := 76 - len([]rune(prefix))
			fmt.Fprintln(w, prefix+string(runes[:width]))
			word = string(runes[width:])
		}
		if len([]rune(line))+len([]rune(word))+1 > 76 && line != prefix {
			fmt.Fprintln(w, line)
			line = prefix
		}
		if line != prefix {
			line += " "
		}
		line += word
	}
	fmt.Fprintln(w, line)
}
func size(bytes uint64) string {
	if bytes < 1<<30 {
		return fmt.Sprintf("%.0f MiB", float64(bytes)/(1<<20))
	}
	return fmt.Sprintf("%.1f GiB", float64(bytes)/(1<<30))
}
func label(c disk.Candidate) string {
	return plain(filepath.Base(filepath.Dir(c.Path)) + " / " + filepath.Base(c.Path))
}
func blockerText(s string) string {
	switch s {
	case "coding_sessions_require_native_handoff":
		return "Linked coding sessions need native handoff before removing this checkout."
	case "coding_session_coverage_incomplete":
		return "Could not fully check coding-session links; keep checkout until native handoff is verified."
	case "coding_sessions_not_checked":
		return "Saved report predates session checks; run scan before approving checkout removal."
	case "private_or_unknown_ignored_state":
		return "Ignored files need review (may be builds, secrets or local data)."
	case "recent_source_activity":
		return "Recently changed; keep for now."
	case "project_in_use":
		return "Open in a running process; keep for now."
	case "activity_check_unavailable":
		return "Could not verify whether this project is in use."
	case "shallow_repository_requires_manual_preservation":
		return "Shallow Git clone; full recovery needs manual review."
	case "source_or_secret_check_requires_review":
		return "Source or private-data checks need manual review."
	case "recent_artifact_activity":
		return "Artifacts changed recently; keep for now."
	case "artifact_uninspectable_or_private":
		return "Contains private data, nested Git, symlinks or unreadable files."
	case "artifact_contains_tracked_files":
		return "Contains version-controlled files; do not clean."
	case "artifact_contains_unignored_files":
		return "Not entirely Git-ignored; needs manual review."
	case "artifact_size_incomplete":
		return "Could not finish measuring this directory; retained."
	case "Codex-managed worktree requires its native archive lifecycle":
		return "Managed by Codex; use its native archive action."
	default:
		return strings.ReplaceAll(plain(s), "_", " ") + "."
	}
}
func renderEvidence(w io.Writer, v disk.Evidence) {
	detail := v.Detail
	if v.Check == "git_source" {
		if _, after, ok := strings.Cut(detail, "; "); ok {
			detail = after
		}
	}
	wrapped(w, "    ", strings.ReplaceAll(v.Check, "_", " ")+": "+v.Outcome+". "+detail)
}
func renderPlan(w io.Writer, p disk.Plan, details bool) {
	var ready, blocked []disk.Candidate
	var readyBytes, blockedBytes uint64
	for _, c := range p.Candidates {
		if c.Eligible && !c.SessionsChecked {
			c.Eligible = false
			c.Blockers = append(append([]string{}, c.Blockers...), "coding_sessions_not_checked")
		}
		if c.Eligible {
			ready = append(ready, c)
			readyBytes += c.SizeBytes
		} else {
			blocked = append(blocked, c)
			blockedBytes += c.SizeBytes
		}
	}
	fmt.Fprintln(styled(w, toneHeading), "\nINFINITE DISK  |  Report only")
	fmt.Fprintf(w, "Free at scan: %s (%.1f%%)\n", size(p.Space.FreeBytes), p.Space.FreePercent)
	fmt.Fprintf(w, "Report: %s (saved batch, not the whole disk)\n", p.CreatedAt.Local().Format("Jan 2, 15:04"))
	if p.DiscoveredProjects > 0 {
		fmt.Fprintf(w, "Checked this batch: %d of %d discovered projects\n", len(p.Candidates), p.DiscoveredProjects)
	}
	if len(p.Locations) > 0 {
		fmt.Fprintln(styled(w, toneHeading), "\nLOCATION COVERAGE")
		providers := map[string]map[string]int{}
		var names []string
		for _, location := range p.Locations {
			if providers[location.Provider] == nil {
				providers[location.Provider] = map[string]int{}
				names = append(names, location.Provider)
			}
			providers[location.Provider][location.Status]++
		}
		sort.Strings(names)
		for _, provider := range names {
			var states []string
			for _, status := range []string{"included", "not configured", "not present", "unavailable"} {
				if n := providers[provider][status]; n > 0 {
					states = append(states, fmt.Sprintf("%d %s", n, status))
				}
			}
			fmt.Fprintf(w, "  %-18s %s\n", plain(provider), strings.Join(states, ", "))
		}
		fmt.Fprintln(w, "  Use locations for paths; locations --add includes missing locations.")
	}
	fmt.Fprintf(styled(w, toneReady), "Ready for approval: %s across %d checkouts\n", size(readyBytes), len(ready))
	fmt.Fprintf(styled(w, toneWarning), "Needs review: %s across %d checkouts (NOT approved savings)\n", size(blockedBytes), len(blocked))
	if len(p.Warnings) > 0 {
		wrapped(styled(w, toneWarning), "! ", "Partial report or scan warnings. This is not a whole-disk inventory.")
	}
	for _, group := range []struct {
		title string
		rows  []disk.Candidate
		color tone
	}{{"READY FOR CHECKOUT REMOVAL", ready, toneReady}, {"KEEP / NEEDS REVIEW", blocked, toneWarning}} {
		if len(group.rows) == 0 {
			continue
		}
		sort.SliceStable(group.rows, func(i, j int) bool { return group.rows[i].SizeBytes > group.rows[j].SizeBytes })
		fmt.Fprintln(styled(w, group.color), "\n"+group.title)
		limit := len(group.rows)
		if !details && limit > 8 {
			limit = 8
		}
		for _, c := range group.rows[:limit] {
			bytes := size(c.SizeBytes)
			if !c.SizeComplete {
				bytes = "unknown"
			}
			fmt.Fprintln(w)
			wrapped(styled(w, toneHeading), fmt.Sprintf("  %9s  ", bytes), label(c))
			for _, b := range c.Blockers {
				wrapped(styled(w, toneWarning), "    ", blockerText(b))
			}
			renderSessionLinks(w, c, details)
			if details {
				wrapped(w, "    ", "Path: "+c.Path)
				fmt.Fprintf(w, "    Select: --project %s\n", plain(c.ID))
				for _, v := range c.Evidence {
					renderEvidence(w, v)
				}
			}
			for _, r := range p.Recommendations {
				if r.CandidateID == c.ID {
					wrapped(w, "    ", "Pi advice (not performed): "+r.Decision+". "+r.Reason)
					if details {
						for _, step := range r.ProposedSteps {
							wrapped(w, "      ", "Proposed only: "+step)
						}
					}
				}
			}
		}
		if limit < len(group.rows) {
			fmt.Fprintf(w, "\n  + %d more; use review --details to see all.\n", len(group.rows)-limit)
		}
	}
	for _, warning := range p.Warnings {
		wrapped(styled(w, toneWarning), "! ", warning)
	}
	for _, warning := range p.SessionWarnings {
		wrapped(styled(w, toneWarning), "! ", warning)
	}
	fmt.Fprintln(w, "\nNothing committed, pushed or deleted.")
	fmt.Fprintln(w, "Build/dependency cleanup: infinite-disk caches scan")
	fmt.Fprintln(w, "Remove checkouts with verified recovery: infinite-disk apply --review")
	if details {
		fmt.Fprintf(w, "Plan: %s\n", p.ID)
	}
}
func renderApproval(w io.Writer, c disk.Candidate, preserveOnly bool) {
	fmt.Fprintf(styled(w, toneHeading), "\n%s  (%s)\n", label(c), size(c.SizeBytes))
	wrapped(w, "  ", "Location: "+c.Path)
	if !c.SessionsChecked {
		wrapped(styled(w, toneWarning), "  ", "This saved report predates session-link checks. Rescan before approving; removal will be refused.")
	}
	renderSessionLinks(w, c, true)
	wrapped(w, "  ", "1. Preserve source on the existing origin at "+c.RemoteHost+" and verify recovery.")
	if !preserveOnly {
		wrapped(styled(w, toneDanger), "  ", "2. Permanently remove this entire local checkout, not just caches.")
	}
	wrapped(styled(w, toneWarning), "  ", "Recovery excludes ignored dependencies, local Git settings and unreachable-only objects. Estimated savings may differ from actual free space.")
}

func renderSessionLinks(w io.Writer, c disk.Candidate, details bool) {
	if len(c.Sessions) == 0 {
		return
	}
	count := max(c.SessionCount, len(c.Sessions))
	wrapped(styled(w, toneWarning), "    ", fmt.Sprintf("%d linked coding sessions; their history is not part of Git recovery.", count))
	for i, s := range c.Sessions {
		if !details && i >= 3 {
			wrapped(w, "      ", "More sessions in review --details or sessions list --project PATH.")
			break
		}
		id := s.ID
		if !details && len(id) > 8 {
			id = id[:8] + "..."
		}
		wrapped(w, "      ", s.Provider+" session: "+id)
		if details {
			wrapped(w, "        ", "Recorded checkout: "+s.Checkout)
			wrapped(w, "        ", "Recorded history location: "+s.Store)
		}
	}
	if details && count > len(c.Sessions) {
		wrapped(w, "      ", "Session evidence is capped in this saved batch. Use sessions list --project PATH --details for the complete current index results.")
	}
}
