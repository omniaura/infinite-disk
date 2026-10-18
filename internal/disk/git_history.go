package disk

import (
	"bytes"
	"context"
	"errors"
)

// Bundles preserve refs/HEAD, not reflog-only or other unreachable objects.
// A linked worktree retains its shared database; a clone destroys its own.
// Keep this expensive check at execution boundaries, not in rolling discovery.
func requireCompleteCloneHistory(ctx context.Context, c Candidate) error {
	if c.Kind != "clone" {
		return nil
	}
	output, err := gitCommand(ctx, c.Path, nil, "fsck", "--unreachable", "--no-reflogs", "--no-progress")
	if err != nil {
		return errors.New("local Git history could not be verified; checkout retained")
	}
	if len(bytes.TrimSpace(output)) != 0 {
		return errors.New("unreachable-only Git history is not covered by recovery; checkout retained for manual preservation")
	}
	return nil
}
