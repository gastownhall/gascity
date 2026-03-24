---
title: "Idle Session Sleep"
---

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-03-23 |
| Author(s) | Codex |
| Issue | N/A |
| Supersedes | N/A |

## Summary

Gas City already has a wake/sleep lifecycle for managed sessions, but
the current controller only uses it for explicit holds, waits, and
work-driven wake-ups. Configured agents stay warm indefinitely, and the
existing `idle_timeout` knob kills a session and immediately re-wakes it
on the same patrol tick. That reduces some stale-session problems, but
it does not actually reduce steady-state resource use.

This design adds an idle-sleep policy for managed sessions. Once a
session reaches a safe idle boundary and stays idle past its effective
keep-warm window, the controller drains it to `state=asleep` with
`sleep_reason=idle`. The next hard wake reason starts it again on
demand.

Gas City's recommended starter policy is:

- resumable interactive sessions: keep warm for `60s`
- fresh interactive sessions: keep legacy always-warm behavior unless
  explicitly opted into auto-sleep
- non-interactive sessions: sleep immediately after idle (`0s`) when the
  routed runtime has `full` idle-sleep capability; on `timed_only`
  runtimes, start with `30s` unless the operator explicitly accepts the
  false-idle risk of `0s`

That starter policy is expressed explicitly in config rather than being
silently activated by table presence. Agents with no idle-sleep policy
configured keep the current always-warm behavior.

## Problem

Today, configured agents remain awake just because they exist in config:
`WakeConfig` is unconditional for fixed agents and pool slots inside the
desired count. That is acceptable for a small city, but it wastes tmux
servers, pods, subprocesses, provider sessions, and controller churn
when the city has many pooled workers or long-lived interactive agents
that sit idle.

The obvious workaround, `idle_timeout`, is not the same feature. The
controller currently interprets it as "stop the process when idle, then
fall through to wakeReasons and start it again immediately if config is
still present." That behavior preserves an always-warm contract and
restarts stale sessions, but it does not let the controller converge to
an intentionally cold slot.

We want:

1. a real "sleep when idle" mechanism that reduces resources,
2. a sensible baseline for pooled versus interactive agents,
3. easy override points at workspace, rig, and agent scope,
4. no surprise context loss for agents that cannot truly resume, and
5. no deadlocks, pool churn loops, or hidden-controller decisions.

## Goals

- Let the controller converge eligible idle sessions to `asleep`.
- Keep the existing wake/sleep bead model instead of inventing a second
  lifecycle state machine.
- Make policy resolution predictable: explicit composed agent override,
  then rig class default, then workspace class default, then
  capability-gated legacy fallback.
- Preserve current behavior when idle sleep is not configured.
- Expose why a session is still awake, why it slept, and which policy
  source resolved its effective keep-warm window.

## Non-goals

- Replacing the existing `idle_timeout` restart semantics.
- Introducing a separate "suspended by idle policy" state distinct from
  `asleep`.
- Guaranteeing warm-context resume on providers that only support fresh
  starts.
- Changing pool occupancy semantics: asleep pool slots still count as
  realized slots.

## Current Behavior

### Wake contract

`wakeReasons()` currently returns reasons from:

- config presence (`WakeConfig`)
- attached terminals (`WakeAttached`)
- durable ready waits (`WakeWait`)
- pending work (`WakeWork`)

If no wake reason remains, the reconciler begins a drain and eventually
stops the runtime, writing `state=asleep`. This is already the right
mechanism for idle sleep.

### Why `idle_timeout` is insufficient

The current idle tracker checks `Provider.GetLastActivity()`. When the
timeout expires, the reconciler stops the session, marks it asleep with
`sleep_reason=idle-timeout`, and then immediately reevaluates wake
reasons in the same tick. Because configured agents still receive
`WakeConfig`, they are re-woken immediately. This is restart-on-idle,
not sleep-on-idle.

### Existing runtime signals

The controller already has optional runtime extension points:

- `GetLastActivity(name)` for activity timestamps
- `Pending(name)` for structured blocking interactions
- `WaitForIdle(name, timeout)` for safe interactive boundaries
- `IsAttached(name)` for terminal attachment detection

These signals are uneven across providers. The design must tolerate:

- full support (`tmux`)
- activity-only support (`k8s`, some `exec`)
- no meaningful idle-boundary support (`subprocess`)
- routed mixed capability (`auto`, `hybrid`)

## Design Principles

1. Keep one session lifecycle model. Idle sleep uses `asleep`, not a
   new administrative state.
2. Policy resolution must be explicit and inspectable.
3. Default behavior must not silently destroy context for fresh-only
   agents.
4. "No wake reason" is necessary but not sufficient. The controller must
   also verify a safe idle boundary before sleeping.
5. Sessions with unresolved work, waits, attachments, or blocking
   interactions must remain awake.
6. Pool and dependency behavior must stay stable when sessions go cold.

## Proposed Design

### Reconciler execution model

Idle sleep is implemented as one controller pass structure, not as a
second hidden state machine. Each patrol tick runs these stages in order:

1. normalize idle-sleep-owned durable metadata and refresh per-session
   policy/capability snapshots
2. compute direct hard-wake roots (`work`, `wait`, `attached`,
   operator/attach intent, `pending`) before any dependency propagation
3. propagate dependency wake upstream over the template DAG and derive
   any dependency-induced pool floor
4. evaluate per-template pool cardinality using
   `effective_desired = max(pool_check_desired, dependency_floor)`, then
   decide wake-before-create versus trim
