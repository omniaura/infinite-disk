# Infinite Disk contributor rules

- Default commands and the native monitor are report-only: no Git writes, pushes, or project deletion without explicit per-project approval.
- Pi is advisory and receives metadata-only inventory. Never give it filesystem, shell, Git-write, approval, or deletion tools; disable resource/extension discovery.
- The Go safety engine owns path validation, active-use checks, secret/private-state detection, immutable plan revalidation, preservation and remote verification.
- Fail closed on unknown activity, symlinks, changed state, unsupported managed worktrees, incomplete preservation, or uncertain credentials. Do not add bypass flags.
- Never log keys, source contents, diffs, authenticated remote URLs, or raw provider/command errors.
- Tests use temporary fixture repositories and bare local remotes, never the user's real projects. Paid-provider smoke tests are explicit, bounded, and never part of ordinary tests.
- Verify with `go test -race ./...`, `go vet ./...`, `npm test --prefix agent`, and the CLI integration tests. Keep scheduler installation/removal reversible and report-only.
- Keep generated binaries, dependencies, test data, credentials and investigation notes out of Git. Use conventional commits and a reviewed PR; main requires CI.
