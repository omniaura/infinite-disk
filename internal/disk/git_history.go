package disk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const completeGitHistoryScope = "all-stored-objects-v1"
const privateHistoryRefs = "refs/infinite-disk/recovery/objects/"

var historyOID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// This metadata contains no object contents or filenames. Synthetic local refs
// make every otherwise unreachable object part of an ordinary offline bundle.
// Those refs are NEVER included in the explicit origin push refspec.
type GitHistoryProof struct {
	Scope         string `json:"scope"`
	ObjectFormat  string `json:"object_format"`
	Objects       int    `json:"objects"`
	ObjectsSHA256 string `json:"objects_sha256"`
	RefsSHA256    string `json:"refs_sha256"`
	PrivateRefs   int    `json:"private_refs"`
}

func cloneUnreachableObjects(ctx context.Context, c Candidate) ([]string, error) {
	if c.Kind != "clone" {
		return nil, nil
	}
	out, err := gitInput(ctx, c.Path, nil, nil, "fsck", "--unreachable", "--no-reflogs", "--no-progress")
	if err != nil {
		return nil, errors.New("local Git history could not be verified; checkout retained")
	}
	var objects []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "unreachable" || !slices.Contains([]string{"blob", "tree", "commit", "tag"}, fields[1]) || !historyOID.MatchString(fields[2]) || seen[fields[2]] {
			return nil, errors.New("unsupported Git integrity output; checkout retained")
		}
		seen[fields[2]] = true
		objects = append(objects, fields[2])
	}
	slices.Sort(objects)
	return objects, nil
}

func (e *Engine) retainCloneObjects(ctx context.Context, c Candidate, objects []string, r *Receipt) error {
	if c.Kind != "clone" || len(objects) == 0 {
		return nil
	}
	var input strings.Builder
	input.WriteString("start\n")
	for _, oid := range objects {
		if !historyOID.MatchString(oid) {
			return errors.New("invalid private Git object inventory")
		}
		// create (not update) is atomic and refuses any existing ref collision.
		fmt.Fprintf(&input, "create %s%s %s\n", privateHistoryRefs, oid, oid)
	}
	input.WriteString("prepare\ncommit\n")
	if input.Len() > 16<<20 {
		return errors.New("private history inventory exceeds safety budget; checkout retained")
	}
	if _, err := gitInput(ctx, c.Path, nil, []byte(input.String()), "update-ref", "--stdin"); err != nil {
		return errors.New("private Git object retention failed; checkout retained")
	}
	r.Steps = append(r.Steps, Evidence{"private_git_history", "retained", fmt.Sprintf("%d unreachable objects pinned locally for private bundle recovery; not pushed", len(objects))})
	return saveReceipt(e.Config, *r)
}

func cloneHistoryInventory(ctx context.Context, path string) (*GitHistoryProof, error) {
	format, err := gitInput(ctx, path, nil, nil, "rev-parse", "--show-object-format=storage")
	if err != nil || !slices.Contains([]string{"sha1", "sha256"}, format) {
		return nil, errors.New("unsupported Git object format; checkout retained")
	}
	objects, err := gitInput(ctx, path, nil, nil, "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	if err != nil || objects == "" {
		return nil, errors.New("complete Git object inventory unavailable; checkout retained")
	}
	lines := strings.Split(objects, "\n")
	slices.Sort(lines)
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 3 || !historyOID.MatchString(fields[0]) || (len(fields[0]) == 40) != (format == "sha1") || !slices.Contains([]string{"blob", "tree", "commit", "tag"}, fields[1]) {
			return nil, errors.New("invalid Git object inventory; checkout retained")
		}
		if _, err := strconv.ParseUint(fields[2], 10, 64); err != nil || i > 0 && strings.Fields(lines[i-1])[0] == fields[0] {
			return nil, errors.New("incomplete or duplicate Git object inventory; checkout retained")
		}
	}
	refs, err := gitInput(ctx, path, nil, nil, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil || refs == "" {
		return nil, errors.New("complete Git ref inventory unavailable; checkout retained")
	}
	private := 0
	for _, line := range strings.Split(refs, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || !strings.HasPrefix(f[0], "refs/") || !historyOID.MatchString(f[1]) {
			return nil, errors.New("invalid Git ref inventory; checkout retained")
		}
		if strings.HasPrefix(f[0], privateHistoryRefs) {
			private++
		}
	}
	return &GitHistoryProof{Scope: completeGitHistoryScope, ObjectFormat: format, Objects: len(lines), ObjectsSHA256: PlanDigest([]byte(strings.Join(lines, "\n"))), RefsSHA256: PlanDigest([]byte(refs)), PrivateRefs: private}, nil
}

func verifyCloneHistoryCoverage(ctx context.Context, c Candidate, expected *GitHistoryProof) error {
	if c.Kind != "clone" {
		return nil
	}
	if !validGitHistoryProof(expected) {
		return errors.New("complete private Git history proof missing; checkout retained")
	}
	if err := requireCompleteCloneHistory(ctx, c); err != nil {
		return err
	}
	actual, err := cloneHistoryInventory(ctx, c.Path)
	if err != nil || *actual != *expected {
		return errors.New("Git object or ref inventory changed after recovery verification; checkout retained")
	}
	return nil
}

func validGitHistoryProof(proof *GitHistoryProof) bool {
	return proof != nil && proof.Scope == completeGitHistoryScope && slices.Contains([]string{"sha1", "sha256"}, proof.ObjectFormat) && proof.Objects > 0 && proof.PrivateRefs >= 0 && validPlanID.MatchString(proof.ObjectsSHA256) && validPlanID.MatchString(proof.RefsSHA256)
}

func (e *Engine) verifyCloneRecovery(ctx context.Context, c Candidate, r *Receipt) error {
	if c.Kind != "clone" {
		return nil
	}
	if !within(filepath.Join(e.Config.StateDir, "recovery"), r.BundlePath) || noSymlinkAncestors(r.BundlePath) != nil {
		return errors.New("unsafe complete-history recovery path; checkout retained")
	}
	s, err := os.Lstat(r.BundlePath)
	if err != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 {
		return errors.New("complete-history bundle is not private; checkout retained")
	}
	sha, err := digestFile(r.BundlePath)
	if err != nil || sha != r.BundleSHA256 {
		return errors.New("complete-history bundle changed; checkout retained")
	}
	return verifyCloneHistoryCoverage(ctx, c, r.GitHistory)
}

// Final checks reject new unreachable work after the verified bundle snapshot.
// The original unreachable objects now have private retention refs in that
// bundle. Linked worktrees retain their shared database. Keep this expensive
// check at execution boundaries, not in rolling discovery.
func requireCompleteCloneHistory(ctx context.Context, c Candidate) error {
	if c.Kind != "clone" {
		return nil
	}
	// Object verification can legitimately exceed the lightweight Git reader's
	// 20-second budget, especially under disk pressure. Use the same bounded
	// two-minute runner as bundle preservation; caller cancellation still wins.
	// Every execution boundary must finish fsck, never reuse an earlier result.
	output, err := gitInput(ctx, c.Path, nil, nil, "fsck", "--unreachable", "--no-reflogs", "--no-progress")
	if err != nil {
		return errors.New("local Git history could not be verified; checkout retained")
	}
	if strings.TrimSpace(output) != "" {
		return errors.New("unreachable-only Git history is not covered by recovery; checkout retained for manual preservation")
	}
	return nil
}
