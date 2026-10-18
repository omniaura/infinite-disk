package disk

// Global cleanup deliberately has a separate, fixed catalog and report namespace.
// Project roots and model output cannot add paths or cleanup commands to it.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
)

type globalHost struct{ home, temp, sharedTemp, platform string }
type globalUse struct {
	activity       Activity
	processes      []string
	processesKnown bool
	mounts         Activity
}
type globalEntry struct {
	rel  string
	info os.FileInfo
	link string
}
type globalSnapshot struct {
	entries     []globalEntry
	fingerprint string
	bytes       uint64
	modified    time.Time
}

var xgoVersionDirectory = regexp.MustCompile(`^go[0-9][A-Za-z0-9._-]*$`)

func (e *Engine) GlobalConfig() Config {
	c := e.Config
	c.StateDir = filepath.Join(c.StateDir, "global")
	return c
}

func (e *Engine) globalEnvironment() (globalHost, error) {
	if e.globalHost != nil {
		return e.globalHost()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return globalHost{}, errors.New("home directory unavailable")
	}
	h := globalHost{home: home, temp: os.TempDir(), sharedTemp: "/tmp", platform: runtime.GOOS}
	for _, p := range []*string{&h.home, &h.temp, &h.sharedTemp} {
		canonical, err := filepath.EvalSymlinks(*p)
		if err != nil {
			return h, errors.New("global cleanup host paths unavailable")
		}
		*p = canonical
	}
	return h, nil
}

func globalCaches(h globalHost) map[string]string {
	return map[string]string{
		"go-build":      filepath.Join(h.home, "Library/Caches/go-build"),
		"uv":            filepath.Join(h.home, ".cache/uv"),
		"bun":           filepath.Join(h.home, ".bun/install/cache"),
		"npm":           filepath.Join(h.home, ".npm/_cacache"),
		"golangci-lint": filepath.Join(h.home, "Library/Caches/golangci-lint"),
		"gopls":         filepath.Join(h.home, "Library/Caches/gopls"),
	}
}

func globalXgoRoots(h globalHost) []string {
	roots := []string{filepath.Join(h.temp, "xgo/go-instrument"), filepath.Join(h.sharedTemp, "xgo/go-instrument")}
	sort.Strings(roots)
	return slices.Compact(roots)
}

func globalScope(h globalHost, c Candidate) bool {
	if !filepath.IsAbs(c.Path) || filepath.Clean(c.Path) != c.Path || c.ID != pathID(c.Path) {
		return false
	}
	if c.Kind == "global-installer" {
		return filepath.Dir(c.Path) == filepath.Join(h.home, "Downloads") &&
			(slices.Contains([]string{".dmg", ".pkg"}, strings.ToLower(filepath.Ext(c.Path))))
	}
	kind, ok := strings.CutPrefix(c.Kind, "global-cache/")
	if !ok {
		return false
	}
	if kind != "xgo" {
		return globalCaches(h)[kind] == c.Path
	}
	for _, root := range globalXgoRoots(h) {
		rel, err := filepath.Rel(root, c.Path)
		parts := strings.Split(rel, string(filepath.Separator))
		if err == nil && len(parts) == 2 && xgoVersionDirectory.MatchString(parts[0]) && parts[1] == "build-cache" {
			return true
		}
	}
	return false
}

