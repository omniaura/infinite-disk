package disk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// The old pathname must be absent for Claude's native missing-worktree
// fallback. Move, don't delete: any failed verification rolls the checkout
// back. Persist the intent BEFORE moving so SIGKILL/disk-full is recoverable.
func (e *Engine) removeWithClaudeHandoff(ctx context.Context, c Candidate, r *Receipt) (err error) {
	parent := filepath.Join(filepath.Dir(c.Path), ".infinite-disk-quarantine")
	if noSymlinkAncestors(parent) != nil {
		return errors.New("unsafe quarantine directory")
	}
	if err = os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	destination := filepath.Join(parent, c.ID+"-"+r.PlanID[:12])
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		return errors.New("quarantine already exists; inspect recovery receipt")
	}
	r.QuarantinePath = destination
	r.Steps = append(r.Steps, Evidence{"session_quarantine", "intent", "Recoverable same-volume move; private session backups verified"})
	if err = saveReceipt(e.Config, *r); err != nil {
		return err
	}
	move := func(moveCtx context.Context, from, to string) error {
		if c.Kind == "worktree" {
			_, err := gitInput(moveCtx, e.Config.StateDir, nil, nil, "--git-dir", c.CommonDir, "worktree", "move", "--", from, to)
			return err
		}
		return os.Rename(from, to)
	}
	if err = move(ctx, c.Path, destination); err != nil {
		return err
	}
	defer func() {
		if err == nil {
			return
		}
		// Ctrl+C cancels native verification, not the safety rollback.
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, statErr := os.Lstat(c.Path); !errors.Is(statErr, os.ErrNotExist) {
			return
		}
		if move(rollbackCtx, destination, c.Path) == nil {
			r.QuarantinePath = ""
			r.Steps = append(r.Steps, Evidence{"session_quarantine", "rolled_back", "Checkout restored; original history and private backups retained"})
		}
	}()
	if err = saveReceipt(e.Config, *r); err != nil {
		return err
	}
	if err = e.handoffClaudeSessions(ctx, r); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	active := e.Activity(ctx)
	if !active.Known || active.Busy(c.Path) || active.Busy(destination) {
		return errors.New("activity changed during session handoff; checkout retained")
	}
	// Reinspect ignored/private state at the actual quarantine, not just the
	// source hash. A new .env or nested repo must never disappear unpreserved.
	check := e.inspectSource(ctx, destination, active)
	if !check.Eligible || check.Fingerprint != c.Fingerprint || check.Head != c.Head || check.CommonDir != c.CommonDir && c.Kind == "worktree" {
		return errors.New("quarantined checkout changed; removal aborted")
	}
	archiveCheck := c
	if e.verifyPrivateArchives(ctx, &archiveCheck) != nil || !slices.Equal(archiveCheck.PrivateArchives, c.PrivateArchives) {
		return errors.New("private archives changed during session handoff; checkout retained")
	}
	for _, h := range r.SessionHandoffs {
		if active.Busy(h.Session.Checkout) {
			return errors.New("associated session workspace became active; checkout retained")
		}
		if err = verifyOriginalPrefix(h); err != nil {
			return err
		}
		sha, hashErr := digestFile(h.Session.Store)
		if hashErr != nil || sha != h.VerifiedStoreSHA256 {
			return errors.New("session changed after native verification; checkout retained")
		}
		for _, f := range h.Files {
			if active.Busy(f.Path) {
				return errors.New("session became active during handoff; checkout retained")
			}
		}
	}
	inventory := e.SessionInventory(ctx)
	if !inventory.Complete {
		return errors.New("session coverage changed during handoff; checkout retained")
	}
	var linked []Session
	for _, s := range inventory.Sessions {
		if s.Matches(c.Path) {
			linked = append(linked, s)
		}
	}
	if len(linked) != len(c.Sessions) {
		return errors.New("linked sessions changed during handoff; checkout retained")
	}
	for i, s := range linked {
		approved := c.Sessions[i]
		if s.Provider != approved.Provider || s.ID != approved.ID || s.Store != approved.Store {
			return errors.New("linked sessions changed during handoff; checkout retained")
		}
		// Only native-verified handoff may change cwd to its approved
		// destination. History/hash checks above reject concurrent mutation.
		if s.Checkout != approved.Checkout {
			verified := false
			for _, h := range r.SessionHandoffs {
				if h.Session.ID == s.ID && h.Status == "native_verified" && s.Checkout == h.Destination {
					verified = true
				}
			}
			if !verified {
				return errors.New("session cwd changed during handoff; checkout retained")
			}
		}
	}
	if _, statErr := os.Lstat(c.Path); !errors.Is(statErr, os.ErrNotExist) {
		return errors.New("original checkout path reappeared; quarantine retained")
	}
	r.Steps = append(r.Steps, Evidence{"claude_handoff", "verified", "Conversation bytes preserved; same session resumed locally twice with zero model turns; no source merge or branch switch"})
	if err = saveReceipt(e.Config, *r); err != nil {
		return err
	}
	if c.Kind == "worktree" {
		_, err = gitInput(ctx, e.Config.StateDir, nil, nil, "--git-dir", c.CommonDir, "worktree", "remove", "--force", destination)
	} else {
		if err = e.verifyCloneRecovery(ctx, Candidate{Path: destination, Kind: "clone"}, r); err != nil {
			return err
		}
		err = os.RemoveAll(destination)
	}
	if err != nil {
		return errors.New("verified quarantine removal incomplete; inspect receipt")
	}
	r.QuarantinePath = ""
	_ = os.Remove(parent)
	return nil
}

// RecoverQuarantine rolls back an interrupted handoff only. It never overwrites
// another checkout, deletes files, launches agents, or rewinds session history.
func (e *Engine) RecoverQuarantine(ctx context.Context, plan, project string) error {
	release, err := acquireLock(e.Config)
	if err != nil {
		return err
	}
	defer release()
	r, err := LoadReceipt(e.Config, plan, project)
	if err != nil {
		return err
	}
	if !sessionPath(r.Path) || pathID(r.Path) != project || r.QuarantinePath != filepath.Join(filepath.Dir(r.Path), ".infinite-disk-quarantine", project+"-"+plan[:12]) {
		return errors.New("receipt does not identify a recoverable quarantine")
	}
	if err := e.safeTarget(r.QuarantinePath); err != nil {
		return err
	}
	if noSymlinkAncestors(filepath.Dir(r.Path)) != nil {
		return errors.New("unsafe original parent")
	}
	if _, err := os.Lstat(r.Path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("recovery never overwrites an existing checkout")
	}
	active := e.Activity(ctx)
	if !active.Known || active.Busy(r.QuarantinePath) || active.Busy(r.Path) {
		return errors.New("checkout activity is unknown or busy")
	}
	common, err := gitText(ctx, r.QuarantinePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return err
	}
	if within(r.QuarantinePath, common) {
		err = os.Rename(r.QuarantinePath, r.Path)
	} else {
		_, err = gitInput(ctx, e.Config.StateDir, nil, nil, "--git-dir", common, "worktree", "move", "--", r.QuarantinePath, r.Path)
	}
	if err != nil {
		return errors.New("quarantine recovery failed; files retained")
	}
	r.QuarantinePath = ""
	r.Result = "quarantine_restored"
	r.Steps = append(r.Steps, Evidence{"session_quarantine", "restored", "Interrupted checkout restored without overwriting source or session history"})
	if err := saveReceipt(e.Config, r); err != nil {
		return err
	}
	return AppendEvent(e.Config, Event{Type: "quarantine_restored", PlanID: plan, CandidateID: project, Outcome: "restored"})
}
