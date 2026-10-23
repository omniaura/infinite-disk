package disk

// Analysis is advisory. It enriches an immutable report, never changes
// eligibility, fetches, writes Git objects, launches agents, or approves actions.
import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type SuggestedAction struct {
	Kind             string `json:"kind"`
	Summary          string `json:"summary"`
	Executable       bool   `json:"executable"`
	RequiresApproval bool   `json:"requires_approval"`
}

type IgnoredEntry struct {
	Path     string `json:"path"`
	Category string `json:"category"`
}

type ProjectAnalysis struct {
	ObservedAt                 time.Time         `json:"observed_at"`
	SourceChecked              bool              `json:"source_checked"`
	ChangedFiles               *int              `json:"changed_files,omitempty"`
	CommitsNotInOriginTracking *int              `json:"commits_not_in_origin_tracking,omitempty"`
	IgnoredComplete            bool              `json:"ignored_complete"`
	IgnoredCount               int               `json:"ignored_count"`
	Ignored                    []IgnoredEntry    `json:"ignored,omitempty"`
	Actions                    []SuggestedAction `json:"actions"`
	Notes                      []string          `json:"notes"`
}

// Preservation has a narrower effect than removal: excluded private ignored
// data and session bindings remain untouched. Every source/secret/activity/Git
// preflight must still have passed; an advisory cannot change this predicate.
func CanPreserveSource(c Candidate) bool {
	if c.Fingerprint == "" || c.Head == "" || !c.SessionsChecked {
		return false
	}
	for _, b := range c.Blockers {
		switch b {
		case "private_or_unknown_ignored_state", "recent_source_activity", "coding_sessions_require_native_handoff", "coding_session_coverage_incomplete":
		default:
			return false
		}
	}
	return true
}

func ignoredCategory(p string) string {
	p = strings.TrimSuffix(filepath.ToSlash(p), "/")
	if sensitiveName.MatchString(p) {
		return "private_data"
	}
	if allowedIgnored(p) {
		return "dependency_candidate"
	}
	for _, part := range strings.Split(p, "/") {
		switch part {
		case ".next", "dist", "build", "target", ".terraform", ".gradle", ".cache", ".turbo":
			return "build_candidate"
		case ".claude", ".codex", ".cursor", ".pi", ".opencode":
			return "agent_state"
		}
	}
	return "unknown_data"
}

