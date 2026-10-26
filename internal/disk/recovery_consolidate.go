package disk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Consolidation turns every retained recovery bundle of one remote into a
// single thin bundle holding only the objects the remote's durable refs lack.
// Bundles of the same repository overlap almost entirely, so this typically
// shrinks gigabytes to megabytes. A thin bundle is only written after a
// restore test proves that the remote's durable objects plus the thin bundle
// reproduce every original tip with complete connectivity; coverage then
// counts the originals as covered so prune can remove them.

const (
	consolidatedDir     = "recovery-consolidated"
	fullMirrorDir       = "remote-full"
	consolidateWorkDir  = "consolidate-work"
	durableRefPrefix    = "refs/infinite-disk-durable/"
	consolidateFetchTTL = 30 * time.Minute
)

type ConsolidatedSource struct {
	Path   string      `json:"path"`
	SHA256 string      `json:"sha256"`
	Bytes  uint64      `json:"bytes"`
	Tips   []BundleTip `json:"tips"`
}

type ConsolidatedBundle struct {
	Version       int                  `json:"version"`
	Remote        string               `json:"remote"`
	Path          string               `json:"path"`
	SHA256        string               `json:"sha256"`
	Bytes         uint64               `json:"bytes"`
	Prerequisites []string             `json:"prerequisites"`
	Sources       []ConsolidatedSource `json:"sources"`
	SourceBytes   uint64               `json:"source_bytes"`
	CreatedAt     time.Time            `json:"created_at"`
	RestoreTested bool                 `json:"restore_tested"`
}

type ConsolidateResult struct {
	Version int                  `json:"version"`
	Bundles []ConsolidatedBundle `json:"bundles"`
	Skipped []BundleCoverage     `json:"skipped,omitempty"`
}

// ConsolidateRecovery builds and restore-tests one thin bundle per remote from
// the bundles coverage could not prove. It deletes nothing; originals become
// prunable through recovery verify/prune once their thin bundle exists.
func (e *Engine) ConsolidateRecovery(ctx context.Context) (ConsolidateResult, error) {
	res := ConsolidateResult{Version: ProtocolVersion}
	unlock, err := acquireLock(e.Config)
	if err != nil {
		return res, err
	}
	defer unlock()
	bundles, err := e.discoverRecoveryBundles()
	if err != nil {
		return res, err
	}
	mirrors := map[string]*remoteMirror{}
	byRemote := map[string][]BundleCoverage{}
	for _, b := range bundles {
		c := e.coverBundle(ctx, b, mirrors)
		switch {
		case c.Status == CoverageCovered:
		case c.Remote == "" || len(c.Tips) == 0 || strings.HasPrefix(c.Reason, "remote ") || strings.HasPrefix(c.Reason, "no receipt") || strings.HasPrefix(c.Reason, "receipt "):
			res.Skipped = append(res.Skipped, c)
		default:
			byRemote[c.Remote] = append(byRemote[c.Remote], c)
		}
	}
	remotes := make([]string, 0, len(byRemote))
	for r := range byRemote {
		remotes = append(remotes, r)
	}
	sort.Strings(remotes)
	for _, remote := range remotes {
		cb, err := e.consolidateRemote(ctx, remote, byRemote[remote])
		if err != nil {
			for _, c := range byRemote[remote] {
				c.Reason = "consolidation failed: " + err.Error()
				res.Skipped = append(res.Skipped, c)
			}
			continue
		}
		res.Bundles = append(res.Bundles, cb)
	}
	return res, nil
}

