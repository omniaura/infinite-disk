# Changelog

## Unreleased

- Checkpointed 100-project scan batches, explicit bounded multi-batch runs, and
  streaming JSON Lines for rolling scan/triage/review workflows.
- Reuse discovery indexes for 15 minutes with explicit partial-coverage warnings
  and refresh; never
  reuse source/activity/session safety decisions. Bound breadth-first discovery
  separately so it cannot consume the entire inspection budget.
- Add phase/count progress, per-project time limits, compact AI batches, exact
  project triage, sanitized inference failure stages, and usable report output
  even when AI fails. Deduplicate nested measured footprints in summaries.

## 0.1.0

Initial approval-first CLI: report-only disk watcher, bounded project/worktree discovery, optional metadata-only Pi triage, per-project approval, verified Git recovery, standalone-clone bundle restore, native macOS/Linux scheduling, private bounded audit state, and fixture-only destructive tests.

V1 intentionally refuses unsupported/private-state cases. Native agent lifecycle adapters, notifications/tray, and Ditto Desktop integration are roadmap work.
