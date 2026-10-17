package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/omniaura/infinite-disk/internal/disk"
)

var paletteCodes = regexp.MustCompile("\x1b\\[(1;36|32|33|1;31|0)m")

func TestColorPolicy(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	for _, tt := range []struct {
		mode    string
		json    bool
		noColor string
		term    string
		want    bool
	}{
		{"auto", false, "", "xterm-256color", false}, // a pipe is not a TTY
		{"always", false, "", "xterm-256color", true},
		{"never", false, "", "xterm-256color", false},
		{"auto", false, "1", "xterm-256color", false},
		{"auto", false, "", "dumb", false},
		{"always", false, "1", "dumb", true}, // explicit override
		{"always", true, "", "xterm-256color", false},
	} {
		t.Setenv("NO_COLOR", tt.noColor)
		t.Setenv("TERM", tt.term)
		var out bytes.Buffer
		w := terminalOutput(&out, tt.mode, tt.json)
		_, _ = io.WriteString(styled(w, toneHeading), "Heading")
		if strings.Contains(out.String(), "\x1b[") != tt.want {
			t.Fatalf("mode=%s json=%t NO_COLOR=%q TERM=%q: %q", tt.mode, tt.json, tt.noColor, tt.term, out.String())
		}
	}
	// Character devices aren't necessarily terminals (notably /dev/null).
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if terminalOutput(file, "auto", false).(*terminalWriter).color {
		t.Fatal("non-terminal character device got color")
	}
	var out bytes.Buffer
	first := terminalOutput(&out, "always", false)
	second := terminalOutput(first, "never", false)
	_, _ = io.WriteString(styled(second, toneHeading), "plain")
	if out.String() != "plain" {
		t.Fatal("writer retained previous color policy", out.String())
	}
}

func TestColoredRenderingPreservesTextAndWrapping(t *testing.T) {
	p := disk.Plan{CreatedAt: time.Unix(0, 0), Candidates: []disk.Candidate{
		{Path: "/projects/ready/backend", Eligible: true, SizeComplete: true, SizeBytes: 1 << 30, RemoteHost: "github.com"},
		{Path: "/projects/evil\x1b[2J/" + strings.Repeat("long", 25), Blockers: []string{"private_or_unknown_ignored_state"}},
	}, Warnings: []string{"Fixture warning"}}
	for name, render := range map[string]func(io.Writer){
		"report":   func(w io.Writer) { renderPlan(w, p, false) },
		"caches":   func(w io.Writer) { renderCaches(w, p) },
		"approval": func(w io.Writer) { renderApproval(w, p.Candidates[0], false) },
	} {
		t.Run(name, func(t *testing.T) {
			var plainText, colored bytes.Buffer
			render(&plainText)
			render(terminalOutput(&colored, "always", false))
			if !strings.Contains(colored.String(), "\x1b[") {
				t.Fatal("expected terminal colors")
			}
			stripped := paletteCodes.ReplaceAllString(colored.String(), "")
			if stripped != plainText.String() || strings.Contains(stripped, "\x1b") {
				t.Fatal("color changed text or allowed a terminal escape injection")
			}
			if !strings.HasSuffix(colored.String(), "\x1b[0m") && name == "approval" {
				t.Fatal("approval left terminal style enabled")
			}
		})
	}
	var out bytes.Buffer
	renderApproval(terminalOutput(&out, "always", false), p.Candidates[0], true)
	if strings.Contains(out.String(), "\x1b[1;31m") || strings.Contains(out.String(), "Permanently remove") {
		t.Fatal("preserve-only used a deletion warning")
	}
}

func TestCLIColorsAndJSON(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := disk.DefaultConfig()
	c.Roots = []string{filepath.Join(base, "projects")}
	c.StateDir = filepath.Join(base, "state")
	config := filepath.Join(base, "config.json")
	if err := disk.SaveConfig(config, c); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.SavePlan(c, disk.Plan{Version: 1, CreatedAt: time.Now(), ConfigDigest: disk.ConfigDigest(c), Candidates: []disk.Candidate{{ID: "aaaaaaaaaaaaaaaa", Path: filepath.Join(c.Roots[0], "fixture"), Eligible: true}}}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"auto", "never", "always"} {
		for _, asJSON := range []bool{false, true} {
			for _, action := range []string{"review", "apply"} {
				cmd := New("test")
				var out, diagnostic bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&diagnostic)
				cmd.SetIn(strings.NewReader("q\n"))
				args := []string{"--config", config, "--color", mode, action}
				if asJSON {
					args = append(args, "--json")
				}
				cmd.SetArgs(args)
				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}
				wantColor := mode == "always" && !asJSON
				if strings.Contains(out.String()+diagnostic.String(), "\x1b[") != wantColor {
					t.Fatalf("%s --color %s json=%t: incorrect color policy", action, mode, asJSON)
				}
				if asJSON && !json.Valid(out.Bytes()) {
					t.Fatal("color broke JSON", out.String())
				}
				if action == "apply" && !strings.Contains(diagnostic.String(), "[y/N]") {
					t.Fatal("missing approval prompt")
				}
			}
		}
	}
	cmd := New("test")
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--config", config, "--color", "rainbow", "review"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--color") {
		t.Fatal("invalid mode accepted", err)
	}
	if _, err := os.Stat(filepath.Join(c.StateDir, "receipts")); !os.IsNotExist(err) {
		t.Fatal("color tests created operation receipts")
	}
}

type failingColorWriter struct{}

func (failingColorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestColorWriterReturnsErrors(t *testing.T) {
	n, err := styled(terminalOutput(failingColorWriter{}, "always", false), toneHeading).Write([]byte("test"))
	if n != 0 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(n, err)
	}
}