5. evaluate each realized session bead in dependency order using the
   resolved wake reasons plus structural precedence rules
6. advance in-flight drains and finalize any stop/wake race outcomes

That model keeps dependency closure and pool-floor math in explicit
pre-passes, while all actual lifecycle decisions still converge through
the existing session wake/drain path.

### 1) Add a dedicated idle-sleep policy surface

Do not overload `idle_timeout`. Keep restart-on-idle and sleep-on-idle
as separate knobs.

Add:

- top-level `[session_sleep]`
- `[rigs.session_sleep]`
- `sleep_after_idle` on `[[agent]]`
- `sleep_after_idle` on `[[rigs.overrides]]`
- `sleep_after_idle` on `[[patches.agent]]`

Conceptually:

```toml
[session_sleep]
interactive_resume = "60s"
interactive_fresh = "off"
noninteractive = "0s"

[[rigs]]
name = "payments"
path = "/repo/payments"
[rigs.session_sleep]
interactive_resume = "5m"

[[agent]]
name = "mayor"
sleep_after_idle = "off"

[[rigs.overrides]]
agent = "worker"
sleep_after_idle = "0s"
```

Semantics:

- omitted value: inherit
- duration string: enable idle sleep with that keep-warm window
- `"off"`: disable idle sleep
- empty string: invalid configuration

Class keys are explicit:

- `interactive_resume`
- `interactive_fresh`
- `noninteractive`

This policy is separate from `agent_defaults`. `agent_defaults` is a
useful authoring convenience, but it is not the right home for this
feature because the controller needs explicit workspace- and rig-scoped
runtime policy. `sleep_after_idle` is not allowed under
`[agent_defaults]`.

### 2) Resolve the effective policy in a strict order

Policy resolution happens after normal config composition. Ambient
defaults do not mutate agent composition; they are read by the
reconciler as a separate policy resolver over the already-composed
`config.Agent`.

Existing agent composition order remains unchanged:

1. pack-defined/inline agent fields
2. `[[rigs.overrides]]`
3. `[[patches.agent]]`

When `sleep_after_idle` is stamped by `[[rigs.overrides]]` or
`[[patches.agent]]`, config composition must preserve provenance so the
reconciler can report both the composed value and the source layer that
won.

Then requested idle-sleep policy resolves on the composed `config.Agent`:

1. explicit `sleep_after_idle` stamped on the composed agent
2. rig default for the resolved session class
3. workspace default for the resolved session class
4. legacy default `"off"`

The controller also records the exact source, not only the value:

| Source | Meaning |
|---|---|
| `agent` | direct `[[agent]] sleep_after_idle` |
| `rig_override` | stamped from `[[rigs.overrides]]` |
| `agent_patch` | stamped from `[[patches.agent]]` |
| `rig_default` | inherited from `[rigs.session_sleep]` |
| `workspace_default` | inherited from `[session_sleep]` |
| `legacy_off` | no idle-sleep policy configured |

Activation is class-specific, not table-driven:

- a workspace or rig default participates only when that class key is
  explicitly set
- omitted class keys mean inherit, not enable
- there is no implicit "empty table turns the feature on" toggle

Requested policy is then filtered by runtime capability:

1. `full`: any duration or `"off"` is valid
2. `timed_only`: non-interactive sessions may use positive durations;
   explicit `0s` remains allowed with a surfaced warning; interactive
   sessions still fail the safety contract unless they also satisfy the
   interactive requirements defined below
3. `disabled`: effective policy becomes `"off"` and the controller
   records the downgrade

The product starter block for cities whose non-interactive workers route
to `full`-capability backends therefore looks like:

```toml
[session_sleep]
interactive_resume = "60s"
interactive_fresh = "off"
noninteractive = "0s"
```

The conservative starter for cities that primarily route pooled workers
to `timed_only` backends is:

```toml
[session_sleep]
interactive_resume = "60s"
interactive_fresh = "off"
noninteractive = "30s"
```

That is explicit, reviewable, and does not silently opt fresh agents
into context-dropping sleep.

### 3) Classify sessions by interaction model

Session class is determined from the resolved agent config:

- `attach != false` and `wake_mode=resume`: interactive_resume
- `attach != false` and `wake_mode=fresh`: interactive_fresh
- `attach == false`: non-interactive

That keeps policy predictable and config-driven. A pooled agent can
still be interactive if it opts into attachment, but the expected common
case is pooled non-interactive workers with `0s` keep-warm.

### 4) Make `WakeConfig` time-conditional when idle sleep is active

`WakeConfig` should not mean "configured agents must stay awake
forever." It should mean "configured agents may be kept warm while their
effective keep-warm window is still open."

When idle sleep is active for a session:

- `WakeConfig` is present only while the keep-warm window is open.
- Once the session sleeps for idle, config alone must stay suppressed
  until a hard re-arm condition occurs.

That suppression prevents a `min=1` pool worker with `0s` keep-warm from
falling into a metronome of:

1. sleep due to idle,
2. see config,
3. wake immediately,
4. sleep again next tick.

Suppression is durable, not in-memory. A session with:

- `state=asleep`
- `sleep_reason=idle`

is treated as config-suppressed on every patrol, including after
controller restart.

Suppression only applies while the session still represents the same
desired identity:

- same template
- same pool slot identity, if any
- same generation/token lineage

If the session leaves the desired set, is recreated under a new
generation, or is later reintroduced as a logically new desired session,
the old idle-sleep suppression latch is discarded even if the stored
fingerprint still happens to match.

