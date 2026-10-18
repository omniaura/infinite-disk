# Architecture and integration contract

Go/Cobra owns scan, immutable plans, approval, preservation, removal, restore, state, and scheduling. An embedded locked Node worker uses the official Pi SDK for optional triage. The executable works without Node for deterministic reports and all preservation operations.

```text
native scheduler -> watcher -> deterministic scan -> private immutable report
                                             -> optional metadata-only Pi triage
user/Desktop -> review -> per-project approval of exact digest
             -> fresh checks -> recovery push/readback -> bundle verification
             -> final checks -> native/isolated removal -> private receipt
```

There is deliberately no edge from the scheduler/model to apply. Pi sees opaque identifiers and coarse scan facts. Paths and Git contents stay in the Go process/local state. Settings/resources/session/credential stores are explicit and non-persistent; all default Pi filesystem and shell tools are disabled. Upstream integration reference: [Ditto Desktop PR #30](https://github.com/ditto-assistant/ditto-desktop/pull/30) and its merged main. Infinite Disk pins Pi 1.3.0 because the Desktop-era 1.0.0 shrinkwrap included a vulnerable brace-expansion dependency; ordinary npm overrides did not override that shrinkwrap. Current locked installation audits clean.

## CLI protocol v1

- `scan --json` returns `Plan`: `version`, `id`, `mode`, `created_at`, `config_digest`, `space`, `candidates`, `warnings`, optional `recommendations` and `agent_steps`.
- `review --plan DIGEST --json` reads an unchanged private report. Hash/config mismatch fails. Reports are not approval records.
- `plans --json` lists saved rotating batches. `actions --plan DIGEST --json` projects compact metadata and Go-built argv suggestions without mutating the full plan. `analyze` refreshes selected evidence, saves a new digest, and never executes suggestions. See [analysis/approval contract](advisory-analysis.md).
- `apply --plan DIGEST --project ID --approve-plan DIGEST --json` is the privileged, explicit one-project operation. A host must obtain consent first; this flag is not a substitute for its consent UI.
- `--non-interactive` fails immediately if exact per-project approval is absent. Cache apply supports the same flags in its separate report namespace. Source-only preservation can retain removal-blocked checkouts only through the narrow deterministic source-safe allowlist; it never clears removal blockers.
- Apply returns a `Receipt` with approved timestamp, actual steps/outcomes, verified recovery commit/ref, private bundle checksum/path when applicable, quarantine path if interrupted, and before/after free bytes.
- `restore` reads a private receipt, verifies bundle SHA-256, and restores to a new destination without contacting the remote.
- `recover --plan DIGEST --project ID` restores a journaled interrupted quarantine without overwriting anything or rewinding conversation history.
- `status --json` gives watcher timestamps, latest report ID, pressure/free space, and a non-sensitive warning. Hosts can detect staleness from `checked_at`.
- `doctor --json` returns prerequisites and key presence only, never the key.

IDs are SHA-256 based (16 hex path identity, 64 hex full serialized-plan digest). The hash is an integrity/change detector, not an authentication MAC. The local user is the authority. Changes to config require a fresh report. Each selected project is re-inspected; approval is not transferable to a path with new source state.

## Recovery semantics

Temporary Git index + raw blob hashing avoid user's staging area, hooks and clean filters. Recovery commits descend from current HEAD and are pushed to a dedicated branch without force. Exact remote ref readback is required. Full-clone bundles include all refs, HEAD and recovery; a fresh bare repository verifies no missing prerequisites. Linked worktrees keep their common Git storage and use native Git lifecycle. Sensitive unknown ignored state blocks removal. Receipt persistence and a synced audit event precede removal.

Unreachable/reflog-only objects and local Git settings are outside the v1 bundle guarantee. A local bundle is recovery, not an off-device backup; losing the disk can lose unpublished refs not on the recovery branch. The roadmap includes encrypted remote archives and provider-native retention.

Claude session continuity has a separate [transaction contract](claude-handoff.md): immutable session-file/destination identities, synced private backups, journaled native Git move/clone rename, zero-turn native binding clear and independent ID resume, fresh source/private/activity/session checks, then removal. Plans and receipts expose optional `session_handoffs`; older plans cannot silently gain an adapter. Unknown provider schemas stay blocked, while verified read-only provider scopes avoid unrelated global vetoes. No native transcript contents or private paths are included in Pi's inventory.

## Budgets and operational behavior

Discovery is depth bounded with a 10,000-project hard cap and five-minute cooperative scan budget, with bounded Git/du subprocesses. Inspection rotates through the discovered project set using a private cursor, so a per-audit project/time limit does not repeatedly starve later roots. Partial discovery/inspection produces explicit warnings; unresponsive filesystem syscalls can exceed a cooperative deadline. Nested-repository inspection is capped at 250,000 entries/project. A busy or opaque project is reported, not forced through. The default monitor checks disk pressure every minute, audits hourly under pressure or daily otherwise, and does not run paid AI. Opt-in AI is at most once per day, four requests, 60-second worker/75-second parent deadline, without retries/cache warming. Logs rotate at five 10-MiB files; report retention is 50. Bundles/receipts require explicit human retention management.

## Design sources

- [Official Pi SDK](https://github.com/earendil-works/pi/tree/main/packages/coding-agent) and the pinned package's SDK declarations/examples: explicit tools, resources, model runtime, and in-memory sessions.
- [Git worktree](https://git-scm.com/docs/git-worktree), [Git bundle](https://git-scm.com/docs/git-bundle), [Git commit-tree](https://git-scm.com/docs/git-commit-tree): native lifecycle and independently verifiable recovery.
- [Apple launchd guidance](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html) and [systemd.service](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html): user-native scheduling.

These sources support the mechanisms, not a claim that any tool can guarantee safe deletion under hostile concurrent mutation.
