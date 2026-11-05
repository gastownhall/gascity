# Mac screen portability

## Current disposition — 2026-11-05

Mayor ruling `gcg-1414145` chooses option (a): retire the old repair, its
landing guard and its final acceptance as overtaken by upstream, and investigate
the remaining Mac conformance gap before selecting a remedy.

[PR #8827](https://github.com/gastownhall/gascity/pull/8827) merged at
`2026-11-04T06:51:34Z` as
`4118c6ab7f33ce7f14d88ba5e2a44ebbb1ab26b9`. GitHub's PR record and local
ancestry were checked against main `44e0d11e46254026cf7a13c1a7c80ad852905f4e`.
The upstream change fixes the Bash 3.2 incompatibilities and allocates the
reference state root under `/tmp`. It also skips
`TestScreenReferenceIsConformant` when the procps `ps` probe fails. Its PR
reports remaining Mac process-observation failures (RPP-OBSERVE-006/010) and
peek hardcopy behavior (RPP-OBSERVE-016) before that skip. These are upstream
observations to diagnose, not native Mac results obtained by PM.

| Bead | Disposition |
|---|---|
| ga-aaowxzui | Completed original Bash diagnosis; retained as evidence |
| ga-5mwagbzi | Retire as abandoned; upstream overtook the repair and builder built/pushed nothing |
| ga-5i098avh | Closed as abandoned; the old delivery condition is retired |
| ga-01wqeilz | Removed from build intake first, then closed as abandoned; no all-five-tests PASS claimed |
| ga-8zkm955m | Sole new P3 diagnosis-only bead, `needs-investigation`, for gascity/investigator |

The successor carries `discovered-from:ga-5mwagbzi` and a reference to the
completed diagnosis. It has no blocking dependency on the abandoned delivery
chain. No implementation, test-authoring or replacement acceptance bead is
created. Normal admission and weekly budget rules apply.

### Investigation acceptance

1. Read current source/history and the pre-skip Mac evidence from #8827.
   Record the actual tested checkout, commands, outputs, exits, run/job URLs
   and relevant OS, Bash, `ps` and screen versions. Separate observations from
   source inference and Linux evidence.
2. Establish the BSD `ps` process-observation causes for RPP-OBSERVE-006/010
   with source anchors and captured evidence, preserving the identity,
   live/unknown and zombie-handling questions.
3. Establish the peek/hardcopy cause for RPP-OBSERVE-016 under macOS screen
   and whether it shares a cause with process observation. A `ps` change
   alone is not evidence that peek is repaired.
4. Report the evidence-supported next scope and any supported-platform,
   dependency, interface or semantics questions for architect/maintainer
   ruling. No remedy is selected and no skip is removed in this investigation.
5. Record findings on ga-8zkm955m and notify PM. Missing native access,
   required logs or an unresolved required ruling remains an explicit blocker
   with the bead open; a skip or Linux pass is not native Mac evidence.

Carry forward the validator/builder warning: test-only commit
`29fabcaf6b64eb09fdf96ad5a51c25fb07402205` on
`origin/fix/ga-5mwagbzi-screen-tests` conflicts substantively with current
main, and its long-TMPDIR regression no longer goes RED with the upstream
`/tmp` allocator. Keep that branch as historical evidence; do not cherry-pick
it as a ready regression or claim the old RED still applies.

The later goal is to remove #8827's Mac conformance skip after diagnosis and
any required ruling. The investigation authorizes no source/test changes,
runtime installation/restart, CI policy changes or implementation. Existing
parser acceptance ga-qf5sdqj stays independent. Mayor's separate check of
#8808 is outside this disposition. The rest of the original plan is preserved
below as historical context; its retired repair and acceptance criteria are
not active assignments.

## Original plan — 2026-11-04 (historical)

PM plan for ga-b3j8zgis, 2026-11-04. Keep the five screen contract tests
portable, as the mayor directed in gcg-344065. Diagnose the unexplained Mac
failures before starting a repair. This is P3 agent-minted machinery work;
normal admission and weekly budget limits apply. It blocks no current delivery.

### Evidence and scope

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

### Work graph

| Bead | Scope | Intake and prerequisite |
|---|---|---|
| ga-aaowxzui | Diagnose the four inventory/identity failures on Mac | Investigator, `needs-investigation`; begin with existing evidence |
| ga-5mwagbzi | Repair all five test portability failures | Validator, `needs-tests`; native blocks dependency on completed diagnosis; same bead proceeds to builder |
| ga-5i098avh | Verify the repair actually lands on main | Unrouted `gate-tracker` and `parent-record`; PM watches delivery |
| ga-01wqeilz | Record final Mac acceptance evidence | Builder, `ready-to-build`; native blocks dependency on the landing guard; evidence only |

Each scope carries `discovered-from` provenance. Native `blocks` edges enforce
the ordering shown above. The landing record is a condition rather than another
implementation task.

### Acceptance and boundaries

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

### Delivery

Commit and re-verify this exact plan path before closing the PM root. Close the
root as planning complete, not repaired. Route the three work beads with their
intake labels and `gc sling --nudge`, verify route or admission-deferral state,
and mail each distinct downstream role. Respect any budget refusal without retry,
priority change, label removal or a manual build. The condition record stays
unassigned and unrouted. No external tracker import, test authoring, production
change, runtime operation, CI policy change or merge is part of this PM task.
