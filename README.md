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

## Approve one project at a time

```sh
infinite-disk apply --review
# Each eligible project: preserve/verify/PERMANENTLY REMOVE? [y/N]

# Optional first step: approve recovery without removing the checkout.
infinite-disk apply --project PROJECT_ID --preserve-only
```

Empty input, EOF, and `no` skip the project. `--review` adds the detailed checks; the ordinary prompt still names the project, estimated size, and action. For a trusted Desktop approval flow, `--project PROJECT_ID --approve-plan EXACT_PLAN_SHA256` is the explicit non-interactive equivalent. There is no `--yes-all`. Plans expire after 24 hours and are rejected when the configuration or source changes.

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

The watcher checks the home filesystem every minute. It audits daily and, under pressure, at most hourly. Pressure defaults to **less than 100 GiB or 15% free**, adjustable in the private configuration. It **only saves reports**. No AI requests are scheduled by default; `agent_enabled` is false. Explicitly enabling it additionally requires a provider key in the service environment and permits at most one bounded triage attempt per day. Keychain integration and hard dollar budgets are roadmap work.

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
