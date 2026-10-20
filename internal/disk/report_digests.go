package disk

import (
	"context"
	"errors"
	"os"
)

// Never attach this cache to Engine, persisted plans or a mutation path. It
// belongs only to one Scan/AnalyzeSelected call. Each candidate still walks
// the complete current session inventory, enforcing link and size budgets.
// File identity includes device/inode, mode, size, nanosecond mtime AND ctime.
// Missing/unsupported identity falls back to a full read, never a cache hit.
type reportDigests struct {
	ctx     context.Context
	entries map[string]reportDigest
	read    func(string) (string, error)
}

type reportDigest struct{ identity, sha string }

var errUncacheableReportIdentity = errors.New("session file identity cannot be reused")

func newReportDigests(ctx context.Context) *reportDigests {
	return &reportDigests{ctx: ctx, entries: map[string]reportDigest{}, read: digestFile}
}

func reportFileIdentity(name string) (string, error) {
	if noSymlinkAncestors(name) != nil {
		return "", errors.New("unsafe session digest path")
	}
	s, err := os.Lstat(name)
	if err != nil || !s.Mode().IsRegular() {
		return "", errors.New("session digest file unavailable")
	}
	identity, err := artifactIdentity(s)
	if err != nil {
		return "", errUncacheableReportIdentity
	}
	return identity, nil
}

func (r *reportDigests) sum(name string) (string, error) {
	if err := r.ctx.Err(); err != nil {
		return "", err
	}
	before, err := reportFileIdentity(name)
	if err != nil {
		// A cache is not a new permission or a way around the caller's complete
		// inventory checks. Unsupported metadata simply gets the existing read.
		if errors.Is(err, errUncacheableReportIdentity) {
			return r.read(name)
		}
		return "", err
	}
	if saved, ok := r.entries[name]; ok && saved.identity == before {
		after, err := reportFileIdentity(name)
		if err != nil || after != before || r.ctx.Err() != nil {
			return "", errors.New("session changed during report inspection")
		}
		return saved.sha, nil
	}
	sha, err := r.read(name)
	if err != nil {
		return "", err
	}
	after, err := reportFileIdentity(name)
	if err != nil || after != before || r.ctx.Err() != nil {
		return "", errors.New("session changed during report inspection")
	}
	if len(r.entries) < 5000 {
		r.entries[name] = reportDigest{before, sha}
	}
	return sha, nil
}
