# Release Gate Criteria Conventions

`release-gates/*.md` files record a reviewer's/agent's sign-off on a deploy
branch, one numbered criterion per row. This doc defines what the "Tests
pass" criterion must contain. No prior doc in the repo defined this — see
"Why this doc exists" below.

## The rule

"Tests pass" must name the specific CI jobs that `ci-required`
(`.github/workflows/ci.yml`) actually gates merge on for the paths the
change touches, and cite their real result — either an actual CI run on the
reviewed commit, or a local invocation that exercises the same coverage.

A criterion that only cites `make test-fast-parallel` and/or a package-
scoped `go test` is **not sufficient** whenever the change touches a path
covered by a job outside the fast tier. Find those jobs from the `changes`
job's path filters (same file): a filter matching the change's paths means
its job is a required, blocking check whenever it runs — not an optional
extra.

The most common miss: anything touching `cmd/gc/**`, `internal/**`, or
`examples/gastown/**` is covered by the `cmd_gc_process` filter, whose job
runs `TestTutorial01` (`cmd/gc/main_test.go`) under `GC_FAST_UNIT=0`
(`cmd/gc/fast_loop_helpers_test.go`). Every other default-tier entry point —
`make test`, `make test-fast-parallel`, bare `go test ./cmd/gc/`,
`make check` — sets `GC_FAST_UNIT` to `1` or leaves it unset, which skips
`TestTutorial01` entirely. Citing any of those alone, for a change in that
filter's scope, does not demonstrate `TestTutorial01` ran. Name
`make test-cmd-gc-process[-parallel]` (or the CI `cmd/gc process` job's
actual result) explicitly, or don't claim that criterion covers this path.

The general principle behind the example: "tests pass" must mean "the
gate's actual required checks passed," not "a command I chose passed."
Don't let convenience substitute for coverage.

## The diff-owned-SKIP rule

A diff-owned `t.Skip` is a criterion-3 ("Tests pass") FAIL. If the diff adds
a test, or turns an existing test into a skip, the skip does not satisfy
criterion 3 — regardless of whether the skip is conditional or unconditional.

Non-diff-owned/ambient skips (platform checks, privilege checks, helper-
process guards, live-provider opt-ins, and other suite-controlled skips that
predate the diff) are tolerated and counted, not excused away silently. A
gate record's `skip_justification` field is where they're counted and
explained — but that field is an **attestation of zero diff-owned skips**
("not applicable — zero skips", "none required for diff-owned coverage; zero
focused tests skipped"), not a waiver slot. As of 2026-09-09,
`skip_justification` appeared on 46 `release-gates/*.md` records and
"diff-owned" on 79; every sampled `skip_justification` record attests zero
diff-owned skips.

**Why**: a diff-owned test that FAILS is loud — it blocks the gate and forces
the implementation to land. A diff-owned test that is SKIPPED is silent — it
can ship indefinitely while the gate record makes the change look covered.
That asymmetry is why diff-owned skips get different treatment from ambient
ones.

**The sanctioned alternative for forward-written tests**: when a test lands
ahead of its implementation, write it to FAIL with an honest not-implemented
sentinel, not to SKIP. `be-x5jqd.1` ("Un-skip Phase 0 R16 CAS + R20 epoch
conformance tests") is the model: forward tests landed failing with an
explicit not-implemented message, then were un-skipped for real as each
implementation piece landed behind them.

**Waiver authority**: a reviewer or deployer cannot waive a diff-owned skip
by recording its own determination — a role cannot manufacture its own waiver
by asserting one in its own review notes (`be-i4p79`, MAYOR RULING
2026-09-09 08:48Z: "A reviewer recording its own determination is not a
mayor/operator grant, and treating it as one would let a role manufacture its
own waiver"). For this repo there is no waiver path at all: under the
operator rulings of 2026-09-18 and 2026-09-26 a gascity gate treats any
`waiver_ref` as `none`, so the remedy for a diff-owned skip is the ordinary
one — land the implementation and un-skip the test, convert the skip to a
not-implemented failure, or take the test out of the diff. `none` /
`none required` is the value a gate record carries; an empty `waiver_ref` is
not an oversight to fill in later. Records written before those rulings show
the older mayor-adjudication form, for example
`release-gates/ga-0cx0oh-session-roster-batching-gate.md`: `waiver_ref:
ga-n3gt3w mayor adjudication dated 2026-08-21 for the named host tmux and
bd-manifest failures; ...`. They are history, not a pattern to copy.

See `ga-xfet6h` (the builder escalation that surfaced this gap) and
`be-i4p79` (an independent mayor ruling on the same principle, ~52 minutes
earlier) for the rulings this section documents.

## Why this doc exists

Two independent gate files recorded "Tests pass: PASS" against suites
structurally incapable of reaching the regression they were meant to catch,
both citing `make test-fast-parallel` plus scoped/package-level commands
that leave `GC_FAST_UNIT` at `1` or unset:

- `release-gates/ga-bucf4p-live-session-workdir-isolation-gate.md`, on the
  branch of open PR #4735 (not yet in `main`): the cwd-collision guard
  change was signed off with a "Tests pass" row that never ran a pool
  scenario. Per the root-cause trace in bead `ga-9x4z1g`,
  `TestTutorial01/08-agent-pools` is the scenario that exercises that path.
- `release-gates/ga-7vhfyj-cwd-fallback-guard-gate.md` (PR #4738): recorded
  "The reviewer independently ran the full `cmd/gc` package: 8,030 PASS,
  0 FAIL, 96 SKIP" — `TestTutorial01` was inside the 96 `SKIP`. The change
  broke `TestTutorial01/01-hello-gas-city` and `TestTutorial01/session-fail`
  on CI shard 7.

Both gates were internally consistent (the cited commands really did pass)
and still missed a real regression, because nothing required the "Tests
pass" criterion to map to the CI jobs `ci-required` actually depends on for
the changed paths. This doc closes that gap for future gate authors; it
does not retroactively correct the two files above.

Full evidence and root-cause traces: bead `ga-9x4z1g` (Design field) and
`ga-9x4z1g.3` (notes).
