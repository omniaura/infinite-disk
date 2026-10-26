package disk

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Recovery coverage proves which retained recovery bundles are fully
// reachable from refs a remote will keep, so they can be pruned without
// losing history. A bundle carries exactly the objects reachable from its
// refs, so a bundle is covered when every one of its tips exists on the
// remote and is reachable from a durable ref there. Anything that cannot be
// proven is retained with a reason; nothing here pushes or writes to Git
// remotes.

const (
	CoverageCovered  = "covered"
	CoverageRetained = "retained"

	coverageDir       = "recovery-coverage"
	mirrorDir         = "remote-mirrors"
	mirrorRefPrefix   = "refs/infinite-disk-mirror/"
	mirrorFetchTimout = 20 * time.Minute
)

type BundleTip struct {
	Ref string `json:"ref"`
	OID string `json:"oid"`
}

type BundleCoverage struct {
	Path      string      `json:"path"`
	SHA256    string      `json:"sha256"`
	Bytes     uint64      `json:"bytes"`
	Receipt   string      `json:"receipt,omitempty"`
	Remote    string      `json:"remote,omitempty"`
	Tips      []BundleTip `json:"tips,omitempty"`
	Status    string      `json:"status"`
	Reason    string      `json:"reason,omitempty"`
	CoveredBy []string    `json:"covered_by,omitempty"`
	// ThinBundle is the restore-tested consolidated bundle that holds this
	// bundle's local-only objects, when the remote alone does not.
	ThinBundle string `json:"thin_bundle,omitempty"`
}

type CoverageReport struct {
	Version       int              `json:"version"`
	ID            string           `json:"id"`
	CreatedAt     time.Time        `json:"created_at"`
	StateDir      string           `json:"state_dir"`
	DurableRefs   []string         `json:"durable_refs"`
	Bundles       []BundleCoverage `json:"bundles"`
	CoveredBytes  uint64           `json:"covered_bytes"`
	RetainedBytes uint64           `json:"retained_bytes"`
}

type BundlePrune struct {
	Path    string      `json:"path"`
	SHA256  string      `json:"sha256"`
	Bytes   uint64      `json:"bytes"`
	Remote  string      `json:"remote,omitempty"`
	Tips    []BundleTip `json:"tips,omitempty"`
	Result  string      `json:"result"`
	Reason  string      `json:"reason,omitempty"`
	Restore string      `json:"restore,omitempty"`
}

type PruneResult struct {
	Version     int           `json:"version"`
	ReportID    string        `json:"report_id"`
	PrunedAt    time.Time     `json:"pruned_at"`
	Bundles     []BundlePrune `json:"bundles"`
	FreedBytes  uint64        `json:"freed_bytes"`
	KeptBundles int           `json:"kept_bundles"`
}

// VerifyRecoveryCoverage inventories every recovery bundle under the state
// directory and proves coverage against fresh remote mirrors. It saves the
// report so prune can require that exact report ID as approval.
func (e *Engine) VerifyRecoveryCoverage(ctx context.Context) (CoverageReport, error) {
	r := CoverageReport{Version: ProtocolVersion, CreatedAt: e.Now().UTC(), StateDir: e.Config.StateDir, DurableRefs: durableRefDescriptions()}
	unlock, err := acquireLock(e.Config)
	if err != nil {
		return r, err
	}
	defer unlock()
	bundles, err := e.discoverRecoveryBundles()
	if err != nil {
		return r, err
	}
	mirrors := map[string]*remoteMirror{}
	for _, b := range bundles {
		r.Bundles = append(r.Bundles, e.coverBundle(ctx, b, mirrors))
	}
	for _, b := range r.Bundles {
		if b.Status == CoverageCovered {
			r.CoveredBytes += b.Bytes
		} else {
			r.RetainedBytes += b.Bytes
		}
	}
	r.ID = coverageReportID(r)
	b, err := jsonBytes(r)
	if err != nil {
		return r, err
	}
	if err := os.MkdirAll(filepath.Join(e.Config.StateDir, coverageDir), 0700); err != nil {
		return r, err
	}
	return r, atomicPrivate(e.coverageReportPath(r.ID), append(b, '\n'))
}

