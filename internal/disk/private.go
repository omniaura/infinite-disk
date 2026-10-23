package disk

// Private archives are a separate, explicit local action. They never grant
// checkout-removal permission, push data, or alter native conversation stores.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

type PrivateEntry struct {
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Bytes    uint64 `json:"bytes"`
	Files    int    `json:"files"`
	Identity string `json:"identity"`
}
type PrivatePlan struct {
	Version      int            `json:"version"`
	ID           string         `json:"id"`
	CreatedAt    time.Time      `json:"created_at"`
	ConfigDigest string         `json:"config_digest"`
	SourcePlan   string         `json:"source_plan"`
	Candidate    Candidate      `json:"candidate"`
	Entries      []PrivateEntry `json:"entries"`
	Bytes        uint64         `json:"bytes"`
}
type PrivateArchive struct {
	Version   int              `json:"version"`
	PlanID    string           `json:"plan_id"`
	Status    string           `json:"status"`
	Moved     []string         `json:"moved"`
	Verified  []string         `json:"verified"`
	Discarded []PrivateDiscard `json:"discarded,omitempty"`
}

func privateID(p PrivatePlan) string { p.ID = ""; b, _ := json.Marshal(p); return PlanDigest(b) }
func privateRelative(p string) bool {
	if !safeRelative(p) || p == "." || strings.ContainsAny(p, "\r\n\x1b") {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}
func readPrivateFile(name string, limit int64) ([]byte, error) {
	if err := noSymlinkAncestors(name); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > limit {
		return nil, errors.New("state must be a bounded private regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if len(b) > int(limit) {
		return nil, errors.New("private state size changed")
	}
	return b, err
}
func (e *Engine) privateDir(id string) string {
	return filepath.Join(e.Config.StateDir, "private-archives", id)
}
func privateIdentity(s os.FileInfo) (string, error) {
	st, ok := s.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return "", errors.New("private file identity unavailable")
	}
	if !s.IsDir() && st.Nlink != 1 {
		return "", errors.New("hardlinked private data requires manual preservation")
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d", st.Dev, st.Ino, s.Mode(), s.Size(), s.ModTime().UnixNano()), nil
}

// Hash every byte and relative name without following links, crossing mounts,
// reading special files or treating incomplete inspection as preservation.
func privateTree(ctx context.Context, root string) (PrivateEntry, error) {
	entry := PrivateEntry{}
	if noSymlinkAncestors(root) != nil {
		return entry, errors.New("symlinked private path")
	}
	base, err := os.Lstat(root)
	if err != nil {
		return entry, err
	}
	entry.Identity, err = privateIdentity(base)
	if err != nil {
		return entry, err
	}
	h := sha256.New()
	count := 0
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		count++
		if count > 250000 {
			return errors.New("private inventory exceeds file budget")
		}
		s, err := d.Info()
		if err != nil {
			return err
		}
		if !sameDevice(base, s) || s.Mode()&os.ModeSymlink != 0 || (!s.IsDir() && !s.Mode().IsRegular()) || s.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return errors.New("unsupported private entry")
		}
		if _, err = privateIdentity(s); err != nil {
			return err
		}
		if strings.EqualFold(d.Name(), ".git") {
			return errors.New("private data contains a nested repository")
		}
		relative, _ := filepath.Rel(root, p)
		size := s.Size()
		if s.IsDir() {
			size = 0
		}
		fmt.Fprintf(h, "%q %d %d\n", filepath.ToSlash(relative), s.Mode(), size)
		if s.IsDir() {
			return nil
		}
		fd, err := syscall.Open(p, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(fd), p)
		defer f.Close()
		opened, err := f.Stat()
		if err != nil || !os.SameFile(s, opened) {
			return errors.New("private file replaced")
		}
		var n int64
		buffer := make([]byte, 64<<10)
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			read, readErr := f.Read(buffer)
			if read > 0 {
				n += int64(read)
				if entry.Bytes+uint64(n) > 2<<30 {
					return errors.New("private archive exceeds 2 GiB budget")
				}
				_, _ = h.Write(buffer[:read])
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return readErr
			}
		}
		entry.Bytes += uint64(n)
		entry.Files++
		after, err := f.Stat()
		if err != nil || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || n != opened.Size() {
			return errors.New("private file changed during read")
		}
		if entry.Bytes > 2<<30 {
			return errors.New("private archive exceeds 2 GiB budget")
		}
		return nil
	})
	if err != nil {
		return entry, err
	}
	entry.SHA256 = hex.EncodeToString(h.Sum(nil))
	return entry, nil
}
func CanArchivePrivate(c Candidate) bool {
	if c.Fingerprint == "" || !c.SessionsChecked {
		return false
	}
	for _, b := range c.Blockers {
		if b != "private_or_unknown_ignored_state" && b != "coding_sessions_require_native_handoff" {
			return false
		}
	}
	return c.Eligible || slices.Contains(c.Blockers, "private_or_unknown_ignored_state")
}
func (e *Engine) privateEntries(ctx context.Context, c Candidate) ([]PrivateEntry, error) {
	ignored, err := gitCommand(ctx, c.Path, nil, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "--no-empty-directory", "-z")
	if err != nil {
		return nil, err
	}
	var selected []string
	count := 0
	var collect func(string) error
	collect = func(relative string) error {
		count++
		if count > 250000 || ctx.Err() != nil {
			return errors.New("private inventory incomplete")
		}
		full := filepath.Join(c.Path, relative)
		if nodeDependencyPath(relative) {
			for _, session := range c.Sessions {
				if within(full, session.Store) {
					return errors.New("dependency path contains native session storage; keep checkout")
				}
			}
			return nil
		}
		s, err := os.Lstat(full)
		if err != nil {
			return err
		}
		if sensitiveName.MatchString(filepath.ToSlash(relative)) {
			selected = append(selected, relative)
			return nil
		}
		if s.IsDir() {
			children, err := os.ReadDir(full)
			if err != nil {
				return err
			}
			for _, child := range children {
				if err = collect(filepath.Join(relative, child.Name())); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, raw := range bytes.Split(ignored, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		relative := strings.TrimSuffix(string(raw), "/")
		if !privateRelative(relative) {
			return nil, errors.New("invalid private relative path")
		}
		if !allowedIgnored(relative) {
			selected = append(selected, relative)
		} else if err = collect(relative); err != nil {
			return nil, err
		}
	}
	slices.Sort(selected)
	var entries []PrivateEntry
	var total uint64
	for _, relative := range selected {
		if slices.ContainsFunc(entries, func(e PrivateEntry) bool { return within(e.Path, relative) }) {
			continue
		}
		for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
			if slices.Contains([]string{".agents", ".claude", ".codex", ".cursor", ".pi", ".opencode", ".t3", ".emdash", ".grok"}, part) && !allowedIgnored(relative) {
				return nil, errors.New("native agent state requires a provider-specific archive")
			}
		}
		full := filepath.Join(c.Path, relative)
		if err := rejectDependencyArchive(ctx, full, relative); err != nil {
			return nil, err
		}
		if err := rejectRegenerableArchive(ctx, full, relative); err != nil {
			return nil, err
		}
		for _, s := range c.Sessions {
			if within(full, s.Store) {
				return nil, errors.New("private data contains native session storage")
			}
		}
		tracked, err := gitCommand(ctx, c.Path, nil, "ls-files", "-z", "--", ":(literal)"+filepath.ToSlash(relative))
		if err != nil || len(tracked) > 0 {
			return nil, errors.New("private archive would include tracked source")
		}
		entry, err := privateTree(ctx, full)
		if err != nil {
			return nil, errors.New("private inventory incomplete or unsupported; project retained")
		}
		entry.Path = relative
		entries = append(entries, entry)
		total += entry.Bytes
		if total > 2<<30 {
			return nil, errors.New("private archive exceeds 2 GiB budget")
		}
	}
	return entries, nil
}
func privatePlanEqual(a, b []PrivateEntry) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func (e *Engine) PlanPrivate(ctx context.Context, p Plan, id string) (PrivatePlan, error) {
	out := PrivatePlan{}
	if p.Version != ProtocolVersion || p.ID != planID(p) || p.ConfigDigest != ConfigDigest(e.Config) || p.Mode != "report-only" {
		return out, errors.New("invalid source plan")
	}
	release, err := acquireLock(e.Config)
	if err != nil {
		return out, err
	}
	defer release()
	var c *Candidate
	for i := range p.Candidates {
		if p.Candidates[i].ID == id {
			c = &p.Candidates[i]
			break
		}
	}
	if c == nil || !CanArchivePrivate(*c) {
		return out, errors.New("project is not available for private archive planning")
	}
	fresh := e.Inspect(ctx, c.Path, e.Activity(ctx))
	if !CanArchivePrivate(fresh) || fresh.Fingerprint != c.Fingerprint || fresh.Head != c.Head || fresh.RemoteDigest != c.RemoteDigest || fresh.CommonDir != c.CommonDir {
		return out, errors.New("source or activity changed; reanalyze first")
	}
	entries, err := e.privateEntries(ctx, fresh)
	if err != nil {
		return out, err
	}
	if len(entries) == 0 {
		return out, errors.New("no private entries to archive")
	}
	out = PrivatePlan{Version: 1, CreatedAt: e.Now().UTC(), ConfigDigest: ConfigDigest(e.Config), SourcePlan: p.ID, Candidate: fresh, Entries: entries}
	for _, entry := range entries {
		out.Bytes += entry.Bytes
	}
	out.ID = privateID(out)
	b, _ := json.MarshalIndent(out, "", "  ")
	if len(b) > 16<<20 {
		return out, errors.New("private plan exceeds size budget")
	}
	err = atomicPrivate(filepath.Join(e.privateDir(out.ID), "plan.json"), b)
	return out, err
}
func (e *Engine) LoadPrivate(id string) (PrivatePlan, error) {
	var p PrivatePlan
	if !validPlanID.MatchString(id) {
		return p, errors.New("exact private plan ID required")
	}
	name := filepath.Join(e.privateDir(id), "plan.json")
	if noSymlinkAncestors(name) != nil {
		return p, errors.New("unsafe private archive path")
	}
	s, err := os.Lstat(name)
	if err != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 || s.Size() > 16<<20 {
		return p, errors.New("private plan unavailable")
	}
	b, err := readPrivateFile(name, 16<<20)
	if err != nil || json.Unmarshal(b, &p) != nil || p.Version != 1 || p.ID != id || privateID(p) != id || p.ConfigDigest != ConfigDigest(e.Config) || pathID(p.Candidate.Path) != p.Candidate.ID {
		return p, errors.New("invalid private plan")
	}
	for _, entry := range p.Entries {
		if !privateRelative(entry.Path) || !validPlanID.MatchString(entry.SHA256) {
			return p, errors.New("invalid private entry")
		}
	}
	return p, nil
}
func (e *Engine) saveArchive(r PrivateArchive) error {
	b, _ := json.MarshalIndent(r, "", "  ")
	if err := atomicPrivate(filepath.Join(e.privateDir(r.PlanID), "receipt.json"), b); err != nil {
		return err
	}
	return syncDir(e.privateDir(r.PlanID))
}

func (e *Engine) privateLinks(project string) ([]string, error) {
	if !validCandidateID.MatchString(project) {
		return nil, errors.New("invalid private archive project identity")
	}
	name := filepath.Join(e.Config.StateDir, "private-archives", "projects", project+".json")
	if noSymlinkAncestors(name) != nil {
		return nil, errors.New("unsafe private archive index")
	}
	b, err := readPrivateFile(name, 8192)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || len(b) > 8192 {
		return nil, errors.New("private archive index unavailable")
	}
	var ids []string
	if json.Unmarshal(b, &ids) != nil || len(ids) > 50 {
		return nil, errors.New("invalid private archive index")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !validPlanID.MatchString(id) || seen[id] {
			return nil, errors.New("invalid private archive index")
		}
		seen[id] = true
	}
	return ids, nil
}
func (e *Engine) PrivateReceipt(id string) (PrivateArchive, error) {
	var r PrivateArchive
	if !validPlanID.MatchString(id) {
		return r, errors.New("invalid private plan ID")
	}
	b, err := readPrivateFile(filepath.Join(e.privateDir(id), "receipt.json"), 1<<20)
	if err != nil {
		return r, err
	}
	if json.Unmarshal(b, &r) != nil || r.Version != 1 || r.PlanID != id || !slices.Contains([]string{"in_progress", "needs_recovery", "verified_local_archive", "restored"}, r.Status) {
		return r, errors.New("invalid private archive receipt")
	}
	return r, nil
}
func (e *Engine) linkPrivate(p PrivatePlan) error {
	ids, err := e.privateLinks(p.Candidate.ID)
	if err != nil {
		return err
	}
	if !slices.Contains(ids, p.ID) {
		ids = append(ids, p.ID)
	}
	if len(ids) > 50 {
		return errors.New("private archive count exceeds safety budget")
	}
	b, _ := json.Marshal(ids)
	name := filepath.Join(e.Config.StateDir, "private-archives", "projects", p.Candidate.ID+".json")
	if err = atomicPrivate(name, b); err != nil {
		return err
	}
	return syncDir(filepath.Dir(name))
}
func (e *Engine) verifyPrivateArchives(ctx context.Context, c *Candidate) error {
	ids, err := e.privateLinks(c.ID)
	if err != nil {
		return err
	}
	c.PrivateArchives = ids
	for _, id := range ids {
		p, err := e.LoadPrivate(id)
		if err != nil || p.Candidate.Path != c.Path {
			return errors.New("private archive identity unavailable")
		}
		name := filepath.Join(e.privateDir(id), "receipt.json")
		if noSymlinkAncestors(name) != nil {
			return errors.New("unsafe private receipt")
		}
		b, err := readPrivateFile(name, 1<<20)
		if err != nil || len(b) > 1<<20 {
			return errors.New("private receipt unavailable")
		}
		var r PrivateArchive
		if json.Unmarshal(b, &r) != nil || r.PlanID != id || r.Version != 1 {
			return errors.New("invalid private receipt")
		}
		if r.Status == "restored" {
			continue
		}
		if r.Status != "verified_local_archive" || len(r.Verified) != len(p.Entries) {
			return errors.New("private archive incomplete; restore or finish recovery before removal")
		}
		discarded, err := discardedPrivate(p, r)
		if err != nil {
			return err
		}
		a := e.Activity(ctx)
		if !a.Known || a.Busy(e.privateDir(id)) {
			return errors.New("private archive is in use or activity unknown")
		}
		for _, entry := range p.Entries {
			if discarded[entry.Path] {
				continue
			}
			if !slices.Contains(r.Verified, entry.Path) {
				return errors.New("private entry lacks verification")
			}
			check, err := privateTree(ctx, filepath.Join(e.privateDir(id), "payload", entry.Path))
			if err != nil || check.SHA256 != entry.SHA256 || check.Bytes != entry.Bytes || check.Files != entry.Files {
				return errors.New("private archive integrity changed")
			}
		}
	}
	return nil
}
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (e *Engine) ArchivePrivate(ctx context.Context, id, approval string) (r PrivateArchive, err error) {
	if id != approval {
		return r, errors.New("exact private-plan approval required")
	}
	release, err := acquireLock(e.Config)
	if err != nil {
		return r, err
	}
	defer release()
	p, err := e.LoadPrivate(id)
	if err != nil {
		return r, err
	}
	if e.Now().Sub(p.CreatedAt) > 24*time.Hour || p.CreatedAt.After(e.Now().Add(time.Minute)) {
		return r, errors.New("private plan expired")
	}
	receipt := filepath.Join(e.privateDir(id), "receipt.json")
	if _, err = os.Lstat(receipt); !errors.Is(err, os.ErrNotExist) {
		return r, errors.New("private archive already attempted; inspect its receipt and restore if needed")
	}
	fresh := e.Inspect(ctx, p.Candidate.Path, e.Activity(ctx))
	if !CanArchivePrivate(fresh) || fresh.Fingerprint != p.Candidate.Fingerprint || fresh.Head != p.Candidate.Head || fresh.RemoteDigest != p.Candidate.RemoteDigest || fresh.CommonDir != p.Candidate.CommonDir {
		return r, errors.New("source or activity changed; no private files moved")
	}
	entries, err := e.privateEntries(ctx, fresh)
	if err != nil || !privatePlanEqual(entries, p.Entries) {
		return r, errors.New("private data changed; make a new private plan")
	}
	rootInfo, err := os.Stat(fresh.Path)
	if err != nil {
		return r, err
	}
	stateInfo, err := os.Stat(e.Config.StateDir)
	if err != nil || !sameDevice(rootInfo, stateInfo) {
		return r, errors.New("private relocation requires the same filesystem")
	}
	payload := filepath.Join(e.privateDir(id), "payload")
	if err = os.Mkdir(payload, 0700); err != nil {
		return r, err
	}
	r = PrivateArchive{Version: 1, PlanID: id, Status: "in_progress", Moved: []string{}, Verified: []string{}}
	if err = e.saveArchive(r); err != nil {
		return r, err
	}
	if err = syncDir(e.privateDir(id)); err != nil {
		return r, err
	}
	if err = e.linkPrivate(p); err != nil {
		return r, err
	}
	defer func() {
		if err != nil {
			r.Status = "needs_recovery"
			_ = e.saveArchive(r)
		}
	}()
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return r, err
		}
		a := e.Activity(ctx)
		if !a.Known || a.Busy(fresh.Path) {
			return r, errors.New("project became active; private data retained in original or archive paths")
		}
		src := filepath.Join(fresh.Path, entry.Path)
		dst := filepath.Join(payload, entry.Path)
		check, checkErr := privateTree(ctx, src)
		if checkErr != nil || check.Identity != entry.Identity || check.SHA256 != entry.SHA256 {
			return r, errors.New("private source changed; no further relocation")
		}
		if err = os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return r, err
		}
		if noSymlinkAncestors(filepath.Dir(dst)) != nil {
			return r, errors.New("unsafe private destination")
		}
		// Intent is durable before the atomic, no-replace move. No original bytes
		// are deleted, even if the process is killed before the next journal write.
		r.Moved = append(r.Moved, entry.Path)
		if err = e.saveArchive(r); err != nil {
			return r, err
		}
		if err = renameExclusive(src, dst); err != nil {
			return r, errors.New("private relocation failed; originals and archive retained")
		}
		if err = syncDir(filepath.Dir(src)); err != nil {
			return r, err
		}
		if err = syncDir(filepath.Dir(dst)); err != nil {
			return r, err
		}
		check, err = privateTree(ctx, dst)
		if err != nil || check.SHA256 != entry.SHA256 || check.Bytes != entry.Bytes || check.Files != entry.Files {
			return r, errors.New("private archive readback failed; data retained, checkout not removed")
		}
		r.Verified = append(r.Verified, entry.Path)
		if err = e.saveArchive(r); err != nil {
			return r, err
		}
	}
	r.Status = "verified_local_archive"
	err = e.saveArchive(r)
	return r, err
}

