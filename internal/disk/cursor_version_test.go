package disk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cursorVersionFixture(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "cursor-agent")
	if err := os.WriteFile(file, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCursorVersionAllowsSlowSupportedStartup(t *testing.T) {
	cursorVersionFixture(t, "sleep 4\nprintf '2026.05.28-a70ca7c\\n'")
	version, err := cursorNamespaceVersion(context.Background())
	if err != nil || version != "2026.05.28-a70ca7c" {
		t.Fatal("slow supported launcher was treated as an unknown namespace", version, err)
	}
}

func TestCursorVersionNeverAcceptsFailedOrOverflowedOutput(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"failed", "printf '2026.05.28-a70ca7c\\n'; exit 1"},
		{"overflow", "printf '" + strings.Repeat("x", 4097) + "'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cursorVersionFixture(t, tc.body)
			if version, err := cursorNamespaceVersion(context.Background()); err == nil || version != "" {
				t.Fatal("unverified output accepted", version, err)
			}
		})
	}
}

func TestCursorVersionPreservesUnknownVersionForExactGate(t *testing.T) {
	cursorVersionFixture(t, "printf 'future-unknown-version\\n'")
	version, err := cursorNamespaceVersion(context.Background())
	if err != nil || version != "future-unknown-version" || version == "2026.05.28-a70ca7c" {
		t.Fatal(version, err)
	}
}

func TestCursorVersionRespectsEarlierCancellation(t *testing.T) {
	cursorVersionFixture(t, "exec sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if version, err := cursorNamespaceVersion(ctx); !errors.Is(err, context.DeadlineExceeded) || version != "" {
		t.Fatal(version, err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("caller cancellation was not bounded")
	}
}