// PruneCoveredBundles deletes the covered bundles of an approved report. Each
// bundle is re-hashed and re-proven against a fresh fetch immediately before
// removal, and a restore receipt is written before anything is deleted.
func (e *Engine) PruneCoveredBundles(ctx context.Context, reportID, approvedID string) (PruneResult, error) {
	res := PruneResult{Version: ProtocolVersion, ReportID: reportID}
	if reportID == "" || reportID != approvedID {
		return res, errors.New("prune requires --approve-report to repeat the exact coverage report ID")
	}
	if !isHexID(reportID) {
		return res, errors.New("invalid coverage report ID")
	}
	unlock, err := acquireLock(e.Config)
	if err != nil {
		return res, err
	}
	defer unlock()
	data, err := os.ReadFile(e.coverageReportPath(reportID))
	if err != nil {
		return res, fmt.Errorf("coverage report %s not found; run recovery verify first", reportID)
	}
	var report CoverageReport
	if err := json.Unmarshal(data, &report); err != nil {
		return res, errors.New("coverage report is unreadable")
	}
	if report.ID != reportID || coverageReportID(report) != reportID || filepath.Clean(report.StateDir) != filepath.Clean(e.Config.StateDir) {
		return res, errors.New("coverage report does not match its ID or this state directory")
	}
	current := map[string]recoveryBundle{}
	bundles, err := e.discoverRecoveryBundles()
	if err != nil {
		return res, err
	}
	for _, b := range bundles {
		current[b.path] = b
	}
	mirrors := map[string]*remoteMirror{}
	var pending []BundlePrune
	for _, planned := range report.Bundles {
		if planned.Status != CoverageCovered {
			continue
		}
		p := BundlePrune{Path: planned.Path, SHA256: planned.SHA256, Bytes: planned.Bytes, Remote: planned.Remote, Tips: planned.Tips}
		b, ok := current[planned.Path]
		switch {
		case !ok:
			p.Result, p.Reason = "kept", "bundle no longer present or no longer a regular recovery bundle"
		case b.sha256 != planned.SHA256:
			p.Result, p.Reason = "kept", "bundle changed since the approved report"
		default:
			fresh := e.coverBundle(ctx, b, mirrors)
			if fresh.Status != CoverageCovered || fresh.Remote != planned.Remote || !slices.Equal(fresh.Tips, planned.Tips) || fresh.ThinBundle != planned.ThinBundle {
				p.Result, p.Reason = "kept", "coverage no longer proven: "+fresh.Reason
			} else {
				p.Result, p.Restore = "pending", restoreInstructions(fresh)
			}
		}
		pending = append(pending, p)
	}
	res.PrunedAt = e.Now().UTC()
	res.Bundles = pending
	receipt := filepath.Join(e.Config.StateDir, coverageDir, reportID+"-prune-"+res.PrunedAt.Format("20060102T150405Z")+".json")
	if err := writePruneReceipt(receipt, res); err != nil {
		return res, fmt.Errorf("write prune receipt before deleting: %w", err)
	}
	for i := range res.Bundles {
		p := &res.Bundles[i]
		if p.Result != "pending" {
			res.KeptBundles++
			continue
		}
		if err := removeVerifiedBundle(p.Path, p.SHA256); err != nil {
			p.Result, p.Reason = "kept", err.Error()
			res.KeptBundles++
			continue
		}
		p.Result = "pruned"
		res.FreedBytes += p.Bytes
	}
	return res, writePruneReceipt(receipt, res)
}

func writePruneReceipt(path string, res PruneResult) error {
	b, err := jsonBytes(res)
	if err != nil {
		return err
	}
	return atomicPrivate(path, append(b, '\n'))
}

// removeVerifiedBundle re-hashes immediately before unlinking so a bundle
// rewritten after re-proof is never removed on a stale digest.
func removeVerifiedBundle(path, want string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("bundle is no longer a regular file")
	}
	got, err := digestFile(path)
	if err != nil || got != want {
		return errors.New("bundle changed during prune")
	}
	return os.Remove(path)
}

type recoveryBundle struct {
	path, sha256 string
	bytes        uint64
	receipt      string
	remote       string
}

// discoverRecoveryBundles walks the state directory without following
// symlinks and maps each bundle to the receipt that created it.
func (e *Engine) discoverRecoveryBundles() ([]recoveryBundle, error) {
	root := filepath.Clean(e.Config.StateDir)
	if info, err := os.Lstat(root); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("state directory %s is not a directory", root)
	}
	receipts := map[string][2]string{}
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if path != root && (d.Name() == "cache" || d.Name() == mirrorDir || d.Name() == coverageDir || d.Name() == consolidatedDir) && filepath.Dir(path) == root {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		switch {
		case strings.HasSuffix(d.Name(), ".bundle"):
			paths = append(paths, path)
		case strings.HasSuffix(d.Name(), ".json") && filepath.Base(filepath.Dir(path)) == "receipts":
			b, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			var r Receipt
			if json.Unmarshal(b, &r) != nil || r.BundlePath == "" {
				return nil
			}
			receipts[filepath.Clean(r.BundlePath)] = [2]string{path, r.RemoteURL}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	out := make([]recoveryBundle, 0, len(paths))
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		sum, err := digestFile(p)
		if err != nil {
			continue
		}
		b := recoveryBundle{path: p, sha256: sum, bytes: uint64(info.Size())}
		if r, ok := receipts[p]; ok {
			b.receipt, b.remote = r[0], r[1]
		}
		out = append(out, b)
	}
	return out, nil
}

