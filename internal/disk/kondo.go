package disk

// Kondo owns the ecosystem catalog. It is invoked ONLY in dry-run mode;
// Infinite Disk independently checks and approves each exact artifact directory.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func (e *Engine) CacheConfig() Config {
	c := e.Config
	c.StateDir = filepath.Join(c.StateDir, "kondo")
	return c
}

var kondoHeader = regexp.MustCompile(`^(/.+?) (Node \(React Native\)|Godot 4\.x|\S+) project(?: \(.*\))?\s*$`)
var kondoArtifact = regexp.MustCompile(`^  └─ (.+) \([0-9.]+(?:B|KiB|MiB|GiB|TiB|PiB|EiB)\)$`)

func parseKondo(output []byte) ([]Candidate, error) {
	var candidates []Candidate
	project := ""
	summary := false
	seen := map[string]bool{}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "Projects cleaned: 0/") && strings.Contains(line, ", Bytes deleted: 0.0B / ") {
			summary = true
			continue
		}
		if summary {
			return nil, errors.New("unsupported Kondo output after summary")
		}
		if m := kondoHeader.FindStringSubmatch(line); m != nil {
			project = m[1]
			continue
		}
		if m := kondoArtifact.FindStringSubmatch(line); m != nil && project != "" && safeRelative(m[1]) {
			path := filepath.Join(project, m[1])
			if seen[path] {
				return nil, errors.New("overlapping Kondo output")
			}
			seen[path] = true
			candidates = append(candidates, Candidate{ID: pathID(path), Path: path, Name: filepath.Base(project), CommonDir: project, Kind: "kondo-cache", Blockers: []string{}, Evidence: []Evidence{{"kondo", "discovered", "Kondo dry-run identified this artifact directory"}}})
			if len(candidates) > 10000 {
				return nil, errors.New("Kondo result exceeded safety limit")
			}
			continue
		}
		return nil, errors.New("unsupported Kondo output; no cleanup authorized")
	}
	if !summary {
		return nil, errors.New("incomplete Kondo output; no cleanup authorized")
	}
	for i, a := range candidates {
		for j, b := range candidates {
			if i != j && within(a.Path, b.Path) {
				return nil, errors.New("overlapping Kondo artifacts require manual review")
			}
		}
	}
	return candidates, nil
}

func (e *Engine) CacheScan(ctx context.Context) (Plan, error) {
	release, err := acquireLock(e.Config)
	if err != nil {
		return Plan{}, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	args := []string{"--dry-run", "--same-filesystem", "--older", fmt.Sprintf("%dd", e.Config.IdleDays), "--"}
	p := Plan{Version: ProtocolVersion, CreatedAt: e.Now().UTC(), Mode: "report-only", ConfigDigest: ConfigDigest(e.CacheConfig()), Candidates: []Candidate{}, Warnings: []string{}}
	for _, root := range minimalRoots(e.Config.Roots) {
		if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
			p.Warnings = append(p.Warnings, "A configured root is missing.")
			continue
		}
		if err := noSymlinkAncestors(root); err != nil {
			return p, err
		}
		args = append(args, root)
	}
	if len(args) == 5 {
		return p, errors.New("no existing discovery roots")
	}
	cmd := exec.CommandContext(ctx, "kondo", args...)
	cmd.Env = gitEnvironment()
	out := &cappedBuffer{limit: 8 << 20}
	diagnostics := &cappedBuffer{limit: 64 << 10}
	cmd.Stdout = out
	cmd.Stderr = diagnostics
	if err := cmd.Run(); err != nil || out.overflow || diagnostics.Len() > 0 || diagnostics.overflow {
		if ctx.Err() != nil {
			return p, ctx.Err()
		}
		return p, errors.New("Kondo dry-run failed; install Kondo 0.9 with --dry-run support, or inspect configured roots")
	}
	candidates, err := parseKondo(out.Bytes())
	if err != nil {
		return p, err
	}
	activity := e.Activity(ctx)
	for _, c := range candidates {
		if ctx.Err() != nil {
			return p, ctx.Err()
		}
		p.Candidates = append(p.Candidates, e.inspectCache(ctx, c, activity))
	}
	home, _ := os.UserHomeDir()
	p.Space, err = DiskSpace(home)
	if err != nil {
		return p, err
	}
	return SavePlan(e.CacheConfig(), p)
}

