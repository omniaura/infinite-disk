# Security policy

Report vulnerabilities privately through [GitHub private vulnerability reporting](https://github.com/omniaura/infinite-disk/security/advisories/new). Do not include credentials, private source, or personal paths in public issues. The latest release is the supported version; this is early-stage software, with no security certification or recovery guarantee.

## Boundaries

The model is untrusted and advisory. Only deterministic Go code owns scope checks, approval validation, Git preservation, and removal. The Pi session has two custom metadata-only tools; it does not receive source or a shell. Model output is validated twice and cannot make an ineligible project eligible. A fresh report, exact plan digest, and explicit project approval are required for apply.

The process runs as the current user, never root, with no privilege escalation. Private state uses 0700 directories and 0600 files. Keys stay in the explicitly invoked worker's memory/environment; no key is copied to config, logs, Git, or a scheduler by installation. Raw Git/provider errors are intentionally not logged. Reports and recovery bundles remain sensitive local data.

This is **not an OS sandbox or a same-user authorization service**. Another process running as you can edit state, invoke the CLI with approval flags, inspect environment credentials, or mutate files between checks. Pi's capability restriction is an application/tool boundary, not isolation against a compromised dependency. Stop active tools before approving removal; use independent backups. Filesystem snapshots, hard links, hostile concurrent mutation, custom global Git transports, and unreachable Git objects have limitations described in the README.

Only credential-free HTTPS/`git@host` origin URLs on configured hosts are allowed. A recovery push goes to that existing origin and can expose changed source to its readers. V1 rejects several secret-shaped files/values, but does not scan all committed history. Private ignored data blocks deletion; only explicitly documented regenerable dependency directories are excepted. Native Codex archive, LFS, submodules, nested repositories, and shared-object/shallow cases are not approximated by destructive fallbacks.

## Release gates

- Fixture-only destructive integration tests; exact-HEAD CI on macOS and Linux.
- Go race tests/vet/govulncheck; locked Node install without lifecycle scripts and npm audit.
- Read-only real Pi/OpenRouter smoke before initial release; no CI provider key.
- Public source/history secret review before visibility changes.
- Required CI and conversation resolution on protected main; no force push/deletion.
- Pinned GitHub Actions, minimal job permissions, Dependabot updates.
