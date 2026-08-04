# Release Gate: push-ownership guard multi-level bead IDs

- Deploy bead: `ga-hzy30q`
- Source bead: `ga-nnjcuc.2`
- Reviewed commit: `7af0671436ba10f2e0f275e3ef627393806651ef`
- Deploy branch: `deploy/ga-hzy30q-gate`
- Evaluated: 2026-07-24
- Gate source: deployer prompt release-gate table. `docs/PROJECT_MANIFEST.md` was not present in this checkout.

## Summary

PASS. This is a single-theme shell guard fix. It changes only the push ownership guard and its shell test harness so branch names containing repeatable dotted bead IDs, such as `ga-o3ko1j.4.3`, resolve the full bead ID instead of truncating to `ga-o3ko1j.4`.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 6 | Branch diverges cleanly from main | PASS | Checked first. `git fetch origin main`; `git merge-tree --write-tree origin/main 7af0671436ba10f2e0f275e3ef627393806651ef` returned tree `5179bfeb0cd424b4a6381c18bfad45b8758ec7d7`; `git diff --check origin/main...7af0671436ba10f2e0f275e3ef627393806651ef` produced no output. |
| 1 | Review PASS present | PASS | Deploy bead `ga-hzy30q` records reviewer PASS for source bead `ga-nnjcuc.2`; source notes contain `REVIEWER VERDICT: PASS`. |
| 2 | Acceptance criteria met | PASS | Commit set is the expected red/green pair: `d877d93e5` and `7af067143`. Diff is limited to `scripts/push-ownership-guard.sh` and `scripts/test-push-ownership-guard.sh`; no `cmd/gc` or dead-assignee fallback files are included. Targeted guard suite includes the regression `resolve/branch-resolves-full-multi-level-subbead-id`. |
| 3 | Tests pass | PASS | `bash scripts/test-push-ownership-guard.sh` passed `20/20`; `shellcheck scripts/push-ownership-guard.sh scripts/test-push-ownership-guard.sh` passed with ShellCheck 0.11.0; `go build ./...` passed; `go vet ./...` passed; `make test-fast-parallel` passed all 8 fast jobs. |
| 4 | No high-severity review findings open | PASS | `bd list --status open --limit 0 | rg -i -- 'ga-hzy30q|ga-nnjcuc\\.2|HIGH|request-changes|security'` returned only sling helper bead `ga-pqt5hs`; no open HIGH/request-changes finding was found. |
| 5 | Final branch is clean | PASS | Before adding this gate file, `git status --short --branch` returned only `## deploy/ga-hzy30q-gate`. The gate file is committed as the final branch tip before push. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem: `scripts/push-ownership-guard.sh` plus its test harness. Removing this fix would only affect branch-to-bead ID resolution for push guard checks. |

## Commands

```bash
git fetch origin main
git merge-tree --write-tree origin/main 7af0671436ba10f2e0f275e3ef627393806651ef
git diff --check origin/main...7af0671436ba10f2e0f275e3ef627393806651ef
git log --oneline --reverse bac288647e0bbbbe2e68bdbe588709eb2827f5ee..7af0671436ba10f2e0f275e3ef627393806651ef
git diff --stat bac288647e0bbbbe2e68bdbe588709eb2827f5ee..7af0671436ba10f2e0f275e3ef627393806651ef
bash scripts/test-push-ownership-guard.sh
shellcheck scripts/push-ownership-guard.sh scripts/test-push-ownership-guard.sh
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go build ./...
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go vet ./...
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE make test-fast-parallel
bd list --status open --limit 0 | rg -i -- 'ga-hzy30q|ga-nnjcuc\.2|HIGH|request-changes|security'
```
