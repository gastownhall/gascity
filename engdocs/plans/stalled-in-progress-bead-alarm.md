# Stalled in-progress-bead alarm plan

*Status: implementation-ready decomposition. Producing bead:
`ga-fh5571.1`. Architecture source: `ga-fh5571`.*

## Outcome

An operator gets a single, durable alarm when an `in_progress` bead stops
changing beyond its priority-scaled threshold even though its session remains
apparently healthy. The platform sends one configured mail and writes one
stable supervisor-log line for each continuous stall episode. `gc doctor`
reports the same condition on demand. The alarm never kills, restarts,
reassigns, or otherwise mutates the work or its session.

The periodic path is a cooldown order executed by the orchestrator with no
agent turn. Detection uses the bead's `updated_at`, which the architecture
investigation proved is not advanced by synthetic lease heartbeats.

## Grounded current state

| Boundary | Current evidence | Consequence |
| --- | --- | --- |
| Architecture bead `ga-fh5571` | Ratifies `updated_at` as the signal, priority-scaled thresholds, bead-keyed episode state, one configured mail plus one log line, a read-only doctor check, and a cooldown order. | This package decomposes the decision; it does not reopen signal, recovery, or scheduling mechanism choices. |
| Current `origin/main` and all-branch history | No stalled-bead episode, `gc beads stall-check`, or matching order implementation exists. The earlier `ga-k3388c` bead was closed before implementation as a duplicate of this PM intake. | There is no working slice to port and no live duplicate implementation bead. |
| Startup-health work | `ga-o04bfr.1.1` produced a bead-backed episode implementation on `builder/ga-o04bfr.1.1`, but review `ga-xhf54z` is currently rejected and the code is not on main. | Use its concrete shape as a precedent only. Do not import an unmerged dependency or create a generic episode abstraction. |
| Configured escalation sibling | `ga-o04bfr.1.2` remains open and proposes the first configured startup-health escalation recipient. | The foundation builder checks its live state before naming this alarm's field: reuse a shipped field or apply the architect-approved provisional scope and record the convergence point. Do not modify the sibling's scope. |
| Incident-specific modal fix | The Claude feedback-survey nudge repair is separate and has landed through PR #5849. | This alarm covers the general silent-stall class and must not duplicate provider-specific modal handling. |
| API and event wire | The approved design needs a CLI command, domain/store behavior, doctor visibility, mail, logging, and a pack order; it specifies no HTTP/SSE or new event type. | Keep the implementation off the API/event wire. If scope expands there, the API control-plane and generated-contract gates become a separate reviewed change. |

No external tracker skill is installed in this PM session, so tracker import was
a no-op.

## Work packages

### 1. RED contracts — `ga-fh5571.1.3`

The validator authors the smallest owning failures for threshold boundaries,
episode transitions and persistence, exactly-once delivery, log-only mode,
doctor read-only behavior, and installed order shape. Tests use injected time
and `t.TempDir()` where the real store boundary matters; they add no sleeps or
open-coded polling.

### 2. Detection and episode foundation — `ga-fh5571.1.4`

Add the priority-threshold and escalation configuration, pure stale-work
detection, and bead-metadata-backed `StalledBeadEpisode` persistence. The
foundation is keyed by target bead ID, propagates store errors, excludes its
tracking records from ready work, and never writes the target bead. It owns no
CLI, doctor registration, or order activation.

### 3. Alarm command and delivery — `ga-fh5571.1.5`

Expose `gc beads stall-check` as a thin projection over the shared detector.
One sweep scans all `in_progress` priorities and produces one configured mail
plus one stable supervisor-log line on the first crossing of a continuous
stall. Empty recipient is successful log-only mode; failed mail remains
retryable; recovery is deliberately absent.

### 4. Read-only doctor visibility — `ga-fh5571.1.6`

Register an on-demand doctor check that reports the same stale set with bead
ID, priority, age, and threshold context. It remains available while the
orchestrator runs and performs no mail, episode write, target mutation, or
session/runtime action.

### 5. Cooldown-order activation and final gates — `ga-fh5571.1.7`

Add the core-pack cooldown order with `no_work_gate = true` and
`idempotent = true`, prove it is present in the installed pack, regenerate the
CLI reference from Cobra source, and run the focused, sharded, docs, vet, and
pre-commit gates. This final package also audits that the delivered feature
introduced no hardcoded role, auto-recovery, API/event wire, or overlap with
the provider-specific modal fix.

Each child bead carries its complete measurable acceptance criteria in its
notes.

## Dependency graph

```text
ga-fh5571.1.3  RED tests (validator)
        |
        v
ga-fh5571.1.4  detection, config, and episode persistence
        |
        +--------------------+
        |                    |
        v                    v
ga-fh5571.1.5          ga-fh5571.1.6
CLI alarm delivery     doctor visibility
        |                    |
        +----------+---------+
                   |
                   v
ga-fh5571.1.7  cooldown order, generated CLI docs, final gates
```

The CLI and doctor increments are independent once the common foundation
exists. The final order package waits for both so installed automation and
on-demand visibility are verified together.

## Acceptance mapping

| Architecture requirement | Owning package |
| --- | --- |
| All-priority scan and priority-scaled thresholds | `.3`, `.4`, `.5` |
| Bead-ID-keyed, restart-safe episode state | `.3`, `.4` |
| One mail and one log line per continuous stall | `.3`, `.5`, `.7` |
| Clear/re-arm on real bead progress or status exit | `.3`, `.4`, `.5` |
| No target/session mutation and no auto-recovery | every implementation package; final audit in `.7` |
| Read-only `gc doctor` visibility | `.3`, `.6` |
| Orchestrator-owned cooldown execution with no agent dependency | `.3`, `.7` |
| Configured recipient, empty log-only mode, zero hardcoded roles | `.3`, `.4`, `.5`, `.7` |
| Generated CLI reference and repository gates | `.7` |

## Risks and controls

- **The sibling escalation field lands during implementation:** the foundation
  bead requires a live-state check at build start and records whether it reused
  the shipped name or used the approved provisional scope.
- **The unmerged startup-health branch looks like a dependency:** it is only a
  structural precedent. This feature remains independently buildable from
  current main, and no shared abstraction is introduced before two accepted
  implementations exist.
- **Concurrent sweeps double-send:** RED coverage owns repeated and overlapping
  sweeps; durable disposition changes only after successful delivery, and the
  final installed-order proof repeats the guarantee.
- **Thresholds create false-positive noise:** values and fallback are explicit
  config, with generous architecture defaults and boundary tests. Go performs
  only timestamp arithmetic; intervention remains human judgment.
- **Store or mail failure becomes silent:** every failure is surfaced with
  context. A mail failure is not persisted as sent, and empty-recipient mode is
  explicitly distinguished from delivery failure.
- **Tests add fleet contention:** TESTING.md's smallest-owner rule applies;
  clocks are injected, store composition uses `t.TempDir()`, and no real-time
  sleeps or broad duplicate journeys are admitted.
- **Generated docs drift:** Cobra source owns command documentation; the final
  package regenerates the reference and runs `make check-docs` rather than
  hand-editing generated output.

## Handoff

- `ga-fh5571.1.3` is labeled `needs-tests` for `gascity/validator` and is the
  only initially unblocked package.
- `ga-fh5571.1.4` through `.7` are labeled `ready-to-build` for
  `gascity/builder` and become actionable through the recorded dependency
  graph.
- Every child has both a parent-child edge to `ga-fh5571.1` and a
  `discovered-from` edge to the original architecture bead `ga-fh5571`.
- The producing bead closes only after this exact plan path is committed and a
  scoped status re-check proves it clean.
