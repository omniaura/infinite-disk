package disk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// A native launch route is not a claim that an app's database was edited or
// that its history/code was restored. The provider owns resumption/rebinding.
type SessionResume struct {
	Version        int       `json:"version"`
	Provider       string    `json:"provider"`
	SessionID      string    `json:"session_id"`
	Original       string    `json:"original_checkout"`
	Destination    string    `json:"destination"`
	Executable     string    `json:"executable"`
	Args           []string  `json:"args"`
	TargetIdentity string    `json:"target_identity"`
	StoreIdentity  string    `json:"store_identity"`
	SavedAt        time.Time `json:"saved_at,omitempty"`
}

func (e *Engine) PrepareSessionResume(ctx context.Context, provider, id, target string) (SessionResume, error) {
	var route SessionResume
	if !sessionID.MatchString(id) || !sessionPath(target) {
		return route, errors.New("select an exact session ID and an absolute, clean project path")
	}
	home, _ := os.UserHomeDir()
	if target == home || target == string(filepath.Separator) || within(target, e.Config.StateDir) || within(e.Config.StateDir, target) {
		return route, errors.New("choose a project checkout, not home, filesystem root, or Infinite Disk state")
	}
	if err := noSymlinkAncestors(target); err != nil {
		return route, errors.New("destination has a symlink or cannot be inspected")
	}
	root, err := gitText(ctx, target, "rev-parse", "--show-toplevel")
	if err != nil || root != target {
		return route, errors.New("destination must be an existing Git checkout root, not a subdirectory")
	}
	inventory := e.SessionInventory(ctx)
	var selected *Session
	for i := range inventory.Sessions {
		s := &inventory.Sessions[i]
		if s.Provider == provider && s.ID == id {
			if selected != nil {
				return route, errors.New("session ID is ambiguous; resolve duplicate native transcripts first")
			}
			selected = s
		}
	}
	if selected == nil {
		return route, errors.New("session not found in readable native metadata; use the owning app")
	}
	if !sessionPath(selected.Checkout) || !selected.Orphaned() {
		return route, errors.New("original checkout is present or unknown; use the app's native worktree handoff first")
	}
	if err := noSymlinkAncestors(selected.Store); err != nil {
		return route, errors.New("session storage has a symlink or cannot be inspected")
	}
	info, err := os.Lstat(selected.Store)
	if err != nil || !info.Mode().IsRegular() || within(target, selected.Store) {
		return route, errors.New("original external session transcript is unavailable or unsafe")
	}
	route = SessionResume{Version: 1, Provider: selected.Provider, SessionID: id, Original: selected.Checkout, Destination: target}
	head, headErr := gitText(ctx, target, "rev-parse", "HEAD")
	common, commonErr := gitText(ctx, target, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if headErr != nil || commonErr != nil {
		return SessionResume{}, errors.New("destination Git identity is unavailable")
	}
	identity, _ := json.Marshal([]string{target, common, head})
	route.TargetIdentity = PlanDigest(identity)
	identity, _ = json.Marshal([]any{selected.Store, info.Size(), info.ModTime().UnixNano()})
	route.StoreIdentity = PlanDigest(identity)
	switch selected.Provider {
	case "Claude Code":
		route.Executable = "claude"
		route.Args = []string{"--resume", id}
	case "Codex":
		if selected.Source != "cli" || selected.Originator != "" && selected.Originator != "codex-tui" && selected.Originator != "codex_cli_rs" {
			return SessionResume{}, errors.New("app-owned Codex session requires native Desktop handoff/restore; CLI routing cannot rebind its app workspace")
		}
		route.Executable = "codex"
		route.Args = []string{"resume", id, "--cd", target}
	default:
		return SessionResume{}, errors.New("native reattachment adapter is not verified for this provider; use its owning app")
	}
	return route, nil
}

// Saving requires separate explicit per-session approval. No native history is
// copied, deleted, or rewritten, and no provider is started here.
func (e *Engine) SaveSessionRoute(ctx context.Context, approved SessionResume) error {
	release, err := acquireLock(e.Config)
	if err != nil {
		return err
	}
	defer release()
	fresh, err := e.PrepareSessionResume(ctx, approved.Provider, approved.SessionID, approved.Destination)
	if err != nil || !sameResume(approved, fresh) {
		return errors.New("session or destination changed; review the native resume route again")
	}
	fresh.SavedAt = e.Now().UTC()
	b, err := json.MarshalIndent(fresh, "", "  ")
	if err != nil {
		return err
	}
	file := filepath.Join(e.Config.StateDir, "session-routes", pathID(fresh.Provider+"/"+fresh.SessionID)+".json")
	if err := atomicPrivate(file, b); err != nil {
		return err
	}
	return AppendEvent(e.Config, Event{Type: "session_resume_route_saved", CandidateID: pathID(fresh.Provider + "/" + fresh.SessionID), Outcome: "native_launch_only"})
}

func (e *Engine) ValidateSessionResume(ctx context.Context, approved SessionResume) error {
	fresh, err := e.PrepareSessionResume(ctx, approved.Provider, approved.SessionID, approved.Destination)
	if err != nil || !sameResume(approved, fresh) {
		return errors.New("session or destination changed; review the native resume route again")
	}
	return nil
}

func sameResume(a, b SessionResume) bool {
	a.SavedAt, b.SavedAt = time.Time{}, time.Time{}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func (e *Engine) LoadSessionRoute(provider, id string) (SessionResume, error) {
	var route SessionResume
	if !sessionID.MatchString(id) {
		return route, errors.New("invalid session ID")
	}
	file := filepath.Join(e.Config.StateDir, "session-routes", pathID(provider+"/"+id)+".json")
	if err := noSymlinkAncestors(file); err != nil {
		return route, errors.New("unsafe session route storage")
	}
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 {
		return route, errors.New("no private saved route; choose --to /absolute/project explicitly")
	}
	b, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(b, &route) != nil || route.Provider != provider || route.SessionID != id || route.Version != 1 {
		return SessionResume{}, errors.New("invalid saved route; choose the destination explicitly")
	}
	return route, nil // PrepareSessionResume regenerates commands and rechecks.
}
