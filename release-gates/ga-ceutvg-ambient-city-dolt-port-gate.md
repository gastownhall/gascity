# Release Gate: ambient-city Dolt port detection arm

- Deploy bead: `ga-ceutvg`
- Source bead: `ga-cnyuq1`
- Review bead: `ga-pzuio2` (closed, PASS)
- Reviewed source SHA: `145ef9124ac4111fd4a5935e47a6af85087e0f70`
- Final candidate SHA: `e3dba4580fff695194f4249611896b13685e4ff1`
- Base: `origin/main@bf665eae52d14bf9f67112c2c3f6915389f3c913`
- Source branch (provenance only): `builder/ga-ceutvg-ambient-city-dolt-port-arm-fix`
- Isolated deploy branch: `deploy/ga-ceutvg-gate`

The repository does not contain `docs/PROJECT_MANIFEST.md` at this revision.
This checklist therefore applies the canonical seven deployer release criteria
and the repository quality gates in `AGENTS.md` and `TESTING.md`.

## Freshness evaluation (criterion 6 first)

PASS. The canonical `scripts/rebase-resolve-lib.sh`
`attempt_bounded_self_rebase` helper replayed the reviewed patch twice as
`origin/main` advanced during evaluation:

- `145ef9124ac4111fd4a5935e47a6af85087e0f70` ->
  `41b37a639bce8f1ab1a1a96f7cdb200917d44a8f`
- `41b37a639bce8f1ab1a1a96f7cdb200917d44a8f` ->
  `e3dba4580fff695194f4249611896b13685e4ff1`

Both helper calls returned `0` and completed their explicit
`--force-with-lease` pushes. `git range-diff` reports all three feature
commits patch-identical between the reviewed range and the final range.
After the criterion-3 suite, `git ls-remote` still reported
`origin/main@bf665eae52d14bf9f67112c2c3f6915389f3c913`, and
`git merge-base --is-ancestor origin/main e3dba4580` returned success.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | `ga-pzuio2` is closed with `[VERDICT: PASS]`. The deploy bead also contains a fresh exact-SHA reviewer PASS for `145ef9124`; the final candidate is a patch-identical bounded rebase as shown by `git range-diff`. |
| 2 | Acceptance criteria met | **PASS** | `internal/testenv` adds an independent, additive ambient-city arm without removing the existing `3307` environment arm; walks upward for `city.toml`; reads `.gc/runtime/packs/dolt/dolt-state.json` via a fixed-shape `json:"port"` field; rejects only positive discovered ports; documents the separately-executed production-`gc` limitation; and exercises neutral synthetic ports `19999`/`20000`, neither `3307` nor fleet port `28231`. The legacy-port regression suite remains green. |
| 3 | Tests pass | **PASS** | `gofmt -l` clean on all three changed files; `go build ./...` PASS; `go vet ./...` PASS; focused `TestAmbientCityDoltPort`, `TestInitRefusesProdDoltPort`, and `TestInitRefusesAmbientCityDoltPort` PASS (6 + 27 + 3 subtests); full `go test -count=1 ./internal/testenv/...` PASS; `TestRepositoryLedgerMatchesCensusAndDocumentation` PASS; explicit `make test-fast-parallel` PASS, all 9 jobs. The final bounded-helper push also passed the configured pre-push hook on the same SHA. |
| 4 | No high-severity review findings open | **PASS** | Review verdict is PASS with zero unresolved HIGH findings. Its OWASP walk found no injection, XSS, SQL, auth, or untrusted-network surface; the guard only adds refusal conditions. |
| 5 | Final branch is clean | **PASS** | Before writing this checklist, `git status --porcelain=v1` was empty and `git diff --check origin/main..HEAD` passed. The checklist is the only deployer-authored file and will be committed on the isolated gate branch. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first. Final candidate contains `origin/main@bf665eae5`; remote main and source ref were re-read after tests and remained stable. |
| 7 | Single feature theme | **PASS** | The feature range changes only `internal/testenv/testenv.go`, `internal/testenv/testenv_internal_test.go`, and `internal/testenv/testenv_test.go`, all implementing and proving one ambient-city Dolt-port refusal arm. |

## Acceptance mapping

- Self-relative detection: `ambientCityDoltPort` walks from the test process
  working directory to a `city.toml` root and reads that city's managed-Dolt
  state.
- Additive behavior: `refuseProdDoltPort` still calls the existing
  `ProdDoltPort` arm before consulting the ambient-city arm.
- Neutral end-to-end proof: matching `19999` panics, non-matching `20000`
  survives, and the explicit opt-out permits `19999`.
- Existing behavior preserved: all 27 `TestInitRefusesProdDoltPort` cases pass.
- Resource budget preserved: the shared subprocess assertion helper keeps the
  checked resource-census ledger unchanged.
- Scope boundary documented: a separately executed production `gc` binary
  does not import `internal/testenv`; that follow-up remains outside this
  change.

## Gate verdict

**PASS** — eligible for an isolated deploy branch and pull request.
