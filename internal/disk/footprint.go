package disk

import (
	"path/filepath"
	"slices"
)

// MeasuredFootprint excludes unmeasured entries and nested double-counting.
// It is allocated directory size, not a promise of net APFS/backup savings.
func MeasuredFootprint(candidates []Candidate) (bytes uint64, overlaps bool) {
	rows := append([]Candidate(nil), candidates...)
	slices.SortFunc(rows, func(a, b Candidate) int { return len(a.Path) - len(b.Path) })
	var counted []string
	for _, c := range rows {
		if !c.SizeComplete {
			continue
		}
		path := filepath.Clean(c.Path)
		covered := false
		for _, parent := range counted {
			if within(parent, path) {
				covered = true
				overlaps = true
				break
			}
		}
		if !covered {
			bytes += c.SizeBytes
			counted = append(counted, path)
		}
	}
	return
}
