# Hold unused fixture ports until cleanup

Planning and implementation bead: `ga-3pv9o04q` (P3). Source: review
`ga-9o5wnprt` of `ga-os7kifr`; source deployment: `ga-5tm4y3hs`.

Two test fixtures close a listener and pass its number onward. A concurrent
test action can acquire the released IPv4 port before either fixture uses it.
The reviewer found both uses unchanged at the source build's base; they are
follow-up work and do not block its deployment.

## Scope and acceptance

1. In `TestCurrentDoltPortIgnoresDeadRuntimeStateAndPrunesDeadPortFile`
   (`cmd/gc/beads_provider_lifecycle_test.go`), replace the listener pick and
   immediate close with `reserveRandomTCPPort(t)`. Keep its assertions about
   dead runtime state and the stale port file unchanged.
2. In `newFsyncGuardFixture` (`cmd/gc/cmd_dolt_state_fsync_probe_test.go`), use
   `reserveRandomTCPPort(t)` for `listening=false`. Preserve the live listener
   for `listening=true`. All five existing false-path consumers retain their
   assertions and pass, as do the true-path consumers.
3. On Linux, the unused number stays bound without listening on both usable
   loopback families until cleanup. Preserve the existing non-Linux contract
   and the remaining residuals of the source ruling. Scope is these two uses;
   no production behavior or helper redesign is authorized.
4. Run the affected existing tests with verbose named PASS evidence through
   Bazel, then `bazel test //cmd/gc:gc_test` and `make check`.
   `make bazel-sync` leaves no uncommitted drift. Resource counts do not grow;
   any counted-call reductions are banked under the resource census policy.
   No new test targets or additional tests duplicating the existing assertions
   are needed.
5. Complexity has zero growth against the pinned merge base. Apply the full
   complexity-zero-growth protocol if the base check is red; judge the diff's
   change lines, including still-live deleted functions, rather than exit
   status. Never edit `engdocs/complexity-baseline.json`.

## Build order and handoff

Reuse this same bead, labelled `ready-to-build`, for the two conversions and
review. Its same-store `blocks` dependencies are source deployment
`ga-5tm4y3hs` and guard repair `ga-786qh71w`. Confirm both changes have landed
on `origin/main` from shipped work records and main ancestry before building.
A closed `gc.work_outcome=blocked` record is insufficient. Do not stack on the
source branch or retry a refused machinery-budget admission.

The P2 guard repair goes first because this cleanup relies on its checks.
Neither dependency points back from the source deployment to these follow-ups.
The existing assertions supply the behavior evidence for this small fixture
conversion; a second test-first implementation bead would duplicate this scope.