func (e *Engine) GlobalScan(ctx context.Context) (Plan, error) {
	h, err := e.globalEnvironment()
	if err != nil {
		return Plan{}, err
	}
	if h.platform != "darwin" {
		return Plan{}, errors.New("global cache and installer cleanup currently supports macOS only")
	}
	release, err := acquireLock(e.Config)
	if err != nil {
		return Plan{}, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	p := Plan{Version: ProtocolVersion, CreatedAt: e.Now().UTC(), ConfigDigest: ConfigDigest(e.GlobalConfig()), Candidates: []Candidate{}, Warnings: []string{}}
	use := e.readGlobalUse(ctx)
	add := func(name, kind, target string) {
		if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
			return
		}
		c := Candidate{ID: pathID(target), Path: target, Name: name, Kind: kind}
		c, _ = e.inspectGlobal(ctx, h, c, use)
		p.Candidates = append(p.Candidates, c)
	}
	for name, target := range globalCaches(h) {
		add(name+" cache", "global-cache/"+name, target)
	}
	for _, root := range globalXgoRoots(h) {
		if err := noSymlinkAncestors(root); err != nil {
			p.Warnings = append(p.Warnings, "An xgo location has unsafe ancestors; retained")
			continue
		}
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			p.Warnings = append(p.Warnings, "An xgo location is unreadable; retained")
			continue
		}
		if len(entries) > 1000 {
			return p, errors.New("xgo discovery exceeds safety limit")
		}
		for _, d := range entries {
			if xgoVersionDirectory.MatchString(d.Name()) {
				add("xgo "+d.Name(), "global-cache/xgo", filepath.Join(root, d.Name(), "build-cache"))
			}
		}
	}
	downloads := filepath.Join(h.home, "Downloads")
	if err := noSymlinkAncestors(downloads); err != nil {
		p.Warnings = append(p.Warnings, "Downloads has unsafe ancestors; installers retained")
	} else {
		entries, err := os.ReadDir(downloads)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			p.Warnings = append(p.Warnings, "Downloads is unreadable; installers retained")
		}
		if len(entries) > 10000 {
			return p, errors.New("Downloads discovery exceeds safety limit")
		}
		for _, d := range entries {
			if !d.IsDir() && slices.Contains([]string{".dmg", ".pkg"}, strings.ToLower(filepath.Ext(d.Name()))) {
				add(d.Name(), "global-installer", filepath.Join(downloads, d.Name()))
			}
		}
	}
	if ctx.Err() != nil {
		return p, ctx.Err()
	}
	sort.Slice(p.Candidates, func(i, j int) bool { return p.Candidates[i].Path < p.Candidates[j].Path })
	p.Space, err = DiskSpace(h.home)
	if err != nil {
		return p, err
	}
	return SavePlan(e.GlobalConfig(), p)
}

func globalProducers(kind string) []string {
	switch kind {
	case "global-cache/xgo":
		return []string{"go", "xgo", "compile", "link"}
	case "global-cache/go-build":
		return []string{"go", "xgo", "compile", "link", "gopls", "golangci-lint"}
	case "global-cache/gopls":
		return []string{"gopls", "go"}
	case "global-cache/golangci-lint":
		return []string{"golangci-lint", "go"}
	case "global-cache/uv":
		return []string{"uv", "pip", "pip3"}
	case "global-cache/bun":
		return []string{"bun"}
	case "global-cache/npm":
		return []string{"npm", "npx", "node"}
	}
	return nil
}

func globalUseBlockers(c Candidate, info os.FileInfo, use globalUse) []string {
	var reasons []string
	if !use.activity.Known {
		reasons = append(reasons, "activity_check_unavailable")
	} else if use.activity.Busy(c.Path) {
		reasons = append(reasons, "global_target_in_use")
	}
	if c.Kind == "global-installer" {
		if !use.mounts.Known {
			return append(reasons, "mounted_images_unknown")
		}
		for _, target := range use.mounts.Paths {
			if target == c.Path {
				return append(reasons, "installer_mounted")
			}
			other, err := os.Stat(target)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return append(reasons, "mounted_images_unknown")
			}
			if err == nil && os.SameFile(info, other) {
				return append(reasons, "installer_mounted")
			}
		}
	} else if !use.processesKnown {
		reasons = append(reasons, "producer_check_unavailable")
	} else {
		for _, name := range use.processes {
			if slices.Contains(globalProducers(c.Kind), name) {
				reasons = append(reasons, "global_cache_producer_running")
				break
			}
		}
	}
	return reasons
}

