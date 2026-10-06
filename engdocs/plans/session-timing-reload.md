# Session timing reload requirements

Goal: after editing a session timing setting, an operator can tell whether the running provider uses the new value or needs a supervisor restart. Root: `ga-mrx2uy`, routed by mayor at P3 on 2026-10-06.

## Problem and evidence

During the October 6 startup incident, the city's desired `setup_timeout` changed from the 10-second default to 90 seconds. Reload reported success, leading operators to believe the longer budget was active. The root's source analysis indicates that seven tmux timing fields are copied at provider construction and that a timing-only reload keeps the original provider.

PM confirmed the same construction and reload predicate on `origin/main` at `0bf48101d7`. This remains a source finding: no runtime reproduction was performed by PM, the exact deployed binary revision is unknown, and no start under an outage established the claimed behavior. An isolated runtime reproduction is required before implementation.

## Required outcome

The architecture decision selects one of the two mayor-authorized contracts:

- Changed timing values apply to subsequent applicable session operations after reload, without requiring a supervisor restart.
- Reload explicitly names the changed timing keys, says their new values are inactive until a supervisor restart, and gives the operator that required action.

The covered keys are `setup_timeout`, `setup_max_timeout`, `nudge_ready_timeout`, `nudge_retry_interval`, `nudge_lock_timeout`, `debounce_ms` and `display_ms`. A successful config parse or `gc config show` is evidence of desired values, not proof of the provider's active values. The selected contract must preserve active sessions and assigned work; a timing-only edit must not force an agent restart.

## Work packages

| Order | Bead | Route | Acceptance |
| --- | --- | --- | --- |
| 1 | `ga-zn6nl6` | architect (`needs-architecture`) | Reproduce one timing-only reload with a known source-built binary in an isolated city/socket; record the subsequent supervisor-driven operation, choose a contract, and document lifecycle and provider scope. |
| 2 | `ga-h196xt` | validator (`needs-tests`), then builder on the same bead | Demonstrate RED for the selected contract, cover the seven fields and reload controls, implement the recorded outcome under `Build:`, and record GREEN and applicable process/race evidence. |
| 3 | `ga-gkfe1z` | builder (`ready-to-build`) | Existing operator guidance explains when the seven settings take effect and how to apply them; generated reference content is edited at its source and documentation gates pass. |

All three carry `discovered-from:ga-mrx2uy`. `ga-h196xt` depends on `ga-zn6nl6`; `ga-gkfe1z` depends on both. The implementation scope is one bead throughout validator-to-builder handoff. The documentation bead has a separate prose scope and must verify the described behavior's actual landing or explicit joint shipment before publishing; implementation-bead closure alone is insufficient.

## Verification and decision boundaries

Architect records the binary SHA, isolated command, old/new values, reload output and observable session-operation result. The reproduction must not alter the live factory or depend on an actual DNS outage. Architect supplies expectations for in-flight operations and non-tmux providers before RED/build begins. If the finding does not reproduce, return the discrepancy to PM instead of implementing a speculative remedy.

Acceptance includes unchanged/equivalent timing controls, invalid/failed reload retaining the old working state, and preservation of `startup_timeout`'s existing live behavior and unrelated reload changes. For restart-required reporting, verify the old operation budget remains until restart and the new budget applies afterward. For live application, verify the subsequent operation uses the new value and follows the recorded lifecycle contract. Each implementation PR links a documented public issue and its end-to-end evidence.

The implementation method belongs to architect and builder within the existing runtime, config and worker boundaries. Update the owning `AGENTS.md` if an invariant or boundary changes. The public documentation follows the Gas City docs skill and updates generated configuration text at its owning source.

## Dependencies and risk

The beads-side worktree script fix `be-f6ldvx` and incident `be-an16sn` are context in the beads store, not blockers for this Gas City contract. Removing the temporary city-level 90-second override belongs to that incident's verified closeout. Expanding the API to expose the active provider's complete session configuration is separate work.

The principal risk is treating desired config as runtime evidence or accidentally disrupting live sessions while applying a timing change. The reproduction prerequisite and explicit lifecycle acceptance address both. Architecture selection, runtime verification and downstream gates remain outstanding when this PM planning bead closes.
