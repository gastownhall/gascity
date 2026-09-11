# Release Gate: ga-n17721

Feature branch: `docs/dolt-mode-safe-troubleshooting-clean`

Head before gate commit: `5144cfc4cde3bc792f4c7407040f2d536e7089ac`
Base checked: `origin/main` at `41c54dcddc241e5f7bdea6f9475efd194bdb6e93`

**Rebase note (2026-06-25):** Branch rebased onto `f2f6a5cf9` (current main tip)
to resolve conflicts introduced when PR #3725 (`ca1dbf00a`) landed the same
`dolt_mode_safe` section with more accurate content. All three docs commits were
automatically dropped as already-upstream during rebase. This branch now only
adds this gate file. The docs content tracked by ga-yqn5py.4, ga-vpvml7, and
ga-kzp5e4 is live in main via PR #3725.

## Scope

Single-bead deploy for `ga-n17721`: publish the `dolt_mode_safe`
troubleshooting docs update.

Source beads:

| Bead | Status | Evidence |
| --- | --- | --- |
| `ga-yqn5py.4` | Closed | Acceptance criteria define the troubleshooting subsection and repair path. |
| `ga-vpvml7` | Closed | Reviewer verdict PASS; one LOW finding filed for JSON field paths. |
| `ga-kzp5e4` | Closed | LOW finding fixed by adding the `.beads` parent path. |

## Checklist

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | PASS | `ga-vpvml7` notes contain `Reviewer Verdict: PASS`. |
| 2 | Acceptance criteria met | PASS (via PR #3725) | Docs content landed in main via PR #3725 (`ca1dbf00a`). That PR added the `dolt_mode_safe` section with reviewed, accurate content. The three docs commits from this branch were dropped during rebase as already-upstream. |
| 3 | Tests pass | PASS | `make check-docs` passed: `ok github.com/gastownhall/gascity/test/docsync 2.027s`. Docs-only change; no Go code or generated docs changed. |
| 4 | No high-severity review findings open | PASS | Review notes contain one LOW finding only; follow-up bead `ga-kzp5e4` is closed with the fix. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before writing this gate file in the isolated deploy worktree. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base origin/main origin/docs/dolt-mode-safe-troubleshooting-clean) origin/main origin/docs/dolt-mode-safe-troubleshooting-clean` reported `merged` with no conflict markers or unmerged paths. |
| 7 | Single feature theme | PASS | After rebase, commit set is this gate file only; the docs content is already in main via PR #3725. |

## Acceptance Evidence

- Added symptom text for `native_store_unavailable gate=dolt_mode_safe` after `gc start`.
- Included the exact `preflight_checker.go:186` fallback reason from current `origin/main`.
- Pointed `gc status --json` users at `jq .beads` for `preflight_gate` and `native_store_eligible`.
- Described the root cause as missing Dolt server mode in `.beads/config.yaml` after upgrade.
- Documented both automatic repair (`gc doctor --fix`, `gc restart`) and the manual `dolt.mode: server` config repair.
- The docs recommend server-mode repair and do not recommend changing or bypassing `checkDoltModeSafe`.

## Notes

The deployer role prompt references `docs/PROJECT_MANIFEST.md` for release
criteria, but that file is not present on this branch. This gate applies the
release criteria from the deployer prompt.
