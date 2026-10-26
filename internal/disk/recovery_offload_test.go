package disk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fake heyditto records its arguments and prints a JSON report, so the test
// never touches Ditto.
func TestRecoveryOffloadHandsRetainedDataToHeyditto(t *testing.T) {
	f := newCoverageFixture(t)
	write(t, filepath.Join(f.state, consolidatedDir, "abc", "thin.bundle"), "thin")
	write(t, filepath.Join(f.state, "session-backups", "s.jsonl"), "{}")
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\necho '{\"generation\":1,\"uploaded\":2}'\n"
	if err := os.WriteFile(filepath.Join(bin, "heyditto"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	res, err := f.e.OffloadRecovery(context.Background(), filepath.Join(bin, "heyditto"), "my-recovery")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(argsFile)
	args := string(got)
	for _, want := range []string{"teleport\narchive\n", "--name\nmy-recovery\n", "infinite-disk-recovery=" + filepath.Join(f.state, consolidatedDir), "infinite-disk-sessions=" + filepath.Join(f.state, "session-backups")} {
		if !strings.Contains(args, want) {
			t.Fatalf("heyditto args missing %q:\n%s", want, args)
		}
	}
	if string(res.Ditto) == "" || len(res.Sources) != 2 {
		t.Fatalf("result: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(f.state, consolidatedDir, "abc", "thin.bundle")); err != nil {
		t.Fatal("offload must keep local copies")
	}
	if _, err := f.e.OffloadRecovery(context.Background(), filepath.Join(bin, "missing-heyditto"), "x"); err == nil || !strings.Contains(err.Error(), "heyditto CLI not found") {
		t.Fatalf("missing CLI: %v", err)
	}
	// Two runs in the same second keep separate reports.
	if _, err := f.e.OffloadRecovery(context.Background(), filepath.Join(bin, "heyditto"), "my-recovery"); err != nil {
		t.Fatal(err)
	}
	reports, _ := filepath.Glob(filepath.Join(f.state, coverageDir, "offload-*.json"))
	if len(reports) != 2 {
		t.Fatalf("each run must keep its report: %v", reports)
	}
	if hint := RestoreHint("my recovery"); !strings.HasSuffix(hint, "--name 'my recovery'") {
		t.Fatalf("restore hint must shell-quote the capsule: %s", hint)
	}
}
