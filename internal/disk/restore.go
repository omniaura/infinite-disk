package disk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

var validCandidateID = regexp.MustCompile(`^[0-9a-f]{16}$`)

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
	if err != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 || s.Size() > 1<<20 {
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
	if _, err = gitInput(ctx, e.Config.StateDir, nil, nil, "clone", "--no-checkout", "--", r.BundlePath, destination); err != nil {
		return errors.New("restore failed; inspect the new destination without deleting it")
	}
	if _, err = gitCommand(ctx, destination, nil, "checkout", "--detach", r.RecoveryCommit); err != nil {
		return errors.New("restore checkout failed; restored Git data was retained")
	}
	// Keep origin pointed at the offline bundle. Reconnecting to a network remote
	// is explicit, avoiding untrusted receipt values becoming command options.
	return AppendEvent(e.Config, Event{Type: "project_restored", PlanID: plan, CandidateID: project, Outcome: "verified_bundle"})
}
