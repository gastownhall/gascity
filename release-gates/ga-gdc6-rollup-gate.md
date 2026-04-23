# Release gate — ga-gdc6 (rollup-ship)

**Bead:** ga-gdc6 — `[ga-h6w + ga-d5y] Ship ADR 0001 + ADR 0002 (mega rollup)`
**Branch:** `release/ga-gdc6`
**Base:** `origin/main @ 99742e36` (Merge PR #1104 — fix/claude-transcript-path-aliases-complete)
**Attempt:** 5 (prior four attempts FAILed; remediation delivered by builder ga-tazy + mayor ga-gdc6-regen + mayor ga-gdc6-runbook)

## Verdict: PASS

All six release-gate criteria verified on the assembled branch. Ready to
push + open PR.

## Criteria

| # | Criterion                                   | Status | Evidence                                                        |
|---|---------------------------------------------|:------:|-----------------------------------------------------------------|
| 1 | Review PASS present (single-pass)           | PASS   | 17/17 review beads PASS; ga-sec DOCS_ONLY carve-out (table below)|
| 2 | Acceptance criteria met (per child)         | PASS   | All 18 children CLOSED; first-pass reviewer PASS on all 17 review beads |
| 3 | Tests pass                                  | PASS   | `go test ./...` summary below; pre-existing failures unchanged   |
| 4 | No high-severity review findings open       | PASS   | Review beads all PASS with no HIGH findings noted                |
| 5 | Final branch clean                          | PASS   | `git status` clean (only stray `.gitkeep` untracked — not added) |
| 6 | Clean cherry-picks onto origin/main         | PASS   | 20/20 picks applied with EXCLUDES=issues.jsonl; no genuine conflicts |

## Children (18) — PASS verdicts

### ADR 0001 (ga-h6w) — read-path routing

| Child  | Review bead | Reviewer verdict | Commit on release branch |
|--------|-------------|:----------------:|--------------------------|
| ga-71l | ga-yaqp     | PASS             | 6dbbf217 (src: 2452afab) |
| ga-2o9 | ga-sooy     | PASS             | f3d0ae1a (src: 540460eb) |
| ga-6q1 | ga-0ly6     | PASS             | 8e04710f (src: 76dc58c2) |
| ga-idc | ga-gys0     | PASS             | e3c57acd (src: e0546140) |
| ga-06g | ga-sk12     | PASS             | 4363f8d8 (src: e97d86e7) |
| ga-6s5 | ga-sxx5     | PASS             | c269975a (src: a7a5ea30) |
| ga-gti | ga-ut5z     | PASS             | 25693be0 (src: 1dc12385) |
| ga-69s | ga-dbrk     | PASS             | 93a68ea4 (src: e1b3392c) |
| ga-2fr | ga-djbd     | PASS             | 33ce6336 (src: 910407ca) |

### ADR 0002 (ga-d5y) — dolt store maintenance

| Child  | Review bead | Reviewer verdict | Commit on release branch |
|--------|-------------|:----------------:|--------------------------|
| ga-zol | ga-i24      | PASS             | 4bd07e58 (src: a257a692) |
| ga-8km | ga-5tb      | PASS             | bb5377d8 (src: 35f2f4c9) |
| ga-8cq | ga-0awq     | PASS             | e0255455 (src: ec71b17e) |
| ga-zoj | ga-yhbi     | PASS             | 2465de28 (src: 72e4bc18) |
| ga-p5n | ga-4xqa     | PASS             | 4272aca5 (src: 19818d49) |
| ga-74d | ga-0ydz     | PASS             | 2ae5b886 (src: bc393010) |
| ga-zn8 | ga-7mah     | PASS             | 5c9d8f7e (src: 0b09a6e3) |
| ga-sec | (DOCS_ONLY) | n/a              | dde9a622 (src: 193e94c6) |
| ga-e3s | ga-4nh2     | PASS             | 0bc354d8 (src: da60f000) |

### Mayor follow-ups (non-child commits, included per CHERRY_PICKS)

| Tag               | Reason                                                                | Commit on release branch |
|-------------------|-----------------------------------------------------------------------|--------------------------|
| ga-gdc6-regen     | Regenerate city-schema.json + config.md + cli.md after [maintenance.dolt] config + new CLI — resolves `test/docsync.TestSchemaFreshness` | 24a2dfc1 (src: 54334be0) |
| ga-gdc6-runbook   | Drop links to internal-only ADR/architecture/rule docs from the ga-sec runbook — resolves `test/docsync.TestLocalMarkdownLinks` | d02e7b9c (src: c63b5244) |

## Cherry-pick log

- Source: individual commits listed in CHERRY_PICKS (no single source branch
  required; SHAs resolved from repo object graph).
- Excludes: `issues.jsonl` (beads-sync artifact; absent on `origin/main`).
- Strategy: `git cherry-pick -n <sha>` with strip-form recipe per deployer spec;
  excluded paths reset from index; final commit via `git commit -C <sha>`.
- Result: **20 applied, 0 skipped, 0 conflicts.** All 20 SHAs verified present
  in the repo object graph via `git cat-file -e`.

## Test summary

Ran `go test ./...` on the assembled branch, plus a re-run of the three
packages that reported FAIL, plus the same three packages on `origin/main
@ 99742e36` as a baseline. The release branch has **zero regressions**
relative to `origin/main`.

| Run | Scope | Duration | `--- FAIL:` lines | Failing packages |
|-----|-------|---------:|------------------:|------------------|
| 1. `go test ./...` on `release/ga-gdc6` | full suite | ~10 min (cmd/gc timed out) | 91 (cut short by 10m package timeout on `TestControllerLoopTick`) | `cmd/gc`, `internal/doctor`, `internal/runtime/k8s` |
| 2. `go test ./cmd/gc ./internal/doctor ./internal/runtime/k8s -timeout 10m` on `release/ga-gdc6` | 3 pkgs | 432s | **145** | same 3 |
| 3. `go test ./cmd/gc ./internal/doctor ./internal/runtime/k8s` on `origin/main @ 99742e36` (temporary worktree) | 3 pkgs | 265s | **145** | same 3 |

**Diff of failing tests between run 2 and run 3** (`comm -3` on sorted
unique test names): empty on both sides — identical 145 test names, in
the identical 3 packages. No test that passes on `origin/main` fails on
this branch.

### Why the failures exist (same root causes on both sides)

- **`internal/doctor`** (1 test) — `dolt server not reachable at
  127.0.0.1:4419`. No dolt server running in the deployer seat; the
  check requires a live dolt endpoint.
- **`internal/runtime/k8s`** (4 tests) — `controller bootstrap requires
  both GC_DOLT_HOST and GC_DOLT_PORT when either is set`. Controller
  script needs k8s + dolt env; neither is configured in this seat.
- **`cmd/gc`** (~140 tests) — large testscript suite driven by
  `go-internal/testscript`. Symptoms include `beads cache: reconcile
  cache: bd list: chdir /tmp/TestX.../...: no such file or directory`
  (a test-cleanup race in the caching-store reconciler reaching into
  tempdirs that `t.TempDir` already removed), plus `dolt server not
  reachable` echoes for the tests whose scripts assume a live store.
  All fail on `origin/main` in the identical environment.

### Run-1 timeout on `TestControllerLoopTick`

Run 1 hit `panic: test timed out after 10m0s` with `TestControllerLoopTick`
listed as the last running test (3m10s on the stack). The re-run (run 2,
same command restricted to the same package with an explicit `-timeout
10m`) completed in 432s with no timeout panic. Treat as a transient /
environmental hiccup — not reproducible on re-run, and not introduced by
the cherry-picks (the baseline run on `origin/main` ran the same suite
without panicking, and `TestControllerLoopTick` isn't on the baseline
failing list either).

### Gate-relevant conclusion

Criterion 3 asks for the rig's test command run on the final branch with
a summary. Established convention on this bead (documented on deployer
gate FAIL #4) is that environment-identical pre-existing failures do not
block the gate — only regressions relative to `origin/main` do. With
zero regressions and zero newly-failing tests, **Criterion 3: PASS.**

## Push target

- `git push --dry-run origin HEAD` → 403 (expected; quad341 cannot push to
  gastownhall/gascity).
- Falling back to `fork` remote → `https://github.com/quad341/gascity.git`.
- PR head ref: `quad341:release/ga-gdc6`, base `main`.

---

🤖 Evaluated by gascity/deployer
