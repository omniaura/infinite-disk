package disk

// Media inventory is deliberately separate from project approval plans. A
// review opportunity, age, filename or model decision is never deletion proof.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
)

type MediaOptions struct {
	Roots      []string
	MaxFiles   int
	MaxDepth   int
	HashBudget uint64
}
type MediaFile struct {
	ID             string    `json:"id"`
	Path           string    `json:"path"`
	Category       string    `json:"category"`
	Bytes          uint64    `json:"size_bytes"`
	Allocated      uint64    `json:"allocated_bytes"`
	Modified       time.Time `json:"modified_at"`
	Identity       string    `json:"identity,omitempty"`
	SHA256         string    `json:"sha256,omitempty"`
	DuplicateGroup string    `json:"duplicate_group,omitempty"`
	DuplicateOf    string    `json:"duplicate_of,omitempty"`
	Suggested      string    `json:"suggested_review"`
	Blockers       []string  `json:"blockers"`
}
type MediaPlan struct {
	Version            int              `json:"version"`
	ID                 string           `json:"id"`
	Mode               string           `json:"mode"`
	CreatedAt          time.Time        `json:"created_at"`
	ConfigDigest       string           `json:"config_digest"`
	Roots              []string         `json:"roots"`
	Files              []MediaFile      `json:"files"`
	Complete           bool             `json:"complete"`
	Warnings           []string         `json:"warnings"`
	Bytes              uint64           `json:"inventoried_bytes"`
	DuplicateCopyBytes uint64           `json:"duplicate_copy_bytes"`
	Approved           bool             `json:"approved"`
	Executed           bool             `json:"executed"`
	Excluded           []MediaExclusion `json:"excluded_scope,omitempty"`
}
type MediaExclusion struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// MediaFacts is the entire model-visible media schema. No name, path, hash,
// contents, OCR, pixels, EXIF or other free-form file metadata is included.
type MediaFacts struct {
	Category       string `json:"category"`
	ExactDuplicate bool   `json:"exact_duplicate"`
	HashChecked    bool   `json:"hash_checked"`
	OldForReview   bool   `json:"old_for_review"`
}

func mediaID(p MediaPlan) string { p.ID = ""; b, _ := json.Marshal(p); return PlanDigest(b) }
func mediaCategory(name string) string {
	n := strings.ToLower(name)
	ext := strings.ToLower(filepath.Ext(name))
	if slices.Contains([]string{".crdownload", ".part", ".partial", ".download", ".tmp"}, ext) {
		return "partial_download"
	}
	if slices.Contains([]string{".png", ".jpg", ".jpeg", ".webp", ".heic", ".gif", ".tiff", ".bmp"}, ext) {
		if strings.HasPrefix(n, "screenshot ") || strings.HasPrefix(n, "screen shot ") {
			return "screenshot"
		}
		return "image"
	}
	for kind, extensions := range map[string][]string{"video": {".mov", ".mp4", ".mkv", ".webm", ".avi"}, "audio": {".wav", ".mp3", ".flac", ".aiff", ".m4a"}, "installer": {".dmg", ".pkg", ".exe", ".msi"}, "archive": {".zip", ".gz", ".zst", ".tar", ".7z", ".rar"}, "document": {".pdf", ".docx", ".xlsx", ".txt", ".md", ".csv"}} {
		if slices.Contains(extensions, ext) {
			return kind
		}
	}
	return "other"
}
func (e *Engine) mediaRoots(requested []string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, errors.New("home directory unavailable")
	}
	allowed := []string{filepath.Join(home, "Downloads"), filepath.Join(home, "Desktop"), filepath.Join(home, "Pictures")}
	explicit := len(requested) > 0
	if !explicit {
		requested = []string{allowed[0], allowed[1], filepath.Join(allowed[2], "Screenshots")}
	}
	var roots []string
	for _, root := range requested {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root || noSymlinkAncestors(root) != nil {
			return nil, errors.New("media roots must be canonical explicit directories inside Downloads, Desktop or Pictures")
		}
		if !slices.ContainsFunc(allowed, func(a string) bool { return within(a, root) }) || within(root, e.Config.StateDir) || within(e.Config.StateDir, root) {
			return nil, errors.New("media root is outside the dedicated user-media scope")
		}
		s, err := os.Lstat(root)
		if errors.Is(err, os.ErrNotExist) && !explicit {
			continue
		}
		if err != nil || !s.IsDir() || mediaOffline(s) {
			return nil, errors.New("media root missing, offline or not a directory")
		}
		roots = append(roots, root)
	}
	if len(roots) == 0 {
		return nil, errors.New("no existing media roots; select --root explicitly")
	}
	sort.Strings(roots)
	return minimalRoots(roots), nil
}

