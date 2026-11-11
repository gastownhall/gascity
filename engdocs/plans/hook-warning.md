# Truthful custom-query warnings on split cities

PM intake: `ga-0dsa4uol` · priority P3 · planning base `6bd609f5bfb`.

Operators need to distinguish an empty work queue from a failed read. A split
city currently prints a categorical warning that every custom query reads one
store, even when that query uses the federated `gc ready` reader. Agents have
interpreted that warning as a refusal and escalated a healthy empty queue.

## Evidence and boundary

The mayor's reproduction records an empty deployer queue during the quiet window:
exit 1, JSON `[]`, and the misleading warning. PM independently reproduced the
same warning on 2026-11-11 after the quiet window, this time with exit 0 and a
nonempty deployer queue. The warning therefore does not establish a failed read.
Both probes were read-only; neither claimed work.

At the planning base, `Agent.FederationBlindOverrides` in
`internal/config/workquery.go` checks whether override fields are nonempty. Its
consumer, `warnFederationBlindOverrides` in `cmd/gc/class_store.go`, turns that
fact into an assertion about reader coverage. History locates the behavior in
`ede16cacde4` (#5161). It is still present on current main.

The separate `ga-zspenhc.4` lint/runbook change is already handed to review as
`ga-0mkk1wow`. It does not supply this runtime warning fix. Its textual-reader
analysis is not an instruction to add a shell parser or change custom-query
execution here. The city-owned split-storage runbook documents the old warning;
its owner can update that note after a verified runtime rollout.

## Work package and acceptance

This is one diagnostic scope on the existing bead. Refine and hand on
`ga-0dsa4uol` itself with `needs-tests`; validator later relabels and routes that
same bead to builder. There is no duplicate test/build pair and no prerequisite
requiring a separate child or dependency edge.

| Work bead | User outcome | Route | Dependency |
|---|---|---|---|
| `ga-0dsa4uol` | A custom federated query is never described as proven single-store or unable to see relocated work merely because it is custom | validator, then builder on the same bead | none |

Acceptance:

1. On a split city, a custom query that reads through `gc ready` never receives
   the categorical assertion that it reads only one store or cannot see the
   relocated coordination class. Silence, a debug-only note, or an explicitly
   conditional advisory satisfies this outcome. The fix must not claim to prove
   arbitrary shell coverage.
2. Preserve useful guidance about genuinely single-store custom readers: if an
   advisory remains, identify the configured key and agent, explain the
   conditional risk, and point to `gc ready`. If the shared path also emits a
   `scale_check` advisory, apply the same truthfulness rule there.
3. Preserve the existing no-warning behavior on single-store cities and for
   agents without overrides. Existing nil-writer/nil-agent guards remain safe.
4. Query text, template expansion, selection, store scoping, ordering, claim
   behavior and stdout JSON remain unchanged. Custom shell is still passed
   through under its existing contract. Do not change quiet-window or admission
   policy, topology, configuration fields, providers, or the ready reader.
5. Preserve hook exit semantics. With read-only `gc hook --json`, a successful
   nonempty queue remains exit 0, a successful empty queue remains exit 1 with
   `[]`, and a failed work-query read retains its existing failure status and
   diagnostic. A warning change cannot mask a failed read or turn a successful
   read into a refusal. Preserve the applicable existing `--claim` contract too.
6. Regression evidence covers a split city with direct `gc ready`, a wrapped or
   piped `gc ready` query, a single-store custom reader, no override, and a
   single-store city. Exercise the real diagnostic/hook path and the
   nonempty/empty/read-failure distinction using controlled fixtures. A text-only
   assertion disconnected from the caller is insufficient. No live city mutation
   or new real-infrastructure suite is required.
7. Validator records a named RED caused by the current false assertion, then
   hands this same bead to builder with these criteria and Build scope intact.
   Builder records named GREEN using Bazel. Tests must fail on the behavior,
   rather than on a missing helper name or an arbitrary replacement sentence.
8. Update adjacent comments or checked reference content only where needed to
   describe the final diagnostic. Preserve generated-document source discipline.
   If an invariant or boundary changes, update its owning `AGENTS.md` in the same
   change; the intended scope changes neither.
9. Run the affected Bazel targets and repository gates, with verbose filtered
   named PASS evidence where a filter supplies the proof. Include
   `//internal/config:config_test` if config diagnostics change, and
   `//cmd/gc:gc_test` for the hook path. Run `make bazel-sync` after adding code or
   test files/imports, and `make check` before handoff. Apply TESTING.md resource
   accounting to any added process load. Changes to API/session lifecycle are
   outside this scope.
10. Require zero complexity growth against the pinned target merge base. If the
    base's `make complexity-check` is green, the canonical merge must be green.
    Otherwise record base/merge rows with no worsening and a measured
    `COMPLEXITY_BASE_REF=<pinned-base> make complexity-diff` with no `new:`,
    `regressed:`, or deleted still-live function. Read the shared complexity rule
    before scoring; never refresh `engdocs/complexity-baseline.json` in this fix.

Build: correct the runtime warning and its explanatory comments, preserving the
original bug description and every acceptance criterion. Suppression versus
conditional wording is an implementation choice within the reported ask. New
reader detection, shell interpretation, static lint policy, query rewriting,
exit-code changes and broader federation work require a separate architecture
decision and are not prerequisites for this diagnostic correction.

## Handoff and completion

No external tracker skill is installed in the PM overlay or materialized local
skill directory; tracker import is a no-op. No tracker manifest is changed.

Commit and push this plan on its bead-specific branch, verify the exact tracked
artifact and remote head, then hand the same open content bead to validator.
Remove PM intake if present, release the PM claim, sling with `--nudge`, read back
the label/route and run `ready-visibility`. An admission-budget refusal remains a
refusal: preserve its deferral, do not retry or override, and report the actual
receipt. PM planning completion does not close the implementation bead or claim
the warning has shipped.
