package main

import (
	"bufio"
	"bytes"
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
