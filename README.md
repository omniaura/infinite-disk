# Infinite Disk

### Ditto Router inference (optional)

The Pi worker can use a personal Ditto Router endpoint instead of OpenRouter.
It still receives only bounded, anonymized project metadata and can only suggest
actions. It cannot approve, push, delete, or read coding-session content.

Use `heyditto endpoints provider-keys --output json` to find a saved personal
provider-key ID (not the raw key), and `heyditto endpoints models gm` for the
server's model catalog. Create an endpoint with an explicit model, `--billing-mode
byok`, and `--provider-key <UUID>` to avoid Ditto-funded fallback. Do not change
your CLI default endpoint unless you intend to.

Create a private directory (0700), then deliver an endpoint-scoped key directly
to a new file using `heyditto endpoints keys create <endpoint> --key-file
<absolute-path> --yes`. The file must not already exist. Never use a Ditto
management key or the raw upstream provider key in Infinite Disk.

```sh
infinite-disk agent setup
infinite-disk agent configure --provider ditto --model gm/gpt-6-luna \
  --base-url https://router.heyditto.ai/v1 --key-file /absolute/private/inference-key
infinite-disk triage --plan latest --json
```

Use the official gateway URL returned by `heyditto endpoints list`. Configuration
stores only a file reference; the selected key is passed privately to the worker.
Configuration does not enable scheduled paid runs. Existing OpenRouter setups
continue to use `OPENROUTER_API_KEY`. Each explicit triage is limited to four model
requests, 4096 output tokens/request and 60 seconds; these are request bounds,
not a dollar-spend guarantee. Pi's local custom-provider cost counters are not
billing evidence; use the Router's receipts/usage for actual costs.
The first request requires the inventory tool; when suggestions are offered,
the next request requires `suggest_action`. The fourth request disables tools
so Pi can return final advice. Advice fails closed if evidence is missing.

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

### Rolling scan, triage, review, repeat

```sh
infinite-disk scan --agent --batch-size 100
infinite-disk review
# Continue from the checkpoint, without re-walking a fresh project index:
infinite-disk scan --agent --batch-size 100
# Or explicitly request three sequential batches, each published immediately:
infinite-disk scan --agent --batch-size 100 --batches 3
```

Interactive `scan` defaults to 100 projects per batch (also capped by the
configuration's `max_projects`). It saves an immutable report before AI triage.
Separate invocations resume after the last inspected project. Discovery
indexes are reused for 15 minutes; partial indexes remain explicitly marked
partial, never treated as whole-disk coverage. `scan --refresh` looks for new folders
immediately. Only discovery paths are cached: activity, sessions, source and
recovery prerequisites are rechecked for each new batch and again before apply.
Discovery gets a 45-second traversal budget within the five-minute scan; an individual project
gets 15 seconds. Incomplete checks stay blocked, and partial coverage is explicit.

Pi receives the largest **20** anonymized metadata records from each batch and
is instructed to return at most three concise recommendations. This is advisory coverage, not a
claim that AI analyzed every inspected project. To advise particular saved
projects: `infinite-disk triage --plan PLAN --project ID1,ID2` (at most 20).
Each `--agent` batch permits at most four paid requests, 4096 output tokens per
request and 60 seconds; `--batches 3` explicitly permits three such budgets.
Multiple batches stop after one pass without repeating projects within that
invocation. Ctrl+C cancels; no cleanup is performed. With `--json --batches N`
where N > 1, each completed report is emitted as one JSON Line as soon as ready.
If inference fails, the deterministic report is still printed and saved, with a
nonzero exit status and a sanitized error on stderr; later batches do not run.
Directory footprints are not net reclaim promises, and nested checkouts are
deduplicated within summary groups.
Unrecorded model action suggestions are discarded with an audit warning, never
turned into tool evidence or approval. Unsupported actions and project IDs are
still rejected; retained suggestions must pass the same Go evidence checks.

### Evidence that agentic mode ran

`scan --agent`, `triage`, and `review` show an **AGENT RUN** panel: configured
provider/model, actual bounded model request count, elapsed time, advisory
coverage, reported input/output tokens, tool-call counts and the recommendations.
`review --json` retains this as `agent_run` plus `agent_steps`. Missing token
usage means unknown, not free inference. Model recommendations and tool records
are suggestions only; source/private/session checks and separate exact CLI
approval still own every write. A plain scan without `--agent` has no AI run.

### Experimental typed decisions (Jev / direct OpenRouter)

`infinite-disk decide --plan PLAN --key-file /absolute/private/openrouter-key`
is an explicit paid, report-only experiment. The key file must be owner-only
(0600), owned by this user, and neither a symlink nor a hardlink. It does not
change your configured Pi/GM model, background monitor, or latest approval plan.
Use `--project ID1,ID2` for a smaller selection and `--json` for structured output.

The command sends at most 20 anonymized project metadata records and typed
Choice questions to OpenRouter's Decisions API with pinned
`typesafe/jev-1.14`. It makes one request (30-second deadline, no retries or
redirects, 32 KiB request and 512 KiB response limits). No source, paths, names,
diffs, remotes, session text, tools, or approval capabilities cross that boundary.
Reported model identity, usage/cost, probabilities and confidence are saved in
private `decision-reports/` sidecars, not executable approval plans. Invalid
answers fail closed; uncertain choices escalate. The experimental confidence
thresholds are not domain-calibrated guarantees, and no confidence grants
permission to delete. Jev generates neither explanations nor tool calls.

This tests fast selection among Go-offered prerequisites against generative Pi
triage; it does not establish superior accuracy or replace deterministic safety
checks. Inspect the original `actions --plan PLAN` evidence and separately approve
every CLI operation. Typed Router-profile support is tracked in
[DITTO-1087](https://omniaura-team.atlassian.net/browse/DITTO-1087).

### Preserve ignored/private state locally

```sh
# Read-only inventory of exact paths and byte hashes; nothing moved:
infinite-disk private plan --plan SOURCE_PLAN --project PROJECT_ID
infinite-disk private review --plan PRIVATE_PLAN
# Separate, exact one-project approval; local relocation, no upload/deletion:
infinite-disk private apply --plan PRIVATE_PLAN --approve-plan PRIVATE_PLAN
# Refresh source/session/eligibility checks, then separately review removal:
infinite-disk analyze --plan SOURCE_PLAN --project PROJECT_ID
infinite-disk apply --review
# Restore source at its original path first, then copy private files back:
infinite-disk private restore --plan PRIVATE_PLAN --approve-plan PRIVATE_PLAN
```

Use the same `--config` throughout. This archives unknown ignored roots and
sensitive-named entries inside allowed dependency artifacts with same-filesystem,
no-overwrite moves, durable intent receipts and independent SHA-256 readback.
Tracked source, native agent state, nested repositories, symlinks, hardlinks,
special files, mount crossings, changed/busy/unknown activity and incomplete
inventories remain protected. A project archive has a 2 GiB byte budget and a
250,000-entry traversal limit. Private data is **local only**, not an off-device
backup and never uploaded to Git or Pi. It consumes retained storage, not savings.

New scans and apply recheck linked private archives; incomplete, modified or busy
archives prevent removal. Interrupted relocation keeps every byte in its original
or journaled archive path; `private restore` recovers moved entries without
overwriting new work. Restore is copy-based and retains the archive; identical
destinations are skipped. Inspect retained staging copies after a failed restore.
Only after a fresh analysis passes can checkout removal be approved separately.
Unknown native/session layouts and dependency attestation remain separate work;
this is not a force-delete option. Archives are never automatically pruned.

### Include your coding apps

```sh
infinite-disk locations       # show known app locations + embedded project layouts
infinite-disk locations --add # explicitly save existing missing roots
infinite-disk scan            # rescan; previous approvals are invalidated
```

Known layouts cover Claude Code, Codex, Cursor, Grok/Grok Bot, T3 Code and Emdash. Embedded `.claude/worktrees`, `.worktrees`, `worktrees` and `workspaces` are discovered under `~/code` and `~/projects`, without following symlinks or traversing dependency/Git stores. Existing configurations are never silently widened: `locations --add` is explicit opt-in. Custom app storage outside these parents still needs an explicit config root. Missing worktrees are reported as not present, not confused with session histories or empty directories. Discovery coverage and the number inspected out of discovered projects are shown in new reports; each scan still uses a bounded rotating inspection batch. Discovery does not remove provider-native safety blockers.

The default report separates **ready for approval** from **needs review** and shows the largest checkouts first. Blocked bytes are not counted as approved savings. It is a rotating project batch, **not a whole-disk inventory**; model downloads, global caches, Docker, media and app data are not included. Saved free-space measurements are labeled separately from current free space.

“Ready for approval” is not a completed recovery proof. At removal time, standalone clones also undergo a bounded local Git integrity/unreachable-history check before writes and again at final/quarantine boundaries. Unreachable-only or reflog-only objects are not covered by a refs/HEAD bundle, so their presence (or an inconclusive check) retains the clone for manual preservation. No bypass is provided. Linked worktrees keep their existing shared Git database. Rolling discovery does not pay this full-object inspection cost for every project.

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

This flow **keeps source, Git history and the checkout**. It never invokes Kondo's recursive delete/all mode and never commits or uploads. Every artifact must be inside a configured project, entirely Git-ignored, untracked, inactive and older than the configured inactivity period. Private-state names, nested Git, broken/escaping symlinks, unreadable/incomplete checks and unsupported managed worktrees block cleanup. Internal package-manager links are hashed, never traversed for deletion, and must resolve inside the exact artifact directory. Every exact path and metadata fingerprint is rechecked after approval. Kondo's rounded logical sizes are not used as guaranteed reclaimed bytes; Infinite Disk measures allocated sizes separately.

These are normally regenerable artifacts, but custom irreplaceable data inside an artifact directory is still your responsibility. Inspect the displayed paths and stop the project's tools before approving. Deleted artifacts are **not backed up**: reinstall dependencies/rebuild afterward. Same-user concurrent filesystem mutation cannot be fully excluded. Partial removal and interruption can leave a partly cleaned artifact; source remains outside the approved paths. Cache reports are separate from checkout reports and use the same 50-report retention policy; operation logs and receipts remain private. The monitor still only runs checkout audits, not this potentially expensive Kondo pass.

For an explicitly authorized automation, `caches apply --plan CACHE_DIGEST --project ARTIFACT_ID --approve-plan CACHE_DIGEST --non-interactive --json` approves one exact artifact without a prompt. There is no batch approve-all. Wrong/expired plans, blocked IDs, changed paths and missing approval fail; `--non-interactive` never waits for stdin.

## Analyze, suggest, then approve

```sh
infinite-disk plans                            # rotating saved batches; do not sum them
infinite-disk actions --filter needs-analysis  # compact suggestions, largest first
infinite-disk actions --all-plans --filter ready # deduplicate all saved batches
infinite-disk analyze --filter needs-analysis --limit 20
infinite-disk actions --plan EXACT_DIGEST --json --limit 10
# Optional paid prioritization of metadata-backed suggestions:
infinite-disk analyze --project PROJECT_ID --agent
```

New scans/monitor audits automatically collect bounded advisory analysis for every inspected project. `analyze` refreshes selected projects in the full saved report; it does not apply anything or reset the original report's expiry. It separates checked source status from unknown, compares HEAD with **local** origin-tracking refs (not live proof of push), classifies bounded ignored-entry examples, and proposes concrete prerequisites. Build/dependency classifications are hints for Kondo inspection, not a new cleanup catalog or deletion permission. Private data stays private; no blanket `git add` or model-generated shell command is executed.

Suggestions distinguish available CLI operations from manual/unsupported prerequisites. A source-safe, idle project may offer **preserve source only** even when ignored private data, recency or a missing session handoff blocks removal. This still needs exact approval, pushes only the checked source snapshot and HEAD ancestry to the existing origin, and keeps the entire checkout, ignored files and sessions untouched. It does not clear a removal blocker. Active projects, unknown activity, unsafe source, filters, special Git storage and other unverified states cannot use this narrower action.

`actions --json` returns a compact versioned **suggestions-only** envelope with `plan_id`, total/matching/returned counts, project evidence, optional Pi advice, and exact command **argv arrays**. These are proposals, not grants of authority. A human or explicitly authorized agent must separately approve an action. Never execute model-written command text. `--limit` and `--filter all|ready|needs-analysis` keep automation output bounded without constructing an invalid filtered plan. Use `plans --json` to address an earlier rotating batch by its exact digest. See [the analysis/approval contract](docs/advisory-analysis.md).

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

Git recovery preserves code, **not the coding conversation or its workspace binding**. Checkout scans include linked sessions and proposed surviving destinations. For supported Claude worktrees, `apply --review` now performs the session handoff automatically after per-project approval: verified private transcript/sidecar backups, reversible quarantine, native resume in the surviving project, persistent cleared-worktree verification, a second resume by normal session ID, then permanent checkout removal. It makes **zero model turns**, disables tools/MCP/hooks, and does not switch branches or merge source in the destination. A failure restores the quarantine; history backups and recovery receipts remain private. See [the Claude handoff contract](docs/claude-handoff.md).

Sessions in a surviving parent stack workspace are backed up and linked to source recovery, but keep their cwd; deleting one child repo must not steal the parent conversation. Unrelated supported stores no longer veto every project. Actually linked unsupported sessions, incomplete coverage, ambiguous identities, changed state and unknown private files still block removal. Old reports require a new scan. Kondo artifact-only cleanup remains separate.

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

The manual orphan-route command above **does not restore source, switch a branch, merge changes, copy transcripts, or rewrite app databases**. Choose the correct project and independently restore preserved source if needed; a legacy orphan's original repository identity might not be verifiable. Saving a route alone is not proof of native handoff. `--launch` explicitly starts the provider with its normal permissions/configuration; hooks and provider charges can apply. Scans/monitoring never start agents. Approved Claude cleanup uses only the isolated local maintenance invocation described above, not a normal coding-agent turn.

Initial provider coverage is deliberately limited:

| Provider | Session metadata | Reopening / cleanup behavior |
| --- | --- | --- |
| Claude Code | Transcript identity/cwd/worktree metadata, encoded namespaces including long paths, external subagents/checkpoints/tasks | Automatic approved cleanup for tested Claude Code **2.1.302**, with native handoff and verified private backup; other versions/cases stay blocked. Manual orphan launch requires 2.1.302+ in the 2.1 series. |
| Codex | Read-only `state_5.sqlite` identity/path fields | Orphan routes for CLI-source sessions only, after verifying `resume --cd` support. Desktop/app-owned sessions require native handoff/restore. |
| Grok | Native URL-encoded cwd/session namespace | Linked sessions detected; native reattachment adapter pending. |
| Cursor | Flat/nested project transcripts; CLI hashed cwd namespaces and bounded descendant-directory matching | Unrelated stores do not block Claude; linked Cursor sessions still need native handoff. CLI namespace adapter is gated to tested 2026.05.28-a70ca7c. |
| T3 Code | Read-only project/thread projections | Linked sessions detected; native handoff adapter pending. |
| Emdash, Pi, OpenCode | Read-only workspace indexes, Pi session headers, OpenCode SQLite/legacy session metadata | Candidate-scoped detection only; actually linked sessions still block removal pending native adapters. |
| Grok Bot / unknown schemas | Presence of known unsupported stores | Coverage incomplete; never fabricated as empty. |

The SQLite metadata readers require `sqlite3`; missing tools, unreadable stores, unknown index versions, symlinks, time/entry limits and unsupported custom data roots fail closed. Lossy project namespace encodings are conservative positive matches, not used to invent a missing absolute path. Metadata headers can be historical; unknown/currently ambiguous cwd is not labeled orphaned. This is not exhaustive support for every coding app or custom storage layout. Session IDs/paths are not sent to Pi.

Remaining provider/Desktop adapters and broader source recovery are tracked in [#6](https://github.com/omniaura/infinite-disk/issues/6); encrypted remote session offload in [#7](https://github.com/omniaura/infinite-disk/issues/7). Local history backups are not off-device archives and are never silently expired.

Sources: [Claude session resumption](https://code.claude.com/docs/en/sessions), [Claude worktree resumption](https://code.claude.com/docs/en/worktrees), and [official OpenAI documentation on native Codex handoff and snapshot/restore](https://learn.chatgpt.com/docs/environments/git-worktrees). Detection of private metadata layouts is conservative and version-specific, not a supported app-mutation API.

## Optional Pi agent

```sh
infinite-disk agent setup
# Supply OPENROUTER_API_KEY in your shell; never put it in config.json.
infinite-disk scan --agent
# Or: infinite-disk triage --plan latest
```

Pi receives **only opaque project IDs, age, estimated bytes, checked-source/count facts, local-tracking commit counts, ignored-example category counts, offered action kinds and mechanical blockers**. No filenames, source, repository names, branches, remotes, session text or local paths are sent. Its only tools are `inventory_summary`, `inspect_project` and `suggest_action`; the last records a suggestion with `approved:false, executed:false`, never calls the CLI. Filesystem/shell tools, extensions, skills, project instructions, persistent sessions, automatic retries, compaction, and cache warming are disabled. At most the 20 largest project metadata records are offered; `agent_steps` records actual advisory coverage. Requests are capped at four, output at 4,096 tokens/request, inspections at two, and the run at 60 seconds (75-second parent deadline). This bounds calls, **not a guaranteed dollar spend**. Provider pricing and metadata exposure still apply. Rerun `agent setup` after upgrading an older worker.

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

V1 deliberately blocks active/recent projects, unknown activity, sensitive changed files, unknown ignored data, submodules, nested repositories, shallow/LFS/annex/shared-object repositories, unusual staged/unstaged combinations, custom repository execution/credential configuration, symlinked targets, and Codex-managed worktrees requiring their owner's archive lifecycle. Only ignored `node_modules`, `.venv`, and `__pycache__` are treated as regenerable; inspect any project where you put irreplaceable data there. Secret detection is conservative, **not a comprehensive data-loss prevention system**. Existing committed secrets/history are not comprehensively scanned; the recovery branch goes to the same trusted origin. Clone deletion refuses unpreserved unreachable/reflog-only history at execution-time checks; bundles still cover refs and approved current source, not local Git configuration, hooks or dependency contents. Preserve-only keeps the original clone and its excluded history.

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
