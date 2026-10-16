package disk

import (
	"context"
	"os"
	"testing"
	"time"
)

// Explicit paid local integration test; never enabled by ordinary tests or CI.
func TestLivePiBoundary(t *testing.T) {
	if os.Getenv("INFINITE_DISK_LIVE_TEST") != "1" || os.Getenv("OPENROUTER_API_KEY") == "" {
		t.Skip("explicit local provider smoke only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	f := newFixture(t)
	p := plan(t, f)
	refs := runGit(t, f.repo, "show-ref")
	before := runGit(t, f.repo, "status", "--porcelain")
	if err := SetupAgent(ctx, f.e.Config); err != nil {
		t.Fatal(err)
	}
	result, err := f.e.Triage(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Recommendations) != 1 || len(result.AgentSteps) == 0 || result.Mode != "report-only" {
		t.Fatal("missing bounded metadata triage result")
	}
	if refs != runGit(t, f.repo, "show-ref") || before != runGit(t, f.repo, "status", "--porcelain") {
		t.Fatal("live triage mutated fixture")
	}
	t.Log("Real Go -> embedded Pi 1.3.0 -> OpenRouter -> validated immutable report passed; zero projects mutated")
}