func (e *Engine) consolidateRemote(ctx context.Context, remote string, sources []BundleCoverage) (ConsolidatedBundle, error) {
	cb := ConsolidatedBundle{Version: ProtocolVersion, Remote: remote, CreatedAt: e.Now().UTC()}
	key := digestString(remote)[:16]
	full := filepath.Join(e.Config.StateDir, "cache", fullMirrorDir, key+".git")
	if err := refreshFullMirror(ctx, full, remote); err != nil {
		return cb, err
	}
	workRoot := filepath.Join(e.Config.StateDir, "cache", consolidateWorkDir)
	if err := os.MkdirAll(workRoot, 0700); err != nil {
		return cb, err
	}
	// The operation lock is held, so any workspace for this remote is left
	// over from an interrupted run; remove it before starting another.
	if stale, err := os.ReadDir(workRoot); err == nil {
		for _, entry := range stale {
			if strings.HasPrefix(entry.Name(), key+"-") {
				_ = os.RemoveAll(filepath.Join(workRoot, entry.Name()))
			}
		}
	}
	work, err := os.MkdirTemp(workRoot, key+"-")
	if err != nil {
		return cb, err
	}
	defer os.RemoveAll(work)
	store, verify := filepath.Join(work, "store.git"), filepath.Join(work, "verify.git")
	for _, dir := range []string{store, verify} {
		if err := initAlternateRepo(ctx, dir, full); err != nil {
			return cb, err
		}
	}
	// Unbundle each source into the store; alternates supply the remote's
	// objects, and a local repack after each bundle keeps only new objects.
	for i, s := range sources {
		if _, err := mirrorGit(ctx, consolidateFetchTTL, store, "bundle", "unbundle", s.Path); err != nil {
			return cb, fmt.Errorf("unbundle source %d: %w", i, err)
		}
		if err := pinTips(ctx, store, fmt.Sprintf("refs/sources/%d/", i), s.Tips); err != nil {
			return cb, err
		}
		if _, err := mirrorGit(ctx, consolidateFetchTTL, store, "repack", "-a", "-d", "-l", "-q"); err != nil {
			return cb, fmt.Errorf("repack: %w", err)
		}
		cb.Sources = append(cb.Sources, ConsolidatedSource{Path: s.Path, SHA256: s.SHA256, Bytes: s.Bytes, Tips: s.Tips})
		cb.SourceBytes += s.Bytes
	}
	negatives, err := durableCommits(ctx, full)
	if err != nil {
		return cb, err
	}
	var in strings.Builder
	in.WriteString("--glob=refs/sources/*\n")
	for _, n := range negatives {
		in.WriteString("^" + n + "\n")
	}
	thin := filepath.Join(work, "thin.bundle")
	if _, err := mirrorGitInput(ctx, consolidateFetchTTL, store, []byte(in.String()), "bundle", "create", "-q", thin, "--stdin"); err != nil {
		return cb, fmt.Errorf("create thin bundle: %w", err)
	}
	if err := restoreTest(ctx, verify, thin, cb.Sources); err != nil {
		return cb, fmt.Errorf("restore test: %w", err)
	}
	header, err := readBundleHeader(thin)
	if err != nil {
		return cb, err
	}
	cb.Prerequisites = header.prerequisites
	sum, err := digestFile(thin)
	if err != nil {
		return cb, err
	}
	info, err := os.Stat(thin)
	if err != nil {
		return cb, err
	}
	dest := filepath.Join(e.Config.StateDir, consolidatedDir, key)
	if err := os.MkdirAll(dest, 0700); err != nil {
		return cb, err
	}
	cb.Path, cb.SHA256, cb.Bytes, cb.RestoreTested = filepath.Join(dest, sum[:16]+".bundle"), sum, uint64(info.Size()), true
	if err := os.Rename(thin, cb.Path); err != nil {
		return cb, err
	}
	b, err := jsonBytes(cb)
	if err != nil {
		return cb, err
	}
	return cb, atomicPrivate(strings.TrimSuffix(cb.Path, ".bundle")+".json", append(b, '\n'))
}

// restoreTest unbundles the thin bundle into a repository that only sees the
// remote's durable objects, then requires every original tip to exist with no
// missing object anywhere in its history.
func restoreTest(ctx context.Context, verify, thin string, sources []ConsolidatedSource) error {
	if _, err := mirrorGit(ctx, consolidateFetchTTL, verify, "bundle", "unbundle", thin); err != nil {
		return fmt.Errorf("thin bundle does not apply on the remote's durable objects: %w", err)
	}
	seen := map[string]bool{}
	var in strings.Builder
	for _, s := range sources {
		for _, t := range s.Tips {
			if !seen[t.OID] {
				seen[t.OID] = true
				in.WriteString(t.OID + "\n")
			}
		}
	}
	out, err := mirrorGitInput(ctx, consolidateFetchTTL, verify, []byte(in.String()), "rev-list", "--objects", "--missing=print", "--stdin")
	if err != nil {
		return fmt.Errorf("walk restored history: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "?") {
			return errors.New("restored history is missing objects")
		}
	}
	return nil
}