func (e *Engine) inspectGlobal(ctx context.Context, h globalHost, c Candidate, use globalUse) (Candidate, globalSnapshot) {
	c.Eligible = true
	c.Blockers = []string{}
	c.Evidence = []Evidence{}
	c.Fingerprint = ""
	c.SizeComplete = false
	c.SizeBytes = 0
	if !globalScope(h, c) || within(c.Path, e.Config.StateDir) || within(e.Config.StateDir, c.Path) {
		block(&c, "outside_global_catalog")
		return c, globalSnapshot{}
	}
	if err := noSymlinkAncestors(c.Path); err != nil {
		block(&c, "symlinked_global_target")
		return c, globalSnapshot{}
	}
	info, err := os.Lstat(c.Path)
	if err != nil || !globalOwned(info) {
		block(&c, "global_target_unreadable_or_not_owned")
		return c, globalSnapshot{}
	}
	for _, reason := range globalUseBlockers(c, info, use) {
		block(&c, reason)
	}
	if c.Kind == "global-installer" {
		if !info.Mode().IsRegular() {
			block(&c, "installer_not_regular")
			return c, globalSnapshot{}
		}
		cutoff := e.Now().Add(-time.Duration(e.Config.IdleDays) * 24 * time.Hour)
		if info.ModTime().After(cutoff) || globalChangeTime(info).After(cutoff) {
			block(&c, "installer_recent")
		}
		if !globalInstallerFormat(c.Path, info) {
			block(&c, "installer_format_unknown")
		}
	} else {
		if !info.IsDir() {
			block(&c, "global_cache_not_directory")
			return c, globalSnapshot{}
		}
	}
	s, err := snapshotGlobal(ctx, c.Path, info)
	c.SizeBytes = s.bytes
	c.LastModified = s.modified
	if err != nil {
		block(&c, "global_tree_unverified_or_private")
		return c, s
	}
	c.Fingerprint = s.fingerprint
	c.SizeComplete = true
	c.Evidence = append(c.Evidence, Evidence{"global_scope", "checked", "Fixed catalog; owned files, identities, device boundary and full metadata manifest checked"})
	return c, s
}

func globalOwned(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	s, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(s.Uid) == os.Getuid()
}

func globalIdentity(info os.FileInfo) string {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d:%d:%d:%d", s.Dev, s.Ino, s.Uid, info.Mode(), info.Size(), info.ModTime().UnixNano(), globalChangeTime(info).UnixNano(), s.Nlink)
}