`sleep_intent=idle` alone does not suppress config wake for a still-
running session. It exists only as a restart-recovery breadcrumb for an
in-flight drain. The suppression latch begins only once the controller
has committed `state=asleep` plus `sleep_reason=idle`.

The controller also stores a `sleep_policy_fingerprint` capturing the
effective wake/sleep policy inputs that matter for re-arm:

- effective `sleep_after_idle`
- resolved session class
- `wake_mode`
- routed backend identity
- effective capability class
- relevant dependency edges
- pool template/slot identity relevant to desired-set membership

Only stable inputs participate in the fingerprint. Transient provider
errors, temporary route lookup failures, or one-tick capability
degradations do not by themselves cause fingerprint re-arm.

Re-arm happens when one of these occurs:

- `WakeWork`
- `WakeWait`
- `WakeAttached`
- explicit operator/manual wake
- pending interaction
- dependency propagation from a hard-wake descendant
- the current `sleep_policy_fingerprint` no longer matches the stored
  fingerprint from when the session went idle-asleep

After the session wakes for a hard reason, or after a policy-fingerprint
change invalidates the old suppression latch, config may keep it warm
again until the next idle-sleep transition.

Implementation hook: `WakeConfig` suppression lives inside
`wakeReasons()` itself, derived from bead metadata (`state`,
`sleep_reason`, `sleep_intent`, fingerprint state), so the wake contract
remains centralized and the function stays pure/read-only.

Before `wakeReasons()` runs, the controller performs one normalization
pass over idle-sleep-owned durable metadata. That pass owns only
idle-prefixed markers:

- if `state=asleep` with idle-based `sleep_reason`, clear lingering
  `idle-*` `sleep_intent`; asleep is terminal for that drain
- if `state!=asleep` and `sleep_intent=idle-stop-confirmed`, clear the
  stale confirmed marker before ordinary evaluation
- if `attach_intent` is expired, clear it before computing hard wake
  roots
- if `sleep_reason` is not idle-based, clear idle-sleep-only suppression
  helpers as stale diagnostics

Idle-sleep normalization does not reinterpret unrelated legacy markers.
Existing non-idle lifecycle breadcrumbs remain owned by their current
features and only participate here as blockers or wake suppressors.

### 5) Add two new hard wake reasons

The wake model needs two more reasons:

- `pending`: the provider reports a structured blocking interaction
- `dependency`: a dependent session needs this session awake

`pending` prevents the controller from sleeping a session that is
waiting for approval or an answer.

`dependency` prevents deadlocks. Today `depends_on` gates start-up, but
sleeping dependencies would strand already-configured dependents if the
controller treats each session in isolation.

Dependency wake is defined as an upstream closure over the acyclic
`depends_on` DAG:

1. compute hard-wake roots after config suppression is applied:
   - `work`
   - `wait`
   - `attached`
   - explicit operator attach or wake
   - `pending`
2. after config suppression is applied, propagate `dependency`
   transitively upstream from those roots only
3. config-suppressed cold slots never originate dependency propagation
4. dependency wake is live, not latched; once no descendant retains a
   hard wake root, the ancestor becomes eligible for idle sleep again

This remains template-scoped, consistent with current dependency start
rules, and avoids wake cascades originating from already-warm but
otherwise idle dependents.

### 6) Start the idle window at `max(last_activity, detached_at)`

Interactive sessions should not sleep immediately just because they were
attached for a long time. The keep-warm countdown starts at:

- the last observed runtime activity timestamp, or
- the moment the controller first observes the session transition from
  attached to detached,

whichever is later.

Implementation detail:

- the reconciler polls `IsAttached()` on each tick,
- detects the attach/detach edge,
- stores `detached_at` in session metadata,
- and uses `max(last_activity, detached_at)` as the idle reference.

`detached_at` must be treated as durable controller state, not an
ephemeral edge hint. On every patrol:

- if `IsAttached()` is true, clear `detached_at`
- if `IsAttached()` is false and `detached_at` is empty, set it to
  `now`
- if `IsAttached()` is false and `detached_at` is already set, preserve
  it

On controller restart, the reconciler reads existing `detached_at`
metadata before evaluating idle sleep. Restart does not reset the detach
timer for providers that can report attachment.

Providers that cannot report attachment permanently use
`last_activity` as the idle reference. For those sessions,
`detached_at` is never populated.

Observed keep-warm expiry is quantized by controller patrol timing.
Effective sleep therefore occurs within:

- `configured keep-warm` at best, and
- `configured keep-warm + patrol_interval + probe_time` at worst

Ambiguous attachment reads fail closed:

- a route lookup failure, stale session lookup, or otherwise unreliable
  attachment observation must not start or advance the detach timer
- for that tick, the session is treated as attachment-unsafe for
  interactive auto-sleep purposes

### 7) Require a safe idle boundary before sleeping

"Idle timeout expired" alone is not enough to sleep an awake session.
Before draining for idle, the controller must confirm a safe boundary.

Preferred path:

1. compute that the effective keep-warm window has expired,
2. verify no hard wake reason currently applies,
3. call `WaitForIdle(name, shortProbeTimeout)` if the provider supports
   it,
4. immediately re-check wake reasons,
5. only then begin or continue an idle drain.

If `work`, `wait`, `attached`, or `pending` appears during or after the
probe, abort the idle-sleep attempt for that tick.