func (e *Engine) inspectCache(ctx context.Context, c Candidate, activity Activity) Candidate {
	c.Eligible = true
	c.Blockers = []string{}
	c.SizeComplete = false
	if c.Kind != "kondo-cache" || !within(c.CommonDir, c.Path) || c.CommonDir == c.Path {
		block(&c, "invalid_artifact_scope")
		return c
	}
	if err := e.safeTarget(c.CommonDir); err != nil {
		block(&c, err.Error())
		return measureCandidate(ctx, c)
	}
	if err := noSymlinkAncestors(c.Path); err != nil {
		block(&c, "symlinked_artifact")
		return c
	}
	if info, err := os.Lstat(c.Path); err != nil || !info.IsDir() {
		block(&c, "artifact_not_a_directory")
		return c
	}
	if !activity.Known {
		block(&c, "activity_check_unavailable")
	} else if activity.Busy(c.CommonDir) {
		block(&c, "project_in_use")
	}
	if !c.Eligible {
		return measureCandidate(ctx, c)
	}
	relative, err := filepath.Rel(c.CommonDir, c.Path)
	if err != nil || !safeRelative(relative) {
		block(&c, "invalid_artifact_scope")
		return c
	}
	// ls-files does not run content filters or hooks. Require an entirely ignored,
	// untracked directory; Kondo classification alone is never deletion permission.
	tracked, err := gitCommand(ctx, c.CommonDir, nil, "ls-files", "-z", "--", ":(literal)"+filepath.ToSlash(relative))
	if err != nil || len(tracked) > 0 {
		block(&c, "artifact_contains_tracked_files")
	}
	top, err := gitText(ctx, c.CommonDir, "rev-parse", "--show-toplevel")
	if err != nil || !within(top, c.CommonDir) {
		block(&c, "artifact_repository_unknown")
	}
	if top != "" && activity.Busy(top) {
		block(&c, "project_in_use")
	}
	if !c.Eligible {
		return measureCandidate(ctx, c)
	}
	h := sha256.New()
	count := 0
	newest := time.Time{}
	err = filepath.WalkDir(c.Path, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		count++
		if count > 250000 {
			return errors.New("artifact inspection limit")
		}
		rel, _ := filepath.Rel(c.CommonDir, path)
		if d.Name() == ".git" || sensitiveName.MatchString(filepath.ToSlash(rel)) {
			return errors.New("artifact contains private state or repository")
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("artifact contains a symlink; requires manual review")
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("artifact contains special files")
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		fmt.Fprintf(h, "%q %d %d %d\n", rel, info.Mode(), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		block(&c, "artifact_uninspectable_or_private")
	}
	// --no-index also checks force-added files; tracked-file rejection above is
	// separate. Every artifact file must be ignored, not merely its parent.
	var paths bytes.Buffer
	if c.Eligible {
		err = filepath.WalkDir(c.Path, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !d.IsDir() {
				rel, _ := filepath.Rel(c.CommonDir, path)
				paths.WriteString(rel)
				paths.WriteByte(0)
			}
			return nil
		})
		if err != nil || paths.Len() > 16<<20 {
			block(&c, "artifact_ignore_state_unknown")
		} else {
			ignored, err := gitInput(ctx, c.CommonDir, nil, paths.Bytes(), "check-ignore", "--no-index", "-z", "--stdin")
			if err != nil || ignored != paths.String() {
				block(&c, "artifact_contains_unignored_files")
			}
		}
	}
	c.LastModified = newest
	if newest.IsZero() || e.Now().Sub(newest) < time.Duration(e.Config.IdleDays)*24*time.Hour {
		block(&c, "recent_artifact_activity")
	}
	c.Fingerprint = hex.EncodeToString(h.Sum(nil))
	c = measureCandidate(ctx, c)
	if !c.SizeComplete {
		block(&c, "artifact_size_incomplete")
	}
	return c
}

// ApplyCache never invokes Kondo's recursive cleaner, commits or pushes. It
// removes only one immutable, revalidated, explicitly approved artifact path.
func (e *Engine) ApplyCache(ctx context.Context, p Plan, id, approval string) (Receipt, error) {
	r := Receipt{Version: ProtocolVersion, PlanID: p.ID, CandidateID: id, ApprovedAt: e.Now().UTC(), Result: "retained"}
	if p.Version != ProtocolVersion || approval != p.ID || planID(p) != p.ID || p.ConfigDigest != ConfigDigest(e.CacheConfig()) || p.Mode != "report-only" || e.Now().Sub(p.CreatedAt) > 24*time.Hour || p.CreatedAt.After(e.Now().Add(time.Minute)) {
		return r, errors.New("cache report expired or changed; rescan")
	}
	release, err := acquireLock(e.Config)
	if err != nil {
		return r, err
	}
	defer release()
	var selected *Candidate
	for _, candidate := range p.Candidates {
		if candidate.ID == id {
			copy := candidate
			selected = &copy
		}
	}
	if selected == nil || !selected.Eligible || selected.Kind != "kondo-cache" {
		return r, errors.New("artifact is not eligible")
	}
	fresh := e.inspectCache(ctx, *selected, e.Activity(ctx))
	if !fresh.Eligible || !fresh.SizeComplete || fresh.Fingerprint != selected.Fingerprint || ctx.Err() != nil {
		return r, errors.New("artifact changed, active, private or unverified; retained")
	}
	r.Path = fresh.Path
	r.BeforeSpace, _ = DiskSpace(fresh.Path)
	if err = AppendEvent(e.Config, Event{Type: "cache_removal_approved", PlanID: p.ID, CandidateID: id, Outcome: "approved"}); err != nil {
		return r, err
	}
	// A second check narrows the race with writers. Same-user mutation cannot be
	// fully excluded without filesystem isolation; stop project tools first.
	fresh = e.inspectCache(ctx, *selected, e.Activity(ctx))
	if !fresh.Eligible || fresh.Fingerprint != selected.Fingerprint || ctx.Err() != nil {
		return r, errors.New("artifact changed before removal; retained")
	}
	if err = os.RemoveAll(fresh.Path); err != nil {
		return r, errors.New("artifact removal incomplete; inspect exact artifact path")
	}
	r.Result = "cache_removed"
	r.AfterSpace, _ = DiskSpace(fresh.CommonDir)
	receipt, _ := marshalJSON(r)
	if err = atomicPrivate(filepath.Join(e.CacheConfig().StateDir, "receipts", p.ID+"-"+id+".json"), receipt); err != nil {
		return r, err
	}
	return r, AppendEvent(e.Config, Event{Type: "cache_removed", PlanID: p.ID, CandidateID: id, Outcome: r.Result})
}
