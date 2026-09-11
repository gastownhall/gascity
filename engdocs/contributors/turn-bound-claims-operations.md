# Turn-bound claims — operator notes

`gc hook --claim` refuses to mint a claim that no agent turn will consume. This
page covers the two knobs and the two failure shapes an operator sees as a
result. Background and rationale: ga-fylee.

## The fences, in one paragraph

A claim is refused outright when the invocation looks like a provider callback
rather than a turn (F-A), or when the invocation has outlived its claim window
(F-B). A claim that is won but cannot be delivered — the provider closed the
tool pipe, or the compare-and-swap landed after the window closed — is released
again and reported as `bead.claim_released` (F-C). A session that acknowledges
drain gives back any `in_progress` claim it is still holding (F-D).

## `GC_HOOK_CLAIM_WINDOW` — the one tuning knob

Default **45s**. It bounds the time from `gc hook --claim` starting to a claim
mutation being allowed to run. Past it the command exits 1, emits
`execution.claim_window_expired`, and deliberately writes **no drain record** —
a spent window is a dead invocation, not an idle store.

Raise it when honest claim latency approaches the window. The alarm that tells
you to is `execution.claim_window_expired` volume: a steady stream of them with
`parent_alive=true` means real claims are being refused (raise the window, or
fix the store), while `parent_alive=false` means the claimer was orphaned by a
dead provider tool call, which is the fence working as intended.

Keep it **strictly below 90s** (`idleClaimNudgeGrace`, `cmd/gc/idle_nudge.go`).
At or above that, the idle-claim backstop can re-nudge a seat whose claim is
still running, and two live claim attempts for one seat is the state the window
exists to prevent. 90s itself is not a legal value — the bound is exclusive.

### Deploy-lane override (interim)

Cities whose observed claim latency runs 15–70s need more than the 45s default
or the fence becomes fresh-claim starvation: honest claims are refused, seats
exit 1, and the demand is re-served without ever being worked.

**75s** is the value to use. It clears the top of the observed latency band with
headroom and still leaves 15s below the backstop grace, so neither end of the
range races.

```toml
[[patches.agent]]
name = "gc.run-operator"
env = { GC_HOOK_CLAIM_WINDOW = "75s" }
```

Repeat the stanza for each role agent of the same class — the override is
per-agent, and an unpatched sibling silently keeps the 45s default.

This is **interim**. It exists because claim latency is currently dominated by
read convergence, not by real work; it retires when the binding-first read lands
(ga-4qdfn) and latency drops back under the default. Before removing it, check
that `execution.claim_window_expired` with `parent_alive=true` is at zero for
the class.

## Failure shape: a leaked marker fences the whole fleet

F-A keys on environment markers gc sets on its own callback lanes:
`GC_HOOK_CALLBACK_LANE`, `GC_MANAGED_SESSION_HOOK`, `GC_HOOK_EVENT_NAME`. They
are per-command prefixes on those lanes and are never part of a turn's
environment.

If an operator exports any of them **fleet-wide** — a shell profile, a systemd
unit, a container env — every claim in every session is refused. The symptom is
distinctive and easy to misread: workers exit **0**, report a clean drain with
`reason=non_turn_context`, and simply never pick up work. It looks like an idle
city, not a broken one.

The refusal names the offending variable on stderr:

```
gc hook --claim: refusing to claim from a non-turn context (GC_HOOK_EVENT_NAME
is set); a provider callback's result reaches no agent turn, so a claim minted
here would be parked the instant it is won
```

So the diagnosis is `env | grep -E 'GC_HOOK_CALLBACK_LANE|GC_MANAGED_SESSION_HOOK|GC_HOOK_EVENT_NAME'`
inside a stuck worker's session. Unset it there; do not add exceptions to the
fence.

## Failure shape: a released claim after a started step

`bead.claim_released` on a bead that already has `execution.step_started` is a
**compensation pair**, not a step that ran. The claim path stamps the step at
claim time and only then discovers it cannot deliver the result. Read the pair
as "no attempt happened". Anything consuming the execution event stream must
not leave such a step in flight waiting for a `execution.step_completed` that
is never coming.

## What is NOT fenced, on purpose

- **Adoption** of work the session already holds. It mints no new obligation,
  and a re-woken holder must still be able to resume.
- **`gc agent script`** deterministic executors. They claim through the raw `bd`
  binary and execute in the same process, so they have no turn to outlive.
- **`gc bd update --claim`.** Worker-pull, and no shipped prompt uses it to
  acquire work; a test pins that so it cannot silently become load-bearing
  while unfenced.