func (e *Engine) analyzeCandidate(ctx context.Context, c Candidate) *ProjectAnalysis {
	a := &ProjectAnalysis{ObservedAt: e.Now().UTC(), SourceChecked: c.Fingerprint != "", Actions: []SuggestedAction{}, Notes: []string{}}
	add := func(kind, summary string, executable, approval bool) {
		a.Actions = append(a.Actions, SuggestedAction{kind, summary, executable, approval})
	}
	if a.SourceChecked {
		n := c.DirtyFiles
		a.ChangedFiles = &n
	}
	if CanPreserveSource(c) {
		add("preserve_source", "Snapshot working changes and HEAD ancestry to a recovery branch on the existing origin; verify remote readback. Keep checkout.", true, true)
	}
	if c.Eligible {
		add("remove_checkout", "Preserve source and supported sessions, verify recovery, then remove this checkout. Ignored data is excluded; local backups reduce net savings.", true, true)
	} else {
		if slices.Contains(c.Blockers, "project_in_use") || slices.Contains(c.Blockers, "recent_source_activity") {
			add("keep", "Keep active or recently changed work; reassess after it is idle.", false, false)
		}
		if slices.Contains(c.Blockers, "activity_check_unavailable") {
			add("verify_activity", "Restore a complete activity check before considering cleanup.", false, false)
		}
		if slices.Contains(c.Blockers, "coding_sessions_require_native_handoff") || slices.Contains(c.Blockers, "coding_session_coverage_incomplete") || strings.Contains(strings.Join(c.Blockers, " "), "native archive lifecycle") {
			add("native_session_handoff", "Preserve session history and use the provider's supported native lifecycle. No generic removal is permitted.", false, true)
		}
		if slices.Contains(c.Blockers, "private_or_unknown_ignored_state") || slices.Contains(c.Blockers, "source_or_secret_check_requires_review") {
			add("private_backup", "Plan an exact private archive; separately approve local relocation and verify recovery, then reanalyze. No private data uploads or checkout deletion.", CanArchivePrivate(c), true)
		}
		if slices.Contains(c.Blockers, "private_or_unknown_ignored_state") {
			add("inspect_artifacts", "Run Kondo dry-run to identify regenerable artifacts separately; this does not authorize deletion.", true, false)
		}
		add("resolve_blockers", "Resolve the listed safety blockers and rescan. Preserve working changes and commits on a verified recovery branch only after the safety checks pass.", false, true)
	}
	// Only inspect metadata in a validated, idle repository that passed the
	// source inspector's Git preflight. Never reinterpret unknown as clean.
	if e.safeTarget(c.Path) != nil || c.Head == "" || c.CommonDir == "" || slices.Contains(c.Blockers, "project_in_use") || slices.Contains(c.Blockers, "repository_execution_or_credential_overrides") || slices.Contains(c.Blockers, "symlinked_git_metadata") {
		a.Notes = append(a.Notes, "Source/ignored details unavailable or intentionally skipped; zero changed files must not be inferred.")
		return a
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ignored, err := gitCommand(ctx, c.Path, nil, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "--no-empty-directory", "-z")
	if err == nil {
		a.IgnoredComplete = true
		for _, raw := range bytes.Split(ignored, []byte{0}) {
			if len(raw) == 0 {
				continue
			}
			p := strings.TrimSuffix(string(raw), "/")
			if !safeRelative(p) {
				a.IgnoredComplete = false
				continue
			}
			a.IgnoredCount++
			if len(a.Ignored) < 25 {
				category := ignoredCategory(p)
				if category == "unknown_data" && noSymlinkAncestors(filepath.Join(c.Path, p)) == nil {
					if _, formatErr := binaryArtifactFormat(filepath.Join(c.Path, p)); formatErr == nil {
						category = "binary_candidate"
					}
				}
				a.Ignored = append(a.Ignored, IgnoredEntry{string(raw), category})
			}
		}
		if a.IgnoredCount > len(a.Ignored) {
			a.Notes = append(a.Notes, "Ignored entry examples are capped at 25; classifications are hints, not deletion permission.")
		}
	}
	refs, err := gitText(ctx, c.Path, "for-each-ref", "--count=1", "--format=%(objectname)", "refs/remotes/origin/")
	if err == nil && refs != "" {
		count, err := gitText(ctx, c.Path, "rev-list", "--count", "HEAD", "--not", "--remotes=origin")
		if err == nil {
			if n, err := strconv.Atoi(count); err == nil && n >= 0 {
				a.CommitsNotInOriginTracking = &n
			}
		}
	}
	a.Notes = append(a.Notes, "Commit comparison uses local origin-tracking refs only; no fetch, remote verification or claim that commits are safely pushed.")
	return a
}

// Analyze refreshes bounded candidates, retaining the full report. Blocked
// candidates may gain suggestions, never approval or a safety override.
func (e *Engine) AnalyzeSelected(ctx context.Context, p Plan, selected map[string]bool) (Plan, error) {
	if p.Version != ProtocolVersion || p.ID != planID(p) || p.ConfigDigest != ConfigDigest(e.Config) || p.Mode != "report-only" {
		return p, errors.New("invalid analysis plan")
	}
	if len(selected) > 5000 {
		return p, errors.New("analysis selection exceeds 5000 projects")
	}
	for id := range selected {
		if !slices.ContainsFunc(p.Candidates, func(c Candidate) bool { return c.ID == id }) {
			return p, errors.New("project ID is not in this plan")
		}
	}
	if len(selected) == 0 {
		return p, nil
	}
	release, err := acquireLock(e.Config)
	if err != nil {
		return p, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	activity := e.Activity(ctx)
	sessions := e.SessionInventory(ctx)
	reportDigests := newReportDigests(ctx)
	for i, c := range p.Candidates {
		if !selected[c.ID] {
			continue
		}
		if ctx.Err() != nil {
			return p, ctx.Err()
		}
		fresh := e.inspectSource(ctx, c.Path, activity)
		attachSessionsContext(ctx, &fresh, sessions)
		e.prepareClaudeHandoffsWithDigest(ctx, &fresh, activity, reportDigests.sum)
		fresh.Analysis = e.analyzeCandidate(ctx, fresh)
		p.Candidates[i] = fresh
	}
	if ctx.Err() != nil {
		return p, ctx.Err()
	}
	// Old Pi advice no longer describes the refreshed observations.
	p.Recommendations = nil
	p.AgentSteps = nil
	p.AgentRun = nil
	p, err = SavePlan(e.Config, p)
	if err != nil {
		return p, err
	}
	return p, AppendEvent(e.Config, Event{Type: "analysis_completed", PlanID: p.ID, Outcome: "suggestions_only"})
}
