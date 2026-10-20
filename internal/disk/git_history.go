package disk

import (
	"context"
	"errors"
	"strings"
)

// Bundles preserve refs/HEAD, not reflog-only or other unreachable objects.
// A linked worktree retains its shared database; a clone destroys its own.
// Keep this expensive check at execution boundaries, not in rolling discovery.
func requireCompleteCloneHistory(ctx context.Context, c Candidate) error {
	if c.Kind != "clone" {
		return nil
	}
	// Object verification can legitimately exceed the lightweight Git reader's
	// 20-second budget, especially under disk pressure. Use the same bounded
	// two-minute runner as bundle preservation; caller cancellation still wins.
	// Every execution boundary must finish fsck, never reuse an earlier result.
	output, err := gitInput(ctx, c.Path, nil, nil, "fsck", "--unreachable", "--no-reflogs", "--no-progress")
	if err != nil {
		return errors.New("local Git history could not be verified; checkout retained")
	}
	if strings.TrimSpace(output) != "" {
		return errors.New("unreachable-only Git history is not covered by recovery; checkout retained for manual preservation")
	}
	return nil
}
