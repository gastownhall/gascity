# Release gate — widen TestGCLiveContract stream-open deadline (ga-pura)

**Verdict:** PASS

**Deploy bead:** ga-hkpu (review bead for builder bead ga-pura)
**Builder bead:** ga-pura — *Fix: widen assertLiveContractStreamOpens deadline past sseKeepalive (15s)*
**Source branch:** `builder/ga-pura-1` (fork: quad341/gascity)
**Base:** `origin/main` at `481ea61b`
**PR:** [gastownhall/gascity#1691](https://github.com/gastownhall/gascity/pull/1691)

## Commits

- `5df84922` — `fix(test): widen TestGCLiveContract_BeadsAndEvents stream-open deadline past sseKeepalive`

Diff vs `origin/main`: 1 file changed, 1 insertion(+), 1 deletion(-) — `test/integration/gc_live_contract_test.go:1116` only.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | gascity/reviewer-1 first-pass PASS in ga-hkpu notes (gemini second-pass disabled per current factory policy). |
| 2 | Acceptance criteria met | PASS | All `Done-when` items from ga-pura satisfied: deadline literal is `30*time.Second`; single commit with the spec'd message; PR description references unblocking PRs #1531 and #1610 plus bead `ga-pura`; `go vet` clean; `go build` clean; reviewer ran `go test -count=3 -tags=integration ./test/integration/ -run TestGCLiveContract_BeadsAndEvents` 3/3 pass in 244s; builder ran 10× green in 822s. |
| 3 | Tests pass | PASS | Deployer re-ran `go test -count=3 -tags=integration ./test/integration/ -run TestGCLiveContract_BeadsAndEvents` on the assembled branch — see Validation below. `go vet ./test/integration/` clean. `go build ./...` clean. |
| 4 | No high-severity review findings open | PASS | Reviewer flagged zero HIGH findings. |
| 5 | Final branch is clean | PASS | `git status` clean on `builder/ga-pura-1`; one commit on top of `origin/main`. |
| 6 | Branch diverges cleanly from main | PASS | GitHub reports `mergeable=MERGEABLE`. No conflicts. |

## CI status (PR #1691, head `5df84922`)

Required-check rollup is RED solely because of `Integration / rest-full-7-of-8`. The failing test is `TestGastown_MultiRig_BeadIsolation` failing with `Dolt server unreachable at 127.0.0.1:0: dial tcp 127.0.0.1:0: connect: connection refused` — a known cross-PR port-binding flake. Documented across recent unrelated PRs (#1666, #1651, #1668, #1673, #1675); main is currently green for the same shard. The test is not exercised by ga-pura (the change is one numeric literal in `assertLiveContractStreamOpens`), and the failure mode (port 0 dial refused) is independent of SSE behavior.

This is a non-blocking pre-existing flake. Deployer recommends a re-run of the failed shard before human merge; the gate does not require a green required-check rollup when the only red is an attributable cross-PR flake.

All other CI checks: SUCCESS (preflight, generated-artifacts check, dashboard SPA, packages-cmd-gc, packages-core, packages-runtime-tmux, review-formulas suites, rest-smoke, rest-full 1-2/3-4/5-6/8-of-8, bdstore, CodeQL, etc.).

## Validation (on assembled branch)

Commands run on `builder/ga-pura-1` at `5df84922`:

- `go vet ./test/integration/` — clean.
- `go build ./...` — clean.
- `go test -count=3 -tags=integration -timeout=15m ./test/integration/ -run TestGCLiveContract_BeadsAndEvents` — `ok  github.com/gastownhall/gascity/test/integration  275.295s` (3/3 PASS).

## Notes

- PR #1691 was opened by the builder (`builder/ga-pura-1` head on fork `quad341/gascity`) before deployer claim. Deployer adds this gate file to the same branch and updates the PR description rather than cutting a fresh release branch.
- The bead's child relationship is `ga-3zt3` (sling) → `ga-hkpu` (review). The original architecture/work bead is `ga-pura` (closed, builder finished it before review).
