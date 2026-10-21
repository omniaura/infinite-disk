package disk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var validCandidateID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// An empty non-bare repository must not have HEAD pointing at a branch being
// fetched. Choose an unborn placeholder absent from the verified bundle, then
// detach at the recovery commit after every original ref has been restored.
func initCompleteRestore(ctx context.Context, dir, bundle, destination, format string) error {
	heads, err := gitInput(ctx, dir, nil, nil, "bundle", "list-heads", bundle)
	if err != nil {
		return err
	}
	refs := map[string]bool{}
	for _, line := range strings.Split(heads, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !historyOID.MatchString(fields[0]) {
			return errors.New("invalid bundle ref inventory")
		}
		refs[fields[1]] = true
	}
	for i := 0; i <= len(refs); i++ {
		name := fmt.Sprintf("infinite-disk-restore-unborn-%d", i)
		if !refs["refs/heads/"+name] {
			_, err = gitInput(ctx, dir, nil, nil, "init", "--initial-branch="+name, "--object-format="+format, "--", destination)
			return err
		}
	}
	return errors.New("restore placeholder unavailable")
}

func LoadReceipt(c Config, plan, project string) (Receipt, error) {
	var r Receipt
	if !validPlanID.MatchString(plan) || !validCandidateID.MatchString(project) {
		return r, errors.New("invalid receipt identifiers")
	}
	path := filepath.Join(c.StateDir, "receipts", plan+"-"+project+".json")
	if err := noSymlinkAncestors(filepath.Dir(path)); err != nil {
		return r, err
	}
	s, err := os.Lstat(path)
	if err != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 || s.Size() > 16<<20 {
		return r, errors.New("receipt must be a private regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	if json.Unmarshal(b, &r) != nil || r.Version != ProtocolVersion || r.PlanID != plan || r.CandidateID != project {
		return r, errors.New("invalid receipt")
	}
	return r, nil
}

// Restore never overwrites an existing path. A verified local Git bundle is
// independent of the original remote and includes all refs plus approved work.
func (e *Engine) Restore(ctx context.Context, plan, project, destination string) error {
	release, err := acquireLock(e.Config)
	if err != nil {
		return err
	}
	defer release()
	r, err := LoadReceipt(e.Config, plan, project)
	if err != nil {
		return err
	}
	if r.BundlePath == "" {
		return errors.New("linked worktree: recover from the shared Git recovery ref or verified remote ref in the receipt")
	}
	if !within(filepath.Join(e.Config.StateDir, "recovery"), r.BundlePath) || noSymlinkAncestors(filepath.Dir(r.BundlePath)) != nil {
		return errors.New("unsafe recovery bundle path")
	}
	s, err := os.Lstat(r.BundlePath)
	if err != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 {
		return errors.New("recovery bundle is not private")
	}
	sha, err := digestFile(r.BundlePath)
	if err != nil || sha != r.BundleSHA256 {
		return errors.New("recovery bundle checksum mismatch")
	}
	if !filepath.IsAbs(destination) || filepath.Clean(destination) != destination || noSymlinkAncestors(filepath.Dir(destination)) != nil {
		return errors.New("restore needs an absolute non-symlinked new destination")
	}
	if _, err = os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return errors.New("restore never overwrites an existing destination")
	}
	if _, err = os.Stat(filepath.Dir(destination)); err != nil {
		return errors.New("restore parent must already exist")
	}
	if r.GitHistory != nil {
		if !validGitHistoryProof(r.GitHistory) {
			return errors.New("invalid complete Git recovery proof")
		}
		// Start empty: clone invents remote-tracking refs that would obscure
		// exact comparison with the approved original ref inventory.
		if err = initCompleteRestore(ctx, e.Config.StateDir, r.BundlePath, destination, r.GitHistory.ObjectFormat); err != nil {
			return errors.New("restore initialization failed; new destination retained")
		}
	} else {
		// Backward compatibility for receipts predating complete-object proof.
		if _, err = gitInput(ctx, e.Config.StateDir, nil, nil, "clone", "--no-checkout", "--", r.BundlePath, destination); err != nil {
			return errors.New("restore failed; inspect the new destination without deleting it")
		}
		if _, err = gitCommand(ctx, destination, nil, "checkout", "--detach", r.RecoveryCommit); err != nil {
			return errors.New("restore checkout failed; restored Git data was retained")
		}
	}
	// clone's default refspec does not restore stash/notes/custom refs. The
	// destination is newly created and detached, so restore every bundled ref
	// there (including remote-tracking refs that differ from clone's defaults).
	if _, err = gitInput(ctx, destination, nil, nil, "-c", "fetch.fsckObjects=true", "fetch", "--force", "--no-tags", "--", r.BundlePath, "refs/*:refs/*"); err != nil {
		return errors.New("full ref restore failed; restored Git data was retained")
	}
	if r.GitHistory != nil {
		if err = verifyCloneHistoryCoverage(ctx, Candidate{Path: destination, Kind: "clone"}, r.GitHistory); err != nil {
			return err
		}
		if _, err = gitCommand(ctx, destination, nil, "checkout", "--detach", r.RecoveryCommit); err != nil {
			return errors.New("restore checkout failed; restored Git data was retained")
		}
		if _, err = gitCommand(ctx, destination, nil, "remote", "add", "origin", r.BundlePath); err != nil {
			return errors.New("offline recovery origin could not be configured; restored data retained")
		}
	}
	// Keep origin pointed at the offline bundle. Reconnecting to a network remote
	// is explicit, avoiding untrusted receipt values becoming command options.
	return AppendEvent(e.Config, Event{Type: "project_restored", PlanID: plan, CandidateID: project, Outcome: "verified_bundle"})
}
