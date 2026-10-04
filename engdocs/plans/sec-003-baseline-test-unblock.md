# SEC-003 Baseline Test Unblock

Owner: `gascity/pm`
Created: 2026-06-09
Source beads: `ga-7psli2`, `ga-x2i1lv.2`

## Goal

Unblock the SEC-003 clean-branch deploy without bundling unrelated baseline
test work into the SEC-003 release unit.

## Context

The deployer failed `ga-7psli2` on the standard release gate for
`release/sec-003-ga-x2i1lv` in `/home/jaword/projects/beads`.

SEC-003 candidate evidence:

- Base: `9a1c88b63aee89b091c9db7e5330a48cb4911987`
- Head: `17c63de541f3ee16147a904f79634c4ec53555f4`
- Diff: `internal/beads/context.go` only, `+6/-1`
- Passing checks before the blocker: build, vet, targeted SEC-003 test, and
  HOME-mismatch smoke
- Failing check: `make test` exits 2 in
  `cmd/bd TestOutputContextFunction CLI_Stealth` with unexpected `git status`
  text

The deployer also reported that the same focused failure reproduces on clean
`origin/main` at `9a1c88b63aee89b091c9db7e5330a48cb4911987`, so this is an
unrelated baseline blocker. SEC-003 should wait until the baseline blocker is
fixed or otherwise cleared, then deploy as a clean single-feature release.

## Work Packages

| Bead | Route | Label | Acceptance focus |
| --- | --- | --- | --- |
| `ga-7psli2.1` | `gascity/builder` | `ready-to-build` | Resolve or objectively clear the baseline `cmd/bd` `CLI_Stealth` failure without bundling SEC-003. |
| `ga-7psli2.2` | `gascity/validator` | `needs-tests` | Independently validate the blocker resolution and confirm no SEC-003 contamination. |
| `ga-7psli2.4` | `gascity/builder` | `ready-to-build` | Resolve or objectively clear the remaining baseline `cmd/bd make test` failures surfaced after `CLI_Stealth` validation. |
| `ga-7psli2.5` | `gascity/validator` | `needs-tests` | Independently validate the remaining baseline gate blocker resolution before deploy retry. |
| `ga-7psli2.3.1` | `gascity/deployer` | `needs-deploy` | Gate and open the separate baseline `cmd/bd` blocker-fix PR without bundling SEC-003. |
| `ga-7psli2.3.2` | `gascity/validator` | `needs-tests` | Confirm the baseline fix is available on `origin/main` or an accepted current SEC-003 baseline before retry. |
| `ga-7psli2.3` | `gascity/deployer` | `needs-deploy` | Retry the SEC-003 release gate only after the baseline blocker is cleared. |

## Dependency Graph

```text
ga-7psli2.1 -> ga-7psli2.2 -> ga-7psli2.4 -> ga-7psli2.5 -> ga-7psli2.3.1 -> ga-7psli2.3.2 -> ga-7psli2.3 -> ga-x2i1lv.2
```

The original clean-branch deploy bead remains blocked by the retry bead so the
SEC-003 PR cannot be opened while the unrelated baseline failure is unresolved.

## Acceptance Summary

1. Builder proves the `cmd/bd TestOutputContextFunction CLI_Stealth` blocker is
   independent of SEC-003 and resolves it as a separate release unit, or records
   an objective no-code resolution.
2. Validator confirms the focused stealth coverage no longer leaks
   `git status` text and that the blocker fix does not include the SEC-003
   home-directory change.
3. Builder resolves or objectively clears the remaining baseline `cmd/bd`
   `make test` failures reported by `ga-7psli2.2`, keeping that work separate
   from SEC-003.
4. Validator confirms the remaining baseline gate failures are cleared and the
   fix contains no SEC-003 behavior changes.
5. Deployer reruns the SEC-003 gate from a current clean `origin/main` base
   after the baseline blocker is cleared.
6. Deployer opens and routes a PR only if the standard gate passes; on failure,
   deployer records exact failed criteria and artifact path and routes back to
   PM.

## 2026-06-09 Follow-up Split

Validator completed `ga-7psli2.2` with the focused `CLI_Stealth` blocker
cleared, but the standard gate still failed on unrelated baseline `cmd/bd`
tests:

- `TestAutoExportGitAddFailureExitsNonZero`
- `TestAutoExportSkipsEmptyExportOverPopulatedJSONL`
- `TestAutoExportSkipsWhenExistingJSONLHasIDsMissingFromStore`
- `TestInitNonInteractiveAutoExportDefaultOffAndOptIn`
- `TestCommitBeadsConfigSkipsGitHooks`

PM created `ga-7psli2.4` and `ga-7psli2.5` so the SEC-003 deploy retry remains
blocked until the remaining baseline gate failures are resolved and validated.

## 2026-06-09 Release Sequencing Update

Validator completed `ga-7psli2.5` with the remaining baseline `cmd/bd` blockers
cleared on `fix/cmd-bd-baseline-test-blockers`:

- Base at validation time: `9a1c88b63aee89b091c9db7e5330a48cb4911987`
- Head: `9678f4535a053b82a7b0d55d22aa48f0495f12d5`
- Diff scope: `cmd/bd/doctor_context_test.go`, `cmd/bd/prime_test.go`,
  `cmd/bd/test_helpers_pure_test.go`
- Validation artifacts:
  `/home/jaword/projects/gc-management/.gc/artifacts/ga-7psli2.5-focused-cmd-bd.log`
  and
  `/home/jaword/projects/gc-management/.gc/artifacts/ga-7psli2.5-make-test.log`

The fix is validated but not yet confirmed available on `origin/main`, so PM
added two blockers before SEC-003 can be retried:

- `ga-7psli2.3.1`: deployer gates and opens the separate baseline blocker-fix
  PR/release unit.
- `ga-7psli2.3.2`: validator confirms the baseline fix is actually available on
  `origin/main` or an accepted current SEC-003 baseline and still excludes
  `internal/beads/context.go`.

`ga-7psli2.3` is retargeted to `gascity/deployer` with `needs-deploy` and
depends on `ga-7psli2.3.2`. The original deploy bead `ga-x2i1lv.2` remains
blocked by `ga-7psli2.3`.

## Tracker Import

No `tracker-to-beads` skill is installed in this PM worktree, so tracker import
is a no-op for this package.
