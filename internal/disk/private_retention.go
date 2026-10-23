package disk

// Recovery retention is state-local and separately approved. It never changes
// a source checkout, Git ref, native session store, or the original archive plan.
import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

type PrivateDiscard struct {
	Entry  PrivateEntry `json:"entry"`
	Policy string       `json:"policy"`
}

type PrivatePrunePlan struct {
	Version  int              `json:"version"`
	ID       string           `json:"id"`
	StateDir string           `json:"state_dir"`
	Archive  string           `json:"archive"`
	Entries  []PrivateDiscard `json:"entries"`
	Bytes    uint64           `json:"bytes"`
}

func privatePruneID(p PrivatePrunePlan) string {
	p.ID = ""
	b, _ := json.Marshal(p)
	return PlanDigest(b)
}

// A recognition boundary is not deletion permission. Private archive creation
// must refuse these outputs; a separate reviewed cleanup still owns removal.
func regenerableBoundary(relative string) bool {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for _, part := range parts {
		switch part {
		case ".git", ".agents", ".claude", ".codex", ".cursor", ".pi", ".opencode", ".t3", ".grok", ".emdash", "vendor", "third_party", "third-party", "fixtures", "testdata":
			return false
		}
	}
	for i, part := range parts {
		switch part {
		case "node_modules", ".venv", "__pycache__", ".mypy_cache", ".pytest_cache", ".ruff_cache", ".next", ".turbo", "dist", "target", "go-build", "go-cache", "build-cache":
			return true
		case ".xgo":
			if i+1 < len(parts) && parts[i+1] == "gen" {
				return true
			}
		}
	}
	return false
}

func rejectRegenerableArchive(ctx context.Context, root, relative string) error {
	if regenerableBoundary(relative) {
		return errors.New("regenerable build/cache output must be reviewed and cleaned separately, not privately archived")
	}
	count := 0
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		count++
		if count > 250000 {
			return errors.New("private regeneration-boundary inspection incomplete")
		}
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if regenerableBoundary(filepath.Join(relative, r)) {
			return errors.New("mixed private entry contains regenerable output; clean it separately and rescan")
		}
		if !d.IsDir() && !binaryArtifactReserved(filepath.Join(relative, r)) {
			if _, err := binaryArtifactFormat(p); err == nil {
				return errors.New("compiled executable requires a separate keep/archive/discard decision, not blanket private preservation")
			}
		}
		return nil
	})
}

func discardedPrivate(p PrivatePlan, r PrivateArchive) (map[string]bool, error) {
	out := map[string]bool{}
	for _, discard := range r.Discarded {
		match := slices.Contains(p.Entries, discard.Entry)
		allowed := discard.Policy == "reviewed_regenerable" && regenerableBoundary(discard.Entry.Path)
		allowed = allowed || discard.Policy == "verified_xgo_gen_only" && filepath.Base(discard.Entry.Path) == ".xgo"
		allowed = allowed || discard.Policy == "reviewed_compiled_only" && !binaryArtifactReserved(discard.Entry.Path) && !sensitiveName.MatchString(filepath.ToSlash(discard.Entry.Path))
		legacyPackage := discard.Policy == "reviewed_regenerable" && legacyInstalledPackageEntry(discard.Entry.Path)
		if !match || !allowed || sensitiveName.MatchString(filepath.ToSlash(discard.Entry.Path)) && !legacyPackage || possibleCodingSession(discard.Entry.Path, nodeDependencyPath(discard.Entry.Path)) || out[discard.Entry.Path] {
			return nil, errors.New("invalid private discard receipt")
		}
		out[discard.Entry.Path] = true
	}
	return out, nil
}