`WaitForIdle` probes must never stall the patrol loop unboundedly:

- each probe has a hard per-session timeout of `1s`
- probes run synchronously inside the single-threaded reconciler
- at most `3` new probes may run in one patrol tick
- the reconciler reserves `2s` of the `5s` `defaultTickBudget` for
  non-probe work, so no new idle probe is started once that reserve
  would be consumed
- remaining candidates are skipped until the next tick
- `advanceSessionDrains` always runs even when the tick admits zero new
  probes

If the provider does not support `WaitForIdle`, the controller may still
sleep based on timed inactivity only when the session capability is
`timed_only`. `disabled` sessions do not auto-sleep.

Interactive safety is stricter than worker safety:

- interactive sessions require reliable attachment detection
- interactive sessions also require either a trustworthy idle-boundary
  signal or structured pending detection
- if those conditions are not met, effective policy becomes `"off"` for
  that session, even when an agent-level override asked for sleep
- interactive auto-sleep therefore fails closed on k8s/exec/ACP-style
  backends today

For tmux-like providers, `WaitForIdle` must treat a foreground process
blocked on stdin or a visible approval prompt as not idle. A provider
that cannot make that distinction is not `full` for interactive sleep.
No provider may be classified `full` for interactive sleep until tests
demonstrate that blocked stdin, approval prompts, and detach/reattach
races do not sleep through active operator work.

### 8) Treat runtime capability per session, not by aggregate provider

Composite providers (`auto`, `hybrid`) route each session to a concrete
backend. The controller must not decide idle-sleep behavior from the
aggregate `Provider.Capabilities()` intersection alone, because that can
hide capabilities that are available for some routed sessions but not
others.

Capability resolution is an explicit controller contract, not ad-hoc
branching at individual call sites. The reconciler resolves a
per-session snapshot:

```text
SessionSleepCapability {
  class: full | timed_only | disabled
  has_activity_clock: bool
  has_idle_boundary: bool
  has_pending_signal: bool
  has_attachment_signal: bool
  adjustment_reason: string
  probe_health: healthy | failed_closed
}
```

`resolveSleepCapability(session)` is the only entry point that converts
routed backend evidence into this snapshot. Policy resolution,
`wakeReasons()`, status surfaces, and tests consume the snapshot rather
than reinterpreting provider errors independently.

The `class` field is the stable routed capability used for policy
resolution, status, and suppression fingerprinting. `probe_health` is a
per-tick diagnostic that records whether this patrol had to fail closed
because a routed call was ambiguous or unavailable. Transient probe
health changes do not by themselves change the stable capability class.

The controller should use routed per-session calls and interpret the
results:

- `Pending(name)` discovered via `InteractionProvider` routing and
  returning `ErrInteractionUnsupported` means no structured pending
  signal for that session
- `GetLastActivity(name)` returning zero time means no useful activity
  timestamp
- `WaitForIdle(name, timeout)` returning unsupported or timeout means the
  session lacks a confirmed boundary in that tick

Transient or ambiguous provider errors are never interpreted as support:

- route lookup failure
- transport error
- stale session lookup
- backend timeout unrelated to the configured idle probe

All of those fail closed for the current tick and surface an adjustment
reason in status/events.

Capability is normative:

| Capability | Contract |
|---|---|
| `full` | activity + safe idle-boundary support |
| `timed_only` | activity without safe idle-boundary support |
| `disabled` | no usable activity clock, so idle sleep is effectively off |

Provider classes in current code:

| Provider | Activity | Pending | Idle boundary | Notes |
|---|---|---|---|---|
| `tmux` | yes | no structured pending today | yes | strongest support |
| `k8s` | yes | no | no | timed-only sleep |
| `exec` | script-dependent | no | no | timed-only when activity exists, otherwise disabled |
| `subprocess` | no useful activity | no | no | disabled |
| `acp` | no | currently unsupported | no | disabled until ACP reports usable activity |
| `auto` / `hybrid` | routed | routed | routed | decide per session, not globally |

Composite providers must route `Pending(name)` the same way they already
route `WaitForIdle(name, timeout)`: by asking the routed backend whether
it implements `InteractionProvider`. Type-assert failure and
`ErrInteractionUnsupported` both map to "no pending capability for this
session."

Until a provider actually surfaces structured pending interactions,
`pending` is a latent wake reason rather than an active production path.

### 9) Keep asleep sessions as pool slots

Idle sleep is not scale-down. A sleeping pool session still represents a
realized pool slot and still counts toward occupancy. It should not be
treated as an orphan or as missing capacity that needs replacement.

Implications:

- asleep pool sessions remain in the session index,
- asleep pool sessions still count toward pool occupancy,
- waking a sleeping slot is cheaper than creating a new slot,
- pool patrol may wake that slot on demand instead of creating a fresh
  sibling.

The pool accounting invariant is:

- `realized = awake + asleep`
- realized slots inside desired cardinality are healthy capacity
- realized slots above desired cardinality are excess and remain
  removable, even if currently asleep

When pool desired cardinality is `0`, every realized slot is excess,
including idle-asleep slots. Trim therefore applies to all realized
slots, highest index first, until occupancy reaches zero.

The exception is live dependency wake. If a downstream hard wake
requires that pooled template awake, slot `1` remains inside the
effective desired floor and must not be trimmed as excess while the
dependency root remains active.

Deterministic slot rules:

- when demand rises, wake existing cold slots before creating new
  siblings
- when waking an existing cold slot, choose the lowest-index asleep slot
  within desired cardinality
