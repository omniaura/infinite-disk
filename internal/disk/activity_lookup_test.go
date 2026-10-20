package disk

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"slices"
	"testing"
)

func TestIndexedActivityMatchesExistingBoundaryRules(t *testing.T) {
	paths := []string{"/a", "/a-b/file", "/a/child/file", "/a/../b/file", "/a//double", "/a/./dot", "/", "/unicodé/🎵/file", "relative/file", "../elsewhere", ".", "", "/a/child/file"}
	queries := []string{"/a", "/a-b", "/a-bad", "/a/child", "/a/child/file", "/a/child/file/descendant", "/a/./child", "/a/other/../child", "/a//child", "/a/child/", "/absent", "/unicodé/🎵", "/", "//", ".", "", "relative", "..", "../elsewhere"}
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 200; i++ {
		paths = append(paths, fmt.Sprintf("/root/%d/%d/file", rng.Intn(30), rng.Intn(30)))
		queries = append(queries, fmt.Sprintf("/root/%d/%d", rng.Intn(30), rng.Intn(30)))
	}
	for _, subset := range [][]string{nil, {"/a-b/file"}, {"/a-b/file", "/a/child/file"}, paths} {
		activity := Activity{Paths: subset, Known: true}
		lookup, err := indexActivity(context.Background(), activity)
		if err != nil {
			t.Fatal(err)
		}
		for _, query := range queries {
			if got, want := lookup.Busy(query), activity.Busy(query); got != want {
				t.Fatalf("indexed activity mismatch for %q: got %v, want %v", query, got, want)
			}
		}
	}
}

func TestActivityIndexIsInvocationLocal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows intentionally uses existing lookup")
	}
	a := Activity{Paths: []string{"/old/project/file"}, Known: true}
	lookup, err := indexActivity(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	a.Paths[0] = "/new/project/file"
	fresh, err := indexActivity(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if !lookup.Busy("/old/project") || lookup.Busy("/new/project") || fresh.Busy("/old/project") || !fresh.Busy("/new/project") {
		t.Fatal("lookup must be private to its activity observation; later observations need a fresh index")
	}
}

func TestCancelledClaudeHandoffNeverBecomesEligible(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := indexActivity(ctx, Activity{Known: true}); err == nil {
		t.Fatal("cancelled activity indexing succeeded")
	}
	c := Candidate{Eligible: false, SessionCount: 1, Blockers: []string{"coding_sessions_require_native_handoff"}, Sessions: []Session{{Provider: "Claude Code", Store: "/session/history", Checkout: "/project"}}}
	new(Engine).prepareClaudeHandoffs(ctx, &c, Activity{Paths: []string{"/other"}, Known: true})
	if c.Eligible || len(c.SessionHandoffs) != 0 || !slices.Contains(c.Blockers, "coding_sessions_require_native_handoff") || !slices.Contains(c.Blockers, "activity_check_unavailable") {
		t.Fatal("cancelled handoff analysis must retain blockers and grant no cleanup eligibility")
	}
}

func BenchmarkSessionActivityLookup(b *testing.B) {
	paths := make([]string, 20000)
	for i := range paths {
		paths[i] = fmt.Sprintf("/Users/fixture/code/other-project-%06d/node_modules/package/file.js", i)
	}
	a := Activity{Paths: paths, Known: true}
	queries := make([]string, 68)
	for i := range queries {
		queries[i] = fmt.Sprintf("/Users/fixture/.claude/projects/stack/session/sidecar-%03d", i)
	}
	b.Run("existing_linear", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			for _, q := range queries {
				if a.Busy(q) {
					b.Fatal("fixture unexpectedly active")
				}
			}
		}
	})
	b.Run("indexed_including_construction", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			lookup, err := indexActivity(context.Background(), a)
			if err != nil {
				b.Fatal(err)
			}
			for _, q := range queries {
				if lookup.Busy(q) {
					b.Fatal("fixture unexpectedly active")
				}
			}
		}
	})
}