func (e *Engine) coverBundle(ctx context.Context, b recoveryBundle, mirrors map[string]*remoteMirror) BundleCoverage {
	c := BundleCoverage{Path: b.path, SHA256: b.sha256, Bytes: b.bytes, Receipt: b.receipt, Remote: b.remote, Status: CoverageRetained}
	if b.receipt == "" {
		c.Reason = "no receipt maps this bundle to a remote"
		return c
	}
	if b.remote == "" {
		c.Reason = "receipt records no remote"
		return c
	}
	host, err := remoteHost(b.remote)
	allowed := err == nil && slices.Contains(e.Config.AllowedRemoteHosts, host)
	if !allowed && !(e.allowLocalRemote && filepath.IsAbs(b.remote)) {
		c.Reason = "remote host is not in allowed_remote_hosts"
		return c
	}
	m, ok := mirrors[b.remote]
	if !ok {
		m = &remoteMirror{remote: b.remote, dir: filepath.Join(e.Config.StateDir, "cache", mirrorDir, digestString(b.remote)[:16]+".git")}
		m.err = m.refresh(ctx)
		mirrors[b.remote] = m
	}
	if m.err != nil {
		c.Reason = "remote mirror unavailable: " + m.err.Error()
		return c
	}
	tips, err := bundleTips(ctx, m.dir, b.path)
	if err != nil {
		c.Reason = "cannot read bundle refs: " + err.Error()
		return c
	}
	c.Tips = tips
	if len(tips) == 0 {
		c.Reason = "bundle lists no refs"
		return c
	}
	missing, err := m.missingObjects(ctx, tips)
	if err != nil {
		c.Reason = "cannot inspect remote mirror: " + err.Error()
		return c
	}
	var count int
	if len(missing) == 0 {
		if count, err = m.unreachableFromDurable(ctx, tips); err != nil {
			c.Reason = "cannot prove reachability: " + err.Error()
			return c
		}
	}
	if len(missing) == 0 && count == 0 {
		c.Status, c.CoveredBy = CoverageCovered, m.durableRefNames()
		return c
	}
	if thin, ok := e.consolidatedCoverage(ctx, b, m); ok {
		c.Status, c.CoveredBy, c.ThinBundle = CoverageCovered, append(m.durableRefNames(), "restore-tested thin bundle "+thin), thin
		return c
	}
	if len(missing) > 0 {
		c.Reason = fmt.Sprintf("%d bundle tip(s) not on the remote (local-only commits or private objects)", len(missing))
	} else {
		c.Reason = fmt.Sprintf("%d commit(s) are only on deletable remote branches", count)
	}
	return c
}

type remoteMirror struct {
	remote, dir   string
	defaultBranch string
	err           error
}

// refresh keeps one treeless mirror per remote. Commits are enough: a remote
// is connected, so a commit reachable there carries its trees and blobs.
func (m *remoteMirror) refresh(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(m.dir, "HEAD")); err != nil {
		if err := os.MkdirAll(m.dir, 0700); err != nil {
			return err
		}
		if _, err := mirrorGit(ctx, time.Minute, m.dir, "init", "--bare", "--quiet"); err != nil {
			return err
		}
	}
	for _, kv := range [][2]string{{"remote.origin.url", m.remote}, {"remote.origin.promisor", "true"}, {"remote.origin.partialclonefilter", "tree:0"}, {"gc.auto", "0"}} {
		if _, err := mirrorGit(ctx, time.Minute, m.dir, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	head, err := mirrorGit(ctx, 2*time.Minute, m.dir, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return fmt.Errorf("ls-remote: %w", err)
	}
	m.defaultBranch = ""
	for _, line := range strings.Split(head, "\n") {
		if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			m.defaultBranch, _, _ = strings.Cut(rest, "\t")
		}
	}
	refspecs := []string{"+refs/heads/*:" + mirrorRefPrefix + "heads/*", "+refs/tags/*:" + mirrorRefPrefix + "tags/*"}
	if host, err := remoteHost(m.remote); err == nil && host == "github.com" {
		refspecs = append(refspecs, "+refs/pull/*/head:"+mirrorRefPrefix+"pull/*")
	}
	args := append([]string{"fetch", "--quiet", "--prune", "--no-tags", "--filter=tree:0", "origin"}, refspecs...)
	if _, err := mirrorGit(ctx, mirrorFetchTimout, m.dir, args...); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	return nil
}