func snapshotGlobal(ctx context.Context, root string, rootInfo os.FileInfo) (globalSnapshot, error) {
	s := globalSnapshot{}
	hash := sha256.New()
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(target string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(s.entries) >= 250000 {
			return errors.New("global entry limit")
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !globalOwned(info) || !sameDevice(rootInfo, info) {
			return errors.New("ownership or device boundary")
		}
		rel, err := filepath.Rel(root, target)
		if err != nil {
			return err
		}
		if strings.EqualFold(d.Name(), ".git") || sensitiveName.MatchString(filepath.ToSlash(rel)) {
			return errors.New("private state or nested Git")
		}
		entry := globalEntry{rel: rel, info: info}
		if info.Mode()&os.ModeSymlink != 0 {
			if info.Sys().(*syscall.Stat_t).Nlink > 1 {
				return errors.New("shared symlink requires review")
			}
			entry.link, err = os.Readlink(target)
			if err != nil {
				return err
			}
			resolved, err := filepath.EvalSymlinks(target)
			if err != nil || !within(root, resolved) {
				return errors.New("escaping or unresolved symlink")
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("special file")
		}
		stat := info.Sys().(*syscall.Stat_t)
		key := fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
		if !seen[key] {
			if stat.Blocks < 0 {
				return errors.New("unknown allocated size")
			}
			s.bytes += uint64(stat.Blocks) * 512
			seen[key] = true
		}
		if info.ModTime().After(s.modified) {
			s.modified = info.ModTime()
		}
		fmt.Fprintf(hash, "%q %s %q\n", rel, globalIdentity(info), entry.link)
		s.entries = append(s.entries, entry)
		return nil
	})
	if err == nil {
		s.fingerprint = hex.EncodeToString(hash.Sum(nil))
	}
	return s, err
}

func globalInstallerFormat(target string, info os.FileInfo) bool {
	f, err := os.Open(target)
	if err != nil {
		return false
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return false
	}
	magic := make([]byte, 4)
	if strings.EqualFold(filepath.Ext(target), ".dmg") {
		if info.Size() < 512 {
			return false
		}
		if _, err = f.Seek(-512, io.SeekEnd); err != nil {
			return false
		}
	}
	if _, err = io.ReadFull(f, magic); err != nil {
		return false
	}
	return string(magic) == map[string]string{".dmg": "koly", ".pkg": "xar!"}[strings.ToLower(filepath.Ext(target))]
}

// ApplyGlobal never executes a provider cleanup command or recursively removes a
// cache directory. Only unchanged manifest files are unlinked; roots, new files,
// nested mounts, escaping links and files a producer changed remain untouched.
func (e *Engine) ApplyGlobal(ctx context.Context, p Plan, id, approval string) (Receipt, error) {
	r := Receipt{Version: ProtocolVersion, PlanID: p.ID, CandidateID: id, ApprovedAt: e.Now().UTC(), Result: "retained"}
	if p.Version != ProtocolVersion || approval != p.ID || p.ID != planID(p) || p.ConfigDigest != ConfigDigest(e.GlobalConfig()) || p.Mode != "report-only" || e.Now().Sub(p.CreatedAt) > 24*time.Hour || p.CreatedAt.After(e.Now().Add(time.Minute)) {
		return r, errors.New("global report expired, changed or not approved; rescan")
	}
	release, err := acquireLock(e.Config)
	if err != nil {
		return r, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	h, err := e.globalEnvironment()
	if err != nil {
		return r, err
	}
	if h.platform != "darwin" {
		return r, errors.New("global cleanup supports macOS only")
	}
	var selected *Candidate
	for _, c := range p.Candidates {
		if c.ID == id {
			copy := c
			selected = &copy
		}
	}
	if selected == nil || !selected.Eligible {
		return r, errors.New("global target is not eligible")
	}
	// Repeat the complete inspection and producer/mount probes immediately before
	// deletion, under the same operation lock as checkout and artifact operations.
	var fresh Candidate
	var s globalSnapshot
	for range 2 {
		fresh, s = e.inspectGlobal(ctx, h, *selected, e.readGlobalUse(ctx))
		if !fresh.Eligible || !fresh.SizeComplete || fresh.Fingerprint != selected.Fingerprint || ctx.Err() != nil {
			return r, errors.New("global target changed, active or unverified; retained")
		}
	}
	r.Path = fresh.Path
	r.BeforeSpace, err = DiskSpace(filepath.Dir(fresh.Path))
	if err != nil {
		return r, errors.New("cannot measure filesystem before global cleanup; retained")
	}
	if err = AppendEvent(e.Config, Event{Type: "global_removal_approved", PlanID: p.ID, CandidateID: id, Outcome: "approved"}); err != nil {
		return r, err
	}
	guard := func() error {
		checkRoot := func() (os.FileInfo, error) {
			if err := noSymlinkAncestors(fresh.Path); err != nil {
				return nil, err
			}
			info, err := os.Lstat(fresh.Path)
			if err != nil || !globalOwned(info) || !os.SameFile(info, s.entries[0].info) {
				return nil, errors.New("global root changed during cleanup")
			}
			return info, nil
		}
		info, err := checkRoot()
		if err != nil {
			return err
		}
		if len(globalUseBlockers(fresh, info, e.readGlobalUse(ctx))) > 0 {
			return errors.New("global producer or activity changed during cleanup")
		}
		if _, err = checkRoot(); err != nil {
			return err
		}
		return ctx.Err()
	}
	removed, retained, removeErr := removeGlobalManifest(ctx, fresh, s, guard)
	var spaceErr error
	r.AfterSpace, spaceErr = DiskSpace(filepath.Dir(fresh.Path))
	r.Result = "global_files_removed"
	if removeErr != nil || retained > 0 || spaceErr != nil {
		r.Result = "global_removal_partial"
	}
	r.Steps = []Evidence{{"manifest_removal", "performed", fmt.Sprintf("Approved files removed: %d; approved files retained: %d. Cache roots, directories and newly created files preserved.", removed, retained)}}
	if spaceErr != nil {
		r.Steps = append(r.Steps, Evidence{"disk_space", "unknown", "After-cleanup filesystem measurement unavailable"})
	}
	b, _ := marshalJSON(r)
	if err = atomicPrivate(filepath.Join(e.GlobalConfig().StateDir, "receipts", p.ID+"-"+id+".json"), b); err != nil {
		return r, err
	}
	if err = AppendEvent(e.Config, Event{Type: "global_files_removed", PlanID: p.ID, CandidateID: id, Outcome: r.Result}); err != nil {
		return r, err
	}
	if removeErr != nil || retained > 0 || spaceErr != nil {
		return r, errors.New("global cleanup incomplete or measurement unavailable; inspect receipt and rescan")
	}
	return r, nil
}
