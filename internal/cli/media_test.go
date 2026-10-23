package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/omniaura/infinite-disk/internal/disk"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMediaCLIReportOnlyJSONReviewAndEscapedNames(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	root := filepath.Join(home, "Downloads")
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Screenshot \x1b[31m.png", "duplicate.png"} {
		p := filepath.Join(root, name)
		if err = os.WriteFile(p, []byte("same"), 0600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-30 * 24 * time.Hour)
		os.Chtimes(p, when, when)
	}
	c := disk.DefaultConfig()
	c.StateDir = filepath.Join(home, "state")
	e := disk.NewEngine(c)
	e.Activity = func(context.Context) disk.Activity { return disk.Activity{Known: true} }
	load := func() (*disk.Engine, error) { return e, nil }
	asJSON := true
	cmd := mediaCommand(load, &asJSON)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"scan", "--root", root})
	if err = cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var p disk.MediaPlan
	if json.Unmarshal(out.Bytes(), &p) != nil || p.Mode != "report-only" || p.Approved || p.Executed || len(p.Files) != 2 {
		t.Fatal(out.String())
	}
	out.Reset()
	renderMedia(&out, p, true)
	if strings.Contains(out.String(), "\x1b[31m") || !strings.Contains(out.String(), `\x1b`) {
		t.Fatal("unsafe terminal filename", out.String())
	}
	if !strings.Contains(out.String(), "Nothing deleted or uploaded") {
		t.Fatal(out.String())
	}
	if _, err = os.Stat(p.Files[0].Path); err != nil {
		t.Fatal("original removed")
	}
	for _, args := range [][]string{{"image-decision", "--file", p.Files[0].ID}, {"drive", "upload", "--archive", strings.Repeat("0", 64)}, {"delete"}} {
		cmd = mediaCommand(load, &asJSON)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if cmd.Execute() == nil {
			t.Fatal("unsupported/unapproved command accepted", args)
		}
	}
}
func TestMediaHelpExplainsImagePrivacyAndNoDefaultSharing(t *testing.T) {
	for _, args := range [][]string{{"media", "--help"}, {"media", "image-decision", "--help"}, {"media", "drive", "upload", "--help"}} {
		cmd := New("test")
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if out.Len() == 0 {
			t.Fatal("empty help")
		}
		if args[1] == "image-decision" && !strings.Contains(out.String(), "PRIVATE PIXELS") {
			t.Fatal("consent not explicit")
		}
	}
}
