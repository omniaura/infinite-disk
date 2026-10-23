package disk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fake Kondo that hangs on one folder proves discovery splits that folder,
// keeps every finished shard, and never reports the hung one as covered.
func TestKondoDiscoverSplitsSlowFoldersAndKeepsPartialResults(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"fast", "slow/a", "slow/b", "broken"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	script := `#!/bin/sh
for last; do :; done
case "$last" in
  */slow) sleep 30 ;;
  */broken) echo oops >&2; exit 1 ;;
  */fast|*/slow/a)
    echo "$last Node project (3 days ago)"
    echo "  └─ node_modules (1.0MiB)" ;;
esac
echo "Projects cleaned: 0/1, Bytes deleted: 0.0B / 1.0MiB"
`
	if err := os.WriteFile(filepath.Join(bin, "kondo"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := kondoShardTimeout
	kondoShardTimeout = 500 * time.Millisecond
	t.Cleanup(func() { kondoShardTimeout = old })

	e := NewEngine(Config{})
	got, warnings, err := e.kondoDiscover(context.Background(), []string{root})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, c := range got {
		paths = append(paths, c.Path)
	}
	want := []string{filepath.Join(root, "fast", "node_modules"), filepath.Join(root, "slow", "a", "node_modules")}
	if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("got %v want %v", paths, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "could not be scanned") {
		// slow/b is empty and succeeds; only the broken folder is skipped.
		t.Fatalf("unexpected warnings %v", warnings)
	}
}