// Copy-based restoration never consumes the archive or overwrites a path.
// Restore source to its original location first; then restore private entries.
func (e *Engine) RestorePrivate(ctx context.Context, id, approval string) error {
	if id != approval {
		return errors.New("exact private-plan approval required")
	}
	release, err := acquireLock(e.Config)
	if err != nil {
		return err
	}
	defer release()
	p, err := e.LoadPrivate(id)
	if err != nil {
		return err
	}
	if e.safeTarget(p.Candidate.Path) != nil || noSymlinkAncestors(p.Candidate.Path) != nil {
		return errors.New("original checkout must be restored to a safe non-symlinked path first")
	}
	s, err := os.Stat(p.Candidate.Path)
	if err != nil || !s.IsDir() {
		return errors.New("restore the source checkout first")
	}
	stateInfo, stateErr := os.Stat(e.Config.StateDir)
	if stateErr != nil || !sameDevice(s, stateInfo) {
		return errors.New("private restore requires the original same-filesystem location")
	}
	a := e.Activity(ctx)
	if !a.Known || a.Busy(p.Candidate.Path) {
		return errors.New("restore requires an idle checkout")
	}
	payload := filepath.Join(e.privateDir(id), "payload")
	r, err := e.PrivateReceipt(id)
	if err != nil {
		return err
	}
	discarded, err := discardedPrivate(p, r)
	if err != nil {
		return err
	}
	var restore []PrivateEntry
	for _, entry := range p.Entries {
		if discarded[entry.Path] {
			continue
		}
		src, dst := filepath.Join(payload, entry.Path), filepath.Join(p.Candidate.Path, entry.Path)
		if _, err = os.Lstat(dst); err == nil {
			hash, hashErr := privateTree(ctx, dst)
			if hashErr != nil || hash.SHA256 != entry.SHA256 {
				return errors.New("private restore never overwrites existing paths")
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		hash, err := privateTree(ctx, src)
		if err != nil || hash.SHA256 != entry.SHA256 {
			return errors.New("private backup is missing or changed; all retained bytes kept")
		}
		restore = append(restore, entry)
	}
	for _, entry := range restore {
		if err = ctx.Err(); err != nil {
			return err
		}
		a = e.Activity(ctx)
		if !a.Known || a.Busy(p.Candidate.Path) {
			return errors.New("restore activity changed; originals and archive retained")
		}
		src, dst := filepath.Join(payload, entry.Path), filepath.Join(p.Candidate.Path, entry.Path)
		if noSymlinkAncestors(filepath.Dir(dst)) != nil {
			return errors.New("unsafe private restore parent")
		}
		if err = os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return err
		}
		// A failed restore must not leave private bytes as untracked source
		// that another cleanup/source-preservation command could upload.
		staging, err := os.MkdirTemp(e.privateDir(id), ".restore-")
		parentInfo, parentErr := os.Stat(filepath.Dir(dst))
		if parentErr != nil || !sameDevice(s, parentInfo) {
			return errors.New("private restore crosses a filesystem")
		}
		if err != nil {
			return err
		}
		candidate := filepath.Join(staging, "entry")
		// On failure keep the partial copy for inspection; never erase user data.
		if err = copyPrivateTree(ctx, src, candidate); err != nil {
			return errors.New("private restore copy incomplete; archive and partial copy retained")
		}
		hash, err := privateTree(ctx, candidate)
		if err != nil || hash.SHA256 != entry.SHA256 {
			return errors.New("private restore readback failed; archive and staged copy retained")
		}
		if err = renameExclusive(candidate, dst); err != nil {
			return errors.New("private restore destination appeared; archive and staged copy retained")
		}
		_ = os.Remove(staging)
	}
	// Restored entries remain archived; this status also resolves an interrupted
	// relocation once every original path has been recovered and verified.
	r.Status = "restored"
	return e.saveArchive(r)
}
func copyPrivateTree(ctx context.Context, src, dst string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if s.IsDir() {
		if err = os.Mkdir(dst, 0700); err != nil {
			return err
		}
		children, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, child := range children {
			if err = copyPrivateTree(ctx, filepath.Join(src, child.Name()), filepath.Join(dst, child.Name())); err != nil {
				return err
			}
		}
		if err = os.Chmod(dst, s.Mode().Perm()); err != nil {
			return err
		}
		return os.Chtimes(dst, s.ModTime(), s.ModTime())
	}
	if !s.Mode().IsRegular() {
		return errors.New("unsupported private restore entry")
	}
	fd, err := syscall.Open(src, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	in := os.NewFile(uintptr(fd), src)
	defer in.Close()
	opened, err := in.Stat()
	if err != nil || !os.SameFile(s, opened) {
		return errors.New("private restore source replaced")
	}
	// Stage privately even when the archived file was group/world-writable.
	// OpenFile applies the caller's umask, so restore the exact archived mode
	// explicitly before hashing the completed staged copy.
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(out, privateContextReader{ctx, io.LimitReader(in, s.Size()+1)})
	if copyErr == nil && n != s.Size() {
		copyErr = errors.New("private restore source size changed")
	}
	if copyErr == nil {
		copyErr = out.Chmod(s.Mode().Perm())
	}
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Chtimes(dst, s.ModTime(), s.ModTime())
}

type privateContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r privateContextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}
