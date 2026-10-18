package disk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func claudeFixture(t *testing.T) (fixture, string, Session) {
	t.Helper()
	f := newFixture(t)
	primary := f.repo
	checkout := filepath.Join(primary, ".claude", "worktrees", "stale-task")
	runGit(t, primary, "worktree", "add", "-b", "stale-task", checkout)
	write(t, filepath.Join(checkout, "unfinished.txt"), "unfinished fixture work\n")
	old(t, filepath.Join(checkout, "source.txt"))
	old(t, filepath.Join(checkout, "unfinished.txt"))
	f.repo = checkout
	f.root = filepath.Dir(checkout)
	f.e.Config.Roots = []string{f.root}
	home := filepath.Join(filepath.Dir(primary), "native-home")
	id := "442a48e7-4026-456b-a375-ed965e8b634f"
	store := filepath.Join(home, ".claude", "projects", claudeProjectKey(checkout), id+".jsonl")
	rows := []any{
		map[string]any{"type": "user", "uuid": "d357f905-554f-4324-bd4c-ce470435b899", "parentUuid": nil, "isSidechain": false, "cwd": checkout, "sessionId": id, "version": "2.1.302", "message": map[string]any{"role": "user", "content": "Disposable Infinite Disk fixture. Do not execute tools."}},
		map[string]any{"type": "assistant", "uuid": "f2638238-2339-41ec-81bc-eb60cb951ae2", "parentUuid": "d357f905-554f-4324-bd4c-ce470435b899", "isSidechain": false, "cwd": checkout, "sessionId": id, "version": "2.1.302", "message": map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6", "content": []any{map[string]any{"type": "text", "text": "Fixture acknowledged."}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}},
		map[string]any{"type": "worktree-state", "sessionId": id, "worktreeSession": map[string]any{"worktreePath": checkout, "originalCwd": primary, "branch": "stale-task", "name": "stale-task"}},
	}
	var data strings.Builder
	for _, row := range rows {
		row.(map[string]any)["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
		b, _ := json.Marshal(row)
		data.Write(b)
		data.WriteByte('\n')
	}
	write(t, store, data.String())
	write(t, strings.TrimSuffix(store, ".jsonl")+"/subagents/agent-child.jsonl", "PRIVATE SUBAGENT\n")
	write(t, filepath.Join(home, ".claude", "file-history", id, "snapshot@v1"), "PRIVATE CHECKPOINT\n")
	s := Session{Provider: "Claude Code", ID: id, Checkout: checkout, Store: store, ProjectKey: claudeProjectKey(checkout)}
	f.e.Sessions = func(ctx context.Context) SessionInventory { return ReadSessions(ctx, home) }
	f.e.claudeRunner = func(_ context.Context, h ClaudeHandoff) error {
		if _, err := os.Lstat(checkout); !errors.Is(err, os.ErrNotExist) {
			return errors.New("old checkout still exists")
		}
		if h.Destination != primary {
			return errors.New("wrong target")
		}
		return appendClaudeClear(h)
	}
	return f, primary, s
}

func appendClaudeClear(h ClaudeHandoff) error {
	f, err := os.OpenFile(h.Session.Store, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(map[string]any{"type": "worktree-state", "sessionId": h.Session.ID, "worktreeSession": nil})
	_, err = f.Write(append(b, '\n'))
	return err
}

func TestClaudeApprovedCleanupPreservesSessionsAndUnfinishedWork(t *testing.T) {
	f, primary, s := claudeFixture(t)
	primaryHead := runGit(t, primary, "rev-parse", "HEAD")
	primaryIndex, _ := digestFile(filepath.Join(primary, ".git", "index"))
	original, _ := os.ReadFile(s.Store)
	p := plan(t, f)
	eligible(t, p.Candidates[0])
	if len(p.Candidates[0].SessionHandoffs) != 1 {
		t.Fatal("no automatic handoff plan")
	}
	r, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true)
	if err != nil {
		t.Fatal(err, r.Steps)
	}
	if r.Result != "removed" || len(r.SessionHandoffs) != 1 || r.SessionHandoffs[0].Status != "native_verified" {
		t.Fatal(r)
	}
	if _, err := os.Stat(f.repo); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("worktree was not removed")
	}
	h := r.SessionHandoffs[0]
	backup, err := os.ReadFile(filepath.Join(h.BackupDir, pathID(s.Store)))
	if err != nil || string(backup) != string(original) {
		t.Fatal("history backup not byte-exact", err)
	}
	if err := verifyOriginalPrefix(h); err != nil {
		t.Fatal(err)
	}
	if len(h.Files) != 3 || h.RecoveryCommit != r.RecoveryCommit {
		t.Fatal("sidecars/recovery linkage missing", h)
	}
	if runGit(t, f.remote, "show", r.RecoveryCommit+":unfinished.txt") != "unfinished fixture work" {
		t.Fatal("unfinished work missing remotely")
	}
	if primaryHead != runGit(t, primary, "rev-parse", "HEAD") {
		t.Fatal("primary branch changed")
	}
	index, _ := digestFile(filepath.Join(primary, ".git", "index"))
	if index != primaryIndex {
		t.Fatal("primary staging changed")
	}
	route, err := f.e.LoadSessionRoute("Claude Code", s.ID)
	if err != nil || route.Destination != primary {
		t.Fatal(route, err)
	}
	if err := f.e.ValidateSessionResume(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadReceipt(f.e.Config, p.ID, p.Candidates[0].ID)
	if err != nil || loaded.SessionHandoffs[0].Status != "native_verified" {
		t.Fatal(loaded, err)
	}
	for _, file := range []string{s.Store, filepath.Join(h.BackupDir, pathID(s.Store)), filepath.Join(h.BackupDir, "manifest.json")} {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatal("non-private recovery", file, err)
		}
	}
}

func TestClaudeHandoffFailuresRollBackWithoutDeletingHistory(t *testing.T) {
	for _, mode := range []string{"native_error", "binding_not_cleared", "history_replaced", "new_private_file", "cancel", "target_changed", "sidecar_changed", "became_active", "new_session"} {
		t.Run(mode, func(t *testing.T) {
			f, primary, s := claudeFixture(t)
			p := plan(t, f)
			eligible(t, p.Candidates[0])
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.e.claudeRunner = func(_ context.Context, h ClaudeHandoff) error {
				switch mode {
				case "native_error":
					return errors.New("fixture failure")
				case "binding_not_cleared":
					return nil
				case "history_replaced":
					write(t, s.Store, `{"type":"user","cwd":`+quoted(f.repo)+`,"message":"replaced"}`+"\n")
					return nil
				case "new_private_file":
					entries, _ := os.ReadDir(filepath.Join(filepath.Dir(f.repo), ".infinite-disk-quarantine"))
					write(t, filepath.Join(filepath.Dir(f.repo), ".infinite-disk-quarantine", entries[0].Name(), ".env"), "private=yes\n")
				case "cancel":
					cancel()
					return context.Canceled
				case "target_changed":
					runGit(t, primary, "commit", "--allow-empty", "-m", "concurrent change")
				case "sidecar_changed":
					write(t, strings.TrimSuffix(s.Store, ".jsonl")+"/subagents/agent-child.jsonl", "concurrent change")
				case "became_active":
					f.e.Activity = func(context.Context) Activity { return Activity{Known: true, Paths: []string{s.Store}} }
				case "new_session":
					write(t, filepath.Join(filepath.Dir(s.Store), "new-session.jsonl"), `{"cwd":`+quoted(f.repo)+`}`+"\n")
				}
				return appendClaudeClear(h)
			}
			r, err := f.e.Apply(ctx, p, p.Candidates[0].ID, p.ID, true)
			if err == nil {
				t.Fatal("unsafe native handoff succeeded", r)
			}
			exists(t, f.repo)
			exists(t, s.Store)
			if r.QuarantinePath != "" {
				t.Fatal("rollback not recorded", r.QuarantinePath)
			}
			if r.RecoveryCommit == "" || len(r.SessionHandoffs) != 1 {
				t.Fatal("no preservation receipt")
			}
			backup := filepath.Join(r.SessionHandoffs[0].BackupDir, pathID(s.Store))
			exists(t, backup)
			if mode == "new_private_file" {
				exists(t, filepath.Join(f.repo, ".env"))
			}
		})
	}
}

func TestClaudeSessionChangesInvalidateApprovalBeforeWrites(t *testing.T) {
	for _, change := range []string{"transcript", "sidecar", "target", "new_session", "duplicate_id", "store_symlink"} {
		t.Run(change, func(t *testing.T) {
			f, primary, s := claudeFixture(t)
			p := plan(t, f)
			eligible(t, p.Candidates[0])
			refs := runGit(t, primary, "show-ref")
			switch change {
			case "transcript":
				appendClaudeClear(ClaudeHandoff{Session: s})
			case "sidecar":
				write(t, strings.TrimSuffix(s.Store, ".jsonl")+"/subagents/new.jsonl", "new private state")
			case "target":
				runGit(t, primary, "commit", "--allow-empty", "-m", "target moved")
			case "new_session":
				b, _ := os.ReadFile(s.Store)
				write(t, filepath.Join(filepath.Dir(s.Store), "new-session.jsonl"), string(b))
			case "duplicate_id":
				b, _ := os.ReadFile(s.Store)
				write(t, filepath.Join(filepath.Dir(filepath.Dir(s.Store)), "other-project", s.ID+".jsonl"), string(b))
			case "store_symlink":
				b, _ := os.ReadFile(s.Store)
				external := filepath.Join(filepath.Dir(f.root), "elsewhere.jsonl")
				write(t, external, string(b))
				os.Remove(s.Store)
				os.Symlink(external, s.Store)
			}
			if _, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true); err == nil {
				t.Fatal("stale approval accepted")
			}
			exists(t, f.repo)
			if change != "target" && refs != runGit(t, primary, "show-ref") {
				t.Fatal("preflight mutated Git")
			}
		})
	}
}

