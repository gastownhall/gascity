# Sling Wake, Nudge Durability, and Coordstore Followups

Date: 2026-05-24

PM intake source:
- Designer handoff mail: gm-wisp-w071x8
- Architect/design-reviewed roots: ga-6do4y, ga-3dxxs, ga-6wy2x, ga-bctco, ga-4xdwz

The expected `docs/architecture/`, `docs/rules/`, and `docs/designs/` directories were not present in this worktree at intake time. The decomposition below uses the architect and designer context embedded in the root beads.

## Goal

Turn the five design-reviewed architecture beads into concrete downstream work packages for validator and builder agents. The first three close the asleep-pool-agent sling/wake/nudge durability gap from ga-gekyy. The last two close the coordstore measurement followups from ga-nfq4r.

## Work Packages

### Pool resume recovery

Root: ga-6do4y

- ga-6do4y.1 -> validator: Cover wake-known-identity pool desired-state recovery.
- ga-6do4y.2 -> builder: Implement wake-known-identity pool resume recovery.

Acceptance focus:
- Known configured pool template with assigned in-progress work and no live session produces Tier="wake-known-identity".
- Unknown assignees are ignored.
- Multiple assigned beads for one asleep template deduplicate to one wake request.
- Live session behavior continues to use Tier="resume".
- `wake-known-identity` sorts with resume-like priority ahead of new work.

Dependency:
- ga-6do4y.2 depends on ga-6do4y.1.

### Sling expansion durable nudge

Root: ga-3dxxs

- ga-3dxxs.1 -> validator: Cover queued nudge creation for asleep pool sling expansion.
- ga-3dxxs.2 -> builder: Enqueue durable sling nudge for asleep expandable pool agents.

Acceptance focus:
- The expandable/pool no-running-instance branch calls pokeController and enqueues exactly one durable queued nudge.
- The queued nudge matches fixed-agent path semantics: target is a.QualifiedName(), source is "sling", message is preserved.
- PR #2535 must be checked before editing to avoid duplicate enqueue behavior.

Dependency:
- ga-3dxxs.2 depends on ga-3dxxs.1.

### Nudge durability retry window

Root: ga-6wy2x

- ga-6wy2x.1 -> validator: Cover queued nudge retry ceiling for asleep targets.
- ga-6wy2x.2 -> builder: Raise queued nudge retry ceiling for asleep target durability.

Acceptance focus:
- Scope is the designer-approved simple raise from 5 to 50 attempts.
- No targetIsRunning gate, session-state snapshot plumbing, or new dead-letter categories are in scope.
- Nudges no longer expire in the former five-attempt window while waiting for asleep agents to wake.
- This work should merge after the pool wake-known-identity recovery.

Dependencies:
- ga-6wy2x.2 depends on ga-6wy2x.1.
- ga-6wy2x.2 depends on ga-6do4y.2.

Risk:
- The root title and original architect test text mention holding Attempts at 0 for asleep targets. The designer review narrowed this bead to the simple 5-to-50 raise and deferred the targetIsRunning gate. The child beads intentionally package the narrowed scope.

### Coordstore host-load latency suppression

Root: ga-bctco

- ga-bctco.1 -> validator: Cover coordstore host-overload latency suppression behavior.
- ga-bctco.2 -> builder: Add coordstore host-load guard for p99 latency gates.

Acceptance focus:
- Linux load ratio is loadavg divided by runtime.NumCPU, with threshold > 0.80.
- Runner emits the specified warning once to the progress writer.
- Scorecard HostOverloaded suppresses only p99/max latency gate failures.
- Correctness and throughput gates still fail normally.
- README/methodology text documents loaded-host latency results as informational.
- PR #2524 must be read before editing internal/benchmarks/coordstore.

Dependency:
- ga-bctco.2 depends on ga-bctco.1.

### Measurement track closure note

Root: ga-4xdwz

- ga-4xdwz.1 -> builder: Append quiesced C=20 baseline note to ga-9wgwc.

Acceptance focus:
- ga-9wgwc receives the designer-provided note that PR #2524 R2.1b is the authoritative quiesced C=20 baseline.
- The note records 9/9 target pass, point-read p99 282us, and the loaded-host artifact explanation.
- ga-9wgwc is not closed unless all of its other work is already terminal.

## Handoff Targets

Validator:
- ga-6do4y.1
- ga-3dxxs.1
- ga-6wy2x.1
- ga-bctco.1

Builder:
- ga-6do4y.2
- ga-3dxxs.2
- ga-6wy2x.2
- ga-bctco.2
- ga-4xdwz.1

All child beads have `source:actual-pm` plus exactly one routing label (`needs-tests` or `ready-to-build`) and `gc.routed_to` metadata set to the target agent.
