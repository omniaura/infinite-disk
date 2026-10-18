package disk

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReadSessionsMetadataOnlyAndConservativeNamespaces(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(home, "projects", "old-checkout")
	id := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	transcript := filepath.Join(home, ".claude", "projects", projectKey(checkout), id+".jsonl")
	write(t, transcript, `{"type":"queue-operation"}`+"\n"+`{"cwd":`+quoted(checkout)+`,"message":"PRIVATE TRANSCRIPT SENTINEL"}`+"\n")
	write(t, filepath.Join(filepath.Dir(transcript), id, "subagents", "agent-child.jsonl"), `{"message":"PRIVATE CHILD"}`)
	grok := filepath.Join(home, ".grok", "sessions", url.PathEscape(checkout), id)
	if err := os.MkdirAll(grok, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, ".cursor", "projects", strings.TrimLeft(projectKey(checkout), "-"), "agent-transcripts", id+".txt"), "PRIVATE CURSOR TRANSCRIPT")
	before, _ := digestFile(transcript)
	inventory := ReadSessions(context.Background(), home)
	if !inventory.Complete || len(inventory.Sessions) != 3 {
		t.Fatal(inventory)
	}
	for _, s := range inventory.Sessions {
		if !s.Matches(checkout) {
			t.Fatal("missed native namespace", s)
		}
	}
	b, _ := json.Marshal(inventory)
	if strings.Contains(string(b), "PRIVATE") {
		t.Fatal("transcript contents leaked")
	}
	after, _ := digestFile(transcript)
	if before != after {
		t.Fatal("discovery changed history")
	}
	if !inventory.Sessions[0].Orphaned() {
		t.Fatal("missing checkout not reported")
	}
	if err := os.MkdirAll(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	if inventory.Sessions[0].Orphaned() {
		t.Fatal("present checkout called orphaned")
	}
	if (Session{}).Orphaned() {
		t.Fatal("unknown cwd called orphaned")
	}
	if err := os.Symlink(transcript, filepath.Join(filepath.Dir(transcript), "linked.jsonl")); err != nil {
		t.Fatal(err)
	}
	if ReadSessions(context.Background(), home).Complete {
		t.Fatal("symlink treated as complete inventory")
	}
}

func quoted(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestSessionAssociationsDoNotIncludeEveryBroadParent(t *testing.T) {
	checkout := "/fixture/code/project/.claude/worktrees/task/workspaces/review/backend"
	for _, parent := range []string{"/fixture/code", "/fixture/code/project"} {
		if (Session{Checkout: parent}).Matches(checkout) || (Session{ProjectKey: projectKey(parent)}).Matches(checkout) {
			t.Fatal("broad surviving parent associated with every child checkout", parent)
		}
	}
	for _, parent := range []string{"/fixture/code/project/.claude/worktrees/task", "/fixture/code/project/.claude/worktrees/task/workspaces/review"} {
		if !(Session{Checkout: parent}).Matches(checkout) {
			t.Fatal("missed enclosing isolated workspace", parent)
		}
	}
}

func TestOpaqueAndUnindexedStoresFailClosed(t *testing.T) {
	for _, relative := range []string{".cursor/chats/fixture/store.db", ".pi/agent/sessions/fixture.jsonl", ".codex/sessions/fixture.jsonl", ".codex/state_6.sqlite", ".cursor/projects/fixture/agent-transcripts/session/fixture.jsonl"} {
		t.Run(relative, func(t *testing.T) {
			home := t.TempDir()
			write(t, filepath.Join(home, relative), "fixture")
			inventory := ReadSessions(context.Background(), home)
			if inventory.Complete || len(inventory.Warnings) == 0 {
				t.Fatal("opaque store claimed complete", relative)
			}
		})
	}
}

func TestNativeSQLiteIndexesUseOnlyMetadata(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(home, ".codex", "state_5.sqlite")
	if err := os.MkdirAll(filepath.Dir(db), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sqlite3", "-init", os.DevNull, db)
	cmd.Stdin = strings.NewReader("CREATE TABLE threads (id TEXT,cwd TEXT,rollout_path TEXT,source TEXT,title TEXT,originator TEXT); INSERT INTO threads VALUES ('codex-session','/fixture/missing','/fixture/transcript.jsonl','cli','PRIVATE TITLE','codex-tui');")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(b))
	}
	before, _ := digestFile(db)
	inventory := ReadSessions(context.Background(), home)
	if !inventory.Complete || len(inventory.Sessions) != 1 || inventory.Sessions[0].Source != "cli" {
		t.Fatal(inventory)
	}
	b, _ := json.Marshal(inventory)
	if strings.Contains(string(b), "PRIVATE") {
		t.Fatal("private index data leaked")
	}
	after, _ := digestFile(db)
	if before != after {
		t.Fatal("read-only index changed")
	}
}

