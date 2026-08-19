# Release gate: ga-v138pa — managed Dolt read-timeout default

**Deploy bead:** `ga-v138pa`
**Review bead:** `ga-id7z3d` (round 1), `ga-vqrdih` (round 2, this record)
**Build bead:** `ga-lfcx72`
**Round-1 reviewed commit:** `9c6ccc8537f5a5cf91b5d67f1dc33c0c55ed4cf7`
**Round-2 (this) evaluated commit:** `cf412ecdd43de3ddb3b134f42783403fe409fbbf`
**Source branch:** `builder/ga-v138pa` (provenance only)
**Evaluation/deploy branch:** `deploy/ga-v138pa-gate`
**Base:** `origin/main`
**Evaluated:** 2026-08-19 (America/Los_Angeles), builder-1

**Verdict:** **PASS — the managed shell fallback now matches the Go/config default at 120,000 ms**

## Round 2 supersedes round 1

Round 1 (`9c6ccc8537f5`, 2026-08-18) FAILed criterion 2/3: the reviewed change
raised the canonical Go/config default from 15,000 ms to 120,000 ms but did
not update the materialized `gc-beads-bd` shell fallback, so
`TestManagedDoltConfigGoWriterMatchesShellFallbackSemantics` caught a real
Go-vs-shell parity mismatch. That FAIL record is preserved below for
provenance. This round evaluates `cf412ecdd43de3ddb3b134f42783403fe409fbbf`,
which includes the required shell-fallback fix
(`examples/bd/assets/scripts/gc-beads-bd.sh`: `15000` → `120000` in both
producer sites, matching Go's existing `120000`) and closes the gap.

## Gate criteria (round 2)

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-vqrdih` (closed, verdict PASS): independently re-read the diff, re-read `config.go`'s `DefaultDoltReadTimeoutMillis` rationale, located and independently re-ran the acceptance test (PASS), OWASP walk found no new input surface, classified the timeout raise as CI-fix-integrity-clean (propagating an already-reviewed Go-side decision, not a fresh bump). |
| 2 | Acceptance criteria met | PASS | The 120,000 ms default now matches across the canonical Go/config path, generated docs, and the materialized shell fallback. |
| 3 | Required tests pass | PASS | See "Verification evidence" below — the diff-owned regression test now passes, and the two non-diff-owned failures observed in the same run are handled per criterion 3a(iii) (exact-base attribution), not waived by assumption. |
| 4 | No HIGH-severity reviewer findings open | PASS | `ga-vqrdih` recorded no blocking findings. |
| 5 | Final branch clean | PASS | Worktree clean at `cf412ecdd4` before this gate record was added. |
| 6 | Branch diverges cleanly from main | PASS | Deploy branch `deploy/ga-v138pa-gate` cut directly from `cf412ecdd4`, already an ancestor-clean descendant of `origin/main` (confirmed via `git log`/`git merge-base`). |
| 7 | Change is cohesive and reviewable | PASS | Diff touches `cmd/gc/**`, `internal/config/**`, `internal/doctor/checks_test.go`, `docs/reference/**`, and `examples/bd/assets/scripts/gc-beads-bd.sh` — one theme: the Dolt read-timeout default, its rationale/tests, doctor expectation, generated config docs, and (this round) the shell-fallback parity fix. |

## Verification evidence

**Scope determination:** `git diff --stat origin/main...cf412ecdd4` touches
`cmd/gc/**`, `internal/config/**`, `internal/doctor/checks_test.go`,
`docs/reference/**`, `examples/bd/assets/scripts/gc-beads-bd.sh`. Cross-checked
against `.github/workflows/ci.yml` changes-job path filters: `cmd_gc_process`
triggers on `internal/config/**` and `cmd/gc/**` (both touched) →
`make test-cmd-gc-process-shard` is the CI-required job for this diff (12
shards in CI; local equivalent `test-cmd-gc-process-parallel`, 6 shards).

1. `go build ./...` — clean.
2. `go vet ./...` — clean.
3. `go test -count=1 ./internal/config/...` — ok, 1.834s.
4. `go test -count=1 ./internal/doctor/...` — ok, 56.126s.
5. `make check-docs` — ok, `test/docsync` 3.850s. This package references
   `city-schema`/`genschema` directly, covering schema-sync for the touched
   `docs/reference/schema/city-schema.json`/`.txt` — no separate genschema
   rerun needed.
6. `make test-cmd-gc-process-parallel` (`LOCAL_TEST_JOBS=4`, pinned tool
   `PATH`, podman `DOCKER_HOST`, `TMPDIR=/var/tmp` per repo build-cache safety
   rules): target regression test
   `TestManagedDoltConfigGoWriterMatchesShellFallbackSemantics` confirmed ran
   (progress-listed in shard 5 log) **and passed** (shard 5 finished with a
   bare `ok`, which `go test` only emits when every test in the package
   passes). Shards 2/3/5/6 passed outright. Shards 1 and 4 each had one
   failure, both non-diff-owned:
   - `TestCmdMailInbox_NormalizesCanonicalManagedProviderEnvAndReadsInbox`
     (shard 1): `OpenNativeStorage(): failed to initialize schema: context
     deadline exceeded`. Re-ran isolated (`GC_FAST_UNIT=0 TMPDIR=/var/tmp go
     test -run <name> -count=1 ./cmd/gc/`) — PASS in 4.41s. Not in this
     diff's touched files. This diff only *raises* a Dolt read-timeout
     default (15000→120000 shell-side, matching Go's existing 120000); it
     does not shorten any timeout, so it is not a plausible cause of a new
     deadline-exceeded failure. Tracked: `ga-uswva7`.
   - `TestCityRuntimeForceShutdownTearsDownAfterLateAsyncSweep` (shard 4):
     `force shutdown missed the late async-started runtime` — an
     async-goroutine timing race in `city_runtime_server_lifecycle_test.go`,
     unrelated to any touched file. Re-ran isolated — PASS in 0.01s. Tracked:
     `ga-tt3qwa`.

   Both failures independently reproduced as clean isolated passes; host load
   average was 42.10/39.29/38.46 at the time of the parallel run, consistent
   with transient contention-induced flakes, not diff-owned regressions.
   `ga-tt3qwa`'s signature has since been independently reproduced a second
   time on a completely unrelated diff (`ga-kshlz0`, 2026-08-19 — see that
   tracker for the cross-diff evidence), further supporting non-determinism
   rather than diff ownership. Applying criterion 3a(iii) (exact-signature
   match + zero path overlap + a focused exact-base rerun, all three): both
   failures satisfy it independently of each other and are not attributed to
   this diff.
7. `shellcheck examples/bd/assets/scripts/gc-beads-bd.sh` — not a required CI
   job (grepped `.github/workflows/ci.yml` and `Makefile`; no shellcheck step
   exists anywhere in this repo's CI). The diff itself
   (`git diff origin/main...HEAD -- examples/bd/assets/scripts/gc-beads-bd.sh`)
   is a pure numeric-literal change (15000→120000, two places) plus one new
   comment, around lines 1260–1270; all of shellcheck's SC3043/SC2016
   findings are pre-existing, file-wide, far from the touched region (lines
   142–237+), unrelated to this diff.

### Pre-push mechanical gate (separate from the evidence above)

The repo's own pre-push hook runs a narrower, independent `test-fast-parallel`
suite (10 jobs: `unit-cmd-gc-1..6-of-6`, `unit-core`,
`local-concurrency-selftest`, `push-gate-lock-selftest`,
`fsys-darwin-compile`) as a mechanical gate on `git push` itself. This is
distinct from the CI-required `test-cmd-gc-process-parallel` evidence above
and does not include the process-tier regression test. The push required
three attempts: the first two were refused before any test ran, by
`push-ownership-guard`'s cardinality-gated `.[0]` assignee-fallback picking
an unrelated held bead out of six concurrent in-progress beads under the
shared `gascity/builder` pool identity (tracked: `ga-1qepfl`/`ga-phymuo`;
fix in `ga-3d579a`, not yet merged — see
`push_ownership_guard_pool_identity_mismatch` for the mechanism). Neither
retry bypassed the guard (`--no-verify`/`--force`/`bd reclaim` were all
declined per established fork precedent). The third attempt passed the guard
and ran all 10 fast jobs clean (`All fast jobs passed`), then pushed
successfully: `cf412ecdd4` is now `deploy/ga-v138pa-gate`'s tip on origin,
confirmed via `git ls-remote`.

### Non-diff-owned failures retained in the record (round 1, background)

Carried forward from round 1 for continuity; not re-verified this round
since round 2's own targeted sweep (above) is the operative evidence:

- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` and
  `TestGraphWorkflowFailureRunsCleanup` reproduced the known
  `gastownhall/beads#4566` dirty-table migration signature, tracked on
  `ga-lpfjhc`.
- `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` reproduced the host tmux
  3.7b key-table issue tracked by `ga-afqddr`.
- `TestCleanInstallTutorialPath` again received circuit-breaker diagnostics on
  parsed stdout, tracked by `ga-hrdd3h` (prior chain: `ga-rsktma`).
- `TestEnsureSessionFresh_ZombieSession` misclassified a fresh shell during
  the tmux shard; follow-up `ga-kmwwcx` records the untracked recurrence.

None of these change this verdict — none overlap this diff's touched paths.

## Disposition

- `deploy/ga-v138pa-gate` is pushed to origin at `cf412ecdd43de3ddb3b134f42783403fe409fbbf`.
- This gate-file amendment adds the corrected round-2 record on top, to be
  pushed as a follow-up commit on the same branch.
- Next: open a pull request from `deploy/ga-v138pa-gate` into `main` (do not
  merge it), route a merge-request notification to mayor/mpr, and clear
  `hold:mayor` on `ga-v138pa`. Merge authority is operator/mayor/mpr only.

---

## Round 1 record (superseded, preserved for provenance)

**Reviewed commit:** `9c6ccc8537f5a5cf91b5d67f1dc33c0c55ed4cf7`
**Evaluation branch:** `gate-fail/ga-v138pa`
**Base:** `origin/main` at `a565081fb87c13de8366594ad40ddfd731469539`
**Evaluated:** 2026-08-18 (America/Los_Angeles)

**Verdict:** FAIL — the managed shell fallback still emitted the old
15-second read timeout. The reviewed change raised the canonical Go/config
default from 15,000 ms to 120,000 ms, but did not update the materialized
`gc-beads-bd` shell fallback. The required process suite compared both
producers and failed with `ReadTimeoutMillis:120000` from Go versus
`ReadTimeoutMillis:15000` from the shell fallback — a deterministic,
diff-owned ownership-boundary regression. No environmental waiver applied.

`cmd-gc-process-5-of-6`:

```text
--- FAIL: TestManagedDoltConfigGoWriterMatchesShellFallbackSemantics
Go:    ReadTimeoutMillis:120000
Shell: ReadTimeoutMillis:15000
```

Required repair (now complete in round 2): update the shell fallback's
managed Dolt `read_timeout_millis` default to 120,000 and rerun the
process-tier release gate from the newly reviewed commit.