// Explicit, offline native compatibility smoke. Never touches real sessions or
// credentials; only the disposable fixture is passed to installed Claude.
func TestNativeClaudeOfflineCleanup(t *testing.T) {
	if os.Getenv("INFINITE_DISK_NATIVE_CLAUDE_TEST") != "1" {
		t.Skip("explicit installed-native fixture smoke only")
	}
	f, primary, s := claudeFixture(t)
	f.e.claudeRunner = nil
	marker := filepath.Join(filepath.Dir(f.root), "hook-must-not-run")
	settings := map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "touch " + marker}}}}}}
	b, _ := json.Marshal(settings)
	write(t, filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(s.Store))), "settings.json"), string(b))
	p := plan(t, f)
	eligible(t, p.Candidates[0])
	head := runGit(t, primary, "rev-parse", "HEAD")
	indexBefore, err := digestFile(filepath.Join(primary, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true)
	if err != nil {
		t.Fatal(err, r.Steps)
	}
	if r.Result != "removed" || r.SessionHandoffs[0].Status != "native_verified" {
		t.Fatal(r)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("native hook ran")
	}
	if runGit(t, primary, "rev-parse", "HEAD") != head {
		t.Fatal("primary changed")
	}
	indexAfter, err := digestFile(filepath.Join(primary, ".git", "index"))
	if err != nil || indexBefore != indexAfter {
		t.Fatal("native maintenance changed primary index", err)
	}
	if err := verifyOriginalPrefix(r.SessionHandoffs[0]); err != nil {
		t.Fatal(err)
	}
	// Restore the private archive into a completely fresh native config root.
	// Native resume must work without consulting the original session store.
	h := r.SessionHandoffs[0]
	originalConfig := filepath.Dir(filepath.Dir(filepath.Dir(s.Store)))
	restoredConfig := filepath.Join(filepath.Dir(originalConfig), "independent-restore")
	for _, file := range h.Files {
		rel, err := filepath.Rel(originalConfig, file.Path)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(h.BackupDir, pathID(file.Path)))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(restoredConfig, rel), string(data))
	}
	rel, _ := filepath.Rel(originalConfig, s.Store)
	h.Session.Store = filepath.Join(restoredConfig, rel)
	if err := runClaudeHandoff(context.Background(), h); err != nil {
		t.Fatal("independent native session restore", err)
	}
	h.byID = true
	if err := runClaudeHandoff(context.Background(), h); err != nil {
		t.Fatal("independent native ID resume", err)
	}
}