- when demand falls, trim highest-index excess slots first
- dependency wake targeting a pool wakes an existing asleep slot rather
  than expanding occupancy when such a slot exists
- if dependency wake targets a pooled dependency template with zero
  realized slots, the controller may realize slot `1` even when the pool
  check currently wants `0`, because dependency liveness temporarily
  overrides pool demand
- dependency liveness for a pooled template requires at least one awake
  slot total, not one awake slot per downstream dependent
- `min` guarantees a realized slot, not a permanently warm slot
- dependency liveness raises an effective desired floor of `1` for that
  patrol on any pooled dependency template it keeps alive; trim operates
  against `max(pool_check_desired, dependency_floor)`
- pooled `WakeWork` is slot-selective, not bulk. It re-arms only the
  minimum cold slots needed to bring awake occupancy up to the current
  effective desired count, choosing lowest-index asleep slots first

Evaluation order is fixed:

1. compute direct hard-wake roots
2. compute dependency closure
3. evaluate pool desired cardinality plus dependency floor against
   realized slots
4. wake-before-create for needed cold slots
5. trim only excess slots outside desired cardinality

Pool `check` sets desired realized cardinality, not guaranteed warm
cardinality. A satisfied desired count made of asleep slots is valid.
Reconciliation must therefore treat `state=asleep` plus idle-suppressed
config as a healthy realized slot, not as missing capacity. Operators
who want warm standby capacity should disable idle sleep for that pool.

Realized-slot accounting is derived from durable session bead/index
state, not from `ListRunning()` alone.

The missing-session rule therefore changes to:

- desired template + no realized session bead: realize or start one
- desired template + realized awake session: healthy
- desired template + realized asleep session with config suppressed:
  healthy cold slot
- desired template + realized asleep session without config suppression:
  wake candidate

If a headless worker self-exits after finishing work and idle sleep is
active with no remaining hard wake reason, that should be treated as a
healthy cold-slot outcome, not a crash-loop signal.

### 10) Make idle drains cancelable and crash-safe

Idle sleep uses the existing drain path, but with stronger cancellation
rules.

Before a session is finally stopped for idle:

- probe for safe idle,
- persist the in-flight idle-drain intent and suppression fingerprint,
- re-check hard wake reasons,
- abort if any hard wake reason reappears,
- request stop,
- re-check provider truth and hard wake reasons after stop completion,
- if a hard wake appeared after stop completed, clear idle intent and
  restart in the same patrol tick,
- otherwise commit `state=asleep`, `sleep_reason=idle`.

Idle drains are cancelable only by hard wake reasons:

- `work`
- `wait`
- `attached`
- `pending`
- `dependency`
- explicit operator attach or wake

`WakeConfig` does not cancel an idle drain, because config is the reason
being suppressed.

An uncertain attachment read is treated like a hard blocker for the
current tick: it prevents both starting a new interactive idle drain and
completing an in-flight one.

Structural reconciliation changes outrank idle sleep. If, during an
idle drain, the session becomes:

- drifted,
- suspended,
- outside desired pool cardinality,
- orphaned or otherwise removed from the desired set,

the controller must abandon the idle-sleep path and converge to the
higher-priority reconciliation action instead of committing
`sleep_reason=idle`.

Across patrol ticks, idle-drain precedence is:

1. desired-set removal, suspension, or orphan cleanup
2. config drift or other structural restart reason
3. hard wake arrival (`work`, `wait`, `attached`, `pending`,
   `dependency`, operator wake)
4. healthy continuation of the idle drain

Restart and patrol recovery use one authoritative outcome table:

| Runtime truth | `state` / `sleep_reason` | `sleep_intent` | Hard wake present | Outcome |
|---|---|---|---|---|
| running | awake | empty | no | keep awake / continue ordinary evaluation |
| running | awake | `idle-stop-pending` | no | continue idle drain |
| running | any | any | yes | clear idle intent and keep/wake awake |
| stopped | asleep / `idle` | empty | no | keep cold slot asleep |
| stopped | asleep / `idle` | empty | yes | wake now |
| stopped | any | `idle-stop-confirmed` | no | commit or preserve `asleep: idle` |
| stopped | any | `idle-stop-pending` | no | clear `idle-stop-pending`, emit recovery-ambiguous event, then rerun ordinary wake and idle-eligibility evaluation before any suppression is re-latched |
| stopped | any | any | yes | clear idle markers and wake now |
| any | any | any | structural action wins | ignore idle path and apply structural action |

The missing-session classifier is therefore constrained:

- desired + not running + no idle markers: ordinary wake candidate
- desired + not running + `idle-stop-pending`: recovery-table path, not
  an immediate cold-slot latch
- desired + not running + `state=asleep` with idle-based reason:
  suppressed cold slot until a hard wake or policy re-arm appears
- desired + not running + eligible non-interactive idle-sleep policy +
  no hard wake reason: commit `asleep` as a cold-slot self-exit outcome
  instead of immediately re-waking

If the controller crashes mid-drain:

- in-memory drain tracking is lost,
- the next patrol re-evaluates the session from provider truth,
- `sleep_intent=idle-stop-pending` means stop was requested but not yet
  durably confirmed,
- `sleep_intent=idle-stop-confirmed` means the controller observed the
  runtime stopped as part of the idle-sleep path,
- only the confirmed state may be laundered into `asleep: idle` on
  restart recovery without counting as a crash,
- restart recovery must recompute current hard wake reasons before
  laundering confirmed intent into `asleep: idle`,
