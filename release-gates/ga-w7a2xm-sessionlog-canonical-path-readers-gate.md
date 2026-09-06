# Release Gate: sessionlog canonical path readers

- Deploy bead: `ga-w7a2xm`
- Source bead: `ga-iawy13.5`
- Review bead: `ga-dbz56p`
- Reviewed deploy source: `da5dfe29fad945583bd73e588797abca958e11c1`
- Deploy branch: `deploy/ga-w7a2xm-gate`
- Base: `origin/main` at `220e46022c5eb74b028d10976e0e6820b243da70`

Result: **PASS**

## Evaluation Order

Criterion 6 was evaluated first, before the test suite.

- `git fetch origin main`: PASS.
- `git merge-tree --write-tree origin/main da5dfe29fad945583bd73e588797abca958e11c1`:
  tree `fbb877cce6b3961c0dea5edd29a68ac012acea47`, exit 0, no conflict diagnostics.
- `origin/main` is not an ancestor of the reviewed source, but the merge-tree
  result proves the reviewed change merges cleanly with the current base.
- No bounded self-rebase was needed.

## Scope

The reviewed range contains one TDD change set:

- `f692e7e3d` — red tests for canonical reader/discovery behavior.
- `61fb67ea6` — canonical comparison keys and documented resolvability checks.
- `da5dfe29f` — preserve lexical Kimi roots while using canonical dedup keys.

It changes only six files in `internal/sessionlog` (280 insertions, 33
deletions): `codex_batch.go`, `codex_batch_test.go`, `kimi_reader.go`,
`kimi_reader_test.go`, `reader.go`, and `reader_canonical_path_test.go`.

## Gate Checklist

| # | Criterion | Status | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | Evaluated first. `git merge-tree --write-tree` against `origin/main@220e46022c5eb74b028d10976e0e6820b243da70` returned tree `fbb877cce6b3961c0dea5edd29a68ac012acea47` with no conflicts. |
| 1 | Review PASS present | PASS | Review bead `ga-dbz56p` is closed with `Review verdict: PASS` for authoritative commit `da5dfe29fad945583bd73e588797abca958e11c1`. |
| 2 | Acceptance criteria met | PASS | All 10 scoped `filepath.EvalSymlinks` sites are classified below; four comparison/dedup sites use `pathutil.NormalizePathForCompare`, six functional traversal checks retain `EvalSymlinks` with adjacent exception comments, canonical keys do not replace returned lexical Kimi paths, and tests cover real/symlink dedup plus broken-link behavior. |
| 3 | Tests pass | PASS | Documented PR-CI-equivalent suites passed with the pinned CI toolchain: fast 10/10 jobs, process 7/7 jobs, integration 14/14 shards, review-formula 3/3 lanes, worker conformance 6/6 profile/phase commands, and scoped sessionlog 293 PASS / 0 FAIL / 7 justified Darwin-only SKIP. `go build ./...` and `go vet ./...` also passed. Full commands and skip justification are below. |
| 4 | No high-severity review findings open | PASS | Reviewer notes report no security, style, correctness, or HIGH-severity findings after the post-green lexical-path regression fix; unresolved HIGH count is 0. |
| 5 | Final branch is clean | PASS | Before writing this checklist, `git status --short --branch` showed a clean detached checkout at the reviewed SHA. This checklist is the only deploy-only delta and will be committed separately on the isolated deploy branch. |
| 7 | Single feature theme | PASS | Every commit and changed path belongs to one subsystem and behavior: canonical identity comparison and symlink traversal semantics in `internal/sessionlog` readers. No independently shippable feature is bundled. |

## Acceptance Evidence

### Per-site classification matrix