func mediaHeld(c *MediaFile, reason string) {
	if !slices.Contains(c.Blockers, reason) {
		c.Blockers = append(c.Blockers, reason)
	}
	c.Suggested = "keep"
}
func mediaPathAllowed(path string) bool {
	for _, name := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		name = strings.ToLower(name)
		if strings.HasPrefix(name, ".") || sensitiveName.MatchString(name) || slices.Contains([]string{"node_modules", "vendor"}, name) || strings.HasSuffix(name, ".app") || strings.HasSuffix(name, ".photoslibrary") || strings.HasSuffix(name, ".download") {
			return false
		}
	}
	return true
}
func mediaStat(s os.FileInfo) (string, uint64, error) {
	st, ok := s.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() || st.Nlink != 1 || !s.Mode().IsRegular() || s.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return "", 0, errors.New("unsafe file metadata")
	}
	id, err := artifactIdentity(s)
	return id, uint64(max(0, st.Blocks)) * 512, err
}
func hashMedia(ctx context.Context, path, identity string, limit uint64) (string, error) {
	if noSymlinkAncestors(path) != nil || !mediaFilesystemLocal(path) {
		return "", errors.New("file path changed")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("file unavailable")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	s, err := f.Stat()
	if err != nil || mediaOffline(s) {
		return "", errors.New("file unavailable or offline")
	}
	id, _, err := mediaStat(s)
	if err != nil || id != identity || uint64(s.Size()) > limit {
		return "", errors.New("file changed or over budget")
	}
	h := sha256.New()
	buf := make([]byte, 128<<10)
	var total uint64
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		n, err := f.Read(buf)
		total += uint64(n)
		if total > limit {
			return "", errors.New("file grew during inspection")
		}
		h.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", errors.New("file read incomplete")
		}
	}
	a, err := f.Stat()
	if err != nil {
		return "", err
	}
	after, _, err := mediaStat(a)
	if err != nil || after != identity || total != uint64(s.Size()) {
		return "", errors.New("file changed during inspection")
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || !os.SameFile(s, pathInfo) {
		return "", errors.New("file path replaced during inspection")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func openMediaNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("source file unavailable")
	}
	return os.NewFile(uintptr(fd), path), nil
}

