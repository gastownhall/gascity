# Restore portable screen contract tests

PM plan for ga-b3j8zgis, 2026-11-04. Keep the five screen contract tests
portable, as the mayor directed in gcg-344065. Diagnose the unexplained Mac
failures before starting a repair. This is P3 agent-minted machinery work;
normal admission and weekly budget limits apply. It blocks no current delivery.

## Evidence and scope

The investigator isolated five `internal/runtime/runtimecontract` failures in
[Mac / make test, run 41737782500](https://github.com/gastownhall/gascity/actions/runs/41737782500/job/126271819343).
They arrived with #8724, independently of the older bd/dolt Bash parser defect.
Current main inspected is `7280d38c50a27f7a8733d78b7ceff3a0951085e0`.

`TestScreenReferenceIsConformant` creates its intended short state root using
`os.MkdirTemp("", "gcs")`. The empty base inherits the long Mac `TMPDIR`,
and the reported complete screen socket path exceeds the script's guard.
The root also records four fake-backend failures returning empty inventory or
identity: `TestScreenScriptAgentAliases`, `TestScreenScriptAgentOutlivesItsBox`,
`TestScreenScriptDeadBoxWithALiveAgent`, and `TestScreenScriptStartNeedsTheBox`.
Their causes remain undetermined. The fake backend also inherits `TMPDIR` for
its state root, but this observation does not prove a shared cause.

The Actions API reports run head `705ca382a228ad525061a8c2a1af8f880798117d`;
the investigator cites checkout merge ref `5089bf1` with main `01a4d1955b5`.
The diagnosis must establish the actual tested checkout from the logs, rather
than treating the run's head SHA as the tested tree.

## Work graph

| Bead | Scope | Intake and prerequisite |
|---|---|---|
| ga-aaowxzui | Diagnose the four inventory/identity failures on Mac | Investigator, `needs-investigation`; begin with existing evidence |
| ga-5mwagbzi | Repair all five test portability failures | Validator, `needs-tests`; native blocks dependency on completed diagnosis; same bead proceeds to builder |
| ga-5i098avh | Verify the repair actually lands on main | Unrouted `gate-tracker` and `parent-record`; PM watches delivery |
| ga-01wqeilz | Record final Mac acceptance evidence | Builder, `ready-to-build`; native blocks dependency on the landing guard; evidence only |

Each scope carries `discovered-from` provenance. Native `blocks` edges enforce
the ordering shown above. The landing record is a condition rather than another
implementation task.

## Acceptance and boundaries

The diagnosis records a concrete cause and Mac evidence for each of the four
unexplained failures: checked-out SHA, commands, stdout, stderr, exits, relevant
shell/tools and fixture/process identity inputs. It identifies shared and separate
causes explicitly. A Linux pass does not diagnose a Mac failure. If evidence or
a required semantics ruling is missing, the diagnosis remains open with a concrete
blocker and returns to PM; an inconclusive close must not release implementation.

One test-first repair follows that evidence. The known reference case gets a
private test-owned state root whose complete socket path fits the applicable
host limit with a long ambient Mac `TMPDIR`. Regression evidence must fail with
the original fixture and pass after repair. Cleanup, isolation and rejection of
overlong paths remain covered. The production socket-limit contract is not
changed as a shortcut.

The four inventory cases retain their existing alive, unknown and identity
assertions. Keep every affected test exercised on Mac and Linux. No new OS skips,
removed tests, weaker assertions, hidden stderr, interpreter upgrade or Linux-only
provider declaration is an acceptable fix. A supported-behavior or interface change
needs an architect/maintainer ruling; PM chooses no such change.

Linux verification uses `bazel test //internal/runtime/runtimecontract:runtimecontract_test` through the isolation
wrapper. Filtered evidence must show the named tests actually run. Real-screen
conformance requires its declared tools; the RBE image's missing-screen skip is
not Mac conformance evidence. Existing native Mac CI retains its documented
engine on the Mac runner. RED precedes the repair and GREEN records actual
execution. Complexity must show zero growth against the pinned base under the
full complexity rule; the feature never refreshes the complexity baseline.

The repair's review/deploy/MPR handoffs carry ga-5i098avh and notify PM on actual
main landing or retirement. PM closes the guard only after checking fresh main,
the source-to-main mapping, and the intended shipped changes. A build or review
close, branch push, or RED/GREEN commit is insufficient. The guard blocks final
acceptance only, never repair or deployment.

Final acceptance reuses a qualifying Mac run containing the landed repair. It
requires named PASS results for all five cases and no runtimecontract failures.
Skipped or absent tests do not count. Other Mac-package failures receive concrete
tracked attribution; this scope may pass while the whole lane stays red, and
the report must say so. Missing Mac evidence remains incomplete.

Existing ga-qf5sdqj remains its separate parser-fix acceptance scope. Reuse a
shared qualifying run where possible; neither scope proves the other's
criterion, and the new screen repair does not become its blocker.

## Delivery

Commit and re-verify this exact plan path before closing the PM root. Close the
root as planning complete, not repaired. Route the three work beads with their
intake labels and `gc sling --nudge`, verify route or admission-deferral state,
and mail each distinct downstream role. Respect any budget refusal without retry,
priority change, label removal or a manual build. The condition record stays
unassigned and unrouted. No external tracker import, test authoring, production
change, runtime operation, CI policy change or merge is part of this PM task.
