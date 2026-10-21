# Claude cleanup and session continuity

Tracks [#6](https://github.com/omniaura/infinite-disk/issues/6). Default scans and the native monitor remain report-only. Each checkout requires approval of its exact saved plan, including session history and destination identities.

## Approved operation

1. Recheck source, activity, all supported provider scopes, native version, unique session IDs, complete session files and destination Git identity.
2. Preserve unfinished source on the existing approved origin; verify the exact recovery commit remotely and the local bundle when applicable.
3. Copy the original transcript plus its external session sidecars, subagents, tasks, file history and session environment into private local recovery storage. SHA-256 readback and fsync precede staging. Source contents/history never go to Pi or public logs.
4. Journal quarantine intent before moving anything. Use `git worktree move` for linked worktrees, same-volume rename for independent clones. The original pathname is now absent, but files are recoverable.
5. Invoke tested Claude Code 2.1.302 or 2.1.305 in the surviving project with a fixed local command, empty tools/MCP, bare mode, hooks disabled, no inherited credentials, a dummy loopback API endpoint, and zero maximum model turns. Claude itself clears its gone-worktree binding. Infinite Disk never rewrites its event store.
6. Require native structured initialization with the same session ID and exact destination cwd, a zero-turn/zero-cost success, on-disk cleared binding, unchanged original transcript prefix, and unchanged sidecars. Independently resume again by normal session ID, not only explicit transcript path. Save the preferred reopening route automatically.
7. Recheck activity, source/private state, session inventory and backup integrity. Only then remove the quarantine. A durable private receipt links original checkout, session ID, backup manifest, surviving destination and source recovery commit.

A surviving parent workspace session stays in its parent. Its history is backed up and linked to the child's recovery, but no native session is started or moved for it. Removing a child repo does not orphan that parent cwd.

## Immutable shared history snapshots

Approved operations store session bytes in `session-backups/shared-v1/<digest>/`.
The digest binds a versioned manifest of provider, session ID, and the complete
sorted path/size/SHA-256 inventory. An identical inventory in another checkout
reuses that snapshot only after rereading the live inventory and every backup
file. Files are private, owned regular files with one link; snapshots do not
hardlink or symlink to live history. Extra/missing files, symlinks, hardlinks,
permission changes, manifest differences, partial writes or bad hashes fail
closed without overwriting/repairing existing recovery data.

Changed transcript or sidecar bytes create another immutable snapshot. Each
checkout's receipt still records its own recovery commit, session destination
and handoff status; the shared byte manifest does not substitute for that
checkout-specific proof. Legacy per-checkout backup paths remain supported.
There is no automatic migration, pruning or garbage collection of either layout.

## Scope and limits

Supported layouts are repo-local `.claude/worktrees/` and `.worktrees/claude-*`, including stack child clones. Linked worktrees must resolve to the primary Git checkout. Independent child clones require exactly one structurally matching primary checkout with the same origin identity. Destinations cannot be symlinks or inside the removed checkout. Target changes invalidate approval; the tool does not reset, merge, check out a branch, or restore old code over current work.

Inventory scans routing metadata through each complete bounded transcript, not just its directory name or a header/tail sample. A hook-created worktree can keep its conversation in the primary project's namespace; its persisted binding still protects the actual checkout. Unknown routing schemas, incomplete inspection and uncertain relocation fail closed. Binding detection is broader than automatic handoff support: unsupported hook-worktree lifecycle cases remain blocked rather than being mistaken for unlinked checkouts.

Native history stays in Claude's original storage namespace. This is **not** migration of the project picker or a silent conversation fork. Claude's documented global ID lookup and cleared binding allow the same conversation to continue from the surviving project. Use the saved route for an explicit destination:

```sh
infinite-disk sessions resume --provider claude --session SESSION_ID --launch
```

That optional interactive launch has its own approval and normal provider permissions/costs. In contrast, automatic cleanup only runs the restricted zero-turn maintenance invocation. No normal model turn, pending task, shell command, subagent, or hook is intentionally resumed by cleanup. Native version drift, any inability to verify this contract, unsupported providers, more than eight linked sessions, unknown storage, symlinks, active sessions, or session backup budgets (512 MiB / 5,000 files per session) keep the checkout.

App-owned Desktop rebinding, encrypted remote archives, and arbitrary custom provider stores are not claimed. A local backup does not survive disk loss; private backups/receipts have no automatic expiry. Restoring historical source remains a separate explicit action.

## Interruption recovery

Ordinary failure/Ctrl+C attempts a bounded rollback even after cancellation. SIGKILL, power loss or failed rollback can leave a journaled quarantine. Inspect the private receipt, then restore it without overwriting anything:

```sh
infinite-disk recover --plan PLAN_ID --project PROJECT_ID
```

Recovery restores checkout placement only. It never rolls back a conversation, deletes backups or starts an agent. If the original path has reappeared or activity is unknown, recovery refuses. Native session metadata already cleared during a partial operation may stay cleared; original conversation bytes and source remain recoverable.

## Verification

Ordinary fixture tests cover actual approved removal, remote source readback, byte-exact backups, preserved primary branch/index, native failures, uncleared bindings, changed source/private state/sidecars/target, duplicate IDs, symlinks, cancellation rollback and crash-quarantine recovery. The opt-in installed-native smoke uses only disposable conversations, an isolated config and a loopback dummy endpoint:

```sh
INFINITE_DISK_NATIVE_CLAUDE_TEST=1 go test ./internal/disk -run '^TestNativeClaudeOfflineCleanup$' -count=1 -v
```

It tests both 2.1.302- and 2.1.305-tagged fixture conversations with the installed native CLI. For each, it removes a fixture checkout, verifies native session continuity and disabled hooks, then independently restores the private archive into a fresh native config and verifies both transcript-path and normal session-ID resume. No real user session or paid provider is involved. Native compatibility is an exact allowlist: untested patch versions stay blocked, and a changed executable still invalidates existing approval.

Sources: [Claude missing-worktree binding behavior](https://code.claude.com/docs/en/worktrees), [global session lookup and duplicate identity handling](https://code.claude.com/docs/en/sessions), the installed Claude 2.1.302 and 2.1.305 CLIs tested with the offline fixture, and official Agent SDK 0.3.302 declarations. Internal metadata formats are version-specific, not a general supported write API.
