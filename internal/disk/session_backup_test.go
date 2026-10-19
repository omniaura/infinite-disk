package disk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func backupFixture(t *testing.T) (fixture, Candidate) {
	t.Helper()
	f, _, _ := claudeFixture(t)
	p := plan(t, f)
	eligible(t, p.Candidates[0])
	return f, p.Candidates[0]
}

func fixtureBackup(t *testing.T, f fixture, c Candidate, id string) Receipt {
	t.Helper()
	r := Receipt{Version: ProtocolVersion, PlanID: strings.Repeat("a", 64), CandidateID: id, RecoveryCommit: strings.Repeat(id[:1], 40)}
	if err := f.e.backupClaudeSessions(c, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSessionSnapshotsReuseBytesNotCheckoutReceipts(t *testing.T) {
	f, c := backupFixture(t)
	r1 := fixtureBackup(t, f, c, "1111111111111111")
	h1 := r1.SessionHandoffs[0]
	manifest, _ := os.ReadFile(filepath.Join(h1.BackupDir, "manifest.json"))
	r2 := fixtureBackup(t, f, c, "2222222222222222")
	h2 := r2.SessionHandoffs[0]
	if h1.BackupDir != h2.BackupDir || h1.RecoveryCommit == h2.RecoveryCommit || h1.RecoveryCommit != r1.RecoveryCommit || h2.RecoveryCommit != r2.RecoveryCommit {
		t.Fatal("shared bytes must retain separate checkout recovery linkage")
	}
	after, _ := os.ReadFile(filepath.Join(h1.BackupDir, "manifest.json"))
	if string(manifest) != string(after) {
		t.Fatal("reused manifest was rewritten")
	}
	entries, err := os.ReadDir(filepath.Dir(h1.BackupDir))
	if err != nil || len(entries) != 1 {
		t.Fatal("duplicate snapshot directories", err)
	}
	for _, file := range h1.Files {
		backup := filepath.Join(h1.BackupDir, pathID(file.Path))
		live, _ := os.Stat(file.Path)
		saved, _ := os.Stat(backup)
		if os.SameFile(live, saved) || privateSnapshotPath(backup, false) != nil {
			t.Fatal("backup aliases live session or is not private single-link storage")
		}
	}
	loaded, err := LoadReceipt(f.e.Config, r1.PlanID, r1.CandidateID)
	if err != nil || loaded.SessionHandoffs[0].RecoveryCommit != r1.RecoveryCommit {
		t.Fatal("earlier receipt overwritten", err)
	}
}

func TestChangedSessionSnapshotsRetainEarlierVersions(t *testing.T) {
	for _, change := range []string{"transcript", "sidecar", "new_sidecar"} {
		t.Run(change, func(t *testing.T) {
			f, c := backupFixture(t)
			r1 := fixtureBackup(t, f, c, "1111111111111111")
			h := c.SessionHandoffs[0]
			first := r1.SessionHandoffs[0]
			original, _ := os.ReadFile(filepath.Join(first.BackupDir, pathID(h.Session.Store)))
			switch change {
			case "transcript":
				if err := appendClaudeClear(h); err != nil {
					t.Fatal(err)
				}
			case "sidecar":
				write(t, strings.TrimSuffix(h.Session.Store, ".jsonl")+"/subagents/agent-child.jsonl", "changed sidecar\n")
			case "new_sidecar":
				write(t, strings.TrimSuffix(h.Session.Store, ".jsonl")+"/subagents/new.jsonl", "new sidecar\n")
			}
			stale := Receipt{PlanID: strings.Repeat("b", 64), CandidateID: "2222222222222222"}
			if err := f.e.backupClaudeSessions(c, &stale); err == nil || len(stale.SessionHandoffs) != 0 {
				t.Fatal("stale inventory reused an old snapshot")
			}
			files, err := claudeFiles(h.Session)
			if err != nil {
				t.Fatal(err)
			}
			c.SessionHandoffs[0].Files = files
			r2 := fixtureBackup(t, f, c, "3333333333333333")
			if first.BackupDir == r2.SessionHandoffs[0].BackupDir {
				t.Fatal("new session state did not get a new immutable version")
			}
			after, _ := os.ReadFile(filepath.Join(first.BackupDir, pathID(h.Session.Store)))
			if string(after) != string(original) {
				t.Fatal("older recovery bytes changed")
			}
		})
	}
}

func TestUnsafeExistingSessionSnapshotFailsClosed(t *testing.T) {
	for _, change := range []string{"corrupt_payload", "corrupt_manifest", "missing_manifest", "extra_file", "payload_symlink", "payload_hardlink", "payload_permissions", "directory_permissions", "directory_symlink"} {
		t.Run(change, func(t *testing.T) {
			f, c := backupFixture(t)
			r1 := fixtureBackup(t, f, c, "1111111111111111")
			h := r1.SessionHandoffs[0]
			payload := filepath.Join(h.BackupDir, pathID(h.Session.Store))
			switch change {
			case "corrupt_payload":
				write(t, payload, "corrupt bytes")
			case "corrupt_manifest":
				write(t, filepath.Join(h.BackupDir, "manifest.json"), "corrupt manifest")
			case "missing_manifest":
				os.Remove(filepath.Join(h.BackupDir, "manifest.json"))
			case "extra_file":
				write(t, filepath.Join(h.BackupDir, "unexpected"), "unexpected bytes")
			case "payload_symlink":
				os.Remove(payload)
				if err := os.Symlink(h.Session.Store, payload); err != nil {
					t.Fatal(err)
				}
			case "payload_hardlink":
				if err := os.Link(payload, filepath.Join(f.root, "alias")); err != nil {
					t.Fatal(err)
				}
			case "payload_permissions":
				os.Chmod(payload, 0644)
			case "directory_permissions":
				os.Chmod(h.BackupDir, 0755)
			case "directory_symlink":
				moved := filepath.Join(f.root, "moved-snapshot")
				if err := os.Rename(h.BackupDir, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, h.BackupDir); err != nil {
					t.Fatal(err)
				}
			}
			r2 := Receipt{PlanID: strings.Repeat("b", 64), CandidateID: "2222222222222222"}
			if err := f.e.backupClaudeSessions(c, &r2); err == nil || len(r2.SessionHandoffs) != 0 {
				t.Fatal("unsafe snapshot was reused or silently repaired")
			}
			exists(t, f.repo)
			exists(t, h.Session.Store)
		})
	}
}

func TestLegacySessionBackupsRemainReadable(t *testing.T) {
	f, c := backupFixture(t)
	h := c.SessionHandoffs[0]
	h.BackupDir = filepath.Join(f.e.Config.StateDir, "session-backups", "legacy", pathID(h.Session.ID))
	if err := os.MkdirAll(h.BackupDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, file := range h.Files {
		if err := copySessionFile(file.Path, filepath.Join(h.BackupDir, pathID(file.Path)), file); err != nil {
			t.Fatal(err)
		}
	}
	fixtureBackup(t, f, c, "1111111111111111")
	if err := verifyOriginalPrefix(h); err != nil {
		t.Fatal("legacy receipt layout no longer recoverable", err)
	}
}

func TestTwoApprovedSiblingCleanupsShareOneParentSnapshot(t *testing.T) {
	f := newFixture(t)
	wrapper := filepath.Join(f.root, ".claude", "worktrees", "shared-parent")
	home := filepath.Join(filepath.Dir(f.root), "native-home")
	store := filepath.Join(home, ".claude", "projects", claudeProjectKey(wrapper), "parent-session.jsonl")
	write(t, store, `{"type":"user","cwd":`+quoted(wrapper)+`,"sessionId":"parent-session","message":{"role":"user","content":"PRIVATE FIXTURE"}}`+"\n")
	for _, task := range []string{"one", "two"} {
		checkout := filepath.Join(wrapper, "workspaces", task, "backend")
		os.MkdirAll(filepath.Dir(checkout), 0700)
		runGit(t, f.root, "clone", "--no-local", f.remote, checkout)
		runGit(t, checkout, "checkout", "main")
		runGit(t, checkout, "config", "user.name", "Infinite Disk Test")
		runGit(t, checkout, "config", "user.email", "fixtures@example.invalid")
		write(t, filepath.Join(checkout, "unfinished.txt"), task+" unfinished work\n")
		old(t, filepath.Join(checkout, "unfinished.txt"))
		old(t, filepath.Join(checkout, "source.txt"))
	}
	f.e.Config.Roots = []string{wrapper}
	f.e.Sessions = func(ctx context.Context) SessionInventory { return ReadSessions(ctx, home) }
	f.e.claudeRunner = func(context.Context, ClaudeHandoff) error { t.Fatal("surviving parent must not be moved"); return nil }
	var receipts []Receipt
	for range 2 {
		p, err := f.e.RunReport(context.Background())
		if err != nil || len(p.Candidates) == 0 {
			t.Fatal("fixture discovery failed", err)
		}
		c := p.Candidates[0]
		eligible(t, c)
		r, err := f.e.Apply(context.Background(), p, c.ID, p.ID, true)
		if err != nil || r.Result != "removed" || r.SessionHandoffs[0].Status != "retained_parent" {
			t.Fatal("approved fixture cleanup failed", err)
		}
		task := filepath.Base(filepath.Dir(c.Path))
		if runGit(t, f.remote, "show", r.RecoveryCommit+":unfinished.txt") != task+" unfinished work" {
			t.Fatal("source recovery linkage lost")
		}
		receipts = append(receipts, r)
	}
	if receipts[0].SessionHandoffs[0].BackupDir != receipts[1].SessionHandoffs[0].BackupDir {
		t.Fatal("approved sibling cleanups duplicated identical session bytes")
	}
	exists(t, wrapper)
	exists(t, store)
}