var legacyPackageComponent = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$`)
var legacyPythonDirectory = regexp.MustCompile(`^python[0-9]+\.[0-9]+$`)

// Older archives selected installed package files such as credentials.py and
// cacert.pem by name. Accept their completed discard receipts only; callers
// must still validate absence. This never approves a new deletion or proves
// the contents of a previously removed file were harmless.
func legacyInstalledPackageEntry(relative string) bool {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for _, part := range parts {
		switch strings.ToLower(part) {
		case ".git", ".agents", ".claude", ".codex", ".cursor", ".pi", ".opencode", ".t3", ".grok", ".emdash", "vendor", "third_party", "third-party", "fixtures", "testdata":
			return false
		}
	}
	for i, part := range parts {
		for _, ancestor := range parts[:i] {
			if sensitiveName.MatchString(ancestor) {
				return false
			}
		}
		pkg := -1
		if part == "node_modules" && i+1 < len(parts) {
			pkg = i + 1
			if strings.HasPrefix(parts[pkg], "@") {
				if !legacyPackageComponent.MatchString(strings.TrimPrefix(parts[pkg], "@")) {
					continue
				}
				pkg++
			}
		} else if part == ".venv" && i+4 < len(parts) && parts[i+1] == "lib" && legacyPythonDirectory.MatchString(parts[i+2]) && parts[i+3] == "site-packages" {
			pkg = i + 4
		}
		if pkg >= 0 && pkg+1 < len(parts) && legacyPackageComponent.MatchString(parts[pkg]) && !sensitiveName.MatchString(parts[pkg]) {
			return true
		}
	}
	return false
}

// Receipts describe completed removal, not permission to suppress a payload
// that still exists. Interrupted or inconsistent discards require inspection.
// These owner-private records are not authentication against same-owner edits.
func (e *Engine) validatedPrivateDiscards(ctx context.Context, p PrivatePlan, r PrivateArchive) (map[string]bool, error) {
	discarded, err := discardedPrivate(p, r)
	if err != nil {
		return nil, err
	}
	for name := range discarded {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		root := filepath.Join(e.privateDir(p.ID), "payload", name)
		if noSymlinkAncestors(root) != nil {
			return nil, errors.New("unsafe discarded payload path; retained")
		}
		if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("discarded payload remains or cannot be inspected; review interrupted cleanup before recovery")
		}
	}
	return discarded, nil
}

// The archive is an immutable state artifact, independent of later project
// discovery configuration. Validate its content ID rather than bypassing the
// config binding of ordinary checkout apply/restore operations.
func (e *Engine) retentionArchive(ctx context.Context, id string) (PrivatePlan, PrivateArchive, error) {
	var p PrivatePlan
	if !validPlanID.MatchString(id) {
		return p, PrivateArchive{}, errors.New("invalid archive ID")
	}
	b, err := readPrivateFile(filepath.Join(e.privateDir(id), "plan.json"), 16<<20)
	if err != nil || json.Unmarshal(b, &p) != nil || p.Version != 1 || p.ID != id || privateID(p) != id || pathID(p.Candidate.Path) != p.Candidate.ID {
		return p, PrivateArchive{}, errors.New("invalid immutable archive plan")
	}
	for _, entry := range p.Entries {
		if !privateRelative(entry.Path) || !validPlanID.MatchString(entry.SHA256) {
			return p, PrivateArchive{}, errors.New("invalid archive entry")
		}
	}
	r, err := e.PrivateReceipt(id)
	if err != nil || r.Status != "verified_local_archive" {
		return p, r, errors.New("only verified local archives can be pruned")
	}
	if _, err = e.validatedPrivateDiscards(ctx, p, r); err != nil {
		return p, r, err
	}
	return p, r, nil
}

func (e *Engine) PlanPrivatePrune(ctx context.Context, id string, paths []string) (PrivatePrunePlan, error) {
	var out PrivatePrunePlan
	release, err := acquireLock(e.Config)
	if err != nil {
		return out, err
	}
	defer release()
	return e.planPrivatePrune(ctx, id, paths)
}

func (e *Engine) planPrivatePrune(ctx context.Context, id string, paths []string) (PrivatePrunePlan, error) {
	out := PrivatePrunePlan{Version: 1, Archive: id, StateDir: e.Config.StateDir}
	p, r, err := e.retentionArchive(ctx, id)
	if err != nil {
		return out, err
	}
	discarded, err := discardedPrivate(p, r)
	if err != nil {
		return out, err
	}
	if len(paths) == 0 || len(paths) > 1000 {
		return out, errors.New("select exact archived entry paths")
	}
	paths = slices.Clone(paths)
	slices.Sort(paths)
	a := e.Activity(ctx)
	if !a.Known || a.Busy(e.privateDir(id)) {
		return out, errors.New("archive is in use or activity unknown")
	}
	for i, name := range paths {
		if possibleCodingSession(name, nodeDependencyPath(name)) {
			return out, errors.New("native session entry cannot be pruned as generated output")
		}
		if i > 0 && paths[i-1] == name || discarded[name] {
			return out, errors.New("duplicate or previously discarded entry")
		}
		index := slices.IndexFunc(p.Entries, func(entry PrivateEntry) bool { return entry.Path == name })
		if index < 0 || !slices.Contains(r.Verified, name) {
			return out, errors.New("only complete verified archive entries can be pruned")
		}
		entry := p.Entries[index]
		root := filepath.Join(e.privateDir(id), "payload", name)
		policy := "reviewed_regenerable"
		if !regenerableBoundary(name) {
			// Never discard all of .xgo merely because the name looks generated.
			children, readErr := os.ReadDir(root)
			if filepath.Base(name) == ".xgo" && readErr == nil && len(children) == 1 && children[0].Name() == "gen" && children[0].IsDir() {
				policy = "verified_xgo_gen_only"
			} else if compiledPrivateRoot(ctx, root, name) == nil {
				policy = "reviewed_compiled_only"
			} else {
				return out, errors.New("entry is not a recognized regenerable boundary or reviewed compiled-only output")
			}
		}
		count := 0
		if err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			count++
			if count > 250000 {
				return errors.New("private retention inspection incomplete")
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			archivedName := filepath.Join(name, rel)
			if possibleCodingSession(archivedName, false) || sensitiveName.MatchString(filepath.ToSlash(archivedName)) {
				return errors.New("private/session data in generated output; retained")
			}
			if !d.IsDir() {
				if _, _, _, err := artifactFileDigest(ctx, path); err != nil {
					return errors.New("generated output content inspection failed; retained")
				}
			}
			return nil
		}); err != nil {
			return out, err
		}
		actual, err := privateTree(ctx, root)
		if err != nil || actual.SHA256 != entry.SHA256 || actual.Files != entry.Files || actual.Bytes != entry.Bytes {
			return out, errors.New("archive content changed; retained")
		}
		out.Entries = append(out.Entries, PrivateDiscard{entry, policy})
		out.Bytes += entry.Bytes
	}
	out.ID = privatePruneID(out)
	return out, nil
}

// Recognition alone never approves discard. A whole selected archive entry
// must consist solely of recognized executables, with no private/native data.
func compiledPrivateRoot(ctx context.Context, root, relative string) error {
	count, files := 0, 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		count++
		if count > 250000 {
			return errors.New("compiled output inventory incomplete")
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := filepath.Join(relative, rel)
		if binaryArtifactReserved(name) || sensitiveName.MatchString(filepath.ToSlash(name)) {
			return errors.New("reserved/private compiled output path")
		}
		if d.IsDir() {
			return nil
		}
		if _, err := binaryArtifactFormat(p); err != nil {
			return errors.New("mixed compiled output contains non-executable work")
		}
		files++
		return nil
	})
	if err != nil {
		return err
	}
	if files == 0 {
		return errors.New("no recognized executables")
	}
	return nil
}

func (e *Engine) PrunePrivate(ctx context.Context, id string, paths []string, approval string) (PrivatePrunePlan, error) {
	var out PrivatePrunePlan
	if !validPlanID.MatchString(approval) {
		return out, errors.New("exact retention-plan approval required")
	}
	release, err := acquireLock(e.Config)
	if err != nil {
		return out, err
	}
	defer release()
	out, err = e.planPrivatePrune(ctx, id, paths)
	if err != nil {
		return out, err
	}
	if approval != out.ID {
		return out, errors.New("retention plan changed or approval does not match")
	}
	_, r, err := e.retentionArchive(ctx, id)
	if err != nil {
		return out, err
	}
	for _, discard := range out.Entries {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		a := e.Activity(ctx)
		if !a.Known || a.Busy(e.privateDir(id)) {
			return out, errors.New("archive activity changed; retained")
		}
		root := filepath.Join(e.privateDir(id), "payload", discard.Entry.Path)
		actual, err := privateTree(ctx, root)
		if err != nil || actual.SHA256 != discard.Entry.SHA256 {
			return out, errors.New("archive changed before removal")
		}
		// Durable intent first. On interruption this entry is explicitly no longer
		// required for recovery; all other entries retain their original hashes.
		r.Discarded = append(r.Discarded, discard)
		if err = e.saveArchive(r); err != nil {
			return out, err
		}
		if err = os.RemoveAll(root); err != nil {
			return out, errors.New("approved discard interrupted; other recovery data retained")
		}
		if err = syncDir(filepath.Dir(root)); err != nil {
			return out, err
		}
	}
	return out, AppendEvent(e.Config, Event{Type: "private_regenerable_pruned", PlanID: out.ID, Outcome: "approved_exact_entries"})
}