func refreshFullMirror(ctx context.Context, dir, remote string) error {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		if _, err := mirrorGit(ctx, time.Minute, dir, "init", "--bare", "--quiet"); err != nil {
			return err
		}
	}
	for _, kv := range [][2]string{{"remote.origin.url", remote}, {"gc.auto", "0"}} {
		if _, err := mirrorGit(ctx, time.Minute, dir, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	m := remoteMirror{remote: remote, dir: dir}
	head, err := mirrorGit(ctx, 2*time.Minute, dir, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return fmt.Errorf("ls-remote: %w", err)
	}
	for _, line := range strings.Split(head, "\n") {
		if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			m.defaultBranch, _, _ = strings.Cut(rest, "\t")
		}
	}
	// Only durable refs: the thin bundle may rely on nothing a remote could
	// delete after merge.
	refspecs := []string{"+refs/tags/*:" + durableRefPrefix + "tags/*", "+refs/heads/infinite-disk/recovery/*:" + durableRefPrefix + "recovery/*"}
	if m.defaultBranch != "" {
		refspecs = append(refspecs, "+refs/heads/"+m.defaultBranch+":"+durableRefPrefix+"default")
	}
	if host, err := remoteHost(remote); err == nil && host == "github.com" {
		refspecs = append(refspecs, "+refs/pull/*/head:"+durableRefPrefix+"pull/*")
	}
	args := append([]string{"fetch", "--quiet", "--prune", "--no-tags", "origin"}, refspecs...)
	if _, err := mirrorGit(ctx, consolidateFetchTTL, dir, args...); err != nil {
		return fmt.Errorf("fetch durable refs: %w", err)
	}
	return nil
}

func initAlternateRepo(ctx context.Context, dir, full string) error {
	if _, err := mirrorGit(ctx, time.Minute, filepath.Dir(dir), "init", "--bare", "--quiet", dir); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "objects", "info", "alternates"), []byte(filepath.Join(full, "objects")+"\n"), 0600)
}

func pinTips(ctx context.Context, repo, prefix string, tips []BundleTip) error {
	var in strings.Builder
	for i, t := range tips {
		fmt.Fprintf(&in, "update %s%d %s\n", prefix, i, t.OID)
	}
	if _, err := mirrorGitInput(ctx, time.Minute, repo, []byte(in.String()), "update-ref", "--stdin"); err != nil {
		return fmt.Errorf("pin bundle tips: %w", err)
	}
	return nil
}

func durableCommits(ctx context.Context, full string) ([]string, error) {
	out, err := mirrorGit(ctx, time.Minute, full, "for-each-ref", "--format=%(objectname)", durableRefPrefix)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var oids []string
	for _, oid := range strings.Fields(out) {
		if !seen[oid] {
			seen[oid] = true
			oids = append(oids, oid)
		}
	}
	return oids, nil
}

type bundleHeader struct {
	prerequisites []string
	tips          []BundleTip
}

// readBundleHeader parses a v2/v3 bundle header without Git: prerequisite
// lines start with '-', ref lines are "<oid> <ref>", a blank line ends it.
func readBundleHeader(path string) (bundleHeader, error) {
	var h bundleHeader
	f, err := os.Open(path)
	if err != nil {
		return h, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	first, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(first, "# v") || !strings.Contains(first, "git bundle") {
		return h, errors.New("not a Git bundle")
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return h, errors.New("truncated bundle header")
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			return h, nil
		case strings.HasPrefix(line, "@"):
		case strings.HasPrefix(line, "-"):
			oid, _, _ := strings.Cut(line[1:], " ")
			h.prerequisites = append(h.prerequisites, oid)
		default:
			oid, ref, _ := strings.Cut(line, " ")
			h.tips = append(h.tips, BundleTip{Ref: ref, OID: oid})
		}
	}
}

// consolidatedCoverage reports whether a verified thin bundle covers b: its
// manifest lists b's exact digest, the thin bundle is unchanged, and every
// prerequisite is still reachable from the remote's durable refs.
func (e *Engine) consolidatedCoverage(ctx context.Context, b recoveryBundle, m *remoteMirror) (string, bool) {
	// ReadDir, not Glob: a state directory may contain glob metacharacters.
	dir := filepath.Join(e.Config.StateDir, consolidatedDir, digestString(b.remote)[:16])
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cb ConsolidatedBundle
		if json.Unmarshal(data, &cb) != nil || !cb.RestoreTested || cb.Remote != b.remote {
			continue
		}
		listed := false
		for _, s := range cb.Sources {
			listed = listed || (s.Path == b.path && s.SHA256 == b.sha256)
		}
		if !listed {
			continue
		}
		if sum, err := digestFile(cb.Path); err != nil || sum != cb.SHA256 {
			continue
		}
		if len(cb.Prerequisites) > 0 {
			tips := make([]BundleTip, 0, len(cb.Prerequisites))
			for _, p := range cb.Prerequisites {
				tips = append(tips, BundleTip{OID: p})
			}
			if missing, err := m.missingObjects(ctx, tips); err != nil || len(missing) > 0 {
				continue
			}
			if n, err := m.unreachableFromDurable(ctx, tips); err != nil || n > 0 {
				continue
			}
		}
		return cb.Path, true
	}
	return "", false
}
