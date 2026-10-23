package disk

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnlistedClaudeThatFailsSelfTestStaysBlocked(t *testing.T) {
	dir := t.TempDir()
	stub := "#!/bin/sh\nif [ \"$1\" = --version ]; then printf '2.1.999 (Claude Code)\\n'; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, _, err := nativeClaudeIdentity(context.Background())
	var blocked *untestedClaudeVersion
	if !errors.As(err, &blocked) || blocked.version != "2.1.999" || !strings.Contains(err.Error(), "self-test") || !strings.Contains(err.Error(), "retained") {
		t.Fatalf("unlisted failing Claude accepted or unclear: %v", err)
	}
}

func TestUnlistedClaudeThatIgnoresHookDisableIsRejected(t *testing.T) {
	dir := t.TempDir()
	// Mimics a CLI that accepts every flag but runs project hooks anyway.
	stub := "#!/bin/sh\nif [ \"$1\" = --version ]; then printf '2.1.998 (Claude Code)\\n'; exit 0; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, _, err := nativeClaudeIdentity(context.Background()); err == nil {
		t.Fatal("a CLI that never emits the native zero-turn contract was accepted")
	}
}

// Runs the real installed Claude against the disposable synthetic session.
// Skipped when Claude is not installed; never contacts a provider.
func TestInstalledClaudePassesBehavioralSelfTest(t *testing.T) {
	bin, err := exec.LookPath("claude")
	if err != nil || os.Getenv("INFINITE_DISK_NATIVE_CLAUDE_TEST") != "1" {
		t.Skip("explicit installed-native self-test only")
	}
	if err := runClaudeProbe(context.Background(), bin); err != nil {
		t.Fatal(err)
	}
}