func (e *Engine) MediaScan(ctx context.Context, o MediaOptions) (MediaPlan, error) {
	p := MediaPlan{Version: 1, Mode: "report-only", CreatedAt: e.Now().UTC(), ConfigDigest: ConfigDigest(e.Config), Complete: true, Files: []MediaFile{}, Warnings: []string{}}
	if o.MaxFiles == 0 {
		o.MaxFiles = 5000
	}
	if o.MaxDepth == 0 {
		o.MaxDepth = 6
	}
	if o.HashBudget == 0 {
		o.HashBudget = 2 << 30
	}
	if o.MaxFiles < 1 || o.MaxFiles > 20000 || o.MaxDepth < 1 || o.MaxDepth > 12 || o.HashBudget > 16<<30 {
		return p, errors.New("media limits require 1..20000 files, 1..12 depth, at most 16 GiB hash budget")
	}
	roots, err := e.mediaRoots(o.Roots)
	if err != nil {
		return p, err
	}
	p.Roots = roots
	release, err := acquireLock(e.Config)
	if err != nil {
		return p, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	activity := e.Activity(ctx)
	dirs := map[string]os.FileInfo{}
	visited := 0
	for _, root := range roots {
		before, err := os.Stat(root)
		if err != nil {
			return p, errors.New("media root changed")
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil {
				p.Complete = false
				p.Warnings = append(p.Warnings, "A media path could not be enumerated; keep all files.")
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			visited++
			if visited > o.MaxFiles*4 {
				p.Complete = false
				p.Warnings = append(p.Warnings, "Entry limit reached; inventory is partial.")
				return fs.SkipAll
			}
			rel, _ := filepath.Rel(root, path)
			depth := len(strings.Split(rel, string(filepath.Separator)))
			if d.IsDir() {
				info, statErr := os.Lstat(path)
				if statErr != nil || !info.IsDir() {
					p.Complete = false
					return filepath.SkipDir
				}
				dirs[path] = info
				if path == root {
					return nil
				}
				info, err := d.Info()
				if err != nil || mediaOffline(info) {
					p.Complete = false
					p.Warnings = append(p.Warnings, "Offline or unreadable directory was not entered.")
					return filepath.SkipDir
				}
				if depth > o.MaxDepth {
					p.Complete = false
					p.Warnings = append(p.Warnings, "Depth limit reached; inventory is partial.")
					return filepath.SkipDir
				}
				name := strings.ToLower(d.Name())
				if strings.HasPrefix(name, ".") || slices.Contains([]string{"node_modules", "vendor"}, name) || strings.HasSuffix(name, ".app") || strings.HasSuffix(name, ".photoslibrary") || strings.HasSuffix(name, ".download") {
					p.Excluded = append(p.Excluded, MediaExclusion{Path: path, Reason: "protected_hidden_or_package_directory"})
					p.Warnings = append(p.Warnings, "Protected/hidden/package folders are explicitly outside this report's scope; their contents and bytes are not inventoried.")
					return filepath.SkipDir
				}
				return nil
			}
			if len(p.Files) >= o.MaxFiles {
				p.Complete = false
				return fs.SkipAll
			}
			c := MediaFile{ID: pathID(path), Path: path, Category: mediaCategory(d.Name()), Suggested: "review_unique", Blockers: []string{}}
			if !mediaPathAllowed(path) {
				mediaHeld(&c, "private_or_hidden_name")
			}
			if !mediaFilesystemLocal(path) {
				mediaHeld(&c, "unqualified_filesystem_not_read")
			}
			s, err := os.Lstat(path)
			if err != nil {
				mediaHeld(&c, "metadata_unavailable")
			} else {
				c.Modified = s.ModTime()
				if s.Size() >= 0 {
					c.Bytes = uint64(s.Size())
				}
				c.Identity, c.Allocated, err = mediaStat(s)
				if err != nil {
					mediaHeld(&c, "unsafe_file_type_links_or_owner")
				}
				if mediaOffline(s) || s.Mode().IsRegular() && s.Size() > 0 && c.Allocated == 0 {
					mediaHeld(&c, "offline_or_sparse_not_read")
				}
			}
			if !activity.Known {
				mediaHeld(&c, "activity_unknown")
			} else if activity.Busy(path) {
				mediaHeld(&c, "file_in_use")
			}
			if c.Category == "partial_download" {
				mediaHeld(&c, "partial_download")
			}
			if c.Modified.After(e.Now().Add(-time.Duration(e.Config.IdleDays) * 24 * time.Hour)) {
				mediaHeld(&c, "recent_file")
			}
			p.Bytes += c.Bytes
			p.Files = append(p.Files, c)
			return nil
		})
		if err != nil {
			return p, err
		}
		after, err := os.Stat(root)
		if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
			p.Complete = false
			p.Warnings = append(p.Warnings, "Media root changed during enumeration.")
		}
	}
	// Hash size-colliding files first. Unique large videos are metadata-only and
	// never silently hydrated/read just because they occupy a lot of space.
	sizes := map[uint64]int{}
	for _, c := range p.Files {
		if len(c.Blockers) == 0 {
			sizes[c.Bytes]++
		}
	}
	groups := map[string][]int{}
	var hashed uint64
	for i := range p.Files {
		c := &p.Files[i]
		if ctx.Err() != nil {
			return p, ctx.Err()
		}
		if len(c.Blockers) > 0 {
			continue
		}
		if sizes[c.Bytes] < 2 {
			continue
		}
		if c.Bytes > 128<<20 || c.Bytes > o.HashBudget-hashed {
			mediaHeld(c, "duplicate_hash_budget_or_size_limit")
			continue
		}
		sum, err := hashMedia(ctx, c.Path, c.Identity, c.Bytes)
		if err != nil {
			mediaHeld(c, "file_changed_or_hash_unavailable")
			continue
		}
		hashed += c.Bytes
		c.SHA256 = sum
		groups[sum+":"+itoa(int(c.Bytes))] = append(groups[sum+":"+itoa(int(c.Bytes))], i)
	}
	for key, indices := range groups {
		if len(indices) < 2 {
			continue
		}
		sort.Slice(indices, func(i, j int) bool { return p.Files[indices[i]].Path < p.Files[indices[j]].Path })
		group := PlanDigest([]byte(key))[:16]
		keeper := p.Files[indices[0]].ID
		for n, index := range indices {
			c := &p.Files[index]
			c.DuplicateGroup = group
			if n == 0 {
				c.Suggested = "keep"
			} else {
				c.DuplicateOf = keeper
				c.Suggested = "review_duplicate"
				p.DuplicateCopyBytes += c.Bytes
			}
		}
	}
	for path, before := range dirs {
		after, err := os.Lstat(path)
		if err != nil || noSymlinkAncestors(path) != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
			p.Complete = false
			p.Warnings = append(p.Warnings, "A directory changed during inventory; rescan before archival.")
		}
	}
	if !p.Complete {
		p.DuplicateCopyBytes = 0
		for i := range p.Files {
			mediaHeld(&p.Files[i], "incomplete_inventory")
		}
	}
	sort.Slice(p.Files, func(i, j int) bool {
		if p.Files[i].Bytes == p.Files[j].Bytes {
			return p.Files[i].Path < p.Files[j].Path
		}
		return p.Files[i].Bytes > p.Files[j].Bytes
	})
	slices.Sort(p.Warnings)
	p.Warnings = slices.Compact(p.Warnings)
	return e.saveMedia(p)
}
func (e *Engine) saveMedia(p MediaPlan) (MediaPlan, error) {
	if err := EnsureState(e.Config); err != nil {
		return p, err
	}
	p.ID = mediaID(p)
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return p, err
	}
	if len(b) > 16<<20 {
		return p, errors.New("media report exceeds 16 MiB; narrow roots")
	}
	if err = atomicPrivate(filepath.Join(e.Config.StateDir, "media-plans", p.ID+".json"), b); err != nil {
		return p, err
	}
	err = atomicPrivate(filepath.Join(e.Config.StateDir, "media-latest"), []byte(p.ID))
	return p, err
}
func (e *Engine) LoadMedia(id string) (MediaPlan, error) {
	var p MediaPlan
	if id == "" || id == "latest" {
		b, err := readMediaPrivate(filepath.Join(e.Config.StateDir, "media-latest"), 64)
		if err != nil {
			return p, errors.New("no saved media report")
		}
		id = string(b)
	}
	if !validPlanID.MatchString(id) {
		return p, errors.New("invalid media report ID")
	}
	path := filepath.Join(e.Config.StateDir, "media-plans", id+".json")
	if noSymlinkAncestors(path) != nil {
		return p, errors.New("unsafe media report path")
	}
	s, err := os.Lstat(path)
	if err != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 || s.Size() > 16<<20 {
		return p, errors.New("media report missing or unsafe")
	}
	b, err := readMediaPrivate(path, 16<<20)
	if err != nil || json.Unmarshal(b, &p) != nil || p.ID != id || mediaID(p) != id || p.Mode != "report-only" || p.Version != 1 || p.ConfigDigest != ConfigDigest(e.Config) || p.Approved || p.Executed {
		return p, errors.New("media report changed or invalid")
	}
	return p, nil
}
func readMediaPrivate(path string, limit int64) ([]byte, error) {
	if noSymlinkAncestors(path) != nil || !mediaFilesystemLocal(path) {
		return nil, errors.New("unsafe private media state path")
	}
	fd, err := openMediaNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer fd.Close()
	before, err := fd.Stat()
	if err != nil || before.Mode().Perm()&0077 != 0 || before.Size() > limit || mediaOffline(before) {
		return nil, errors.New("unsafe private media state")
	}
	id, _, err := mediaStat(before)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(fd, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("private media state over budget")
	}
	after, err := fd.Stat()
	if err != nil {
		return nil, err
	}
	afterID, _, err := mediaStat(after)
	pathInfo, pathErr := os.Lstat(path)
	if err != nil || afterID != id || pathErr != nil || !os.SameFile(before, pathInfo) {
		return nil, errors.New("private media state changed")
	}
	return b, nil
}

