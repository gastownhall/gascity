# Reliable held-port release checks

Planning bead: `ga-786qh71w` (P2). Source: review `ga-9o5wnprt` of
`ga-os7kifr`; companion cleanup: `ga-3pv9o04q` (P3).

The Linux guard tests can report a leaked port after their own sockets have
closed: another action in the shared executor can acquire that number before
the test probes it. The reviewer observed failures in 1/32, 1/32 and 3/32 remote
actions under different stress shapes. These are observations, not a measured
default-gate failure rate.

## Scope and sequence

Reuse `ga-786qh71w` through architecture, validator, builder and review. Do not
create a separate implementation bead for its RED test. Keep the source
deployment `ga-5tm4y3hs` independent: neither follow-up blocks it.

| Work | Routing | Prerequisite |
| --- | --- | --- |
| `ga-786qh71w`: settle the release-check contract, then test and repair it | `needs-architecture`, then `needs-tests` to validator; same bead goes to builder | `ga-5tm4y3hs` lands the reviewed helpers and six added tests |
| `ga-3pv9o04q`: convert two pick-and-close fixture uses | `ready-to-build` to builder | Source deployment and `ga-786qh71w` both land |

Use same-store `blocks` edges. A closed build record with
`gc.work_outcome=blocked` is not a landing receipt. Before starting, confirm
the prerequisite deployment shipped and its changes are present on `origin/main`.
Do not stack on the in-flight source branch or retry a refused admission.

## Acceptance

1. The architect records what AC3e proves and chooses the release-check
   mechanism. The review offers bounded retries without sleeps or observing
   the hold's own sockets; PM selects neither. Address why a durable foreign
   holder cannot be mistaken for an unreleased fixture socket, or explicitly
   state the remaining limit in the revised contract.
2. The validator records a compiling RED case for the chosen contract; the
   same bead reaches the builder with its exact reproduction and ordering
   guard. A deliberately unreleased own socket must still be detected.
3. The repaired check covers all three existing consumers of
   `requireLoopbackPortFree`. Socket cleanup, both loopback families when
   available, and the existing hold semantics remain covered. No production
   behavior changes and no sleeps or additional test targets are introduced.
4. Reproduce the reviewer's remote stress shape: 32 actions, each running
   `TestPortHelpersHoldTheNumberUntilCleanup` together with
   `TestReservedLoopbackPortIsNeverReturnedByRawBindZero`, 100 iterations per
   test per action. Record executor, command, counts and logs; require zero
   failures and actual named PASS evidence, not an empty filter or skipped run.
5. Use Bazel only. Run the focused verbose test, `bazel test //cmd/gc:gc_test`
   and `make check`; run `make bazel-sync` if files or imports change. Resource
   counts do not grow. Existing guards for 64 tries, rejected candidates,
   wrapped errors and errno classification are retained rather than duplicated.
6. Complexity has zero growth against the pinned merge base. Follow the
   complexity-zero-growth protocol for a red base, judging `complexity-diff`
   output rather than exit status. Never edit `engdocs/complexity-baseline.json`.

## Build and limits

After the ruling, repair the test probe and its tests in `cmd/gc` only. The
reviewed source is `dabfb4ad82395239430488c3a13b6e1386f1bda4`; locate symbols by
name because the review corrected its line references. The v4-only fallback
return-value test is optional architecture scope, not authorization to rewrite
already covered AC2 cases. The possible churn effect on other residual fixtures
is unmeasured and creates no extra work package here.

The architect hands this same bead to the validator after recording the ruling.
The validator hands it to the builder, which implements and obtains review.
The P3 cleanup stays behind the landed guard fix. No implementation, test run,
build admission, or shipment is asserted by this plan.
