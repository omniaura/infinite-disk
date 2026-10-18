# Infinite Disk

**A developer-workspace steward that asks before it acts.** Go + Cobra for deterministic safety, Pi for optional read-only triage. Standalone first; designed for desktop integrations through versioned JSON.

> `scan`, `triage`, and the background watcher **never commit, push, or delete projects**. No autonomous deletion mode exists. Approval is per project, and preservation must verify before removal. This is a young tool: start with reports, inspect the evidence, and keep independent backups.

## Install

macOS and Linux; Git, `lsof`, and `du` are required. Node **22.19+** and npm are needed only for Pi. Download a binary and verify `SHA256SUMS` from [Releases](https://github.com/omniaura/infinite-disk/releases), or build:

```sh
go install github.com/omniaura/infinite-disk/cmd/infinite-disk@latest
infinite-disk config init --root /absolute/path/to/worktree-parent
infinite-disk scan
infinite-disk review --details
```

Repeat `--root` for multiple project-parent directories. With no roots supplied, discovery checks `~/code`, `~/projects`, and common agent worktree directories. This is discovery scope, **not blanket deletion permission**. Missing roots produce warnings. Source inactivity defaults to seven days; age alone never authorizes removal.

## Read a report

### Include your coding apps

```sh
infinite-disk locations       # show known app locations + embedded project layouts
infinite-disk locations --add # explicitly save existing missing roots
infinite-disk scan            # rescan; previous approvals are invalidated
```

Known layouts cover Claude Code, Codex, Cursor, Grok/Grok Bot, T3 Code and Emdash. Embedded `.claude/worktrees`, `.worktrees`, `worktrees` and `workspaces` are discovered under `~/code` and `~/projects`, without following symlinks or traversing dependency/Git stores. Existing configurations are never silently widened: `locations --add` is explicit opt-in. Custom app storage outside these parents still needs an explicit config root. Missing worktrees are reported as not present, not confused with session histories or empty directories. Discovery coverage and the number inspected out of discovered projects are shown in new reports; each scan still uses a bounded rotating inspection batch. Discovery does not remove provider-native safety blockers.

The default report separates **ready for approval** from **needs review** and shows the largest checkouts first. Blocked bytes are not counted as approved savings. It is a rotating project batch, **not a whole-disk inventory**; model downloads, global caches, Docker, media and app data are not included. Saved free-space measurements are labeled separately from current free space.

Use `review --details` for evidence, or `review --project PROJECT_ID --details` for one project. Internal IDs and full paths stay in detail views. Terminal reports and approval prompts use your terminal's palette: cyan headings, green ready savings, amber warnings, and red permanent-removal notices. Text labels remain meaningful without color. Colors are automatic only on a terminal (stdout and stderr detected independently); pipes, `NO_COLOR=1`, and `TERM=dumb` stay plain. Use `--color always` to explicitly force colors or `--color never` to disable them; these override the automatic environment checks. `--json` never contains ANSI colors, even with `--color always`.

```sh
infinite-disk apply --review                # automatic terminal colors
infinite-disk review --color always         # force colors if auto detection fails
infinite-disk review --color never          # plain text
```

## Build/dependency cleanup with Kondo

Coding-session safety is separate from artifact cleanup; see [session links and orphan recovery](#session-links-and-orphan-recovery).

Install [Kondo 0.9](https://github.com/tbillington/kondo/releases/tag/v0.9), with `--dry-run` support, on your PATH. It supplies the ecosystem/artifact catalog; Infinite Disk does not duplicate that catalog.

```sh
infinite-disk caches scan     # Kondo dry-run, followed by independent checks
infinite-disk caches review   # separate saved artifact report
infinite-disk caches apply    # approve exact directories individually
# Or select one: infinite-disk caches apply --project ARTIFACT_ID
```

This flow **keeps source, Git history and the checkout**. It never invokes Kondo's recursive delete/all mode and never commits or uploads. Every artifact must be inside a configured project, entirely Git-ignored, untracked, inactive and older than the configured inactivity period. Private-state names, nested Git, symlinks, unreadable/incomplete checks and unsupported managed worktrees block cleanup. Every exact path and metadata fingerprint is rechecked after approval. Symlink-heavy dependency trees may therefore remain blocked in this first adapter. Kondo's rounded logical sizes are not used as guaranteed reclaimed bytes; Infinite Disk measures allocated sizes separately.

These are normally regenerable artifacts, but custom irreplaceable data inside an artifact directory is still your responsibility. Inspect the displayed paths and stop the project's tools before approving. Deleted artifacts are **not backed up**: reinstall dependencies/rebuild afterward. Same-user concurrent filesystem mutation cannot be fully excluded. Partial removal and interruption can leave a partly cleaned artifact; source remains outside the approved paths. Cache reports are separate from checkout reports and use the same 50-report retention policy; operation logs and receipts remain private. The monitor still only runs checkout audits, not this potentially expensive Kondo pass.

## Approve one project at a time

```sh
infinite-disk apply --review
# Each eligible project: preserve/verify/PERMANENTLY REMOVE? [y/N]
# Enter skips. q, quit, EOF or Ctrl+C stop the review.

# Optional first step: approve recovery without removing the checkout.
infinite-disk apply --project PROJECT_ID --preserve-only
```

Enter and `no` skip the project. `q`, `quit`, EOF and Ctrl+C end the review; Ctrl+C exits with status 130. EOF never approves even a partial `yes` line. `--review` adds detailed checks and any Pi advisory; the ordinary prompt still names the project, estimated size, upload destination host, recovery exclusions, and action. For a trusted Desktop approval flow, `--project PROJECT_ID --approve-plan EXACT_PLAN_SHA256` is the explicit non-interactive equivalent. There is no `--yes-all`. Plans expire after 24 hours and are rejected when the configuration, source, Git refs, or remote identity changes. Rescan before applying a previously preserve-only project again; receipts and bundles are intentionally not overwritten.

After approval, the Go engine:

1. Rechecks source, private state, configured scope, and current-user open files/working directories.
2. Creates a recovery snapshot using a temporary index, without moving your branch or changing your staging area. Hooks and clean filters are not run.
3. Pushes a dedicated `infinite-disk/recovery/...` branch to the existing `origin`, then verifies the exact SHA using a remote readback. **This can upload your changed source to that remote.** Remote hosts must be explicitly allowed; GitHub is the default.
4. For a standalone clone, creates a private all-refs Git bundle and verifies it independently. For a linked worktree, retains recovery in its shared Git repository and the verified remote.
5. Revalidates, records the preservation receipt, and removes only the approved checkout. Linked worktrees use native `git worktree remove`; clones are renamed into a same-parent quarantine and checked again before removal.

Network, disk, activity, or safety-check failures retain the project. If interrupted during removal, consult the receipt and its quarantine path. There is no promise that deletion is atomic or that activity cannot start between checks; stop the project's tools before approving it.

### Restore

```sh
infinite-disk restore --plan PLAN_ID --project PROJECT_ID --to /absolute/new/destination
```

For standalone clones, restore verifies the bundle checksum and checks out the recovery commit, preserving unpublished refs. The destination must not exist. The restored `origin` points at the offline bundle: reconnect to a network remote explicitly. For linked worktrees, use the receipt's recovery commit/ref with the retained shared repository or fetch the verified recovery branch. Recovery bundles and receipts are **never automatically purged**.

## Session links and orphan recovery

Git recovery preserves code, **not the coding conversation or its workspace binding**. Checkout scans now include local session IDs and storage paths in `review --details`. Associations include enclosing isolated worktree workspaces conservatively, not broad surviving parents like `~/code`. Linked sessions block checkout removal until native handoff is complete; incomplete/opaque session coverage also blocks removal. Old plans without session checks cannot be applied: rescan. This intentionally can reduce ready checkout savings to zero; the separate Kondo artifact flow keeps its existing checks and never deletes the checkout.

```sh
infinite-disk sessions list --project /absolute/checkout --details
infinite-disk sessions list --orphans
# Preview a native reopening route without writing or launching anything:
infinite-disk sessions reattach --provider claude --session SESSION_ID --to /absolute/project --json
# Review and approve a private preferred reopening destination:
infinite-disk sessions reattach --provider claude --session SESSION_ID --to /absolute/project
# Optionally start the native agent after per-session approval:
infinite-disk sessions resume --provider claude --session SESSION_ID --launch
```

`--to` must be an existing, non-symlinked Git checkout root. The original recorded checkout must be missing, not merely inaccessible or unknown. A duplicate session ID blocks routing. Each approval rechecks the original external transcript and destination Git identity; a changed route requires review again. Enter, no, q, EOF and Ctrl+C do not approve. JSON only previews, never saves/launches. Routes are private local files under `session-routes/`; the monitor never creates or applies them.

This **does not restore source, switch a branch, merge changes, copy transcripts, or rewrite app databases**. Choose the correct project and independently restore the preserved source if needed; a legacy orphan's original repository identity might no longer be verifiable. A saved route is not evidence of a successful native rebind. `--launch` explicitly starts the provider with its normal permissions/configuration and no permission-bypass flags; native hooks and provider charges can apply. No agent is launched during scans or cleanup.

Initial provider coverage is deliberately limited:

| Provider | Session metadata | Reopening / cleanup behavior |
| --- | --- | --- |
| Claude Code | Top-level transcript headers and native encoded project namespaces, without including conversation contents | Orphan CLI route; launching requires Claude Code 2.1.302+ in the 2.1 series. Existing-worktree handoff remains native. |
| Codex | Read-only `state_5.sqlite` identity/path fields | Orphan routes for CLI-source sessions only, after verifying `resume --cd` support. Desktop/app-owned sessions require native handoff/restore. |
| Grok | Native URL-encoded cwd/session namespace | Linked sessions detected; native reattachment adapter pending. |
| Cursor | Known flat project transcript namespaces | Linked sessions detected. Nested transcripts and opaque CLI stores report incomplete coverage. Native reattachment pending. |
| T3 Code | Read-only project/thread projections | Linked sessions detected; native handoff adapter pending. |
| Emdash, Pi, OpenCode, Grok Bot | Presence of known session stores | Coverage incomplete; no deletion or fabricated empty-session result. |

The SQLite metadata readers require `sqlite3`; missing tools, unreadable stores, unknown index versions, symlinks, time/entry limits and unsupported custom data roots fail closed. Lossy project namespace encodings are conservative positive matches, not used to invent a missing absolute path. Metadata headers can be historical; unknown/currently ambiguous cwd is not labeled orphaned. This is not exhaustive support for every coding app or custom storage layout. Session IDs/paths are not sent to Pi.

Provider-native automatic pre-removal handoff, verified Desktop rebinding, full session export/restore and additional adapters remain tracked in [#6](https://github.com/omniaura/infinite-disk/issues/6) and [#7](https://github.com/omniaura/infinite-disk/issues/7). This first increment protects sessions and helps reopen supported existing orphans; it does **not** implement automatic reattachment during checkout cleanup.

Sources: [Claude session resumption](https://code.claude.com/docs/en/sessions), [Claude worktree resumption](https://code.claude.com/docs/en/worktrees), and [official OpenAI documentation on native Codex handoff and snapshot/restore](https://learn.chatgpt.com/docs/environments/git-worktrees). Detection of private metadata layouts is conservative and version-specific, not a supported app-mutation API.

## Optional Pi agent

```sh
infinite-disk agent setup
# Supply OPENROUTER_API_KEY in your shell; never put it in config.json.
infinite-disk scan --agent
# Or: infinite-disk triage --plan latest
```

Pi receives **only opaque project IDs, age, estimated bytes, dirty-file counts, and mechanical blockers**. No filenames, source, repository names, branches, remotes, or local paths are sent. Its only tools are `inventory_summary` and `inspect_project`; filesystem/shell tools, extensions, skills, project instructions, persistent sessions, automatic retries, compaction, and cache warming are disabled. Requests are capped at four, output at 1,024 tokens/request, inspections at two, and the run at 60 seconds (75-second parent deadline). This bounds calls, **not a guaranteed dollar spend**. Provider pricing and metadata exposure still apply.

Pi recommendations are advisory. They cannot override a blocker or authorize a Git write/deletion. Reports distinguish performed checks from proposed future steps. Pi 1.3.0 is pinned with a lockfile; the architecture follows Ditto Desktop's Pi integration, with a newer dependency version to address an upstream transitive advisory.

## Native monitoring

```sh
infinite-disk service install --start
infinite-disk status
infinite-disk doctor
infinite-disk service uninstall
```

Installs only this user's Infinite Disk launchd agent (macOS) or systemd user service (Linux), with a stable private copy of the executable. `--platform cron` installs a marked five-minute cron block and preserves other entries. Systemd user services normally require an active login/user manager; this does not enable lingering or elevate privileges.

The watcher checks the home filesystem every minute. It audits daily and, under pressure, at most hourly. Pressure defaults to **less than 100 GiB or 15% free**, adjustable in the private configuration. It **only saves reports**. Large inventories are inspected in rotating bounded windows; warnings identify partial reports, and the next audit resumes rather than starving later projects. No AI requests are scheduled by default; `agent_enabled` is false. Explicitly enabling it additionally requires a provider key in the service environment and permits at most one bounded triage attempt per day. Keychain integration and hard dollar budgets are roadmap work.

## Logs, integration, and limits

| Artifact | Default location / policy |
| --- | --- |
| Config | `~/.config/infinite-disk/config.json`, mode 0600; no provider keys |
| State | `~/.local/share/infinite-disk`, mode 0700 |
| Actual operations | `logs/events.jsonl`, 10 MiB × five files, mode 0600 |
| Reports | `plans/`, latest 50; `review --json` |
| Preservation | `receipts/` and `recovery/`, private; no automatic expiry |
| Watcher health | `watch.json`; `status --json` |

`--json` reports use protocol version 1. Local plans/receipts do contain project paths and should not be uploaded casually. Recovery bundles contain source and Git history; keep the private state on an encrypted disk and back it up. The watcher does not clean its own recovery bundles or model dependency installation.

V1 deliberately blocks active/recent projects, unknown activity, sensitive changed files, unknown ignored data, submodules, nested repositories, shallow/LFS/annex/shared-object repositories, unusual staged/unstaged combinations, custom repository execution/credential configuration, symlinked targets, and Codex-managed worktrees requiring their owner's archive lifecycle. Only ignored `node_modules`, `.venv`, and `__pycache__` are treated as regenerable; inspect any project where you put irreplaceable data there. Secret detection is conservative, **not a comprehensive data-loss prevention system**. Existing committed secrets/history are not comprehensively scanned; the recovery branch goes to the same trusted origin. Removed clones retain refs and the approved current source, not every unreachable/reflog-only object, Git configuration, hooks, or dependency contents.

Sizes are `du` allocated estimates, not guaranteed reclaimed physical bytes (APFS clones and snapshots matter). Receipts include before/after filesystem free bytes; concurrent activity can affect the difference. Monitoring currently follows the home filesystem, not all mounted volumes. Windows, provider-aware lifecycle adapters, notifications/tray UI, and Ditto Desktop integration are [roadmap issues](https://github.com/omniaura/infinite-disk/issues?q=is%3Aissue+label%3Aroadmap).

## Development

```sh
go test -race ./...
go vet ./...
cd agent
npm ci --ignore-scripts
npm test
npm audit --omit=dev --audit-level=moderate
# Explicit paid local smoke only, with your own key:
node test/live.mjs
```

All destructive tests use fixture repositories and a local bare remote. No provider credentials are required in CI. See [CONTRIBUTING](CONTRIBUTING.md), [security model](SECURITY.md), and [architecture](docs/architecture.md). MIT licensed; no telemetry is sent by Infinite Disk. Optional model calls go to OpenRouter.