// durableArgs names refs a remote keeps: its default branch, tags, GitHub PR
// head refs (kept after branch deletion) and infinite-disk's own recovery
// namespace. Other branches can be deleted after merge, so they don't count.
func (m *remoteMirror) durableArgs() []string {
	args := []string{"--glob=" + mirrorRefPrefix + "tags/*", "--glob=" + mirrorRefPrefix + "pull/*", "--glob=" + mirrorRefPrefix + "heads/infinite-disk/recovery/*"}
	if m.defaultBranch != "" {
		args = append(args, mirrorRefPrefix+"heads/"+m.defaultBranch)
	}
	return args
}

func (m *remoteMirror) durableRefNames() []string {
	out := []string{"tags", "pull-request heads", "infinite-disk/recovery branches"}
	if m.defaultBranch != "" {
		out = append([]string{"default branch " + m.defaultBranch}, out...)
	}
	return out
}

func durableRefDescriptions() []string {
	return []string{"remote default branch", "remote tags", "GitHub pull-request head refs", "remote infinite-disk/recovery/* branches"}
}

func (m *remoteMirror) missingObjects(ctx context.Context, tips []BundleTip) ([]string, error) {
	var in strings.Builder
	for _, t := range tips {
		in.WriteString(t.OID + "\n")
	}
	out, err := mirrorGitInput(ctx, 2*time.Minute, m.dir, []byte(in.String()), "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || (fields[1] != "commit" && fields[1] != "tag") {
			missing = append(missing, line)
		}
	}
	return missing, nil
}

func (m *remoteMirror) unreachableFromDurable(ctx context.Context, tips []BundleTip) (int, error) {
	args := []string{"rev-list", "--count"}
	seen := map[string]bool{}
	for _, t := range tips {
		if !seen[t.OID] {
			seen[t.OID] = true
			args = append(args, t.OID)
		}
	}
	args = append(append(args, "--not"), m.durableArgs()...)
	out, err := mirrorGit(ctx, 5*time.Minute, m.dir, args...)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

func bundleTips(ctx context.Context, dir, bundle string) ([]BundleTip, error) {
	out, err := mirrorGit(ctx, 2*time.Minute, dir, "bundle", "list-heads", bundle)
	if err != nil {
		return nil, err
	}
	var tips []BundleTip
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		oid, ref, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok || !isHexID(oid) {
			continue
		}
		tips = append(tips, BundleTip{Ref: ref, OID: oid})
	}
	sort.Slice(tips, func(i, j int) bool { return tips[i].Ref+tips[i].OID < tips[j].Ref+tips[j].OID })
	return tips, nil
}

// restoreInstructions rebuilds a pruned bundle's history from the remote, plus
// its thin bundle when local-only objects live there. Unbundling the thin
// bundle needs the remote's durable objects, so clone first.
func restoreInstructions(c BundleCoverage) string {
	oids := make([]string, 0, len(c.Tips))
	for _, t := range c.Tips {
		oids = append(oids, t.OID)
	}
	tips := strings.Join(slices.Compact(slices.Sorted(slices.Values(oids))), " ")
	if c.ThinBundle != "" {
		return fmt.Sprintf("git clone --mirror %s restore.git && git -C restore.git bundle unbundle %s && git -C restore.git cat-file -e %s", c.Remote, c.ThinBundle, tips)
	}
	return fmt.Sprintf("git clone %s && git fetch origin %s", c.Remote, tips)
}

// mirrorGit runs Git against a mirror with lazy fetching disabled, so a
// missing object reads as missing instead of silently downloading.
func mirrorGit(ctx context.Context, timeout time.Duration, dir string, args ...string) (string, error) {
	return mirrorGitInput(ctx, timeout, dir, nil, args...)
}

func mirrorGitInput(ctx context.Context, timeout time.Duration, dir string, input []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := append([]string{"--no-optional-locks", "-c", "core.hooksPath=/dev/null", "-c", "protocol.file.allow=always"}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = dir
	cmd.Env = gitEnvironment()
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	stdout := &cappedBuffer{limit: 64 << 20}
	cmd.Stdout = stdout
	if err := cmd.Run(); err != nil {
		// Like gitCommand, never surface raw Git output: it can echo remote URLs.
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s timed out", args[0])
		}
		return "", fmt.Errorf("git %s failed", args[0])
	}
	return stdout.String(), nil
}

func coverageReportID(r CoverageReport) string {
	r.ID = ""
	b, _ := jsonBytes(r)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (e *Engine) coverageReportPath(id string) string {
	return filepath.Join(e.Config.StateDir, coverageDir, id+".json")
}

func digestString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func isHexID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
