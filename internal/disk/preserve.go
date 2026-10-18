package disk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func gitInput(ctx context.Context, dir string, env []string, input []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	argv := append([]string{"--no-optional-locks", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "diff.autoRefreshIndex=false", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = dir
	cmd.Env = gitEnvironment()
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = strings.NewReader(string(input))
	b := &cappedBuffer{limit: 16 << 20}
	cmd.Stdout = b
	if err := cmd.Run(); err != nil {
		return "", errors.New("preservation command failed; project was not deleted")
	}
	return strings.TrimSpace(b.String()), nil
}

func (e *Engine) preserve(ctx context.Context, c Candidate, planID string, r *Receipt) error {
	remote, err := gitText(ctx, c.Path, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if PlanDigest([]byte(remote)) != c.RemoteDigest {
		return errors.New("origin identity changed")
	}
	if host, err := remoteHost(remote); err != nil || host != c.RemoteHost {
		if !(e.allowLocalRemote && filepath.IsAbs(remote) && c.RemoteHost == "local-fixture") {
			return errors.New("origin remote changed")
		}
	}
	_, changed, _, _, err := fingerprint(ctx, c.Path)
	if err != nil {
		return err
	}
	indexFile, err := os.CreateTemp(e.Config.StateDir, "index-*")
	if err != nil {
		return err
	}
	index := indexFile.Name()
	indexFile.Close()
	os.Remove(index)
	defer os.Remove(index)
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err = gitCommand(ctx, c.Path, env, "read-tree", "HEAD"); err != nil {
		return err
	}
	for _, relative := range changed {
		full := filepath.Join(c.Path, relative)
		info, statErr := os.Lstat(full)
		if errors.Is(statErr, os.ErrNotExist) {
			if _, err = gitCommand(ctx, c.Path, env, "update-index", "--force-remove", "--", relative); err != nil {
				return err
			}
			continue
		}
		if statErr != nil {
			return statErr
		}
		var data []byte
		mode := "100644"
		if info.Mode()&os.ModeSymlink != 0 {
			link, e := os.Readlink(full)
			if e != nil {
				return e
			}
			data = []byte(link)
			mode = "120000"
		} else {
			data, err = os.ReadFile(full)
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0111 != 0 {
				mode = "100755"
			}
		}
		if sensitiveName.MatchString(filepath.ToSlash(relative)) || secretValue.Match(data) {
			return errors.New("sensitive changed content; private backup required")
		}
		oid, err := gitInput(ctx, c.Path, env, data, "hash-object", "--no-filters", "-w", "--stdin")
		if err != nil {
			return err
		}
		if _, err = gitCommand(ctx, c.Path, env, "update-index", "--add", "--cacheinfo", mode, oid, relative); err != nil {
			return err
		}
	}
	tree, err := gitInput(ctx, c.Path, env, nil, "write-tree")
	if err != nil {
		return err
	}
	message := []byte(fmt.Sprintf("Infinite Disk approved recovery %s\n\nPlan %s\n", c.ID, planID))
	commit, err := gitInput(ctx, c.Path, nil, message, "commit-tree", tree, "-p", c.Head)
	if err != nil {
		return err
	}
	ref := fmt.Sprintf("refs/heads/infinite-disk/recovery/%s-%s", c.ID, planID[:12])
	r.RecoveryRef = ref
	r.RecoveryCommit = commit
	r.RemoteURL = remote
	r.Steps = append(r.Steps, Evidence{"recovery_commit", "created", "Snapshot created without changing the user's branch or index"})
	if err = saveReceipt(e.Config, *r); err != nil {
		return err
	}
	if _, err = gitInput(ctx, c.Path, nil, nil, "push", "--no-verify", "--", remote, commit+":"+ref); err != nil {
		return err
	}
	r.Steps = append(r.Steps, Evidence{"recovery_push", "completed", "Remote write completed; exact readback verification follows"})
	if err = saveReceipt(e.Config, *r); err != nil {
		return err
	}
	remoteState, err := gitInput(ctx, c.Path, nil, nil, "ls-remote", "--refs", "--", remote, ref)
	if err != nil {
		return err
	}
	fields := strings.Fields(remoteState)
	if len(fields) != 2 || fields[0] != commit || fields[1] != ref {
		return errors.New("remote preservation verification failed")
	}
	// Keep a local recovery ref as well. No user's branch, index or worktree is changed.
	localRef := "refs/infinite-disk/recovery/" + c.ID + "-" + planID[:12]
	if _, err = gitCommand(ctx, c.Path, nil, "update-ref", localRef, commit); err != nil {
		return err
	}
	r.Steps = append(r.Steps, Evidence{"remote_recovery", "verified", "Recovery ref resolves to the exact preservation commit"})
	if c.Kind == "clone" {
		shallow, err := gitText(ctx, c.Path, "rev-parse", "--is-shallow-repository")
		if err != nil || shallow != "false" {
			return errors.New("shallow clone needs a provider-specific preservation adapter")
		}
		dir := filepath.Join(e.Config.StateDir, "recovery", c.ID)
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		bundle := filepath.Join(dir, planID[:12]+".bundle")
		if _, err = os.Lstat(bundle); err == nil {
			return errors.New("recovery bundle already exists; inspect prior receipt")
		}
		if _, err = gitInput(ctx, c.Path, nil, nil, "bundle", "create", bundle, "--all", "HEAD"); err != nil {
			return err
		}
		if err = os.Chmod(bundle, 0600); err != nil {
			return err
		}
		r.BundlePath = bundle
		r.Steps = append(r.Steps, Evidence{"git_bundle", "created", "Private bundle written; independent restore verification follows"})
		if err = saveReceipt(e.Config, *r); err != nil {
			return err
		}
		verify, err := os.MkdirTemp(e.Config.StateDir, "bundle-verify-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(verify)
		if _, err = gitCommand(ctx, e.Config.StateDir, nil, "init", "--bare", verify); err != nil {
			return err
		}
		if _, err = gitInput(ctx, verify, nil, nil, "bundle", "verify", bundle); err != nil {
			return errors.New("recovery bundle is not independently restorable")
		}
		// Header/prerequisite verification alone does not unpack the payload.
		// Restore every ref into this fresh bare repository and validate objects.
		if _, err = gitInput(ctx, verify, nil, nil, "-c", "fetch.fsckObjects=true", "fetch", "--no-tags", "--", bundle, "refs/*:refs/*"); err != nil {
			return errors.New("independent bundle restore failed")
		}
		if _, err = gitInput(ctx, verify, nil, nil, "fsck", "--full", "--no-reflogs", "--no-dangling"); err != nil {
			return errors.New("bundle object integrity verification failed")
		}
		verifiedCommit, verifyErr := gitText(ctx, verify, "rev-parse", localRef)
		if verifyErr != nil || verifiedCommit != commit {
			return errors.New("bundle recovery commit verification failed")
		}
		sha, err := digestFile(bundle)
		if err != nil {
			return err
		}
		r.BundlePath = bundle
		r.BundleSHA256 = sha
		r.Steps = append(r.Steps, Evidence{"git_bundle", "verified", "All Git refs preserved in a private independently verified bundle"})
	}
	return nil
}

func saveReceipt(c Config, r Receipt) error {
	b, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return e
	}
	if len(b) > 16<<20 {
		return errors.New("recovery receipt exceeds private state limit")
	}
	return atomicPrivate(filepath.Join(c.StateDir, "receipts", r.PlanID+"-"+r.CandidateID+".json"), b)
}

// Apply requires an explicitly selected project and the exact approved plan ID.
// The scheduler and Pi worker never call it.
func (e *Engine) Apply(ctx context.Context, plan Plan, candidateID, approvedPlan string, remove bool) (r Receipt, err error) {
	if approvedPlan != plan.ID || planID(plan) != plan.ID || plan.ConfigDigest != ConfigDigest(e.Config) {
		return r, errors.New("exact unchanged plan approval is required")
	}
	if e.Now().Sub(plan.CreatedAt) > 24*time.Hour || plan.CreatedAt.After(e.Now().Add(time.Minute)) {
		return r, errors.New("plan expired; rescan")
	}
	var candidate *Candidate
	for i := range plan.Candidates {
		if plan.Candidates[i].ID == candidateID {
			candidate = &plan.Candidates[i]
			break
		}
	}
	if candidate == nil || (remove && !candidate.Eligible) || (!remove && !CanPreserveSource(*candidate)) {
		return r, errors.New("project is not an eligible candidate in this plan")
	}
	if !candidate.SessionsChecked {
		return r, errors.New("plan predates coding-session safety checks; rescan before applying")
	}
	release, err := acquireLock(e.Config)
	if err != nil {
		return r, err
	}
	defer release()
	fresh := e.Inspect(ctx, candidate.Path, e.Activity(ctx))
	if (remove && !fresh.Eligible) || (!remove && !CanPreserveSource(fresh)) || fresh.Fingerprint != candidate.Fingerprint || fresh.Head != candidate.Head || fresh.RemoteDigest != candidate.RemoteDigest || fresh.CommonDir != candidate.CommonDir || fresh.Kind != candidate.Kind || !sameHandoffs(fresh.SessionHandoffs, candidate.SessionHandoffs) || !slices.Equal(fresh.PrivateArchives, candidate.PrivateArchives) {
		return r, errors.New("project changed, became active, or no longer passes safety checks")
	}
	r = Receipt{Version: ProtocolVersion, PlanID: plan.ID, CandidateID: candidate.ID, Path: candidate.Path, ApprovedAt: e.Now().UTC(), Result: "approved", Steps: []Evidence{{"approval", "received", "Explicit per-project approval of an immutable plan"}}}
	r.BeforeSpace, _ = DiskSpace(candidate.Path)
	if err = AppendEvent(e.Config, Event{Type: "project_approved", PlanID: plan.ID, CandidateID: candidate.ID}); err != nil {
		return r, err
	}
	defer func() {
		if err != nil {
			r.Result = "blocked_or_failed"
		}
		if saveErr := saveReceipt(e.Config, r); err == nil && saveErr != nil {
			err = saveErr
		}
		_ = AppendEvent(e.Config, Event{Type: "project_operation_finished", PlanID: plan.ID, CandidateID: candidate.ID, Outcome: r.Result})
	}()
	if err = e.preserve(ctx, fresh, plan.ID, &r); err != nil {
		return r, err
	}
	fp, _, _, _, err := fingerprint(ctx, candidate.Path)
	if err != nil || fp != candidate.Fingerprint {
		return r, errors.New("source changed during preservation; project retained")
	}
	r.Result = "preserved"
	if err = saveReceipt(e.Config, r); err != nil {
		return r, err
	}
	if !remove {
		return r, nil
	}
	if err = e.backupClaudeSessions(fresh, &r); err != nil {
		return r, err
	}
	if err = e.safeTarget(candidate.Path); err != nil {
		return r, err
	}
	active := e.Activity(ctx)
	if !active.Known || active.Busy(candidate.Path) {
		return r, errors.New("project activity changed; project retained")
	}
	// Re-inspect ignored/private state too: it is deliberately not part of the
	// source fingerprint and may have appeared while the recovery push ran.
	final := e.Inspect(ctx, candidate.Path, active)
	if !final.Eligible || final.Fingerprint != candidate.Fingerprint || !sameHandoffs(final.SessionHandoffs, candidate.SessionHandoffs) || !slices.Equal(final.PrivateArchives, candidate.PrivateArchives) {
		return r, errors.New("final safety check changed; project retained")
	}
	if err = AppendEvent(e.Config, Event{Type: "removal_started", PlanID: plan.ID, CandidateID: candidate.ID, Outcome: "preservation_verified"}); err != nil {
		return r, err
	}
	if len(fresh.SessionHandoffs) > 0 {
		if err = e.removeWithClaudeHandoff(ctx, fresh, &r); err != nil {
			return r, err
		}
	} else if fresh.Kind == "worktree" {
		if within(fresh.Path, fresh.CommonDir) {
			return r, errors.New("shared Git directory would be removed")
		}
		if _, err = gitInput(ctx, e.Config.StateDir, nil, nil, "--git-dir", fresh.CommonDir, "worktree", "remove", "--force", fresh.Path); err != nil {
			return r, err
		}
	} else {
		quarantine := filepath.Join(filepath.Dir(candidate.Path), ".infinite-disk-quarantine")
		if info, e := os.Lstat(quarantine); e == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return r, errors.New("unsafe quarantine directory")
		}
		if err = os.MkdirAll(quarantine, 0700); err != nil {
			return r, err
		}
		destination := filepath.Join(quarantine, candidate.ID+"-"+plan.ID[:12])
		if _, err = os.Lstat(destination); err == nil {
			return r, errors.New("quarantine already exists")
		}
		if err = os.Rename(candidate.Path, destination); err != nil {
			return r, err
		}
		r.QuarantinePath = destination
		if err = saveReceipt(e.Config, r); err != nil {
			_ = os.Rename(destination, candidate.Path)
			return r, err
		}
		check := e.inspectSource(ctx, destination, e.Activity(ctx))
		archiveCheck := *candidate
		if !check.Eligible || check.Fingerprint != candidate.Fingerprint || e.verifyPrivateArchives(ctx, &archiveCheck) != nil || !slices.Equal(archiveCheck.PrivateArchives, candidate.PrivateArchives) {
			if os.Rename(destination, candidate.Path) == nil {
				r.QuarantinePath = ""
			}
			return r, errors.New("quarantine revalidation failed; removal aborted")
		}
		if err = os.RemoveAll(destination); err != nil {
			return r, errors.New("quarantine removal incomplete; inspect recovery receipt")
		}
		_ = os.Remove(quarantine)
	}
	if _, err = os.Lstat(candidate.Path); !errors.Is(err, os.ErrNotExist) {
		return r, errors.New("removal could not be confirmed")
	}
	r.Result = "removed"
	r.QuarantinePath = ""
	r.AfterSpace, _ = DiskSpace(filepath.Dir(candidate.Path))
	r.Steps = append(r.Steps, Evidence{"project_removal", "confirmed", "Approved checkout removed after preservation and source/activity revalidation"})
	return r, nil
}

func PlanDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
