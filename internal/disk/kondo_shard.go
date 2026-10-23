package disk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// A single Kondo process over every root cannot finish on machines with many
// agent worktrees, and one timeout used to discard all findings. Discovery is
// sharded instead: each folder gets its own bounded dry-run, a folder that
// times out is split into its children, and whatever completed is kept. Sharding
// only changes where Kondo looks; every candidate still passes inspectCache.
const (
	kondoShardWorkers = 4
	kondoMaxDepth     = 4
)

var kondoShardTimeout = 90 * time.Second

type kondoShard struct {
	dir   string
	depth int
}

type kondoShardOutcome struct {
	candidates []Candidate
	timedOut   bool
	failed     bool
}

// kondoDiscover returns candidates from every shard that completed, plus
// warnings describing coverage that was skipped. A missing Kondo binary is the
// only hard error; per-folder failures degrade to partial coverage.
func (e *Engine) kondoDiscover(ctx context.Context, roots []string) ([]Candidate, []string, error) {
	if _, err := exec.LookPath("kondo"); err != nil {
		return nil, nil, errors.New("Kondo is not installed; install Kondo 0.9 with --dry-run support")
	}
	var all []Candidate
	var warnings []string
	skippedSlow, skippedFailed := 0, 0
	level := make([]kondoShard, 0, len(roots))
	for _, root := range roots {
		// Start from each root's children: the root itself is almost always a
		// parent folder, and scanning it whole is what timed out.
		children := kondoChildren(root)
		if len(children) == 0 {
			level = append(level, kondoShard{root, 1})
			continue
		}
		for _, child := range children {
			level = append(level, kondoShard{child, 1})
		}
	}
	done := 0
	total := len(level)
	for len(level) > 0 && ctx.Err() == nil {
		outcomes := make([]kondoShardOutcome, len(level))
		sem := make(chan struct{}, kondoShardWorkers)
		var wg sync.WaitGroup
		var mu sync.Mutex
		for i, shard := range level {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				outcomes[i] = e.kondoShardRun(ctx, shard.dir)
				mu.Lock()
				done++
				e.progress("Finding build artifacts with Kondo", done, total)
				mu.Unlock()
			}()
		}
		wg.Wait()
		var next []kondoShard
		for i, o := range outcomes {
			shard := level[i]
			switch {
			case o.timedOut && shard.depth < kondoMaxDepth:
				children := kondoChildren(shard.dir)
				for _, child := range children {
					next = append(next, kondoShard{child, shard.depth + 1})
				}
				total += len(children)
				if len(children) == 0 {
					skippedSlow++
				}
			case o.timedOut:
				skippedSlow++
			case o.failed:
				skippedFailed++
			default:
				all = append(all, o.candidates...)
			}
		}
		level = next
	}
	if ctx.Err() != nil {
		warnings = append(warnings, "Artifact discovery hit its overall time limit; results are partial.")
	}
	if skippedSlow > 0 {
		warnings = append(warnings, fmt.Sprintf("%d folders were too slow for Kondo to scan and were skipped; results are partial.", skippedSlow))
	}
	if skippedFailed > 0 {
		warnings = append(warnings, fmt.Sprintf("%d folders could not be scanned by Kondo and were skipped; results are partial.", skippedFailed))
	}
	slices.SortFunc(all, func(a, b Candidate) int {
		if a.Path < b.Path {
			return -1
		}
		if a.Path > b.Path {
			return 1
		}
		return 0
	})
	// Shards are disjoint folders, but keep the fail-closed overlap rule.
	for i := 1; i < len(all); i++ {
		if within(all[i-1].Path, all[i].Path) || all[i-1].Path == all[i].Path {
			return nil, warnings, errors.New("overlapping Kondo artifacts require manual review")
		}
	}
	if len(all) > 10000 {
		return nil, warnings, errors.New("Kondo result exceeded safety limit")
	}
	return all, warnings, nil
}

func (e *Engine) kondoShardRun(parent context.Context, dir string) kondoShardOutcome {
	ctx, cancel := context.WithTimeout(parent, kondoShardTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kondo", "--dry-run", "--same-filesystem", "--older", "0d", "--", dir)
	cmd.Env = gitEnvironment()
	cmd.WaitDelay = time.Second
	out := &cappedBuffer{limit: 8 << 20}
	diagnostics := &cappedBuffer{limit: 64 << 10}
	cmd.Stdout = out
	cmd.Stderr = diagnostics
	err := cmd.Run()
	if parent.Err() != nil {
		return kondoShardOutcome{failed: true}
	}
	if ctx.Err() != nil {
		return kondoShardOutcome{timedOut: true}
	}
	if err != nil || out.overflow || diagnostics.Len() > 0 || diagnostics.overflow {
		return kondoShardOutcome{failed: true}
	}
	// A folder with no projects prints only the summary line.
	candidates, err := parseKondo(out.Bytes())
	if err != nil {
		return kondoShardOutcome{failed: true}
	}
	return kondoShardOutcome{candidates: candidates}
}

// kondoChildren lists real (non-symlink) subfolders worth scanning. Hidden
// agent worktree folders are included; repository internals and dependency
// directories are not, since Kondo reports those as artifacts of their parent.
func kondoChildren(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		switch entry.Name() {
		case ".git", "node_modules":
			continue
		}
		out = append(out, filepath.Join(dir, entry.Name()))
	}
	return out
}
