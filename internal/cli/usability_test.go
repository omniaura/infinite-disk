package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"github.com/omniaura/infinite-disk/internal/disk"
	"io"
	"strings"
	"testing"
	"time"
)

func TestBlockedInputCancellation(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := readLine(ctx, bufio.NewReader(reader)); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked input ignored cancellation")
	}
}
func TestCancelledInputCannotApprove(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	line, err := readLine(ctx, bufio.NewReader(strings.NewReader("yes\n")))
	if line != "" || !errors.Is(err, context.Canceled) {
		t.Fatal("approved after cancellation", line, err)
	}
}
func TestReadablePlanGroupsAndSanitizes(t *testing.T) {
	p := disk.Plan{ID: strings.Repeat("a", 64), Candidates: []disk.Candidate{
		{ID: "one", Name: "backend", Path: "/projects/small/backend", Eligible: true, SessionsChecked: true, SizeComplete: true, SizeBytes: 1 << 30},
		{ID: "two", Name: "backend", Path: "/projects/big/backend", Eligible: true, SessionsChecked: true, SizeComplete: true, SizeBytes: 2 << 30},
		{ID: "bad", Path: "/projects/evil\x1b[2J/backend", SizeComplete: true, SizeBytes: 3 << 30, Blockers: []string{"private_or_unknown_ignored_state"}},
	}}
	var out bytes.Buffer
	renderPlan(&out, p, false)
	s := out.String()
	if strings.Index(s, "big / backend") > strings.Index(s, "small / backend") {
		t.Fatal("not sorted")
	}
	for _, bad := range []string{p.ID, "private_or_unknown_ignored_state", "\x1b"} {
		if strings.Contains(s, bad) {
			t.Fatal("debug/control text leaked", bad)
		}
	}
	if !strings.Contains(s, "NOT approved savings") || !strings.Contains(s, "3.0 GiB across 2 checkouts") {
		t.Fatal(s)
	}
}
func TestApprovalClearlyDistinguishesCheckoutFromCaches(t *testing.T) {
	var out bytes.Buffer
	renderApproval(&out, disk.Candidate{Path: "/test/work/backend", RemoteHost: "github.com"}, false)
	for _, want := range []string{"entire local checkout", "not just caches", "github.com", "Recovery excludes"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("missing approval disclosure", want)
		}
	}
}

func TestReportDoesNotDoubleCountNestedCheckouts(t *testing.T) {
	p := disk.Plan{Candidates: []disk.Candidate{
		{Path: "/projects/parent", Eligible: true, SessionsChecked: true, SizeComplete: true, SizeBytes: 2 << 30},
		{Path: "/projects/parent/child", Eligible: true, SessionsChecked: true, SizeComplete: true, SizeBytes: 1 << 30},
	}}
	var out bytes.Buffer
	renderPlan(&out, p, false)
	if !strings.Contains(out.String(), "2.0 GiB across 2 checkouts") || !strings.Contains(out.String(), "overlap") {
		t.Fatal(out.String())
	}
}
