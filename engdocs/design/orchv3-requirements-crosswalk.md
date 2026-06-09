# OrchV3 Requirements Crosswalk

| Field | Value |
|---|---|
| Status | Draft |
| Date | 2026-06-08 |
| Author(s) | Codex |
| Primary issue | [#1709](https://github.com/gastownhall/gascity/issues/1709) |
| Tracker | [#2503](https://github.com/gastownhall/gascity/issues/2503) |
| Related | [#2120](https://github.com/gastownhall/gascity/issues/2120), [#2504](https://github.com/gastownhall/gascity/issues/2504) |

This note maps the OrchV3 proposal and requirements in #1709 against the
current formula authoring/runtime direction and the older OrchV3 tracker in
#2503.

It intentionally uses product-facing terms only:

- formula authoring model
- formula runtime semantics
- package/import contract
- run/event/state model
- host/runtime integration

It does not introduce or depend on any public naming for underlying
implementation work.

## Summary

The current formula-runtime direction covers a large share of the semantics that
motivated OrchV3: structured values, typed outcomes, authored gather policies,
formula composition, event streams, run events, editor diagnostics, and
conformance gates.

Beads and convoys remain Gas City host concepts. They should not become part of
the formula authoring surface. Formula code can receive structured data and
produce structured data; the host/adapters decide how those values correspond to
beads, convoys, ready sets, and bead graph mutations.

The remaining OrchV3 risk is mostly product/runtime integration:

- durable Runs
- restart/recovery
- convoy and bead integration
- human-in-the-loop
- operator intervention
- dashboard and bead-native observability
- package/module import implementation

The useful planning split is therefore:

1. Keep formula semantics and authoring completeness moving in the formula
   workstream.
2. Stabilize the package/import contract before module/package semantics land.
3. Treat Run persistence, bead projection, HITL, and dashboard work as Gas City
   integration lanes.

## Status Legend

- **Covered**: the current formula-runtime direction directly satisfies the
  requirement at the authoring/semantic level.
- **Partial**: the current formula-runtime direction gives the semantic shape,
  but Gas City host/runtime/storage/UI work remains.
- **Open**: the requirement still needs design or implementation.
- **Deferred**: intentionally postponed to a later integration lane.

## Requirement Crosswalk

| Requirement | Status | Reading | Suggested plan |
|---|---:|---|---|
| R1. Runs are first-class, persistent, addressable entities | Partial | The runtime direction has run events, run handles, and debugger projections, but Gas City does not yet have durable Run entities. | Define the Gas City Run entity and query API after the host/run-event protocol stabilizes. |
| R2. Run state survives controller restarts | Open | The event-stream model is replay-friendly, but no durable restart contract exists yet. | Start with an append-only run event log and minimal checkpoints before introducing heavier storage. |
| R3. Sessions are Runs | Open | Session unification is directionally aligned, but not selected for the immediate formula-runtime release. | Fold into the agent/session/prompt design lane. |
| R4. Steps can pause awaiting input | Open | Event streams can represent request/reply, but HITL pause/resume is not first-class yet. | Design a durable human-task state tied to Run/step events. |
| R5. Operator mutations have defined downstream semantics | Open | Skip, abort, re-prompt, relabel, and kill-polecat need explicit semantics. | Create an operator-intervention design packet after Run persistence and HITL. |
| R6. Formulas operate over a convoy input contract | Partial | Formula inputs can represent host-provided work data, but beads and convoys are host concepts and must not become part of the formula authoring surface. | Treat this as host/adapter work: map bead or convoy input into ordinary formula input values through the host boundary. |
| R7. Step input mode is author-declared per step | Partial | Whole-input steps, scatter/gather, and stream map/reduce cover general execution shape. Convoy, bead, ready-set, and polecat details remain Gas City host behavior. | Hold a focused beads-integration design discussion: which behaviors are formula patterns, which are host adapters, and which require native Gas City scheduling. |
| R8. Steps can produce structured convoys as output | Open | Formula steps can produce structured output, but no host contract turns that output into bead graph structure yet. | Define a host-side typed convoy handoff shape and adapter that writes parent/child/dep bead edges. |
| R9. Sub-formulas are addressable, namespaced, standalone units | Partial | Package/module work subsumes the naming/import/versioning need. If the goal is tree expansion, macro expansion may also cover the authoring shape. | Keep this in the modules/packages lane; revisit tree-expansion ergonomics in the macro lane. Do not solve it with PackV2-only exports. |
| R10. Sub-formula invocation expands inline into the parent Run | Partial | The current direction treats composed formula work as part of one correlated run world. Implementation still needs hardened provenance and host support. | Ensure nested formula execution contributes to the parent run event stream with clear boundaries. |
| R11. Steps record sub-formula provenance | Partial | Source and run-event metadata exist in the formula-runtime direction, but sub-formula invocation provenance needs to be explicit. | Add invocation provenance to run events and dashboard/debug projections. |
| R12. HITL disposition class | Open | HITL can be modeled by request/reply data today, but runtime scheduling/notification policy is open. | Define HITL as a Run/step state and outcome family, not as a one-off session mechanism. |
| R13. Pending HITL steps are surfaced to humans | Open | This is primarily client/runtime work. | Define the durable notification source and dashboard surface. |
| R14. HITL authorization | Open | Needs Gas City identity/authorization policy. | Keep approval authority in Gas City; the formula runtime should carry typed request/response data. |
| R15. Gather outcomes follow author-declared policy | Covered | Authored gather/reduce policies replace the old hardcoded any-fail behavior. | Keep conformance and real-formula examples around this behavior. |
| R16. Succeeded with reduced coverage | Covered | Degraded outcome semantics are present in the formula-runtime direction. | Map legacy soft-fail vocabulary carefully during migration. |
| P1. Drain completion is quiescence | Partial | Stream gather/reduce terminal behavior covers part of the model, but convoy ready-set quiescence is not implemented. | Prototype as an event-backed formula pattern over ready-work streams. |
| P2. Recursively discovered work joins active drain | Open | Requires dynamic bead/convoy discovery and scheduling. | Defer until convoy adapter and event-backed drain prototype exist. |
| R17. Stdlib sub-formula library | Open | The package/module model makes this possible, but no standard library exists yet. | After modules/packages, seed shred-plan and scatter-gather packages. |
| R18. Pack distribution extends to author-supplied sub-formulas | Covered | Package distribution is the right unit for reusable formulas and macros. The existing import contract can carry these packages if kept stable. | Keep `[imports.<key>]` as the rendezvous with `source` and `version`. |
| R19. Sub-formula version pinning | Covered | Content versioning is package-level only. There is no separate sub-formula or per-content versioning story. | Use package import `version`; explicitly rule out sub-package content versioning unless a future product decision reopens it. |
| R20. Dashboard renders Runs as central object | Open | Runtime events can feed a dashboard, but the dashboard/API are not built. | Define run list/detail/timeline APIs from the event model. |
| R21. Operator mutations reflect immediately | Open | Requires a Run mutation API and live projection. | Design after R5. |
| R22. Run identity/provenance projected onto affected beads | Open | The formula-runtime side can carry provenance; Gas City bead paths need to write it. | Add Run ID and step/invocation provenance to bead create/claim/mutate/close paths. |
| R23. Snapshots are collected during Runs | Open | Formula source/input/output snapshots need a persistence contract. | Start with formula source snapshot, input snapshot, event log, and terminal outcome; add bead/file diffs later. |

## Technical Consideration Crosswalk

| Technical consideration | Status | Reading | Suggested plan |
|---|---:|---|---|
| TC1. Typed dispositions | Partial | Typed outcomes exist in the formula-runtime model and should be treated as the normative disposition model. Gas City Go paths still need typed disposition APIs. | Project the formula-runtime outcome model into Go-side typed disposition values. Change the model only for legitimate product/runtime requirements, not to preserve legacy stringly shapes. |
| TC2. Run as a bead | Open | Still a Gas City architecture decision. | Prototype with an append-only event log; decide whether a Run bead owns or references structured payload. |
| TC3. Agent ABI | Open | The agent handoff contract remains under-specified. | Revisit after agent/session/prompt design and host integration settle. |
| TC4. Data flow and variable scoping | Covered | Structured values, input schemas, local bindings, outcome refs, dispatch, and formula input/output flow address the core need. | Keep conformance strict; use real formula corpus inspection to find design pressure. |
| TC5. Gather policy expression | Covered | Policy is authored in formula steps rather than a separate TOML expression DSL. | Preserve this direction; avoid inventing a second policy DSL. |
| TC6. HITL primitive design | Open | Needs durable human-task lifecycle and typed responses. | Treat as a dedicated design lane. |
| TC7. Operator intervention semantics | Open | Not yet specified. | Couple to R5 and Run mutation API. |
| TC8. Run visualization | Partial | The desired dashboard experience should be driven by the run event stream. Debug projections exist; the remaining work is integration with the existing in-progress visualization experience. | Feed the current visualization from run events first. If required events are missing, add them coherently to the run event model rather than inventing a parallel feed. |
| TC9. Migration/backwards compatibility | Partial | Existing formula TOML/graph behavior remains compatibility input. | Use corpus inspection and explicit versioning/compatibility gates. |
| TC10. Optimized storage/execution | Partial | The current host/runtime direction addresses formula execution outside bead explosion, but Gas City integration remains. | Land host protocol first; integrate with supervisor/control plane later. |

## What #2503 Should Become

#2503 is still useful as the OrchV3 coordination umbrella, but its framing is
older than the current formula-runtime direction. It should be updated to track
these integration lanes rather than reopening already-settled authoring and
semantic questions.

Suggested #2503 structure:

1. **Formula authoring/runtime semantics**
   - current formula syntax and conformance
   - source/diagnostic/editor readiness
   - real formula corpus coverage

2. **Package/import contract**
   - `[imports.<key>]` with `source` and `version`
   - package/module name resolution
   - visibility and re-export
   - registry metadata hardening

3. **Run host and persistence**
   - run creation
   - run event stream
   - run status/query
   - restart/recovery
   - snapshots

4. **Gas City integration**
   - convoy input adapter
   - convoy output adapter
   - bead provenance projection
   - bead/convoy host-boundary rules
   - dashboard and bead-native correlation

5. **HITL and operator control**
   - human task model
   - approval/rejection authorization
   - skip/abort/re-prompt/relabel/kill semantics

## Package/Import Contract Notes

The package name is not package identity. It is the suggested binding/display
name unless overridden by the importer.

The current import shape remains the right rendezvous:

```toml
[imports.<import-key>]
source = "<registry-uri-or-path>"
version = "<version>"
```

Rules to preserve:

- `source` is the stable location or locator.
- `version` is the selected package version.
- The import key is local binding metadata, not global identity.
- Path-like sources can use `source`, but they do not provide strong global
  identity.
- Registry handles, cache keys, and acquisition metadata should not leak into
  formula/package name resolution.
- Versioning is package-level. Reusable formulas, macros, and other package
  content do not get independent version pins.

## Immediate Plan

1. Keep the public OrchV3 tracker focused on product/runtime terms.
2. Update #2503 only with sanitized requirement buckets and links to concrete
   implementation issues.
3. Let the pack registry lane front-load import-contract hardening.
4. Let the formula-runtime lane finish current release smoke and conformance.
5. Start a dedicated beads-integration discussion for host-side bead/convoy
   mapping, drain behavior, and output handoff.
6. Start a dedicated Run host/persistence design once formula smoke stabilizes.
7. Defer HITL/operator mutation details until Run state and host boundaries are
   less fluid.

## Open Questions

- Should a Run be represented directly as a bead, or should a Run bead point to
  an append-only event log?
- What is the minimum restart-safe snapshot for a Run?
- How much HITL can be represented with ordinary request/reply events before a
  first-class runtime disposition is required?
- Which bead/convoy behaviors belong in host adapters versus reusable formula
  patterns versus native Gas City scheduling?
- What exact run events does the current visualization need that the formula
  runtime does not already emit?
