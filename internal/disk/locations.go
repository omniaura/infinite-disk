package disk

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type Location struct {
	Provider string `json:"provider"`
	Path     string `json:"path"`
	Status   string `json:"status"`
}

// Named layouts are roots only, never deletion permission. Session/history
// directories are deliberately not mistaken for disposable worktrees.
func providerLocations(home string) []Location {
	var rows []Location
	for _, item := range []struct{ provider, path string }{
		{"Claude Code", ".claude/worktrees"}, {"Codex", ".codex/worktrees"}, {"Cursor", ".cursor/worktrees"},
		{"Grok", ".grok/worktrees"}, {"Grok Bot", ".grokbot/worktrees"}, {"T3 Code", ".t3/worktrees"},
		{"Emdash", "emdash/worktrees"}, {"Emdash", ".emdash/worktrees"},
	} {
		rows = append(rows, Location{Provider: item.provider, Path: filepath.Join(home, item.path)})
	}
	return rows
}

func locationProvider(path string) string {
	switch {
	case strings.Contains(path, ".claude/"):
		return "Claude Code"
	case strings.Contains(path, ".codex/"):
		return "Codex"
	case strings.Contains(path, ".cursor/"):
		return "Cursor"
	case strings.Contains(path, ".grok/"):
		return "Grok"
	case strings.Contains(path, ".t3/"):
		return "T3 Code"
	case strings.Contains(strings.ToLower(path), "emdash/"):
		return "Emdash"
	default:
		return "Project workspaces"
	}
}

func locationStatus(c Config, path string) string {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "not present"
	}
	if err != nil || !info.IsDir() || noSymlinkAncestors(path) != nil {
		return "unavailable"
	}
	for _, root := range c.Roots {
		if within(root, path) {
			return "included"
		}
	}
	return "not configured"
}

// Locations optionally discovers embedded .claude/worktrees, .worktrees,
// worktrees and workspaces under explicit project parents. It skips dependency
// trees, never follows symlinks, and bounds metadata traversal. Custom paths
// outside these parents must still be supplied explicitly in config.
func Locations(c Config, home string, embedded bool) ([]Location, []string) {
	return LocationsContext(context.Background(), c, home, embedded)
}

func LocationsContext(ctx context.Context, c Config, home string, embedded bool) ([]Location, []string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows := providerLocations(home)
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Path] = true
	}
	for _, root := range c.Roots {
		if !seen[root] {
			rows = append(rows, Location{Provider: locationProvider(root), Path: root})
			seen[root] = true
		}
	}
	var warnings []string
	if embedded {
		count := 0
		for _, parent := range []string{filepath.Join(home, "code"), filepath.Join(home, "projects")} {
			if _, err := os.Stat(parent); errors.Is(err, os.ErrNotExist) {
				continue
			}
			err := filepath.WalkDir(parent, func(path string, d fs.DirEntry, walkErr error) error {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if walkErr != nil {
					return filepath.SkipDir
				}
				if !d.IsDir() {
					return nil
				}
				count++
				if count > 100000 {
					return errors.New("location discovery limit")
				}
				if path != parent && skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				rel, _ := filepath.Rel(parent, path)
				if strings.Count(rel, string(filepath.Separator)) >= 8 {
					return filepath.SkipDir
				}
				if path != parent && (d.Name() == "worktrees" || d.Name() == ".worktrees" || d.Name() == "workspaces") {
					if !containsCheckout(ctx, path) {
						return nil
					}
					if !seen[path] {
						rows = append(rows, Location{Provider: locationProvider(path), Path: path})
						seen[path] = true
					}
					// This root recursively covers deeper embedded copies too.
					return filepath.SkipDir
				}
				return nil
			})
			if err != nil {
				warnings = append(warnings, "Embedded location discovery hit its time or 100000-directory budget; add custom roots explicitly.")
				break
			}
		}
	}
	for i := range rows {
		rows[i].Status = locationStatus(c, rows[i].Path)
	}
	slices.SortFunc(rows, func(a, b Location) int {
		if v := strings.Compare(a.Provider, b.Provider); v != 0 {
			return v
		}
		return strings.Compare(a.Path, b.Path)
	})
	return rows, warnings
}

func containsCheckout(ctx context.Context, root string) bool {
	found := false
	count := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			return nil
		}
		count++
		if count > 2000 {
			return errors.New("checkout probe limit")
		}
		if path != root {
			if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
				found = true
				return errors.New("found checkout")
			}
		}
		if skipDirs[d.Name()] {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(root, path)
		if strings.Count(rel, string(filepath.Separator)) >= 3 {
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

func AddLocations(c Config, rows []Location) (Config, error) {
	for _, row := range rows {
		if row.Status != "not configured" {
			continue
		}
		if locationStatus(c, row.Path) != "not configured" {
			continue
		}
		c.Roots = append(c.Roots, row.Path)
	}
	return c, c.Validate()
}

func minimalRoots(roots []string) []string {
	sorted := append([]string{}, roots...)
	slices.Sort(sorted)
	var out []string
	for _, root := range sorted {
		covered := false
		for _, prior := range out {
			if within(prior, root) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, root)
		}
	}
	return out
}
