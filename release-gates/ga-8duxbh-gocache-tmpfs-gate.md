# Release gate: AGENTS.md GOCACHE tmpfs guidance

Bead: ga-8duxbh
Source bead: ga-lqzg77
Review bead: ga-mjy308
Original reviewed commit: b751bb6ab4f1d20088e42ec3c0a3a540b9c3b959
Clean deploy commit before this gate file: 5452a1ade
Branch: deploy/ga-8duxbh-gocache-tmpfs
Base: origin/main at 85319eb60

## Summary

This gate evaluates the reviewed documentation fix that removes the
`AGENTS.md` cold-build guidance that sent explicit `GOCACHE` values to `/tmp`.
The builder branch `gc-builder-2-91e0bf41098b` was not directly reviewable
against current `origin/main`; its branch diff carried broad unrelated history.
The reviewed one-file commit was cherry-picked cleanly onto a fresh branch from
current `origin/main`, producing commit `5452a1ade`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-mjy308` is closed with `REVIEWER VERDICT: PASS`. |
| 2 | Acceptance criteria met | PASS | `AGENTS.md` no longer contains `GOCACHE=$(mktemp`; repo no longer contains `Safe alternative for cold builds`; `CLAUDE.md` is unchanged from `origin/main`; `AGENTS.md` retains the `go clean -cache` hard ban and `go clean -testcache` exception; new guidance names `/var/tmp`, uses `trap 'rm -rf "$tmp"' EXIT`, and sets both `GOCACHE="$tmp"` and `TMPDIR="$tmp"`. |
| 3 | Tests pass | PASS | `TMPDIR=/var/tmp/g8.Dv2Iuv make test-fast-parallel` passed all fast jobs. `TMPDIR=/var/tmp/gascity-gate-ga-8duxbh-build.* make build` passed. `TMPDIR=/var/tmp/gascity-gate-ga-8duxbh-vet.* go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Review notes contain no HIGH findings and ended in PASS. |
| 5 | Final branch is clean | PASS | `git status --short --branch` clean before writing this gate file; final status rechecked after committing gate. |
| 6 | Branch diverges cleanly from main | PASS | Final branch was cut from current `origin/main` (`85319eb60`), then the reviewed one-file patch was cherry-picked cleanly. |
| 7 | Single feature theme | PASS | The final diff touches only `AGENTS.md` and is limited to build-cache guidance for avoiding tmpfs exhaustion. |

## Test log notes

An earlier `make test-fast-parallel` run used a verbose TMPDIR path under
`/var/tmp/gascity-gate-ga-8duxbh-test-persist.MRmkqy`; it failed because several
Unix socket tests exceeded the platform path length, with `bind: invalid
argument` and one explicit `sockPath(...) exceeds limit 100`. The gate was
rerun with the short disk-backed TMPDIR `/var/tmp/g8.Dv2Iuv`, and all fast jobs
passed.

## Final diff

```text
M AGENTS.md
```
