package disk

// Binary recognition is a review heuristic, never deletion authority. Unknown
// formats and vendor/fixture paths remain blocked. No binary is ever executed.
import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
)

const maxBinaryArtifact = 1 << 30

func binaryArtifactReserved(relative string) bool {
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		switch strings.ToLower(part) {
		case ".git", ".agents", ".claude", ".codex", ".cursor", ".pi", ".opencode", ".t3", ".grok", ".emdash", "vendor", "third_party", "third-party", "fixtures", "testdata", "node_modules", ".venv":
			return true
		}
	}
	return false
}

func artifactIdentity(s os.FileInfo) (string, error) {
	base, err := privateIdentity(s)
	if err != nil {
		return "", err
	}
	st, ok := s.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return "", errors.New("artifact ownership unknown")
	}
	v := reflect.ValueOf(st).Elem().FieldByName("Ctim")
	if !v.IsValid() {
		v = reflect.ValueOf(st).Elem().FieldByName("Ctimespec")
	}
	if !v.IsValid() {
		return "", errors.New("artifact change time unknown")
	}
	return base + ":" + fmt.Sprint(v.Interface()), nil
}

func binaryArtifactFormat(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("binary unavailable")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	s, err := f.Stat()
	if err != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0111 == 0 || s.Size() < 64 || s.Size() > maxBinaryArtifact {
		return "", errors.New("not a bounded executable artifact")
	}
	var b [64]byte
	if _, err = io.ReadFull(f, b[:]); err != nil {
		return "", err
	}
	// Thin 64-bit Mach-O executable, not a library/object/fat container.
	if binary.LittleEndian.Uint32(b[:4]) == 0xfeedfacf && binary.LittleEndian.Uint32(b[12:16]) == 2 {
		return "Mach-O executable", nil
	}
	if string(b[:4]) != "\x7fELF" || b[4] != 2 || b[6] != 1 {
		return "", errors.New("unsupported executable format")
	}
	var order binary.ByteOrder = binary.LittleEndian
	if b[5] == 2 {
		order = binary.BigEndian
	} else if b[5] != 1 {
		return "", errors.New("unknown ELF encoding")
	}
	kind := order.Uint16(b[16:18])
	if kind == 2 {
		return "ELF executable", nil
	}
	// ELF shared libraries also use ET_DYN. Require a bounded PT_INTERP table
	// before recognizing a PIE executable; never infer from executable mode alone.
	offset, width, count := order.Uint64(b[32:40]), order.Uint16(b[54:56]), order.Uint16(b[56:58])
	if kind != 3 || width != 56 || count == 0 || count > 256 || offset > uint64(s.Size()) || uint64(count)*56 > uint64(s.Size())-offset {
		return "", errors.New("not a recognized PIE executable")
	}
	for i := uint64(0); i < uint64(count); i++ {
		var p [56]byte
		if _, err = f.ReadAt(p[:], int64(offset+i*56)); err != nil {
			return "", err
		}
		if order.Uint32(p[:4]) == 3 {
			return "ELF PIE executable", nil
		}
	}
	return "", errors.New("shared library or unsupported executable")
}