// VerifyMediaFile is local verification only, not evidence of a cloud backup.
func (e *Engine) VerifyMediaFile(ctx context.Context, c MediaFile) error {
	if c.ID != pathID(c.Path) || noSymlinkAncestors(c.Path) != nil || !mediaPathAllowed(c.Path) {
		return errors.New("file path identity changed or protected")
	}
	if _, err := e.mediaRoots([]string{filepath.Dir(c.Path)}); err != nil {
		return err
	}
	if len(c.Blockers) > 0 || c.Identity == "" {
		return errors.New("file is held for review")
	}
	if !mediaFilesystemLocal(c.Path) {
		return errors.New("filesystem not qualified for local content reads")
	}
	s, err := os.Lstat(c.Path)
	if err != nil || mediaOffline(s) {
		return errors.New("file missing or offline")
	}
	id, _, err := mediaStat(s)
	if err != nil || id != c.Identity || uint64(s.Size()) != c.Bytes || !s.ModTime().Equal(c.Modified) {
		return errors.New("file changed; rescan")
	}
	a := e.Activity(ctx)
	if !a.Known || a.Busy(c.Path) {
		return errors.New("file active or activity unknown")
	}
	if c.SHA256 != "" {
		sum, err := hashMedia(ctx, c.Path, c.Identity, c.Bytes)
		if err != nil || sum != c.SHA256 {
			return errors.New("file content changed; rescan")
		}
	}
	return nil
}