- if attach intent or explicit operator wake is present during recovery,
  the idle path is cleared before any cold-slot commit,
- a still-running runtime is treated as awake again,
- a runtime gone while intent is only pending emits an ambiguity event,
  preserves crash accounting, clears the stale in-flight idle marker, and
  then goes through a fresh same-tick wake/idle evaluation before any
  cold-slot suppression is allowed,
- that ambiguous path must not by itself create durable config
  suppression.

Crash accounting consumes controller-classified exit intent:

- `idle_controller_stop`: the controller requested stop as part of an
  idle drain and later confirmed it; exempt from crash-loop accounting
- `idle_self_exit`: a non-interactive session exited on its own while
  idle sleep was eligible, no hard wake reason remained, and the
  controller committed the result as a cold slot; exempt from
  crash-loop accounting
- `unexpected_exit`: all other exits, including `idle-stop-pending`
  ambiguity; counted normally

### 11) Expose the full decision in metadata and status surfaces

Operators need to know why a session slept and why another did not.

Add or standardize the following bead metadata:

- `detached_at`
- `requested_sleep_after_idle`
- `effective_sleep_after_idle`
- `sleep_policy_source` with values such as `agent`, `rig_override`,
  `agent_patch`, `rig_default`, `workspace_default`, `legacy_off`
- `sleep_policy_fingerprint`
- `sleep_policy_fingerprint_inputs`
- `sleep_policy_adjustment_reason`
- `sleep_probe_health`
- `sleep_intent`
- `attach_intent`
- `config_wake_suppressed`
- `current_sleep_blockers`
- `last_sleep_abort_blockers`
- `last_sleep_blocked_at`
- `sleep_decision_snapshot`
- `sleep_capability` with values such as `full`, `timed_only`,
  `disabled`
- `sleep_reason`

Definitions:

- `current_sleep_blockers`: blocker set from the latest completed patrol
- `last_sleep_abort_blockers`: blocker set that canceled the most recent
  idle-sleep attempt
- `config_wake_suppressed`: whether config wake is currently latched off
  due to idle sleep
- `sleep_intent`: durable stop-path marker (`idle-stop-pending`,
  `idle-stop-confirmed`, or empty)
- `attach_intent`: durable operator attach/wake request with expiry,
  cleared when attach starts, fails definitively, or expires
- `sleep_decision_snapshot`: canonical struct recorded in metadata and
  events with:
  - `evaluated_at`
  - `requested_sleep_after_idle`
  - `effective_sleep_after_idle`
  - `policy_source`
  - `session_class`
  - `sleep_capability`
  - `sleep_probe_health`
  - `idle_reference_at`
  - `keep_warm_deadline`
  - `config_wake_suppressed`
  - `hard_wake_roots`
  - `dependency_roots`
  - `current_blockers`
  - `probe_result`
  - `drain_phase`

Authoritative precedence:

1. `state` and `sleep_reason` define the public lifecycle state
2. `sleep_intent` only describes an in-flight or recovered stop path
3. `config_wake_suppressed` is derived from lifecycle state plus
   suppression rules; it is not an independent lifecycle state
4. blocker and snapshot fields are diagnostic, not state-machine inputs

Event payloads should carry structured fields for:

- effective keep-warm duration
- requested keep-warm duration
- policy source
- session class
- stable routed sleep capability
- current probe health / fail-closed diagnostics
- policy adjustment reason
- blockers present at decision time
- whether a sleep attempt was aborted and why
- a canonical sleep decision snapshot

Idle-sleep observability is evented explicitly:

| Event | Trigger | Required fields |
|---|---|---|
| `session.sleep_policy_resolved` | requested or effective policy changes | requested/effective duration, source, class, capability, adjustment reason |
| `session.draining` | idle drain begins | reason=`idle`, blockers snapshot before probe |
| `session.sleep_aborted` | idle drain is canceled | abort reason, abort blockers, capability |
| `session.stopped` | idle sleep commits | reason=`idle`, suppression state, fingerprint |
| `session.woke` | idle-asleep session wakes | wake reason, previous sleep reason |
| `session.sleep_capability_changed` | capability class changes | previous capability, new capability, requested/effective duration |
| `session.updated` | edge-triggered change in material sleep state | blockers, source, effective duration, stable capability, probe health, suppression |

Status output should distinguish at least:

- `stopped (asleep: idle)`
- `stopped (suspended)`
- awake but blocked from sleeping, with the blocker summary
- awake with idle sleep configured but masked by `idle_timeout`

Example `gc status` renderings:

```text
mayor     awake    sleep=60s(source=workspace_default capability=full blockers=attached)
worker-1  asleep   reason=idle sleep=0s(source=rig_override suppressed=yes)
polecat   awake    sleep=off(source=workspace_default adjustment=fresh-agent-default)
api       awake    sleep=60s(masked_by=idle_timeout:30s source=agent capability=full)
```

When `dependency` is present in blockers or event payloads, the payload
must include both:

- the specific downstream session or sessions keeping the current
  session awake
- the terminal hard-wake root or roots that originated the propagation

`session.updated` is edge-triggered. The comparison tuple is:

- effective duration
- policy source
- capability
- suppression state
- current blocker set

Transient sub-tick changes are not guaranteed observable.

## Detailed Behavior

### Effective timeout examples

1. Workspace enables idle sleep safely:

```toml
[session_sleep]
interactive_resume = "60s"
interactive_fresh = "off"
noninteractive = "0s"
```

