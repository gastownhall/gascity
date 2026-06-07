# Release gate - fingerprint versioning and engdocs (ga-qqguy0 / ga-s760.4)

**Verdict:** PASS

- Deploy bead: `ga-qqguy0` (review handoff for `ga-s760.4`)
- Source bead: `ga-s760.4` - Engdocs: Fingerprint upgrade-compatibility contract
- Stacked dependency included in branch: `ga-s760.1` - Hash versioning + drift-check upgrade-compat
- Branch evaluated: `deployer/ga-qqguy0-gate`
- Reviewed source branch: `fork/builder/ga-s760-4-1`
- Final commits over `origin/main`:
  - `a0e938ad1` - test(runtime,reconciler): hash versioning + silent rebaseline (ga-s760.1)
  - `61e6e4b36` - feat(runtime,reconciler): version stored fingerprints and silently rebaseline legacy hashes (ga-s760.1)
  - `6cc405a64` - docs(architecture): document fingerprint versioning contract

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS verdict in bead notes | PASS | `ga-qqguy0` notes contain `Review Verdict: PASS` from `gascity/reviewer`; reviewed commits are `a0e938ad1`, `61e6e4b36`, and `6cc405a64`. |
| 2 | Acceptance criteria met | PASS | `ga-s760.1`: `runtime.FingerprintVersion` is the single version constant; `ConfigFingerprint`, `CoreFingerprint`, and `LiveFingerprint` emit `v1:<sha256>`; legacy or version-mismatched stored hashes rebaseline through `silentRebaselineSessionHashes`; same-version real drift still drains. `ga-s760.4`: the Fingerprint Versioning section is present in current `engdocs/architecture/session.md`, with the v1 changelog, PR review checklist, bump/not-bump rules, and See Also link. The original `agent-protocol.md` target was stale; `session.md` is the current architecture doc for this surface. |
| 3 | Tests pass on final branch | PASS | `git diff --check origin/main..HEAD`; focused runtime/reconciler/docsync tests; `make test-fast-parallel`; and `go vet ./...` all passed. Full command list below. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list three INFO findings only; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | After the gate file was committed, `git status --porcelain=v1` was empty. This commit was amended only to record that evidence. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` completed with tree `ffd46f718b2b3beae7a069117a14e707877f1190` and no conflicts. |

## Acceptance details

### `ga-s760.1` - runtime and reconciler compatibility

- New stored hashes carry the current version prefix from
  `runtime.FingerprintVersion`.
- `runtime.IsLegacyOrMismatchedVersion` classifies unversioned hashes and
  different-version hashes as compatibility rebaseline cases.
- Core-drift, live-drift, and asleep named-session drift paths call the
  silent-rebaseline helper before any same-version drift handling.
- `silentRebaselineSessionHashes` writes `started_config_hash`,
  `started_live_hash`, `live_hash`, and `core_hash_breakdown`, then updates the
  in-memory session metadata copy.
- Same-version hash mismatches continue down the existing config-drift drain
  path.

### `ga-s760.4` - architecture documentation

- `engdocs/architecture/session.md` now has a `## Fingerprint Versioning`
  section between `## Testing` and `## Known Limitations`.
- The section documents the versioning model, silent-rebaseline behavior,
  version bump rules, non-bump cases, PR review checklist, and `v1` changelog.
- The See Also footer links back to the new Fingerprint Versioning section.
- The doc verification date is refreshed to `2026-05-16`.

## Validation

- `git diff --check origin/main..HEAD` - PASS
- `go test ./internal/runtime -run 'TestFingerprintVersionedOutputFormat|TestIsLegacyOrMismatchedVersion' -count=1` - PASS
- `go test ./cmd/gc -run 'TestReconcilerSilentRebaseline|TestReconcilerStillDrainsOnSameVersionRealDrift' -count=1` - PASS
- `go test ./test/docsync -count=1` - PASS
- `make test-fast-parallel` - PASS (`All fast jobs passed`)
- `go vet ./...` - PASS

## Push target

`git push --dry-run origin HEAD` succeeded, so `PUSH_REMOTE=origin` for this
release branch.
