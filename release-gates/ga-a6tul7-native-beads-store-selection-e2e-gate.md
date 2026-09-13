# Release gate - native beads store selection E2E coverage (ga-a6tul7 / ga-3birkh)

Result: FAIL

## Context

- Deploy bead: `ga-a6tul7`
- Review bead: `ga-3birkh`
- Source bead: `ga-r20uo4`
- PR: https://github.com/gastownhall/gascity/pull/2640
- Branch: `builder/ga-l2souo-6-e2e`
- Head evaluated: `9f321b32c326c0d852afbe932fd4bb09b028674b`
- Base evaluated: `origin/main` at `4b19290c8ea2713c250c9cf9f073ea64236e9cc5`
- Manifest note: `docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate uses the deployer role release criteria plus `TESTING.md`.

## Release Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-3birkh` is closed with `Review Verdict: PASS`, reviewer `gascity/reviewer`, head `9f321b32c`, branch `builder/ga-l2souo-6-e2e`, PR #2640. Deploy bead `ga-a6tul7` records the same reviewed head and no blockers. |
| 2 | Acceptance criteria met | PASS | The branch contains the native Dolt store adapter, seven preflight checks, factory fallback, status JSON diagnostics, typed `bead.deleted` event registration, dashboard/API generated surfaces, and `TestOpenStoreAtForCity(EligibleNative|ContextDrift|ForceFallback)` E2E coverage. Focused selection tests passed locally. |
| 3 | Tests pass | PASS | Local gate commands passed: `GOTOOLCHAIN=auto go test ./internal/beads -run 'TestOpenStoreAtForCity(EligibleNative|ContextDrift|ForceFallback)' -count=1`; `GOTOOLCHAIN=auto make check-native-dependency-surface`; `GOTOOLCHAIN=auto go vet ./...`; `GOTOOLCHAIN=auto make test-fast-parallel`; `GOTOOLCHAIN=auto make dashboard-check`; explicit dashboard preview smoke served `http://127.0.0.1:4187/` and returned 22,831 bytes. Initial inherited `GOTOOLCHAIN=local` was Go 1.26.2 and failed before test execution, so the gate used `GOTOOLCHAIN=auto`, which resolved Go 1.26.3 as required by this branch. |
| 4 | No high-severity review findings open | FAIL | `gh pr checks 2640` reports failing security checks at this head. `Image vulnerabilities` fails with two HIGH findings in `usr/local/bin/bd`: `CVE-2026-41602` (`github.com/apache/thrift` v0.19.0, fixed in 0.23.0) and `CVE-2026-34986` (`github.com/go-jose/go-jose/v4` v4.1.3, fixed in 4.1.4). `deps.env` still pins `BD_VERSION=v1.0.4`, so the container scan packages the vulnerable bd binary even though the install script knows v1.0.5 checksums. CodeQL check run `78708764017` reports one critical and ten high annotations. Most annotations are in files outside this PR diff, but one critical annotation is in `internal/config/config.go:2915`; these remain unresolved as GitHub security check failures. |
| 5 | Final branch is clean | PASS | Before writing this gate artifact, `git status --short --branch` showed the feature branch clean and up to date with `origin/builder/ga-l2souo-6-e2e`; `git diff --check origin/main...HEAD` produced no output. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` returned 0. `git merge-tree origin/main HEAD` returned 0 and produced a synthetic tree (`5ce37c08b997dde1346bae3d6f67985734f32e8a`), with no conflicts. GitHub reports PR #2640 `mergeable=MERGEABLE`. |
| 7 | Single feature theme | PASS | The commit set is one deploy theme: native beads store selection and immediate support surfaces. Dependency/toolchain and CI changes are tied to the beads/native store dependency floor and image build for this feature; API/dashboard/schema changes expose the new status and event contracts. No independent user-facing feature is introduced. |

## Failed Gate Detail

The release gate fails on criterion 4. The builder needs to clear or formally resolve the current high/critical security findings before release:

- Update the packaged `bd` version path so container images no longer install vulnerable `bd` v1.0.4. The current branch adds v1.0.5 checksums in `.github/scripts/install-bd-archive.sh`, but `deps.env` still says `BD_VERSION=v1.0.4`.
- Re-run container scan on the PR branch and verify `Image vulnerabilities` is green.
- Resolve or obtain maintainer dismissal for the CodeQL annotations. The current check run reports 1 critical and 10 high findings; the critical annotation is `internal/config/config.go:2915` (`Potentially unsafe quoting`).

## Commands Run

```text
GOTOOLCHAIN=auto go test ./internal/beads -run 'TestOpenStoreAtForCity(EligibleNative|ContextDrift|ForceFallback)' -count=1
GOTOOLCHAIN=auto make check-native-dependency-surface
GOTOOLCHAIN=auto go vet ./...
GOTOOLCHAIN=auto make test-fast-parallel
GOTOOLCHAIN=auto make dashboard-check
GOTOOLCHAIN=auto make dashboard-smoke
npm run preview -- --host 127.0.0.1 --port 4187
curl -fsS http://127.0.0.1:4187/
git diff --check origin/main...HEAD
git merge-tree origin/main HEAD
gh pr checks 2640
gh run view 26706519281 --job 78708734697 --log-failed
gh api repos/gastownhall/gascity/check-runs/78708764017
gh api repos/gastownhall/gascity/check-runs/78708764017/annotations --paginate
```
