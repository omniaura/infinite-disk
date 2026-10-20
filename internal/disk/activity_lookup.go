package disk

import (
	"context"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
)

// A lookup belongs to one immutable activity observation, not an engine or a
// saved plan. Every later preservation/removal check still reads fresh activity.
// Session handoffs can check thousands of sidecars against tens of thousands
// of open paths; repeatedly running filepath.Rel over that cross product can
// consume the inspection deadline without doing useful new work.
type activityLookup struct {
	source Activity
	paths  []string
}

func indexActivity(ctx context.Context, source Activity) (activityLookup, error) {
	lookup := activityLookup{source: source}
	if runtime.GOOS == "windows" {
		// Preserve platform-specific volume/case behavior until separately tested.
		return lookup, ctx.Err()
	}
	lookup.paths = make([]string, 0, len(source.Paths))
	for i, p := range source.Paths {
		if i%256 == 0 && ctx.Err() != nil {
			return activityLookup{}, ctx.Err()
		}
		// For an absolute query, Rel rejects a relative observed path. Do not
		// turn a relative path into an absolute one by consulting the current cwd.
		if filepath.IsAbs(p) {
			lookup.paths = append(lookup.paths, filepath.Clean(p))
		}
	}
	sort.Strings(lookup.paths)
	lookup.paths = slices.Compact(lookup.paths)
	return lookup, ctx.Err()
}

func (lookup activityLookup) Busy(path string) bool {
	if runtime.GOOS == "windows" || !filepath.IsAbs(path) {
		return lookup.source.Busy(path)
	}
	clean := filepath.Clean(path)
	i := sort.SearchStrings(lookup.paths, clean)
	if i < len(lookup.paths) && lookup.paths[i] == clean {
		return true
	}
	prefix := clean
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	i = sort.SearchStrings(lookup.paths, prefix)
	return i < len(lookup.paths) && strings.HasPrefix(lookup.paths[i], prefix)
}
