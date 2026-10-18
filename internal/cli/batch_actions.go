package cli

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/omniaura/infinite-disk/internal/disk"
)

// Keep each action bound to its real immutable plan, never a merged approval
// digest. Deduplicate BEFORE filtering: a later blocker hides an older ready
// result. Parent/child sizes may overlap and are deliberately not totaled.
func allProjectActions(c disk.Config, config, project, filter string, limit int) (actionReport, error) {
	r := actionReport{Version: 1, Mode: "suggestions-only", Projects: []actionItem{}, Warnings: []string{"Historical saved batches, not a fresh whole-disk scan. Newer observations supersede older paths; state is rechecked at approval. Nested paths can overlap: do not sum their sizes. Each action keeps its original plan and expiry."}}
	if _, err := projectActions(disk.Plan{}, config, "", filter, limit); err != nil {
		return r, err
	}
	entries, err := os.ReadDir(filepath.Join(c.StateDir, "plans"))
	if err != nil {
		return r, err
	}
	byPath := map[string]actionItem{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		p, err := disk.LoadPlan(c, strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			continue
		}
		r.SavedBatches++
		items, err := projectActions(p, config, "", "all", 5000)
		if err != nil {
			return r, err
		}
		for _, item := range items.Projects {
			old, exists := byPath[item.Path]
			if !exists || item.ObservedAt.After(old.ObservedAt) || (item.ObservedAt.Equal(old.ObservedAt) && !item.Eligible && old.Eligible) {
				byPath[item.Path] = item
			}
		}
	}
	r.Total = len(byPath)
	found := project == ""
	for _, item := range byPath {
		if project != "" && item.ID != project {
			continue
		}
		found = true
		if filter == "ready" && !item.Eligible || filter == "needs-analysis" && item.Eligible {
			continue
		}
		r.Matching++
		r.Projects = append(r.Projects, item)
	}
	if !found {
		return r, errors.New("project ID is not in the saved reports")
	}
	sort.Slice(r.Projects, func(i, j int) bool {
		if r.Projects[i].SizeBytes == r.Projects[j].SizeBytes {
			return r.Projects[i].ID < r.Projects[j].ID
		}
		return r.Projects[i].SizeBytes > r.Projects[j].SizeBytes
	})
	if len(r.Projects) > limit {
		r.Projects = r.Projects[:limit]
	}
	r.Returned = len(r.Projects)
	return r, nil
}
