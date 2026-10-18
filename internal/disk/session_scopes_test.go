package disk

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdditionalSessionScopesAreReadOnlyAndCandidateScoped(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(home, "code", "project", ".claude", "worktrees", "task")
	child := filepath.Join(checkout, "deep", "src")
	os.MkdirAll(child, 0700)
	id := "fixture-session"
	write(t, filepath.Join(home, ".cursor", "projects", strings.TrimLeft(projectKey(checkout), "-"), "agent-transcripts", id, id+".jsonl"), "PRIVATE CURSOR")
	write(t, filepath.Join(home, ".cursor", "chats", cursorHash(child), id, "store.db"), "OPAQUE PRIVATE DATABASE")
	write(t, filepath.Join(home, ".pi", "agent", "sessions", "--scope--", "fixture.jsonl"), `{"type":"session","version":3,"id":"pi-session","cwd":`+quoted(filepath.Join(home, "unrelated"))+`}`+"\n"+`{"message":"PRIVATE PI"}`)
	write(t, filepath.Join(home, ".local", "share", "opencode", "storage", "session", "project", "session.json"), `{"id":"opencode-session","directory":`+quoted(filepath.Join(home, "unrelated"))+`,"title":"PRIVATE TITLE"}`)
	inv := ReadSessions(context.Background(), home)
	if !inv.Complete || len(inv.Sessions) != 4 {
		t.Fatal(inv)
	}
	b, _ := json.Marshal(inv)
	if strings.Contains(string(b), "PRIVATE") {
		t.Fatal("private session data leaked")
	}
	c := Candidate{Path: checkout, Eligible: true}
	attachSessions(&c, inv)
	if c.Eligible || c.SessionCount != 2 {
		t.Fatal("nested Cursor cwd was not protected", c.Blockers, c.SessionCount)
	}
	other := filepath.Join(home, "other")
	os.MkdirAll(other, 0700)
	c = Candidate{Path: other, Eligible: true}
	attachSessions(&c, inv)
	if !c.Eligible {
		t.Fatal("unrelated store blocked all checkouts", c.Blockers)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c = Candidate{Path: other, Eligible: true}
	attachSessionsContext(ctx, &c, inv)
	if c.Eligible {
		t.Fatal("cancelled scope walk treated as complete")
	}
}

func TestNativeAdditionalSQLiteScopesAndUnknownSchemas(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 unavailable")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []struct{ file, sql string }{
		{filepath.Join(home, ".local", "share", "opencode", "opencode.db"), "CREATE TABLE session(id TEXT,directory TEXT,title TEXT); INSERT INTO session VALUES ('fixture','/fixture/unrelated','PRIVATE');"},
		{filepath.Join(home, "Library", "Application Support", "emdash", "emdash4.db"), "CREATE TABLE projects(id TEXT,path TEXT); CREATE TABLE workspaces(id TEXT,path TEXT,type TEXT); INSERT INTO workspaces VALUES ('fixture','/fixture/unrelated','local');"},
	} {
		os.MkdirAll(filepath.Dir(source.file), 0700)
		cmd := exec.Command("sqlite3", source.file)
		cmd.Stdin = strings.NewReader(source.sql)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(err, string(b))
		}
	}
	inv := ReadSessions(context.Background(), home)
	if !inv.Complete || len(inv.Sessions) != 2 {
		t.Fatal(inv)
	}
	write(t, filepath.Join(home, ".local", "share", "opencode", "new-schema.db"), "unknown")
	if ReadSessions(context.Background(), home).Complete {
		t.Fatal("unknown schema treated as covered")
	}
}

func TestClaudeLongPathNamespace(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkout := "/fixture/" + strings.Repeat("long-folder/", 20) + "repo"
	if len(claudeProjectKey(checkout)) <= 200 {
		t.Fatal("missing long namespace suffix")
	}
	write(t, filepath.Join(home, ".claude", "projects", claudeProjectKey(checkout), "fixture.jsonl"), `{"cwd":`+quoted(checkout)+`}`+"\n")
	inv := ReadSessions(context.Background(), home)
	if !inv.Complete || len(inv.Sessions) != 1 || inv.Sessions[0].Checkout != checkout {
		t.Fatal(inv)
	}
}

func TestClosedDatabaseRejectsSpecialFilesBeforeOpening(t *testing.T) {
	if _, err := exec.LookPath("mkfifo"); err != nil {
		t.Skip("mkfifo unavailable")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "store.db")
	if err := exec.Command("mkfifo", file).Run(); err != nil {
		t.Skip("FIFO fixtures unavailable")
	}
	if _, err := readClosedSessionRows(context.Background(), file, "SELECT 1"); err == nil {
		t.Fatal("special store file accepted")
	}
}

func TestNativeClaudeProofRequiresIdentityCWDAndZeroTurns(t *testing.T) {
	h := ClaudeHandoff{Session: Session{ID: "fixture"}, Destination: "/fixture/primary"}
	valid := `{"type":"system","subtype":"init","session_id":"fixture","cwd":"/fixture/primary","tools":[],"mcp_servers":[]}` + "\n" + `{"type":"result","subtype":"success","session_id":"fixture","is_error":false,"num_turns":0,"total_cost_usd":0,"duration_api_ms":0,"modelUsage":{}}` + "\n"
	if err := verifyClaudeOutput([]byte(valid), h); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(valid, `"num_turns":0`, `"num_turns":1`, 1), strings.Replace(valid, `"total_cost_usd":0`, `"total_cost_usd":1`, 1), strings.Replace(valid, `"num_turns":0,`, "", 1), strings.Replace(valid, `"tools":[]`, `"tools":[{}]`, 1), strings.Replace(valid, "/fixture/primary", "/fixture/wrong", 1), strings.ReplaceAll(valid, "fixture\"", "other\"")} {
		if verifyClaudeOutput([]byte(bad), h) == nil {
			t.Fatal("invalid native proof accepted", bad)
		}
	}
}