func TestLinkedAndUnknownSessionChecksBlockRemoval(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(unknownString(unknown), func(t *testing.T) {
			f := newFixture(t)
			p := plan(t, f)
			eligible(t, p.Candidates[0])
			f.e.Sessions = func(context.Context) SessionInventory {
				if unknown {
					return SessionInventory{Complete: false}
				}
				return SessionInventory{Complete: true, Sessions: []Session{{Provider: "Claude Code", ID: "fixture", Checkout: f.repo}}}
			}
			fresh := f.e.Inspect(context.Background(), f.repo, Activity{Known: true})
			if fresh.Eligible {
				t.Fatal("session link did not block cleanup")
			}
			before := runGit(t, f.repo, "show-ref")
			if _, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true); err == nil {
				t.Fatal("new session link bypassed revalidation")
			}
			exists(t, f.repo)
			if before != runGit(t, f.repo, "show-ref") {
				t.Fatal("blocked session cleanup mutated Git")
			}
		})
	}
}

func unknownString(v bool) string {
	if v {
		return "incomplete"
	}
	return "linked"
}

func TestSessionAppearingDuringPreservationKeepsCheckout(t *testing.T) {
	f := newFixture(t)
	p := plan(t, f)
	calls := 0
	f.e.Sessions = func(context.Context) SessionInventory {
		calls++
		inventory := SessionInventory{Complete: true}
		if calls > 1 {
			inventory.Sessions = []Session{{Provider: "Claude Code", ID: "new-session", Checkout: f.repo}}
		}
		return inventory
	}
	r, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true)
	if err == nil || r.RecoveryCommit == "" {
		t.Fatal("expected verified preservation then blocked removal", err)
	}
	exists(t, f.repo)
}

func TestOldPlanCannotRemoveWithoutSessionChecks(t *testing.T) {
	f := newFixture(t)
	p := plan(t, f)
	p.Candidates[0].SessionsChecked = false
	p, err := SavePlan(f.e.Config, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Apply(context.Background(), p, p.Candidates[0].ID, p.ID, true); err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatal(err)
	}
	exists(t, f.repo)
}

func TestNativeResumeRouteIsPrivateRevalidatedAndNotAppMutation(t *testing.T) {
	f := newFixture(t)
	transcript := filepath.Join(filepath.Dir(f.root), "history", "session.jsonl")
	write(t, transcript, "PRIVATE ORIGINAL TRANSCRIPT")
	s := Session{Provider: "Claude Code", ID: "native-id", Checkout: filepath.Join(f.root, "already-removed"), Store: transcript}
	f.e.Sessions = func(context.Context) SessionInventory {
		return SessionInventory{Complete: true, Sessions: []Session{s}}
	}
	route, err := f.e.PrepareSessionResume(context.Background(), s.Provider, s.ID, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if route.Executable != "claude" || !slices.Equal(route.Args, []string{"--resume", s.ID}) {
		t.Fatal(route)
	}
	before, _ := digestFile(transcript)
	if err := f.e.SaveSessionRoute(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	saved, err := f.e.LoadSessionRoute(s.Provider, s.ID)
	if err != nil || !sameResume(route, saved) {
		t.Fatal(saved, err)
	}
	file := filepath.Join(f.e.Config.StateDir, "session-routes", pathID(s.Provider+"/"+s.ID)+".json")
	info, _ := os.Stat(file)
	if info.Mode().Perm() != 0600 {
		t.Fatal("route not private")
	}
	after, _ := digestFile(transcript)
	if before != after {
		t.Fatal("native history was rewritten")
	}
	s.Provider, s.Source = "Codex", "vscode"
	if _, err := f.e.PrepareSessionResume(context.Background(), "Codex", s.ID, f.repo); err == nil {
		t.Fatal("Desktop workspace treated as CLI")
	}
	s.Source = "cli"
	s.Originator = "Codex Desktop"
	if _, err := f.e.PrepareSessionResume(context.Background(), "Codex", s.ID, f.repo); err == nil {
		t.Fatal("app originator treated as CLI-owned")
	}
	s.Originator = "codex-tui"
	route, err = f.e.PrepareSessionResume(context.Background(), "Codex", s.ID, f.repo)
	if err != nil || !slices.Equal(route.Args, []string{"resume", s.ID, "--cd", f.repo}) {
		t.Fatal(route, err)
	}
	s.ID = "changed"
	if err := f.e.SaveSessionRoute(context.Background(), route); err == nil {
		t.Fatal("changed session approved")
	}
	s.ID = "native-id"
	write(t, transcript, "CHANGED TRANSCRIPT")
	if err := f.e.ValidateSessionResume(context.Background(), route); err == nil {
		t.Fatal("changed transcript accepted")
	}
	route, err = f.e.PrepareSessionResume(context.Background(), "Codex", s.ID, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, f.repo, "commit", "--allow-empty", "-m", "destination advanced during review")
	if err := f.e.SaveSessionRoute(context.Background(), route); err == nil {
		t.Fatal("changed destination accepted")
	}
	f.e.Sessions = func(context.Context) SessionInventory {
		return SessionInventory{Complete: true, Sessions: []Session{s, s}}
	}
	if _, err := f.e.PrepareSessionResume(context.Background(), "Codex", s.ID, f.repo); err == nil {
		t.Fatal("duplicate session ID accepted")
	}
}
