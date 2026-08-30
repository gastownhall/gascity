# Release gate: ga-xxh2cz — source-derived build time

**Deploy bead:** `ga-xxh2cz`  
**Source bead:** `ga-wf71cz`  
**Build bead:** `ga-vx62cc`  
**Reviewed commit:** `279316bde0bbdbe3c29ac762a0b9fb14cfcbced9`  
**Source branch:** `builder/ga-vx62cc` (provenance only)  
**Deploy branch:** `deploy/ga-xxh2cz-gate`  
**Base:** `origin/main` at `f25eec9381331c6e747c4f565b51baf2cf152316`  
**Evaluated:** 2026-08-18 (America/Los_Angeles)

**Verdict:** **FAIL — WAIVED; no candidate-owned failure**

The patch replaces wall-clock `BUILD_TIME` with the immutable HEAD commit date
and adds a deterministic source-level regression test. The required full-union
sweep preserved four unrelated failures across four jobs. All four have
independent ownership evidence and no plausible call path from the two-file
provenance diff, so they are waived without converting the recorded sweep to
green.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `gascity/reviewer` independently re-reviewed exact commit `279316bde0bbdbe3c29ac762a0b9fb14cfcbced9` in round 2 and sent durable PASS mail `gm-wisp-lyrolh`. |
| 2 | Acceptance criteria met | PASS — 2 explicit waivers | `TestBuildTimeIsSourceDerivedNotWallClock` proves the value is derived from `git show --format=%cI HEAD`, rejects wall-clock derivation, and requires the non-git fallback. `TestBuildStampsWorkingTreeDirtiness` preserves the dirty marker. Mayor mails `gm-wisp-2rpri4` and `gm-wisp-o7ixlp` explicitly waive a live linker/cache timing test and the pre-existing `gc version --long` coverage gap; both mails were independently peek-verified. |
| 3 | Required tests pass | **FAIL — WAIVED** | `Makefile` is a `shared` path in `.github/workflows/ci.yml`, so `make test-local-full-parallel` ran the 40-job full union. Result: 36 PASS, 4 FAIL. The four failures are attributed below; none is candidate-owned. Focused provenance, policy, build, vet, format, and lint gates passed. |
| 4 | No HIGH-severity review findings open | PASS | Round-2 review reports no blockers, majors, minors, style findings, or security findings. |
| 5 | Final branch is clean | PASS | The exact reviewed commit had a clean worktree before this gate record; `git diff --check origin/main...HEAD` passed. |
| 6 | Branch diverges cleanly from main | PASS | The candidate is 3 commits ahead and 2 behind current main. `git merge-tree --write-tree --messages origin/main HEAD` completed without conflict and produced tree `49f615beebcadf47d15ce03c19763bb1593f875b`. |
| 7 | Change is cohesive and reviewable | PASS | The branch-wide diff is one Makefile assignment plus one 41-line regression test in `scripts/build_provenance_test.go`: 42 insertions, 1 deletion, one build-provenance theme. |

## Focused and static evidence

- `go test -count=1 -v ./scripts/...`: PASS, including
  `TestBuildTimeIsSourceDerivedNotWallClock` and
  `TestBuildStampsWorkingTreeDirtiness`.
- `make -pn` resolved `BUILD_TIME` to
  `2026-08-18T17:37:13-07:00`, exactly matching `git show -s --format=%cI
  HEAD`.
- `make build`: PASS. `./bin/gc version --long` reported
  `dev (commit: 279316bde0, built: 2026-08-18T17:37:13-07:00)`.
- `go build ./...`: PASS.
- `go vet ./...`: PASS.
- `make test-ci-policy`: PASS.
- `make fmt-check-changed`: PASS; no changed existing Go files.
- `make lint-affected`: PASS; no affected lint paths selected.
- `git diff --check origin/main...HEAD`: PASS.

## Required full-union evidence

```text
PATH=/var/tmp/gc-gate-ga-f8it32-tools/bin:$PATH \
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true TMPDIR=/var/tmp LOCAL_TEST_JOBS=4 \
make test-local-full-parallel
```

Result: **36 jobs passed, 4 jobs failed**. Logs are under
`/var/tmp/gc-local-tests.c8zBvw`.

### Waived, non-candidate failures

1. `cmd-gc-process-4-of-6` —
   `TestCityRuntimeForceShutdownTearsDownAfterLateAsyncSweep` missed the
   late-started runtime. `ga-550z2h` contains a forced reproduction, confirmed
   test-only root cause, validated fix, and prior occurrence on this exact
   Makefile BUILD_TIME feature. A focused `-count=20` rerun reproduced twice,
   consistent with that tracker. The candidate touches neither `cmd/gc` nor
   lifecycle code.
2. `integration-packages-runtime-tmux-2-of-3` and `-3-of-3` —
   `TestGetKeyBinding_CapturesDefaultBinding` and its `WithArgs` sibling saw an
   empty host tmux 3.7b default key table. This host-specific signature is
   tracked by `ga-afqddr`; the candidate touches no runtime/tmux path.
3. `integration-rest-full-1-of-8` — `TestCleanInstallTutorialPath` received a
   circuit-breaker cleanup diagnostic before the expected issue prefix on
   stdout. `ga-hrdd3h` tracks the current recurrence; `ga-rsktma` records the
   mayor's signature-specific attribution ruling for this machine-state
   trigger. The candidate neither invokes nor changes beads output handling.

There were no candidate-owned failures. These waivers preserve the literal
FAIL result and do not weaken or delete any test.

## Disposition

- Proceed from the isolated `deploy/ga-xxh2cz-gate` branch only.
- Open a pull request from that branch; never push or open from
  `builder/ga-vx62cc`.
- Route merge authority to mayor/mpr. The deployer does not merge.