- `mayor` (`attach=true`, `wake_mode=resume`) sleeps after 60s idle
- `worker` (`attach=false`) requests immediate sleep after idle
- `polecat` (`attach=true`, `wake_mode=fresh`) remains off by workspace
  default unless it is explicitly opted in

2. No workspace or rig policy configured:

- all agents inherit `legacy_off`
- no idle sleep occurs
- current always-warm behavior remains

3. Rig override for a busy repo:

```toml
[rigs.session_sleep]
interactive_resume = "5m"
```

- agents in that rig stay warm longer than the workspace default
- agent-level `sleep_after_idle` still wins

### Resolution examples

| Agent field | Rig default | Workspace default | Effective value | Source |
|---|---|---|---|---|
| `30s` from `[[agent]]` | `5m` | `60s` | `30s` | `agent` |
| omitted + rig override stamps `0s` | `5m` | `60s` | `0s` | `rig_override` |
| omitted + patch stamps `"off"` | `5m` | `60s` | `off` | `agent_patch` |
| omitted | `5m` | `60s` | `5m` | `rig_default` |
| omitted | omitted | `60s` | `60s` | `workspace_default` |
| omitted | omitted | omitted | `off` | `legacy_off` |

### Sleep blockers

A session cannot auto-sleep while any of these are true:

- attached terminal
- durable ready wait
- structured pending interaction
- assigned work that should wake it now
- dependency wake propagation from an awake dependent
- active drain reason unrelated to idle sleep

The controller should surface the blockers as data, not just logs.

### Attach Behavior

Attaching to an idle-asleep interactive session is an explicit hard-wake
path:

1. `gc attach` (or equivalent UI action) records an explicit operator
   attach intent with a bounded expiry
2. the controller treats that intent as a hard wake root even while
   config wake is suppressed
3. the session wakes according to its `wake_mode`
4. the attach intent is cleared when attachment starts, fails
   definitively, or expires
5. if `wake_mode=fresh`, the attach surface must warn that reopening the
   session will start a fresh conversation rather than resuming terminal
   state

Attach intent expiry defaults to the effective
`[session].startup_timeout`, clamped to the range `[30s, 5m]`. The CLI
surface must render a normative state machine explicitly:

- `already awake`: attach immediately without writing `attach_intent`
- `fresh confirmation required`: print the target mode and consequence
  before prompting or requiring `--confirm-fresh`
- `wake accepted`: `attach_intent` persisted; print
  `waking <session>... (timeout <duration>)`
- `waiting`: show that wake is still in progress
- `timed out`: attach did not begin before expiry; report whether the
  controller still sees the session `running`, `stopped`, or `unknown`
- `failed definitively`: provider/controller returned a terminal failure
- `canceled`: the client aborted the wait and best-effort cleared its
  `attach_intent`

- continue waiting until attach succeeds, wake fails definitively, the
  attach intent expires, or the user cancels
- on expiry, print whether the wake state is `running`, `stopped`, or
  `unknown`; do not present an ambiguous timeout as a confirmed failure
- on wake failure, surface the provider/controller failure without
  silently retrying
- on client-side Ctrl+C, best-effort clear `attach_intent`; if the
  controller has already consumed it, the benign race is that the
  session may still finish waking once

For `wake_mode=fresh`, attach is context-destroying by definition. The
interactive surface must require explicit confirmation before creating a
fresh session:

- interactive TTY: prompt for confirmation before writing `attach_intent`
- non-interactive use: require an affirmative flag (for example
  `--confirm-fresh`) or fail fast
- both paths must print the concrete consequence string: `will start a
  new session and discard prior interactive context`

Concurrent attach attempts are single-writer in practice. If another
attach is already waking the same session, the second caller should see
`session is already being woken by another attach` rather than a silent
overwrite or opaque timeout.

`gc attach` only records `attach_intent` for sessions that are asleep due
to idle policy or already awake. If the session is asleep for a
non-idle reason such as suspension, the CLI should fail fast with that
reason instead of trying to wake it.

An idle-asleep interactive session must never require unrelated work to
arrive before a user can re-open it.

### Provider warnings

If a user sets `sleep_after_idle = "0s"` on a provider that lacks a safe
idle-boundary signal, config validation or status output should make the
risk explicit. The feature may still be allowed, but it should not be a
silent foot-gun.

Validation rules:

- reject empty string
- reject negative durations
- reject unknown sentinel strings other than `"off"`
- accept partial `[session_sleep]` tables; omitted keys mean inherit
- do not inherit interactive auto-sleep onto sessions that lack both
  attachment detection and a safe boundary
- do not inherit interactive auto-sleep onto sessions that lack both
  `Pending()` support and `WaitForIdle()`
- reject explicit interactive auto-sleep on sessions that still fail the
  attachment plus boundary/pending safety contract
- warn when `wake_mode=fresh` is combined with any non-`"off"`
  `sleep_after_idle`
- downgrade `disabled` sessions to effective `"off"` with a surfaced
  warning when static validation cannot determine the route ahead of time
- surface when `idle_timeout` is masking idle sleep so operators do not
  misread restart-on-idle as failed cold-slot rollout

### `idle_timeout` coexistence

If an agent has both `idle_timeout` and `sleep_after_idle` configured,
`idle_timeout` keeps its existing precedence: restart-on-idle is checked
first, then the restarted session begins a fresh keep-warm window for
idle sleep.

