# Plan: nudge-status follow-up

Work: `ga-c3oyx3m8` · Priority: P4 · Date: 2026-11-26 UTC

## Deliverable

Keep the mayor's three requested cleanups together in one small PR, on the
existing work bead. The source is review `ga-6rkckm6g`, passed at
`9d6266cae782eaebd106a619947680beb41ec4ea`, and deploy `ga-47xdfaui`.

1. Correct the two status-test comments that still describe materialization.
   Preserve their fake-provider isolation and assertions.
2. Correct the first-reference list in
   `engdocs/design/named-configured-sessions.md`: nudge status observes an
   identity without materializing a session. Preserve the other verbs' rules.
3. Include the underlying store-open cause in the unavailable-store status
   diagnostic, using the existing error-preserving opening facility. Preserve
   the existing classification, target identity and unsuccessful exit.

Re-locate all cited sites on landed main; review line numbers are hints. The
additional design-table observation in the review is outside this package.
Keep other resolver callers, mutating verbs, queue maintenance and output
schema behavior unchanged. This follow-up does not amend the reviewed PR.

## Landing gate and ownership

At intake, fetched main `77143ecd18ff6f0199dfee3a412825f1fd9fa8a2` lacked
`resolveNudgeStatusTarget`, and deploy `ga-47xdfaui` was in progress. The
cleanup must wait for that deploy's actual PR landing.

| Record | Purpose | Disposition |
| --- | --- | --- |
| `ga-47xdfaui` | Existing observe-only status deploy | Existing deploy and merge chain |
| `ga-2l9yjvzt` | Verify actual main landing and content | Open, unassigned, unrouted landing record; never sling |
| `ga-c3oyx3m8` | All three follow-up edits | Open, routed to PM, unassigned after sequencing |

The follow-up has same-store `blocks` edges to the deploy and landing record.
The landing record has `parent-record` and a `discovered-from` provenance edge;
it carries no implementation scope or worker intake label. The scheduling
graph has both prerequisites before the single follow-up, with no cycle.

Mayor accepted landing-record closure through the existing merge chain in
mail `gcg-17177601` and recorded ownership on the landing bead. Closure requires
the prerequisite PR and main squash SHA, normal squash/work-record evidence,
and inspection of main's observe-only status resolution and
`TestCmdGCIntegrationNudgeStatusPreservesAvailableStore` cases. A gate PASS or
closed build/deploy alone does not satisfy the record.

After verified landing, notify PM with `--notify`. PM re-reads the work bead,
main content and prerequisite outcomes, then routes this SAME P4 bead with
`needs-tests` to validator. Validator authors the diagnostic regression and
hands this same bead to builder. No second implementation bead, stacked
`base_ref`, premature admission attempt or budget override is authorized.
If the predecessor is abandoned or materially changes, leave the record open
and notify PM to revise scope.

## Acceptance

- All three edits above are present, with no additional behavioral change
  beyond the diagnostic cause. The original bead carries the complete scope.
- A distinctive injected store-open failure reaches the error from both text
  and JSON status invocations, with nonzero exit, unavailable-store
  classification and target identity retained. It produces no healthy status
  or misleading session-not-found answer. The regression fails before the
  diagnostic change and passes after it; test outcomes, not helper selection.
- Existing successful status output, unknown-target distinction, no
  materialization/start and queue/store-preservation coverage remain green.
  Use existing fake/file seams and preserve other nudge verbs' contracts.
- Named Bazel tests report real PASS lines, including the predecessor's
  integration-tagged preservation test under its matching configuration.
  `make check`, docs sync and applicable repository checks pass.
- Complexity has zero growth against the pinned base, without editing
  `engdocs/complexity-baseline.json`. Use gate `GATE_BASE_SHA`, or record the
  target merge base outside a gate. A green base requires a green merge
  check. Otherwise retain base/merge outputs with no worsened row and a
  successful diff measurement with no new, regressed or still-living deleted
  function; exit status alone is insufficient.

## PM disposition

Commit and publish this plan, verify owner notifications and release the PM
claim. Keep the work open with `needs-pm` behind both landing prerequisites.
The same-store landing record supplies the durable exit; no worker admission
or implementation completion is claimed by this plan. Tracker import is a
skip-safe no-op because neither supported importer exists.
