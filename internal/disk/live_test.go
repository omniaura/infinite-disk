package disk

import (
	"context"
	"os"
	"path/filepath"
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
	write(t, filepath.Join(f.repo, ".git", "info", "exclude"), "private/\n")
	write(t, filepath.Join(f.repo, "private", "state"), "private fixture, never sent to Pi")
	write(t, filepath.Join(f.repo, "pending.txt"), "unfinished fixture work\n")
	old(t, filepath.Join(f.repo, "pending.txt"))
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
	if result.Candidates[0].Eligible || (result.Recommendations[0].Decision != "analyze" && result.Recommendations[0].Decision != "blocked") || len(result.Recommendations[0].SuggestedActions) == 0 {
		t.Fatal("missing blocked-project suggestions or safety override")
	}
	usedSuggestion := false
	for _, step := range result.AgentSteps {
		if step.Check == "suggest_action" {
			usedSuggestion = true
		}
	}
	if !usedSuggestion {
		t.Fatal("Pi did not exercise suggestion-only tool")
	}
	if refs != runGit(t, f.repo, "show-ref") || before != runGit(t, f.repo, "status", "--porcelain") {
		t.Fatal("live triage mutated fixture")
	}
	if runGit(t, f.remote, "for-each-ref", "--format=%(refname)") != "refs/heads/main" {
		t.Fatal("suggestion changed remote")
	}
	t.Log("Real Go -> embedded Pi 1.3.0 -> OpenRouter -> validated immutable report passed; zero projects mutated")
}