func artifactFileDigest(ctx context.Context, path string) (string, string, uint64, error) {
	if noSymlinkAncestors(path) != nil {
		return "", "", 0, errors.New("symlinked artifact")
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > maxBinaryArtifact {
		return "", "", 0, errors.New("unsupported artifact")
	}
	identity, err := artifactIdentity(before)
	if err != nil {
		return "", "", 0, err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", "", 0, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", "", 0, errors.New("artifact replaced")
	}
	h := sha256.New()
	buf, overlap := make([]byte, 64<<10), []byte{}
	var nbytes uint64
	for {
		if ctx.Err() != nil {
			return "", "", 0, ctx.Err()
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			nbytes += uint64(n)
			if nbytes > uint64(before.Size()) {
				return "", "", 0, errors.New("artifact grew during inspection")
			}
			window := append(overlap, buf[:n]...)
			if secretValue.Match(window) {
				return "", "", 0, errors.New("artifact contains sensitive-looking content")
			}
			start := len(window) - 4096
			if start < 0 {
				start = 0
			}
			overlap = append([]byte{}, window[start:]...)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", "", 0, readErr
		}
	}
	after, err := f.Stat()
	current, statErr := os.Lstat(path)
	if err != nil || statErr != nil || !os.SameFile(before, current) {
		return "", "", 0, errors.New("artifact replaced")
	}
	for _, info := range []os.FileInfo{after, current} {
		v, e := artifactIdentity(info)
		if e != nil || v != identity || uint64(info.Size()) != nbytes {
			return "", "", 0, errors.New("artifact changed")
		}
	}
	return hex.EncodeToString(h.Sum(nil)), identity, nbytes, nil
}

func (e *Engine) inspectBinaryArtifact(ctx context.Context, c Candidate, activity Activity) Candidate {
	c.Eligible, c.Blockers, c.SizeComplete = true, []string{}, false
	if c.Kind != "binary-artifact" || c.Path == c.CommonDir || !within(c.CommonDir, c.Path) || e.safeTarget(c.CommonDir) != nil || noSymlinkAncestors(c.Path) != nil {
		block(&c, "invalid_artifact_scope")
		return c
	}
	if c.ArtifactDisposition != "keep" && c.ArtifactDisposition != "archive" && c.ArtifactDisposition != "discard" {
		block(&c, "invalid_artifact_disposition")
		return c
	}
	rel, err := filepath.Rel(c.CommonDir, c.Path)
	if err != nil || !safeRelative(rel) || sensitiveName.MatchString(filepath.ToSlash(rel)) {
		block(&c, "artifact_contains_private_names")
		return c
	}
	for parent := filepath.Dir(c.Path); parent != c.CommonDir; parent = filepath.Dir(parent) {
		if parent == filepath.Dir(parent) {
			block(&c, "invalid_artifact_scope")
			return c
		}
		if _, statErr := os.Lstat(filepath.Join(parent, ".git")); statErr == nil || !errors.Is(statErr, os.ErrNotExist) {
			block(&c, "artifact_contains_nested_repository")
			return c
		}
	}
	projectDevice, projectErr := os.Stat(c.CommonDir)
	fileDevice, fileErr := os.Lstat(c.Path)
	if projectErr != nil || fileErr != nil || !sameDevice(projectDevice, fileDevice) {
		block(&c, "artifact_filesystem_unknown")
		return c
	}
	if binaryArtifactReserved(rel) {
		block(&c, "artifact_vendor_fixture_or_session_path")
		return c
	}
	if !activity.Known {
		block(&c, "activity_check_unavailable")
		return c
	}
	if activity.Busy(c.CommonDir) {
		block(&c, "project_in_use")
		return c
	}
	top, err := gitText(ctx, c.CommonDir, "rev-parse", "--show-toplevel")
	if err != nil || top != c.CommonDir {
		block(&c, "artifact_repository_unknown")
		return c
	}
	tracked, err := gitCommand(ctx, c.CommonDir, nil, "ls-files", "-z", "--", ":(literal)"+filepath.ToSlash(rel))
	if err != nil || len(tracked) > 0 {
		block(&c, "artifact_contains_tracked_files")
		return c
	}
	ignored, err := gitInput(ctx, c.CommonDir, nil, []byte(rel+"\x00"), "check-ignore", "--no-index", "-z", "--stdin")
	if err != nil || ignored != rel+"\x00" {
		block(&c, "artifact_contains_unignored_files")
		return c
	}
	format, err := binaryArtifactFormat(c.Path)
	if err != nil {
		block(&c, "artifact_binary_format_unverified")
		return c
	}
	digest, identity, bytes, err := artifactFileDigest(ctx, c.Path)
	if err != nil {
		block(&c, "artifact_private_changed_or_unverified")
		return c
	}
	s, err := os.Lstat(c.Path)
	if err != nil {
		block(&c, "artifact_changed")
		return c
	}
	c.LastModified = s.ModTime()
	c.ArtifactMode = uint32(s.Mode().Perm())
	if e.Now().Sub(s.ModTime()) < time.Duration(e.Config.IdleDays)*24*time.Hour {
		block(&c, "recent_artifact_activity")
	}
	c.SizeBytes, c.SizeComplete = bytes, true
	projectInfo, err := os.Stat(top)
	if err != nil {
		block(&c, "artifact_repository_unknown")
		return c
	}
	projectIdentity, err := artifactIdentity(projectInfo)
	if err != nil {
		block(&c, "artifact_repository_unknown")
		return c
	}
	c.Fingerprint = PlanDigest([]byte(identity + "\n" + digest + "\n" + top + "\n" + projectIdentity + "\n" + c.ArtifactDisposition))
	c.Evidence = []Evidence{{"binary_artifact", "likely_rebuildable", format + "; ignored and untracked, not proof of disposability"}, {"artifact_sha256", "checked", digest}, {"retention", "disclosed", fmt.Sprintf("%d bytes retained locally if archived; no file bytes reclaimed by archive", bytes)}}
	return c
}

func artifactDigestEvidence(c Candidate) string {
	for _, v := range c.Evidence {
		if v.Check == "artifact_sha256" && v.Outcome == "checked" && validPlanID.MatchString(v.Detail) {
			return v.Detail
		}
	}
	return ""
}

func copyArtifactFile(ctx context.Context, src, dst string) error {
	fd, err := syscall.Open(src, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	in := os.NewFile(uintptr(fd), src)
	defer in.Close()
	s, err := in.Stat()
	if err != nil || !s.Mode().IsRegular() || s.Size() > maxBinaryArtifact {
		return errors.New("unsupported artifact copy")
	}
	if noSymlinkAncestors(filepath.Dir(dst)) != nil {
		return errors.New("unsafe artifact copy parent")
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(out, privateContextReader{ctx, io.LimitReader(in, s.Size()+1)})
	if copyErr == nil && n != s.Size() {
		copyErr = errors.New("artifact size changed during copy")
	}
	if copyErr == nil {
		copyErr = out.Chmod(s.Mode().Perm())
	}
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closed := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closed
}

func (e *Engine) applyBinaryArtifact(ctx context.Context, p Plan, c Candidate, r Receipt) (Receipt, error) {
	fresh := e.inspectBinaryArtifact(ctx, c, e.Activity(ctx))
	if !fresh.Eligible || !fresh.SizeComplete || fresh.Fingerprint != c.Fingerprint || ctx.Err() != nil {
		return r, errors.New("binary artifact changed, active or unverified; retained")
	}
	r.Path, r.ArtifactSHA256 = fresh.Path, artifactDigestEvidence(fresh)
	r.BeforeSpace, _ = DiskSpace(fresh.CommonDir)
	r.Steps = []Evidence{{"artifact_approval", "received", "Exact file and disposition bound to approved plan"}}
	receiptPath := filepath.Join(e.CacheConfig().StateDir, "receipts", p.ID+"-"+c.ID+".json")
	if _, err := os.Lstat(receiptPath); !errors.Is(err, os.ErrNotExist) {
		return r, errors.New("artifact action already attempted; inspect receipt")
	}
	if err := AppendEvent(e.Config, Event{Type: "artifact_choice_approved", PlanID: p.ID, CandidateID: c.ID, Outcome: c.ArtifactDisposition}); err != nil {
		return r, err
	}
	save := func() error {
		b, err := marshalJSON(r)
		if err != nil {
			return err
		}
		return atomicPrivate(receiptPath, b)
	}
	if c.ArtifactDisposition == "keep" {
		r.Result = "artifact_kept"
		r.AfterSpace, _ = DiskSpace(c.CommonDir)
		return r, save()
	}
	if c.ArtifactDisposition == "archive" {
		dir := filepath.Join(e.CacheConfig().StateDir, "archives", p.ID+"-"+c.ID)
		if noSymlinkAncestors(dir) != nil {
			return r, errors.New("unsafe artifact archive")
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return r, err
		}
		manifest, err := marshalJSON(p)
		if err != nil {
			return r, err
		}
		if err = atomicPrivate(filepath.Join(dir, "plan.json"), manifest); err != nil {
			return r, err
		}
		r.ArtifactArchivePath = filepath.Join(dir, "payload")
		if err := copyArtifactFile(ctx, c.Path, r.ArtifactArchivePath); err != nil {
			return r, errors.New("artifact copy incomplete; original retained")
		}
		digest, _, bytes, err := artifactFileDigest(ctx, r.ArtifactArchivePath)
		if err != nil || digest != r.ArtifactSHA256 || bytes != c.SizeBytes {
			return r, errors.New("artifact archive readback failed; original retained")
		}
		original, _ := os.Lstat(c.Path)
		archived, _ := os.Lstat(r.ArtifactArchivePath)
		if original == nil || archived == nil || original.Mode() != archived.Mode() {
			return r, errors.New("artifact archive permissions differ; original retained")
		}
		r.RetainedBytes = bytes
		r.Result = "artifact_archive_verified"
		if err = save(); err != nil {
			return r, err
		}
	}
	// Recheck activity, identity, full bytes, Git state and disposition after any
	// archive copy. No recursive removal and no pre-existing file overwrite.
	fresh = e.inspectBinaryArtifact(ctx, c, e.Activity(ctx))
	if !fresh.Eligible || fresh.Fingerprint != c.Fingerprint || ctx.Err() != nil {
		return r, errors.New("artifact changed before removal; original retained")
	}
	if err := os.Remove(c.Path); err != nil {
		return r, errors.New("artifact not removed; inspect exact file")
	}
	if c.ArtifactDisposition == "archive" {
		r.Result = "artifact_archived"
	} else {
		r.Result = "artifact_discarded"
	}
	r.AfterSpace, _ = DiskSpace(c.CommonDir)
	if err := save(); err != nil {
		return r, err
	}
	return r, AppendEvent(e.Config, Event{Type: "artifact_choice_completed", PlanID: p.ID, CandidateID: c.ID, Outcome: r.Result})
}

// RestoreBinaryArtifact keeps the archive, requires the source project first,
// and uses exclusive creation. Existing paths are never overwritten.
func (e *Engine) RestoreBinaryArtifact(ctx context.Context, p Plan, id, approval string) error {
	if p.Version != ProtocolVersion || p.Mode != "report-only" || p.ID != approval || p.ID != planID(p) || p.ConfigDigest != ConfigDigest(e.CacheConfig()) {
		return errors.New("exact artifact plan approval required")
	}
	release, err := acquireLock(e.Config)
	if err != nil {
		return err
	}
	defer release()
	var c Candidate
	for _, v := range p.Candidates {
		if v.ID == id {
			c = v
		}
	}
	if c.Kind != "binary-artifact" || c.ArtifactDisposition != "archive" || e.safeTarget(c.CommonDir) != nil || noSymlinkAncestors(c.Path) != nil {
		return errors.New("restore original source project first")
	}
	rel, relErr := filepath.Rel(c.CommonDir, c.Path)
	if relErr != nil || !safeRelative(rel) || rel == "." || !within(c.CommonDir, c.Path) || binaryArtifactReserved(rel) || sensitiveName.MatchString(filepath.ToSlash(rel)) || pathID(c.Path) != id {
		return errors.New("unsafe artifact restore path")
	}
	a := e.Activity(ctx)
	if !a.Known || a.Busy(c.CommonDir) {
		return errors.New("restore requires an idle project")
	}
	data, err := readPrivateFile(filepath.Join(e.CacheConfig().StateDir, "receipts", p.ID+"-"+id+".json"), 1<<20)
	if err != nil {
		return err
	}
	var r Receipt
	if err = json.Unmarshal(data, &r); err != nil {
		return err
	}
	expected := filepath.Join(e.CacheConfig().StateDir, "archives", p.ID+"-"+id, "payload")
	if r.Path != c.Path || r.PlanID != p.ID || r.CandidateID != id || r.ArtifactArchivePath != expected || r.ArtifactSHA256 != artifactDigestEvidence(c) || (r.Result != "artifact_archived" && r.Result != "artifact_archive_verified") {
		return errors.New("artifact archive receipt invalid")
	}
	digest, _, n, err := artifactFileDigest(ctx, expected)
	if err != nil || digest != r.ArtifactSHA256 || n != c.SizeBytes {
		return errors.New("artifact archive changed")
	}
	archiveInfo, err := os.Lstat(expected)
	if err != nil || uint32(archiveInfo.Mode().Perm()) != c.ArtifactMode {
		return errors.New("artifact archive permissions changed")
	}
	if _, err = os.Lstat(c.Path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("artifact restore never overwrites")
	}
	top, err := gitText(ctx, c.CommonDir, "rev-parse", "--show-toplevel")
	if err != nil || top != c.CommonDir {
		return errors.New("restore original Git checkout first")
	}
	tracked, err := gitCommand(ctx, c.CommonDir, nil, "ls-files", "-z", "--", ":(literal)"+filepath.ToSlash(rel))
	if err != nil || len(tracked) > 0 {
		return errors.New("restore path is tracked")
	}
	ignored, err := gitInput(ctx, c.CommonDir, nil, []byte(rel+"\x00"), "check-ignore", "--no-index", "-z", "--stdin")
	if err != nil || ignored != rel+"\x00" {
		return errors.New("restore path is no longer ignored")
	}
	if err = copyArtifactFile(ctx, expected, c.Path); err != nil {
		return errors.New("restore incomplete; archive retained and no existing file overwritten")
	}
	digest, _, n, err = artifactFileDigest(ctx, c.Path)
	if err != nil || digest != r.ArtifactSHA256 || n != c.SizeBytes {
		return errors.New("restored artifact verification failed; archive retained")
	}
	return AppendEvent(e.Config, Event{Type: "artifact_restored", PlanID: p.ID, CandidateID: id, Outcome: "verified_archive_retained"})
}

// Archive manifests outlive the rotating 50-report cache catalog.
func (e *Engine) LoadArtifactRestorePlan(id, artifact string) (Plan, error) {
	if !validPlanID.MatchString(id) || len(artifact) != 16 {
		return Plan{}, errors.New("exact artifact and plan identities required")
	}
	for _, c := range artifact {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return Plan{}, errors.New("invalid artifact identity")
		}
	}
	if p, err := LoadPlan(e.CacheConfig(), id); err == nil {
		return p, nil
	}
	file := filepath.Join(e.CacheConfig().StateDir, "archives", id+"-"+artifact, "plan.json")
	b, err := readPrivateFile(file, 1<<20)
	if err != nil {
		return Plan{}, errors.New("archived artifact plan unavailable")
	}
	var p Plan
	if json.Unmarshal(b, &p) != nil || p.Version != ProtocolVersion || p.Mode != "report-only" || p.ID != id || planID(p) != id || p.ConfigDigest != ConfigDigest(e.CacheConfig()) || len(p.Candidates) != 1 || p.Candidates[0].ID != artifact || p.Candidates[0].Kind != "binary-artifact" || p.Candidates[0].ArtifactDisposition != "archive" {
		return Plan{}, errors.New("archived artifact plan changed")
	}
	return p, nil
}

// PlanBinaryArtifact is report-only. The exact keep/archive/discard action is
// included in the immutable digest, so consent to archive cannot mean discard.
func (e *Engine) PlanBinaryArtifact(ctx context.Context, project, relative, disposition string) (Plan, error) {
	if !filepath.IsAbs(project) || !safeRelative(relative) || relative == "." {
		return Plan{}, errors.New("explicit project root and safe relative artifact path required")
	}
	release, err := acquireLock(e.Config)
	if err != nil {
		return Plan{}, err
	}
	defer release()
	path := filepath.Join(project, relative)
	c := Candidate{ID: pathID(path), Path: path, CommonDir: project, Name: filepath.Base(project), Kind: "binary-artifact", ArtifactDisposition: disposition}
	if s, statErr := os.Lstat(path); statErr == nil && s.IsDir() {
		if disposition != "discard" {
			return Plan{}, errors.New("Kondo directory selection requires explicit discard disposition; keep is the default")
		}
		if e.safeTarget(project) != nil || noSymlinkAncestors(path) != nil {
			return Plan{}, errors.New("unsafe artifact selection")
		}
		catalogCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(catalogCtx, "kondo", "--dry-run", "--same-filesystem", "--older", "0d", "--", project)
		cmd.Env = gitEnvironment()
		out, diagnostics := &cappedBuffer{limit: 8 << 20}, &cappedBuffer{limit: 64 << 10}
		cmd.Stdout, cmd.Stderr = out, diagnostics
		if err := cmd.Run(); err != nil || out.overflow || diagnostics.Len() > 0 || diagnostics.overflow {
			return Plan{}, errors.New("Kondo catalog could not verify the directory selection")
		}
		catalog, err := parseKondo(out.Bytes())
		if err != nil {
			return Plan{}, err
		}
		parent := ""
		for _, v := range catalog {
			if v.CommonDir == project && within(v.Path, path) {
				parent = v.Path
				break
			}
		}
		if parent == "" {
			return Plan{}, errors.New("directory is not inside an artifact discovered by Kondo")
		}
		c.Kind = "kondo-cache"
		c.Evidence = []Evidence{{"kondo", "discovered", "Selected directory is within a Kondo dry-run artifact"}, {"artifact_selection", "catalog_subset", parent}}
		c = e.inspectCache(ctx, c, e.Activity(ctx))
	} else {
		c = e.inspectBinaryArtifact(ctx, c, e.Activity(ctx))
	}
	space, _ := DiskSpace(project)
	warning := "Binary classification is advisory. Nothing deleted or archived. Choose an exact disposition and approve separately."
	if c.Kind == "kondo-cache" {
		warning = "Kondo directory selection only; nothing deleted. Independent artifact guards and exact approval still apply."
	}
	return SavePlan(e.CacheConfig(), Plan{Version: ProtocolVersion, CreatedAt: e.Now().UTC(), Mode: "report-only", ConfigDigest: ConfigDigest(e.CacheConfig()), Space: space, Candidates: []Candidate{c}, Warnings: []string{warning}})
}
