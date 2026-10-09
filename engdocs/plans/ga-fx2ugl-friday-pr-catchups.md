# Friday PR catch-up merges

PM root: **ga-fx2ugl**. Source: mayor mails **gm-wisp-cjj244l**,
**gm-wisp-nb4yrng**, and **gm-wisp-7j8mbjk**.

Prepare two existing PRs for the **2026-10-09 10:00Z** MPR reset
(Friday 03:00 PDT). The mayor expressly authorized Codex routing for these
catch-ups before reset. Both remain on the outage deny list until reset;
changing that list or merging a PR belongs to the existing MPR/operator path.

## Initial intake baseline and scopes

PM fetched main at `1cfa66847f7def9d45569fd02c5d657545e92a43` and both
live PR heads. At intake, GitHub reported their original heads; its mergeability
response was `UNKNOWN`. Local `git merge-tree --write-tree --name-only`
independently identified exactly these conflicts, without editing source.

| PR | Catch-up bead | Priority | Pinned head and lease | Conflict |
| --- | --- | --- | --- | --- |
| #7334 | **ga-5tqdjw** | P1 | `2a8c4c7322eb84fbcf6c94f7423272471cfdea16` | `internal/runtime/proctable/scan_linux_test.go` |
| #3842 | **ga-m3rfm0** | P2 | `f41c95e78bd04375c92ea8342ac92f6f4de1e088` | `cmd/gc/BUILD.bazel` |

Both are independent `ready-to-build` scopes routed to **gascity/codex**.
Their complete acceptance criteria and Build instructions live in their notes.
Priority expresses order; neither catch-up blocks the other's implementation.
Existing tests supply verification, so no duplicate test-first or build bead
is required. Two bounded scopes meet the planning workflow's 1–10-child exit
criterion; no additional architecture, UX, or feature work is introduced.

For **#7334**, start from its pinned head and merge fresh main. Keep both
sides' tests exactly once and preserve the settled **ga-f0tblg** parent-read
contract and upstream #7365 sweep behavior. Production `scan_linux.go`
auto-merges. Verify the automatic merge as well as the manual test resolution,
run `go test ./internal/runtime/proctable/...`, and record actual affected test
results. The child retains `gc.fixes_tracker=ga-q17kpi` because its head carries
the existing orphan-selection fix. Tracker closure requires main landing.

For **#3842**, start from its pinned head and merge fresh main. Regenerate
`BUILD.bazel` from the merged source with the repository's Bazel sync/Gazelle
workflow, preserving both sides' sources and dependencies. Never choose one
side of generated data. Verify generation leaves no drift, build `cmd/gc`, and
run the affected continuation, claim, and turn-bound tests. Its old June
changes-requested review is not additional catch-up scope.

## Lineage and publication

- **ga-5tqdjw** carries `discovered-from` edges to this PM root and the
  existing mayor-owned conflict audit **ga-ap88oi**. Do not file another tracker.
- **ga-m3rfm0** traces to this root and prior deploy **ga-3oog6w**. No current
  open #3842 conflict audit was found; add its real edge if one appears later.
- Each worker creates an owned isolated branch from the pinned PR head and
  **merges main without rebasing**. Prove that both the original cleared head
  and the chosen merged-main SHA are ancestors of the resulting head.