func TestRecoverInterruptedClaudeQuarantineNeverOverwrites(t *testing.T) {
	f, _, _ := claudeFixture(t)
	p := plan(t, f)
	c := p.Candidates[0]
	eligible(t, c)
	r := Receipt{Version: 1, PlanID: p.ID, CandidateID: c.ID, Path: c.Path, QuarantinePath: filepath.Join(filepath.Dir(c.Path), ".infinite-disk-quarantine", c.ID+"-"+p.ID[:12]), Result: "interrupted"}
	os.MkdirAll(filepath.Dir(r.QuarantinePath), 0700)
	if err := saveReceipt(f.e.Config, r); err != nil {
		t.Fatal(err)
	}
	runGit(t, filepath.Dir(c.CommonDir), "--git-dir", c.CommonDir, "worktree", "move", c.Path, r.QuarantinePath)
	// A newly created destination is never replaced by recovery.
	os.MkdirAll(c.Path, 0700)
	write(t, filepath.Join(c.Path, "new.txt"), "new work")
	if f.e.RecoverQuarantine(context.Background(), p.ID, c.ID) == nil {
		t.Fatal("recovery overwrote new work")
	}
	os.Remove(filepath.Join(c.Path, "new.txt"))
	os.Remove(c.Path)
	if err := f.e.RecoverQuarantine(context.Background(), p.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	exists(t, filepath.Join(c.Path, "unfinished.txt"))
	loaded, err := LoadReceipt(f.e.Config, p.ID, c.ID)
	if err != nil || loaded.QuarantinePath != "" || loaded.Result != "quarantine_restored" {
		t.Fatal(loaded, err)
	}
}

func TestClaudeStackChildCloneRetainsParentSession(t *testing.T) {
	for _, layout := range []string{".claude/worktrees/fixture", ".worktrees/claude-fixture", ".claude/worktrees/no-primary"} {
		t.Run(layout, func(t *testing.T) {
			f := newFixture(t)
			primary := f.repo
			wrapper := filepath.Join(f.root, layout)
			checkout := filepath.Join(wrapper, "workspaces", "task", filepath.Base(primary))
			os.MkdirAll(filepath.Dir(checkout), 0700)
			runGit(t, f.root, "clone", "--no-local", f.remote, checkout)
			runGit(t, checkout, "checkout", "main")
			runGit(t, checkout, "config", "user.name", "Infinite Disk Test")
			runGit(t, checkout, "config", "user.email", "fixtures@example.invalid")
			if layout == ".claude/worktrees/no-primary" {
				// The child clone still has source recovery, but there is no
				// canonical same-origin primary to hand a conversation to. A
				// conversation already in the surviving wrapper must stay there.
				moved := primary + "-retained"
				if err := os.Rename(primary, moved); err != nil {
					t.Fatal(err)
				}
				primary = moved
			}
			write(t, filepath.Join(checkout, "unfinished.txt"), "child clone work\n")
			old(t, filepath.Join(checkout, "unfinished.txt"))
			old(t, filepath.Join(checkout, "source.txt"))
			home := filepath.Join(filepath.Dir(f.root), "native-home")
			store := filepath.Join(home, ".claude", "projects", claudeProjectKey(wrapper), "parent-session.jsonl")
			write(t, store, `{"type":"user","cwd":`+quoted(wrapper)+`,"sessionId":"parent-session","message":{"role":"user","content":"PRIVATE PARENT"}}`+"\n")
			before, _ := digestFile(store)
			f.repo = checkout
			f.e.Config.Roots = []string{filepath.Dir(checkout)}
			f.e.Sessions = func(ctx context.Context) SessionInventory { return ReadSessions(ctx, home) }
			f.e.claudeRunner = func(context.Context, ClaudeHandoff) error {
				t.Fatal("surviving parent session must not be moved")
				return nil
			}
			p := plan(t, f)
			eligible(t, p.Candidates[0])
			r, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true)
			if err != nil || r.Result != "removed" || r.SessionHandoffs[0].Status != "retained_parent" {
				t.Fatal(r, err)
			}
			exists(t, wrapper)
			exists(t, primary)
			exists(t, store)
			after, _ := digestFile(store)
			if before != after {
				t.Fatal("parent session was changed")
			}
			if r.BundlePath == "" {
				t.Fatal("clone Git recovery bundle missing")
			}
			if runGit(t, f.remote, "show", r.RecoveryCommit+":unfinished.txt") != "child clone work" {
				t.Fatal("child source recovery missing")
			}
		})
	}
}
