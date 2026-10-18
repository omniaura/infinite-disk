package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omniaura/infinite-disk/internal/disk"
)

func sessionFixture(t *testing.T) (*disk.Engine, disk.Session, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "project with spaces")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"-c", "user.name=Fixture", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture"}} {
		cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = target
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(err, string(b))
		}
	}
	store := filepath.Join(base, "native-history.jsonl")
	if err := os.WriteFile(store, []byte("PRIVATE TRANSCRIPT"), 0600); err != nil {
		t.Fatal(err)
	}
	s := disk.Session{Provider: "Claude Code", ID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Checkout: filepath.Join(base, "removed"), Store: store}
	c := disk.DefaultConfig()
	c.Roots = []string{target}
	c.StateDir = filepath.Join(base, "state")
	e := disk.NewEngine(c)
	e.Sessions = func(context.Context) disk.SessionInventory {
		return disk.SessionInventory{Complete: true, Sessions: []disk.Session{s}}
	}
	return e, s, target
}

func TestSessionRouteApprovalAndJSONAreSeparateFromNativeLaunch(t *testing.T) {
	for _, input := range []string{"", "yes", "\n", "n\n", "q\n", "yes\n"} {
		t.Run(strings.ReplaceAll(input, "\n", "newline"), func(t *testing.T) {
			e, s, target := sessionFixture(t)
			asJSON := false
			cmd := sessionsCommand(func() (*disk.Engine, error) { return e, nil }, &asJSON)
			var out, diagnostic bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&diagnostic)
			cmd.SetIn(strings.NewReader(input))
			cmd.SetArgs([]string{"reattach", "--provider", "claude", "--session", s.ID, "--to", target})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			_, err := e.LoadSessionRoute(s.Provider, s.ID)
			if (err == nil) != (input == "yes\n") {
				t.Fatal("incorrect approval boundary", input, err)
			}
			if !strings.Contains(diagnostic.String(), "NOT the old code") || !strings.Contains(diagnostic.String(), "[y/N]") {
				t.Fatal("missing disclosure", diagnostic.String())
			}
			b, _ := os.ReadFile(s.Store)
			if string(b) != "PRIVATE TRANSCRIPT" {
				t.Fatal("history changed")
			}
		})
	}
	e, s, target := sessionFixture(t)
	asJSON := true
	cmd := sessionsCommand(func() (*disk.Engine, error) { return e, nil }, &asJSON)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetIn(strings.NewReader("yes\n"))
	cmd.SetArgs([]string{"reattach", "--provider", "claude", "--session", s.ID, "--to", target})
	if err := cmd.Execute(); err != nil || !json.Valid(out.Bytes()) {
		t.Fatal(err, out.String())
	}
	if _, err := e.LoadSessionRoute(s.Provider, s.ID); err == nil {
		t.Fatal("JSON planning saved a route")
	}
}

func TestNativeResumeUsesExactArgsCWDAndRetainsInput(t *testing.T) {
	e, s, target := sessionFixture(t)
	bin := filepath.Join(filepath.Dir(target), "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(bin, "claude")
	stub := "#!/bin/sh\nif [ \"$1\" = --version ]; then printf '2.1.302 (Claude Code)\\n'; exit 0; fi\nprintf '%s\\n' \"$PWD\" \"$@\"\n/bin/cat\n"
	if err := os.WriteFile(file, []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	asJSON := false
	cmd := sessionsCommand(func() (*disk.Engine, error) { return e, nil }, &asJSON)
	var out, diagnostic bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostic)
	cmd.SetIn(strings.NewReader("yes\nINPUT FOR NATIVE AGENT\n"))
	cmd.SetArgs([]string{"reattach", "--provider", "claude", "--session", s.ID, "--to", target, "--launch"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	want := target + "\n--resume\n" + s.ID + "\nINPUT FOR NATIVE AGENT\n"
	if !strings.Contains(out.String(), want) {
		t.Fatal("native argv/cwd/input mismatch", out.String())
	}
	for _, forbidden := range []string{"skip-permissions", "bypass-approvals", "--force", "--print"} {
		if strings.Contains(out.String(), forbidden) {
			t.Fatal("native permissions bypass", out.String())
		}
	}
	if !strings.Contains(diagnostic.String(), "provider costs") {
		t.Fatal("launch cost disclosure absent")
	}
	if err := os.WriteFile(file, []byte("#!/bin/sh\nprintf '2.1.100\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := verifyNativeResume(context.Background(), disk.SessionResume{Provider: "Claude Code", Executable: "claude"}); err == nil {
		t.Fatal("unverified old native version accepted")
	}
}

func TestReportShowsLinkedSessionsWithoutPrivateContents(t *testing.T) {
	var out bytes.Buffer
	c := disk.Candidate{Sessions: []disk.Session{{Provider: "Claude Code", ID: "session-id", Checkout: "/fixture/old", Store: "/fixture/history.jsonl"}}}
	renderSessionLinks(&out, c, true)
	for _, want := range []string{"session-id", "not part of Git recovery", "/fixture/history.jsonl"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("missing session evidence", want)
		}
	}
}
