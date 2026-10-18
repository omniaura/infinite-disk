package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniaura/infinite-disk/internal/disk"
)

func TestSignalHelper(t *testing.T) {
	if os.Getenv("INFINITE_DISK_SIGNAL_HELPER") != "1" {
		return
	}
	os.Args = append([]string{"infinite-disk"}, strings.Split(os.Getenv("INFINITE_DISK_SIGNAL_ARGS"), "\n")...)
	main()
	os.Exit(0)
}

// Exercise the actual CLI command/error/approval path. Only the discovery and
// activity executables are fixtures; Git checks and cache removal are real and
// limited to this temporary repository. No user's cache or remote is touched.
func TestCLIApprovedCacheCleanupIntegration(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "projects")
	repo := filepath.Join(root, "fixture")
	artifact := filepath.Join(repo, "node_modules")
	if err = os.MkdirAll(filepath.Join(artifact, "package"), 0700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{filepath.Join(repo, "source.txt"): "source stays\n", filepath.Join(repo, ".gitignore"): "node_modules/\n", filepath.Join(artifact, "package", "index.js"): "regenerable fixture\n"} {
		if err = os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git failed: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.invalid")
	git("add", "source.txt", ".gitignore")
	git("commit", "-m", "fixture")
	head := git("rev-parse", "HEAD")
	indexBefore, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-30 * 24 * time.Hour)
	if err = filepath.Walk(artifact, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, when, when)
	}); err != nil {
		t.Fatal(err)
	}
	fakebin := filepath.Join(base, "bin")
	if err = os.Mkdir(fakebin, 0700); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"lsof":  "#!/bin/sh\nprintf 'p123\\nn/fixture-unrelated-open-file\\n'\n",
		"kondo": "#!/bin/sh\n[ \"$1\" = --dry-run ] || exit 91\nprintf '%s Node project (30 days ago)\\n  └─ node_modules (1.0KiB)\\nProjects cleaned: 0/1, Bytes deleted: 0.0B / 1.0KiB\\n' \"$INFINITE_DISK_CACHE_FIXTURE\"\n",
	} {
		if err = os.WriteFile(filepath.Join(fakebin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	c := disk.DefaultConfig()
	c.Roots = []string{root}
	c.StateDir = filepath.Join(base, "state")
	config := filepath.Join(base, "config.json")
	if err = disk.SaveConfig(config, c); err != nil {
		t.Fatal(err)
	}
	call := func(args ...string) ([]byte, []byte, error) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestSignalHelper$")
		args = append([]string{"--config", config, "--json"}, args...)
		cmd.Env = append(os.Environ(), "INFINITE_DISK_SIGNAL_HELPER=1", "INFINITE_DISK_SIGNAL_ARGS="+strings.Join(args, "\n"), "PATH="+fakebin+string(os.PathListSeparator)+os.Getenv("PATH"), "INFINITE_DISK_CACHE_FIXTURE="+repo)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.Bytes(), stderr.Bytes(), err
	}
	out, diagnostic, err := call("caches", "scan")
	if err != nil {
		t.Fatalf("scan: %v %s", err, diagnostic)
	}
	var p disk.Plan
	if err = json.Unmarshal(out, &p); err != nil || len(p.Candidates) != 1 || !p.Candidates[0].Eligible {
		t.Fatalf("invalid fixture plan: %v %s", err, out)
	}
	id := p.Candidates[0].ID
	_, diagnostic, err = call("caches", "apply", "--plan", p.ID, "--project", id, "--approve-plan", strings.Repeat("0", 64), "--non-interactive")
	if err == nil || !json.Valid(diagnostic) {
		t.Fatal("wrong approval did not emit JSON failure", string(diagnostic))
	}
	if _, err = os.Stat(artifact); err != nil {
		t.Fatal("wrong approval removed artifact")
	}
	out, diagnostic, err = call("caches", "apply", "--plan", p.ID, "--project", id, "--approve-plan", p.ID, "--non-interactive")
	if err != nil {
		t.Fatalf("approved CLI cleanup: %v %s", err, diagnostic)
	}
	var receipts []disk.Receipt
	if err = json.Unmarshal(out, &receipts); err != nil || len(receipts) != 1 || receipts[0].Result != "cache_removed" {
		t.Fatal("invalid receipt", string(out), err)
	}
	if _, err = os.Lstat(artifact); !os.IsNotExist(err) {
		t.Fatal("approved artifact retained")
	}
	if _, err = os.Stat(filepath.Join(repo, "source.txt")); err != nil {
		t.Fatal("source removed")
	}
	indexAfter, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil || !bytes.Equal(indexBefore, indexAfter) || git("rev-parse", "HEAD") != head {
		t.Fatal("source branch/index changed")
	}
}
func TestCtrlCExitsBlockedApproval(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := disk.DefaultConfig()
	c.Roots = []string{filepath.Join(base, "projects")}
	c.StateDir = filepath.Join(base, "state")
	config := filepath.Join(base, "config.json")
	if err = disk.SaveConfig(config, c); err != nil {
		t.Fatal(err)
	}
	_, err = disk.SavePlan(c, disk.Plan{Version: 1, CreatedAt: time.Now(), ConfigDigest: disk.ConfigDigest(c), Candidates: []disk.Candidate{{ID: "aaaaaaaaaaaaaaaa", Path: filepath.Join(c.Roots[0], "fixture"), Eligible: true, SessionsChecked: true, RemoteHost: "github.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalHelper$")
	cmd.Env = append(os.Environ(), "INFINITE_DISK_SIGNAL_HELPER=1", "INFINITE_DISK_SIGNAL_ARGS=--config\n"+config+"\napply\n--review\n--color\nalways")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	prompt := make(chan struct{})
	go func() {
		reader := bufio.NewReader(output)
		var seen bytes.Buffer
		for {
			b, err := reader.ReadByte()
			if err != nil {
				return
			}
			seen.WriteByte(b)
			if strings.Contains(seen.String(), "[y/N]") {
				close(prompt)
				return
			}
		}
	}()
	select {
	case <-prompt:
	case <-time.After(10 * time.Second):
		t.Fatal("no approval prompt")
	}
	if err = cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 130 {
			t.Fatal("expected cancellation exit 130", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ctrl+C did not exit blocked approval")
	}
	if _, err = os.Stat(filepath.Join(c.StateDir, "receipts")); !os.IsNotExist(err) {
		t.Fatal("cancelled approval wrote receipts")
	}
}
