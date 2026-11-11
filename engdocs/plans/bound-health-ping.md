# Bound the health store-open ping

Content bead: `ga-3ipmuxh0` (P3). Landing record: `ga-86p3uakv`.
Planning base: `607e3570e3fa2409cb6d216c36a243b35a99d6fb`.

## Outcome and scope

A wedged `bd ping` must return a useful health failure within the existing
health-operation budget. It must not hold the operation open until the outer
provider runner kills it. Fast success, ordinary failure and cities without a
`.beads` directory retain their existing behavior.

This is acceptance refinement of review round 1 minor 5b from `ga-inhcbxv`.
The validator returned `ga-3ipmuxh0` because its notes and acceptance field
contained no measurable criteria. PM adds acceptance and `Build:` notes to
that same bead and returns it to `needs-tests`; there is no second build bead.
The description and original provenance remain intact.

Only the store-open ping bound is in scope. `op_recover`'s helper-return path,
the Go recovery policy, the data-read classifier and the parent bead's
corrupt-store experiment are separate. This plan does not reopen or repeat
those activities, change the existing health deadline, or choose a shell
timeout implementation.

## Source and landing evidence

At the planning base, `origin/main` contains neither
`health_store_open_gate` nor
`cmd/gc/gc_beads_bd_health_data_read_test.go`. Both exist in upstream
`builder/ga-rmrnl1` commit
`579c80f2fcc9409713fc303c59c541e269bb5910`, which is being reviewed as
`ga-4ymxxmkn`. Read-only inspection of that commit shows:

- `op_health` calls `health_store_open_gate` after its query/read-only/data-read
  checks. The data-read probe has a 20-second cap.
- The gate skips a missing city `.beads` directory; otherwise it captures
  `run_bd_pinned "$GC_CITY_PATH" ping` output and fails with that output on a
  nonzero result. There is no gate-local timeout.
- `run_bd_pinned` supplies the selected city, working directory, store and
  authenticated server environment. Its other callers must keep their behavior.
- The upstream shell tests already pin ping success, failure with useful error
  output, and the missing-directory bypass.
- `providerOpTimeout("health")` retains the generic 30-second provider budget.
  Separately established provider-owned/proxied budgets are outside this scope.

`gc actual-factory work-record-check` reported the upstream commit
`NOT-LANDED`, including the patch-equivalent check. Closed `ga-rmrnl1` and
`ga-inhcbxv` records carry `gc.work_outcome=blocked`; their status is not landing
evidence. The accepted upstream commit may change during review.

The same-store blocking edge is therefore:

```text
ga-3ipmuxh0 --blocked-by--> ga-86p3uakv
```

`ga-86p3uakv` is an open, unassigned, unrouted `parent-record`, deliberately
waiting for the upstream landing. It is a prerequisite record, not another
implementation scope. The upstream landing coordinator (mayor/deployer), or PM
on the same live evidence, closes it only after recording the final accepted
commit's landing on the allowed target and verifying the gate and its tests
are present there. Then notify PM and validator. A review close or branch push
does not clear it.

## Measurable acceptance

The full acceptance contract is stored in both the bead's acceptance field and
its notes. Its required behavior is:

1. Preserve the direct/managed health operation's 30-second budget and the
   existing data-read cap. Ping and cleanup fit within the time available after
   preceding health phases. Do not start a fresh full budget after a slow probe;
   if the budget is exhausted, fail promptly. Broader lifecycle budget changes
   require an architect ruling if implementation exposes a concrete conflict.
2. A hung ping returns a nonzero health result and a diagnostic identifying the
   store-open/ping timeout. Prove that the health operation itself returns before
   the test's outer watchdog; the watchdog killing it is a failure.
3. Timely success passes promptly. Ordinary nonzero failure preserves useful bd
   error text, including chunk/store-open signatures, and is distinguished from
   timeout. Missing `.beads` passes without invoking the ping or its timer.
4. Preserve pinned target/credentials and unrelated `run_bd_pinned` calls. Emit
   no new credential-bearing diagnostics. Leave the upstream query, read-only,
   data-read and connection-warning outcomes unchanged.
5. Success, failure and timeout leave no probe or timeout-watcher process or
   inherited output pipe holding the operation open. A TERM-resistant stub also
   terminates within the bound. Cleanup targets only this probe's processes.
6. Shell evidence exercises the real pinned wrapper with a controlled bd
   executable, plus composed `op_health` with a slow earlier probe and hung
   ping. Cover success, failure, timeout, cleanup and missing-store bypass with
   short controlled deadlines. No real Dolt server, model session, 20–30-second
   test wait or live-store mutation is needed.
7. RED demonstrates the missing timeout behavior after the prerequisite lands;
   missing upstream functions and compiler failures are not behavioral RED.
   Filtered Bazel runs name each passing test; run `//cmd/gc:gc_test`, full
   `make check`, shell guards and any matching integration tier reached by the
   final diff. Follow `TESTING.md` for subprocess and sleep resource accounting.
8. Require zero complexity growth against the pinned target merge base under
   the shared rule: a green base requires a green merge; otherwise compare the
   base/merge rows and require no worsening or new/regressed/deleted-live
   function. Do not edit `engdocs/complexity-baseline.json` in this feature.

## Handoff and completion

After the landing record clears, validator freshens main, writes the shell
tests, and hands the same `ga-3ipmuxh0` to builder with its `Build:` notes and
ordering guard intact. Neither role copies the upstream gate into this scope
or stacks on another worker's branch.

PM completion is a committed, pushed and verified-clean plan plus a verified
route back to validator and a released PM claim. The content bead stays open
because its tests and implementation remain outstanding. While the landing
record is open it must be absent from the canonical ready set; a route or wake
does not authorize bypassing that dependency. No external tracker skill is
installed, so tracker import is a no-op.