This is intentional for backward compatibility, but the controller must
emit a validation warning when both are configured on the same agent
because the restart path can mask the expected resource savings from
idle sleep. Status and event surfaces should report this as
`masked_by=idle_timeout:<duration>`, not only as an off-line warning.

## Rollout

This feature ships behind configuration presence, not a global code flag.
Adding support for the feature to the binary without adding
`[session_sleep]` config produces no behavior change.

Compatibility guarantees:

- if a class key is absent at agent, rig, and workspace scope, that
  class keeps exact legacy always-warm behavior
- enabling one class does not implicitly enable any other class
- adding a partial `[session_sleep]` table does not change omitted
  classes
- pool `min` changes from "warm slot" to "realized slot" only for pools
  that actually enable idle sleep

Initial rollout guidance:

```toml
[session_sleep]
interactive_resume = "60s"
interactive_fresh = "off"
noninteractive = "0s"
```

Use that block when pooled non-interactive sessions route to `full`
capability backends. If the initial rollout is mostly `timed_only`
backends, start instead with:

```toml
[session_sleep]
interactive_resume = "60s"
interactive_fresh = "off"
noninteractive = "30s"
```

That keeps the desired baseline direction without changing cities that
have not opted in yet.

Operators should explicitly disable idle sleep for agents that must stay
warm or cannot tolerate context loss:

```toml
[[agent]]
name = "polecat"
sleep_after_idle = "off"
```

### Provider rollout guidance

| Provider capability | Suggested first rollout |
|---|---|
| `full` | use the starter policy directly |
| `timed_only` non-interactive | start with `30s`; explicit `0s` is an opt-in risk tradeoff, not the default starter |
| `timed_only` interactive | stay inherited-off unless the agent explicitly opts in |
| `disabled` | no idle sleep; fix provider capability first |

Until providers implement structured `Pending()`, `timed_only`
non-interactive sessions have no pending-interaction guard beyond their
activity clock. That is acceptable for initial rollout only with an
explicitly non-zero starter window such as `30s`.

### Migration from `idle_timeout`

| Knob | Trigger | Post-action | Next message behavior | Best use |
|---|---|---|---|---|
| `idle_timeout` | no activity for timeout | stop and restart | always fresh process start | stale-session recovery |
| `sleep_after_idle` | safe idle + no hard wake reason | drain to asleep | wake on demand | resource reduction |

Migration guidance:

1. Keep existing `idle_timeout` agents unchanged until you explicitly
   want cold-slot behavior.
2. When enabling `sleep_after_idle` on an agent that already uses
   `idle_timeout`, expect restart-on-idle to win until you remove or
   lengthen `idle_timeout`.
3. With idle sleep active, pool `min` guarantees a realized slot, not a
   permanently warm one. Agents that must stay warm should set
   `sleep_after_idle = "off"`.
4. Downgrading to an older binary is safe because the controller is
   single-active:
   - if `[session_sleep]` remains in config, the older binary ignores the
     new suppression metadata and treats configured non-running sessions
     as ordinary wake candidates
   - if `[session_sleep]` is removed first, always-warm behavior is
     restored after one patrol on either binary

## Test Harness Requirements

Deterministic verification requires explicit test seams, not wall-clock
sleep loops. The fake runtime used by reconciler tests must support
scriptable hooks or barriers for:

- `WaitForIdle`
- `Stop`
- `IsRunning`
- `GetLastActivity`
- `Pending`
- `IsAttached`

The harness also needs:

- a fake clock
- a way to inject state changes between probe, re-check, stop, and
  asleep commit
- restart-recovery tests that reload persisted metadata into a fresh
  controller instance

Required interleavings:

- wake arrives before probe returns
- wake arrives after probe returns but before drain intent persists
- wake arrives after drain intent persists but before `Stop`
- wake arrives after `Stop` returns but before asleep commit
- controller restart at each cut point above

## Testing Strategy

Add coverage for:

1. config parsing and validation:
   - omitted versus `"off"` versus duration versus empty string
   - workspace, rig, agent, override, and patch precedence
   - class-specific keys for resume, fresh, and noninteractive
2. wake reason resolution:
   - config suppression after idle sleep
   - durable re-arm on `work`, `wait`, `attached`, `pending`,
     `dependency`, and policy-fingerprint changes
3. interactive timer behavior:
   - detach edge sets `detached_at`
   - timer uses `max(last_activity, detached_at)`
   - durable restart recovery of `detached_at`
4. provider capability cases:
   - full boundary support
   - timed-only support
   - no useful activity support
   - composite `InteractionProvider` routing
5. pool and dependency correctness:
   - asleep pool slot still counts toward occupancy
   - excess asleep slots above desired remain removable
   - dependency wake propagation prevents deadlock without latching
6. drain races:
   - wake reason appears during idle probe
   - controller restart during idle drain
   - late hard wake after stop request but before idle commit
   - wake arrives after `Stop` but before asleep commit
7. status and event payloads:
   - source, blockers, capability, and reason surfaced correctly
8. legacy non-regression:
   - `idle_timeout` still restarts when idle sleep is off
   - drift restart still overrides idle-sleep candidacy deterministically
   - orphan cleanup and suspended-agent cleanup still converge
   - pool trim order keeps excess cold slots removable

## Decision

Idle session sleep should be built-in controller behavior, not an order
or agent-side convention. The controller already owns session wake/sleep
state, and the required mechanics depend on config composition, provider
capabilities, dependency management, and status/event observability that
only the controller can coordinate coherently.