- Each resolution receives a fresh quick review, followed by a fresh deployer
  gate for the resulting SHA. Existing clearances **55861287894** (#7334) and
  **55890099459** (#3842) remain evidence for their original heads only.
- The **mayor alone** lease-pushes the gated new head to
  `quad341:deploy/ga-tngr86-gate` (#7334) or
  `gastownhall/gascity:work/ga-zdunsh-hook-claim-continuation-nudge` (#3842),
  using the full pinned leases above, and verifies remote head identity.
  The deployer posts clearance for the newly published SHA. Mayor closes
  **ga-ap88oi** after verified gated-head publication, not at build completion.

Re-read live PR and bead phase before mutation. If a head or lease moved,
reconcile with the mayor rather than overriding it. PM writes requirements
and this plan; workers resolve and test code. Neither PM nor Codex pushes an
original PR branch, opens a replacement PR, merges it, or reuses stale clearance.
No installed external tracker skill was found; tracker import is a no-op.

## Published-head CI investigation

Updated **2026-10-09 05:07Z** following mayor order **gm-wisp-tzfxsr0**
and ownership update **gm-wisp-hazis1o**.
Both catch-ups received independent reviews and full local release gates.
PR #3842 is published at `80a6ca8bf68c5d82ba36c29db7307e586774a508`
with new clearance **55954193446**. PR #7334 is published and mergeable at
`7e3e75149d58e7a6f8f157f4186d56e4855d7c3c`; mayor closed conflict audit
**ga-ap88oi** after publication. Its clearance is now withheld because the
published GitHub acceptance run failed `TestDashboard_PrintsSupervisorNotice`
at `test/acceptance/dashboard_serve_test.go:32` after 15.71 seconds, reporting
that the supervisor did not become ready.

The existing condition tracker is **ga-01huv8**. Deploy bead **ga-p5lgoe**
is open and unassigned, with a blocking dependency on that tracker and
`gc.gate_verdict=HOLD`. Earlier local passes remain evidence for their tested
tree; they do not resolve this later failure. The test is unchanged, but its
child binary imports the changed proctable package, so causality remains open.

Mayor-owned **ga-sa1gi4** is the single active investigation, P1, routed to the
generic **gascity/codex** pool and claimed by **gm-wisp-yeesp4o**. Its
`discovered-from` edge names the existing tracker. PM's concurrently prepared
**ga-kdoc9o** was closed as superseded after live verification that the mayor's
scope was already in progress; no second investigation was performed. The
tracker remains a record; it is never dispatched. This is one specific
delegated task while the Claude investigator is unavailable before 10:00Z,
with no role or provider configuration change.

The worker first examines the mayor's hypothesis: PID namespaces may make
supervisor parent processes unreadable and expose the changed orphan-root
selection during startup. This is a hypothesis, not a root-cause finding.
The investigation must:

1. Recover the immutable CI-tested merge tree and its matching base, and trace
   the supervisor startup paths that could reach the changed behavior.
2. Run the named acceptance test or a direct supervisor readiness probe under
   a PID namespace on both trees, at least ten runs per ref, and ten runs per
   ref on the host without the namespace. Record commands, binary identity,
   UID, parent visibility, execution conditions and actual PASS/FAIL/SKIP
   results. A refused setup or skipped body is an explicit environment gap;
   a direct probe must state its differences from the failing acceptance body.
3. Search main and unrelated PR acceptance runs from approximately September
   25 through October 9 for the same test and readiness-failure signature.
   Record real run/job links and SHAs; other failures or copies of this run
   do not establish pre-existence.
4. Hand a verifiable causal proof to mayor/deployer, or file exactly one
   reproduction-backed fix bead after deduplication. That fix carries
   `gc.fixes_tracker=ga-01huv8`, discovery edges to this investigation and the
   tracker, pinned reproduction evidence, measurable acceptance criteria and
   a narrow Build scope. Implementation is outside this investigation.

The failing run is **37884958244**, attempt 1, job **113672834041**, acceptance
shard 8 of 18. Its log records checkout
`4df1b2dd94dd6b069903bd92cc89830d8d728bc9`, merging the published source into
`42b11ccbd1b1beefedda115e654dd72c80d74778`; the workflow's subsequent
fresh-merge step must also be verified. Evidence is retained under
`/var/tmp/ga-p5lgoe-gate.anlvwra0`, including the CI log, ownership report,
full import closure and reachability record. The investigation bead contains
the complete evidence pointers and handoff requirements. Both the immutable
CI tree and the worker's freshly materialized comparison trees must be named;
a result from one tree is not automatically evidence about another.

Passing paired runs alone leave this CI-only condition inconclusive. The
worker reports uncertainty and missing conditions rather than inventing a
proof or a speculative fix. Mayor/deployer own verified hold resolution and
fresh gating; the worker does not retry GitHub CI, edit implementation, push
the published branch, post clearance, merge, raise timeouts or reduce coverage.
The gate blocker is repointed to an actual fix when one is filed, and the
condition tracker closes only after its remedy lands. Runtime tracker
**ga-q17kpi** and the PM plan-publication wait remain until confirmed main
landing. No new duplicate gate, tracker or test/build pair is created.
