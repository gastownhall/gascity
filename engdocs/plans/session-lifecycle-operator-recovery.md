# Session Lifecycle Operator Recovery Plan

Source beads: `ga-k0n20.1`, `ga-4arrr.1`, `ga-ueidu.1`
Design parents: `ga-k0n20`, `ga-4arrr`, `ga-ueidu`
Priority: P2

## Goal

Make operator-directed session recovery observable and reliable:
explicit resets preempt autonomous gates, circuit-open sessions explain
themselves in `gc session list`, and intentionally detached work remains
owned while its external tmux job is alive.

## Work Packages

1. `ga-k0n20.1.1` - Builder: move restart_requested kill before autonomous gates
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: `restart_requested=true` alive-kill path runs after
     drain-ack handling and before rate-limit, stability, and churn gates;
     existing drain-ack behavior is preserved; regression coverage proves
     explicit reset kill is not skipped by rate-limit, stability, or churn
     guards.

2. `ga-k0n20.1.2` - Builder: clear circuit breaker after explicit reset or kill
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-k0n20.1.1`
   - Acceptance: successful `restart_requested` kill bumps the
     circuit-breaker reset generation for the session identity; `gc session
     kill` uses the same recoverability contract; tests cover an
     open-circuit session becoming wakeable after explicit reset or kill.

3. `ga-k0n20.1.3` - Builder: show reset-pending while live reset is requested
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: `sessionReason` returns exactly `reset-pending` when
     `restart_requested=true` and the runtime session is alive; the value is
     display-only and writes no `sleep_reason` or new metadata; priority is
     higher than `circuit-open`, `sleep_reason`, and wake reasons; tests cover
     alive and not-alive reset-requested states.

4. `ga-4arrr.1.1` - Builder: show circuit-open from session circuit metadata
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-k0n20.1.3`
   - Acceptance: `sessionReason` returns exactly `circuit-open` when
     `session_circuit_state` equals `circuitOpen.String()`; implementation
     reads the existing metadata key and does not instantiate circuit-breaker
     state; tests cover exact string choice and non-matching metadata.

5. `ga-4arrr.1.2` - Builder: preserve REASON fallback order after blocking states
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-4arrr.1.1`
   - Acceptance: final priority is `reset-pending`, then `circuit-open`, then
     `sleep_reason`, then wake reasons; no-config fallback remains unchanged;
     existing wake reasons still display when no blocking state is present;
     conflict cases show higher-priority reasons winning.

6. `ga-4arrr.1.3` - Builder: cover REASON priority behavior with session tests
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-4arrr.1.2`, `ga-k0n20.1.3`
   - Acceptance: tests cover `reset-pending`, `circuit-open`, `sleep_reason`
     fallback, wake/config fallback, and reset-pending-over-circuit-open
     conflict; tests use configuration-supplied agent names and do not add
     hardcoded role names; `go test ./cmd/gc` or the narrow affected package
     passes.

7. `ga-ueidu.1.1` - Builder: parse and execute gc.detached tmux probes
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: `gc.detached` accepts `tmux:<socket>:<session>` parsed with
     `strings.SplitN(spec, ":", 3)`; unsupported or malformed specs fail
     safely; probe uses `exec.CommandContext` with a 1s timeout and runs
     `tmux -L <socket> has-session -t <session>`; result model distinguishes
     alive exit 0, dead exit 1, and error or timeout.

8. `ga-ueidu.1.2` - Builder: protect orphan-release with detached probe results
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-ueidu.1.1`
   - Acceptance: missing `gc.detached` preserves current release behavior;
     alive probe skips release and logs a diagnostic; dead probe releases and
     clears `gc.detached`; probe errors or timeouts skip release until the
     in-memory per-bead error counter reaches 3, then release and clear;
     `error_count` is not persisted to bead metadata.

9. `ga-ueidu.1.3` - Builder: suppress session.stranded for live detached work
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-ueidu.1.2`
   - Acceptance: alive detached probe suppresses `session.stranded`; dead
     detached probe clears `gc.detached` before emitting the existing
     diagnostic; absent or erroring probe preserves existing diagnostic
     behavior; tests do not kill or depend on the default tmux server and
     target only explicit test sockets.

## Dependency Graph

- `ga-k0n20.1.1` blocks `ga-k0n20.1.2`.
- `ga-k0n20.1.3` blocks `ga-4arrr.1.1` and `ga-4arrr.1.3`.
- `ga-4arrr.1.1` blocks `ga-4arrr.1.2`, which blocks `ga-4arrr.1.3`.
- `ga-ueidu.1.1` blocks `ga-ueidu.1.2`, which blocks `ga-ueidu.1.3`.

## Guardrails

- Do not add hardcoded role names; all tests must use configured agent names.
- Do not add new metadata for `reset-pending` or `circuit-open`; both are
  display-only values derived from existing state.
- Do not persist detached probe error counters to bead metadata.
- Do not run or kill the default tmux server in tests; target only explicit
  test sockets.
- Keep `gc.detached` scoped to intentional detached work and bead ownership;
  it does not add a new `gc session list` row or REASON value.