| File / function | Behavior class | Disposition |
|-----------------|----------------|-------------|
| `codex_batch.go` / `markCodexBatchRoot` | Comparison preparation: root dedup identity | Migrated to `pathutil.NormalizePathForCompare`; the first lexical root remains the scan/return path. |
| `reader.go` / `appendCodexRolloutMatch` | Comparison preparation: transcript dedup identity | Migrated to `pathutil.NormalizePathForCompare`; the first lexical match is retained. |
| `reader.go` / `appendCodexCandidatesFromDir` | Comparison preparation: candidate dedup identity | Migrated to `pathutil.NormalizePathForCompare`. |
| `kimi_reader.go` / `canonicalKimiSessionRoot` | Comparison preparation: visited-root identity | Migrated to `pathutil.NormalizePathForCompare`; callers store the key separately and preserve the lexical root for path construction. |
| `reader.go` / `findCodexRolloutBySuffixIn` | Functional existence/resolvability check | Bare `filepath.EvalSymlinks` retained with adjacent canonical-path exception comment; broken roots are skipped. |
| `reader.go` / `collectCodexCandidatesInDays` | Functional existence/resolvability check | Bare `filepath.EvalSymlinks` retained with adjacent canonical-path exception comment; broken roots are skipped. |
| `reader.go` / `findCodexSessionFileIn` | Functional existence/resolvability check | Bare `filepath.EvalSymlinks` retained with adjacent canonical-path exception comment; broken roots are skipped. |
| `kimi_reader.go` / `findKimiSessionFileInVisited` | Functional existence/resolvability check | Bare `filepath.EvalSymlinks` retained with adjacent canonical-path exception comment; broken roots are skipped. |
| `kimi_reader.go` / `findKimiSessionFilesInVisited` | Functional existence/resolvability check | Bare `filepath.EvalSymlinks` retained with adjacent canonical-path exception comment; broken roots are skipped. |
| `kimi_reader.go` / `findKimiSessionFileByIDInVisited` | Functional existence/resolvability check | Bare `filepath.EvalSymlinks` retained with adjacent canonical-path exception comment; broken roots are skipped. |

`rg -n -C 3 EvalSymlinks` confirms exactly the six documented bare production
calls remain, each with the required adjacent explanation. The regression
tests verify relative/absolute and real/symlink roots deduplicate to the same
records, symlinked not-yet-existing leaves share identity, and broken roots
retain skip-on-error behavior.

## Test Evidence

The repository has no `docs/PROJECT_MANIFEST.md`, so the test command was
selected from `TESTING.md`, `.github/workflows/ci.yml`, and
`engdocs/contributors/release-gate-criteria-conventions.md`. Because the
change is under `internal/sessionlog`, the required fast, `cmd/gc` process,
integration, and worker lanes were all exercised.

- `make test-fast-parallel` with `LOCAL_TEST_JOBS=6`: **10 PASS, 0 FAIL, 0 SKIP jobs**.
- `make test-cmd-gc-process-parallel` with `LOCAL_TEST_JOBS=6`: **7 PASS, 0 FAIL, 0 SKIP jobs** (six process shards plus product-metrics testhook).
- Exact PR integration matrix from `.github/workflows/ci.yml`: **14 PASS, 0 FAIL, 0 SKIP shards** — four `packages-core` shards, `packages-cmd-gc-integration`, six `packages-runtime-tmux` shards, `bdstore`, and two `rest-smoke` shards.
- `make test-integration-review-formulas`: **3 PASS, 0 FAIL, 0 SKIP lanes** (basic, retries, recovery).
- `make test-worker-core` and `make test-worker-core-phase2-all` for `claude/tmux-cli`, `codex/tmux-cli`, and `gemini/tmux-cli`: **6 PASS, 0 FAIL, 0 SKIP commands**.
- `go test -count=1 -json ./internal/sessionlog/...`: **293 top-level PASS, 0 FAIL, 7 SKIP**. The seven skips explicitly require Darwin's `/tmp` to `/private/tmp` alias and are inapplicable on this Linux gate host.
- `go build ./...`: PASS.
- `go vet ./...`: PASS.
- `git diff --check f4650c515c8bc660ff47ba85f8751641bf564732..da5dfe29fad945583bd73e588797abca958e11c1`: PASS.

### Runner-isolation diagnostics

Initial convenience sweeps were not used as PASS evidence. The host's global
Beads configuration enables a shared Dolt server and its installed `bd` binary
does not match CI's pinned v1.1.0 archive; host tmux 3.7b also differs from the
Ubuntu runner behavior. Those leaked settings produced shared-workspace gate
errors and startup timeouts during a 13-way `make test-local-full-parallel`
run. An unsharded all-packages integration sweep run concurrently with other
tmux-heavy suites also produced a shell-observation collision. The final
evidence above uses the checksum-verified CI `bd` v1.1.0 archive, tmux 3.4,
isolated state, bounded concurrency, and the exact sharded PR matrix. The
push-only full REST suite is not part of the PR release criterion; its required
PR smoke subset passed 2/2 shards.
