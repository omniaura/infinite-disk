# Suggestions are not authority

Infinite Disk separates **observation → suggestion → explicit approval → revalidation → execution → receipt**. The monitor and Pi never cross the approval boundary. An authorized external agent can use the same trusted-user CLI as a person, but must obtain action-specific authority first; opaque IDs and digests are not OS authentication.

## Read-only workflow

1. `scan --json` discovers a bounded rotating batch and automatically analyzes metadata for each inspected project. No source, refs, index or remote is changed.
2. `plans --json` lists saved report digests; `latest` is only one rotating batch. Reports overlap, so their bytes cannot be summed as total reclaimable disk.
3. `actions --plan DIGEST --filter needs-analysis --limit 20 --json` is compact: `version`, `mode:suggestions-only`, `plan_id`, `total`, `matching`, `returned`, `warnings`, `projects`. Each project has ID/path/size, removal eligibility, blockers, analysis, optional Pi advice, and known CLI command argv.
4. `analyze --plan DIGEST --project ID --json` refreshes that project's safety/source/session facts and saves a **new full immutable plan**. Omitted projects remain unchanged; the original creation time/24-hour expiry remains. `--filter`/`--limit` select largest-first. Analysis is bounded by five minutes and the same operation lock. Cancellation does not save a partial analysis as a new plan.
5. `--agent` explicitly opts into bounded paid Pi prioritization. Pi sees a projection of opaque metadata only, not filenames, ignored paths, source/diffs, remote URLs, branch names or transcript content. `inspect_project` cannot perform arbitrary reads; `suggest_action` only accepts offered kinds for an inventoried project and returns `approved:false, executed:false`.

Missing source checks are **unknown**, not zero changes. Missing local origin-tracking refs are **unknown**, not zero unpublished commits. The local HEAD/tracking comparison never fetches and cannot prove current remote durability. Ignored examples (maximum 25) distinguish private names, agent state, dependency/build candidates and unknown data; directory entries do not prove the contents are regenerable. Kondo remains the artifact catalog, independently checked before any removal. Current source fingerprints/secret checks are conservative, not comprehensive scanning of already committed history.

`actions --all-plans --filter ready --json` reviews the saved batches together. It deduplicates by path **before filtering** so a newer blocked observation hides an older ready result. Each project retains its own `plan_id`, observation timestamp, exact approval argv and original expiry; there is no synthetic combined approval digest and `approval_granted` stays false. This is historical coverage, not a fresh coherent snapshot. Parent/child paths can overlap, so no summed savings total is emitted. Use each offered `analyze` command for fresh evidence; `--all-plans` cannot be combined with a single `--plan` and is deliberately not an apply/analyze batch-approval flag.

## Approval and supported actions

`commands` contains argv arrays built by Go, never shell text from a model. Preserve argv boundaries, including paths containing spaces. An executable suggestion is still not approved. Unsupported suggestions have no destructive command.

| Suggestion | Operation | Gate |
| --- | --- | --- |
| `preserve_source` | `apply --preserve-only` | Safe checked source/Git identity and known idle activity; exact approval; verified recovery push/readback; checkout, ignored data and sessions stay |
| `remove_checkout` | `apply` | All removal blockers resolved, supported session continuity; exact approval; preservation and fresh final checks |
| `inspect_artifacts` | `caches scan` | Read-only Kondo dry-run; its own cache report does not approve deletion |
| `private_backup` | Manual private backup prerequisite | No generic adapter; never push ignored data as source |
| `native_session_handoff` | Provider-native prerequisite | Unsupported adapters stay blocked, never raw-delete their metadata |
| `keep`, `verify_activity`, `resolve_blockers` | Observation/manual prerequisites | No write or approval authority |

Source-only preservation permits just the explicit removal-only blocker allowlist: unknown ignored data, recent source activity, missing native session handoff or incomplete session coverage. It still requires a complete source fingerprint/secret preflight, session inspection, valid origin and Git identity, and fresh inactivity checks. Any other blocker rejects it. It never reclassifies removal eligibility, changes the user's branch/index, backs up private ignored data or rewrites sessions. HEAD ancestry is on the recovery branch; full clone refs are in the separately verified local bundle, **not all uploaded**. Unreachable/reflog-only objects and local Git settings remain excluded from that bundle; preserve-only keeps their original database. Clone removal separately refuses unpreserved unreachable history or an inconclusive execution-time integrity check.

For trusted noninteractive callers:

```sh
infinite-disk apply --plan DIGEST --project ID --approve-plan DIGEST --non-interactive --preserve-only --json
infinite-disk apply --plan DIGEST --project ID --approve-plan DIGEST --non-interactive --json
infinite-disk caches apply --plan CACHE_DIGEST --project ARTIFACT_ID --approve-plan CACHE_DIGEST --non-interactive --json
```

These commands are privileged execution requests. A caller must not treat a returned command, model recommendation or `executable:true` as consent. No `--yes-all` or model-to-apply edge exists. Wrong project/plan, config/source/remote changes and expiration fail closed. `--non-interactive` with missing explicit approval fails immediately rather than waiting for stdin. An explicitly targeted blocked action returns a nonzero exit rather than misleading `[]` success.

## Results and errors

Successful CLI JSON stays on stdout. The actual executable emits versioned error JSON on stderr when `--json` is set: `{version:1,error:{code,message}}`. Codes include `operation_in_progress`, `cancelled` and `command_failed`; cancellation exits 130, other errors exit 1. For operation contention, inspect `status --json` and retry after the audit; do not stop unrelated coding agents or disable the monitor automatically.

Apply can have partial side effects before failing (for example, a successful recovery push followed by failed readback). Consult its stdout receipts and private durable receipt, not just exit status. Estimated candidate bytes are not guaranteed net space: nested repositories/paths overlap, APFS clones/snapshots affect allocation, and local backups cost space. Measure current disk free bytes and disclose concurrent activity. Local backups are not an off-device archive; none expire silently.
