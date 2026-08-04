# Release Gate: Playwright Chromium cold-cache timeout hardening

Bead: `ga-xb94fi`
Source bead: `ga-bt679z`
Deploy branch: `deploy/ga-xb94fi-gate`
Deploy source: `de92acf33a5a4782ecbda75fa875bf75f06acf39`

Result: PASS

Note: the deploy bead description still names the stale pre-rebase source
`ebd23371b`. The bead notes contain a verified builder handoff updating the
deploy source to `de92acf33a5a4782ecbda75fa875bf75f06acf39`; this gate was run
against that updated source.

## Evaluation Order

Criterion 6 was evaluated first per deployer instructions.

- `git fetch origin main`: PASS.
- `git rev-parse origin/main`: `7e6ad17b311ba3776b4273471d0b51c70e8a6863`.
- `git merge-base origin/main de92acf33a5a4782ecbda75fa875bf75f06acf39`:
  `cdb1b4260f962519b2313a3e495a1cc158f893e2`.
- `git merge-tree --write-tree origin/main de92acf33a5a4782ecbda75fa875bf75f06acf39`:
  `b9bb2cb2aa3f4adde5b000c2a1da8b07a5e3743d`, with no conflict diagnostics.

No bounded self-rebase was needed.

## Scope

Commit set:

- `868cccd19` - `fix(ci): harden Playwright Chromium install against cold-cache timeouts`
- `de92acf33` - `chore(cipolicy): update pinned CI execution hash for Playwright cache fix`

Changed paths:

- `.github/workflows/ci.yml`
- `scripts/cipolicy/policy.go`

`git diff --stat origin/main...HEAD` reports exactly 2 files changed, 23
insertions, 5 deletions.

## Gate Checklist

| # | Criterion | Status | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | Evaluated first. `git merge-tree --write-tree origin/main de92acf33a5a4782ecbda75fa875bf75f06acf39` returned tree `b9bb2cb2aa3f4adde5b000c2a1da8b07a5e3743d` with no conflicts. |
| 1 | Review PASS present | PASS | `bd show ga-bt679z` contains `Reviewer re-review verdict: PASS`; source bead `ga-bt679z` is closed with reason `pass`; deploy bead `ga-xb94fi` records reviewer PASSED status. |
| 2 | Acceptance criteria met | PASS | `ci.yml` splits Playwright cache restore/save, uses `actions/cache/restore` before install and `actions/cache/save` with `if: always()` after install, preserves the same pinned cache SHA and key, wraps install in a 3-attempt retry with 10s/20s backoff, raises install timeout from 5 to 12 minutes, and updates the CI policy execution hash. Follow-up `ga-8e0ukr` tracks the out-of-scope cache-quota root cause. |
| 3 | Tests pass | PASS | `python3 yaml.safe_load` on `.github/workflows/ci.yml` PASS; `go test ./scripts/cipolicy/...` PASS; `go test ./scripts/... -run TestPushOwnershipGuard -v` PASS; `make test-ci-policy` PASS; `go build ./cmd/gc/` PASS; `make test-fast-parallel` PASS (`All fast jobs passed`); `go vet ./...` PASS. |
| 4 | No high-severity review findings open | PASS | Reviewer notes say all done-when criteria are met with no outstanding blockers; `bd search "ga-bt679z HIGH"`, `bd search "ga-xb94fi HIGH"`, and `bd search "Playwright Chromium high severity"` returned no matching issues. |
| 5 | Final branch is clean | PASS | Before committing this gate file, `git status --short --branch` showed only `?? release-gates/ga-xb94fi-playwright-chromium-cache-timeout-gate.md`; final clean status is rechecked after the gate commit before push. |
| 7 | Single feature theme | PASS | The two-commit set touches one subsystem/theme: Dashboard SPA CI Playwright Chromium install hardening and the corresponding CI policy hash update. |

## Manifest Note

`docs/PROJECT_MANIFEST.md` is not present in this worktree, so no additional
repo-specific release criteria were available from that path. The gate used the
deployer release criteria and the repo testing guidance in `TESTING.md`.
