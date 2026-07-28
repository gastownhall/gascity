---
title: "Managed Dolt Process Placement"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-07-21 |
| Author(s) | sjarmak |
| Issue | N/A |
| Supersedes | N/A |

## Summary

`gc` starts the managed `dolt sql-server` with a plain `exec.Command` and never
places it in a cgroup of its own, so the server inherits the cgroup and the
`oom_score_adj` of whichever process first triggered auto-start. On a deployment
where that starter was an agent pane, the kernel's memcg OOM killer selected the
shared bead store four times in one morning, and each respawn took a new port
and opened the Dolt circuit breaker across every rig.

This places the managed server into a dedicated, bounded systemd user slice
through a transient scope. The slice ships with an 8 GiB `MemoryMax`, a 2 GiB
`MemoryLow`, and `ManagedOOMPreference=avoid`. Placement is default-on and
fail-closed for managed Dolt: if the user manager cannot apply and read back
that policy, `gc` does not start an unwrapped server or publish an adopted
server as controller-ready.

## Problem

Four kills on 2026-07-21, read from the kernel ring buffer rather than from any
summary:

```
09:28:56  pid=2784208  anon-rss=849,236kB
09:33:21  pid=4041070  anon-rss=864,084kB
09:42:00  pid=31084    anon-rss=1,060,144kB
09:49:05  pid=365769   anon-rss=829,032kB
```

Every one carried the same shape: `oom-kill:constraint=CONSTRAINT_MEMCG`,
`oom_memcg=.../gascity.slice`, `task=dolt`, `oom_score_adj:200`. The server was
running inside the slice that holds agent panes, so when that slice reached its
`MemoryMax` the kernel treated the bead store for 23 rigs as exactly as
disposable as one agent.

The consequence was not a Dolt outage. Each kill respawned the server on a
different port, which desynchronized the supervisor's `GC_DOLT_PORT` pin, which
opened the Dolt circuit breaker city-wide, which froze the order floor: one
process-placement decision cascading into an outage across every rig on the box.

Placement is also nondeterministic. Earlier the same day the server sat in
`app.slice`, having been started by the supervisor; later it sat in the agent
slice, having been started by a control-dispatcher pane. Any report that the
server is "out of the agent slice" describes one respawn rather than a property
of the system.

Two separate inheritances produce this, and fixing either alone leaves the bug
reachable. The cgroup determines which memory limit the server counts against.
The `oom_score_adj` determines whether the kernel picks it once that limit is
breached, and that is the mechanism which made a modest process look enormous.
`oom_badness` adds `adj * totalpages / 1000` to the score, so on a 62.4 GiB host
an adjustment of 200 contributes roughly 12.5 GiB of page-equivalent bonus: an
850 MB server ranked as though it held about 13 GB. Nothing in `gc` sets
that value. It comes from the systemd user manager and is inherited by every
descendant process across fork.

## Goals

- Place the managed `dolt sql-server` deterministically, regardless of which
  process triggers auto-start.
- Lower the inherited badness bonus where the unprivileged process is allowed
  to do so, and report the actual inherited value where the manager-imposed
  floor prevents lowering.
- Bound the dedicated slice and apply `ManagedOOMPreference=avoid`.
- Reparent an owned watchdog/server pair that survives a supervisor restart
  without restarting either process or changing the listener port.
- Preserve every existing PID-tracking guarantee: health checks, termination,
  and the `/proc` start-time reuse guard must continue to address the right
  process.
- Fail closed before controller work when placement cannot be proved.

## Non-goals

- Writing to an operator's systemd configuration. `DefaultOOMScoreAdjust` in
  `user.conf` is the operator's to set.
- Eliminating host-global OOM risk. The bounded sibling prevents agent-slice
  pressure from selecting Dolt, but user-level or system-level pressure can
  still do so.
- Placing any other `gc`-managed process. The tmux server has a similar latent
  inherited-cgroup problem, but one instance does not justify generalizing the
  mechanism further than the shared helper already does.

## Proposed Design

### Wrapping the spawn in a transient scope

The spawn becomes:

```
systemd-run --user --scope --slice=gcdolt.slice --collect --quiet -- <command>
```

Two properties of `--scope` make this safe for a caller that tracks its child by
PID. systemd-run registers the scope and then execs the command in place, so the
PID observed from `Cmd.Start` is the PID the wrapped process keeps; and
`--quiet` keeps systemd-run's own output off the child's stdout, where the
watchdog handshake lives. Both were verified directly rather than assumed:

```
outer_pid=3210047                    # what Cmd.Start would return
inner_pid=3210047                    # the wrapped process's own $$
inner_comm=bash                      # comm changed in place
any_lingering_systemd_run: none
```

This is the load-bearing property of the whole design. If systemd-run forked
instead, `snapshotManagedDoltStartIdentity`, `terminateManagedDoltPIDGuarded`,
and the reaping goroutine would all be addressing a process that exits
immediately after handoff. Because it execs, none of that machinery changes.

The mechanics live in `internal/runtime/systemdscope`, which the tmux
agent-slice wrapper also uses. That wrapper previously carried its own copy of
the same flag shape; consolidating removes the risk of the two drifting on a
detail as consequential as `--scope`.

### Placing the watchdog rather than the server

Production spawns the server underneath a scope watchdog, so the wrapper is
applied to the watchdog and the server inherits placement across fork. That
costs one D-Bus registration instead of two, and it matches the ownership
model: a watchdog exists to share the fate of the server it guards, so the two
belong in the same slice and under the same ceiling.

The watchdog-free path (`GC_DOLT_SCOPE_WATCHDOG=0`) is wrapped directly. A
single "wrap every managed-dolt exec" choke point was considered and rejected;
see Alternatives.

### Adopting a surviving process tree

The supervisor uses `KillMode=process`, so a controlled supervisor restart can
leave the canonical watchdog and server alive in the old controller's cgroup.
Spawn wrapping cannot move those existing PIDs.

Before exporting the adopted port, the new controller verifies the published
runtime state, exact server PID and port holder, managed ownership, watchdog
parent relationship, exact watchdog argv, process-tree shape, and both
process start identities. It then uses one `StartTransientUnit` D-Bus call with
the watchdog and server PIDs in the scope's `PIDs=` property. After the job
converges, it rechecks both identities, cgroups, runtime state, port holder, and
tree shape. Any uncertainty fails closed; adoption never converts into a Dolt
restart.

The watchdog-free path is verified separately by its exact `dolt sql-server
--config` argv, canonical config path, managed-process environment sentinel,
absence of session attribution, ownership, listener, and start identity. Its
single server PID is attached and rechecked with the same fail-closed rules.
Pre-sentinel direct servers are intentionally not adopted because their origin
cannot be proved strongly enough.

### Bounded slice policy

Before either spawn or adoption, `gc` applies and reads back:

```
MemoryMax=8589934592
MemoryLow=2147483648
ManagedOOMPreference=avoid
```

The 8 GiB ceiling is based on a 5.7 GiB observed peak plus watchdog and
compaction headroom. The 2 GiB low protection preserves a useful working set
under systemd-oomd pressure. `MemoryLow` is only effective when the ancestor
hierarchy participates in memory protection; on the reference host its
ancestors currently report zero, so it is declarative protection for a future
ancestor policy rather than a host-global guarantee.

`ManagedOOMPreference=avoid` steers systemd-oomd only. It does not affect the
kernel memcg OOM killer that caused the incident, so the bounded sibling slice
is the primary isolation mechanism.

### Best-effort inherited badness adjustment

`oom_score_adj` is settable only on the calling process and inherited across
fork, and Go's `os/exec` exposes no pre-exec hook, so the value has to be
written before the spawn. The two paths differ in what they do afterwards:

The watchdog attempts to lower its own adjustment toward zero and keeps the
result, because it exists only to supervise one server. The watchdog-free path
attempts the same lowering across the fork and restores the caller's previous
value immediately.

This is best-effort. On the reference host the user manager established an
unprivileged floor of 200, and writing zero returned `permission denied`.
Tests therefore record and compare the watchdog/server's actual inherited
value rather than asserting zero. Negative values are never attempted.

### Slice naming is a correctness constraint

systemd derives slice nesting from the unit name: `a-b.slice` is a child of
`a.slice`. A slice named after the parent of the agent slice would therefore
nest inside the very memcg whose breach does the killing, and would have
prevented none of the four kills above. The default is `gcdolt.slice`, flat and
top-level, and `TestManagedDoltDefaultSliceEscapesTheAgentMemcg` fails if anyone
renames it back into that hierarchy.

The protection this buys is bounded and worth stating plainly. The server
escapes the agent slice's memory limit, but it remains a descendant of the user
manager's own slice, so a user-level or system-level OOM can still select it.

### Fail-closed defaults

The canonical managed-Dolt path has no unwrapped fallback. A missing
`systemd-run`, unreachable user bus, empty slice setting, property mismatch, or
unsafe adoption stops the managed-Dolt/controller preflight with the underlying
cause. Test binaries still decline implicit systemd placement unless they opt
in explicitly, so unit tests do not depend on a user manager.

A probe failure is cached for a retry window rather than for the process
lifetime. `gc supervisor run` stays up for weeks, and caching a transient bus
failure forever would silently disable placement for that entire lifetime behind
a single log line, which is the failure this design exists to prevent.

Placement defaults on, unlike the sibling `GC_AGENT_SLICE` knob, which is
opt-in. Opt-in cannot fix this class of bug, because the bug is that the
starting process varies: any starter that did not set the variable would put the
server back in an arbitrary cgroup. The blast radius also differs. `GC_DOLT_SLICE`
governs one `gc`-owned singleton and wraps argv directly, so
`/proc/<pid>/cmdline` still reads `dolt sql-server --config ...` and existing
discovery predicates are unaffected, whereas `GC_AGENT_SLICE` wraps arbitrary
pane commands through `sh -c` and changes what tmux reports as the pane command
for the pane's whole life.

## Alternatives Considered

**Relying on `ManagedOOMPreference=avoid` alone.** Rejected.
`ManagedOOMPreference` steers systemd-oomd; every kill on 2026-07-21 was
`CONSTRAINT_MEMCG`, the kernel's own memcg killer, which ignores the setting.
The shipped policy includes it as defense-in-depth alongside the actual
bounded-sibling fix.

**Naming the slice as a child of the agent slice's parent.** Rejected on the
nesting rule above. It reads as the obvious name and is the most dangerous
option in the set, because it looks correct, passes every test that does not
check the cgroup path, and leaves the server exactly where it was.

**Setting `OOMScoreAdjust=0` as a scope property.** systemd rejects it. Scope
units do not accept exec-context properties, since there is no exec for systemd
to configure; `systemd-run --property=OOMScoreAdjust=0 --scope` fails with
`Unknown assignment`.

**Correcting `DefaultOOMScoreAdjust` in the operator's `user.conf`.** This is
the deepest available fix and it belongs to the operator. `gc` writing into a
user's systemd configuration would be a worse altitude violation than correcting
the attribute at the boundary where `gc` itself creates the process.

**A single spawn choke point.** Wrapping every managed-dolt `exec.Cmd` in one
helper would double-wrap the watchdog's inner spawn, adding a second D-Bus round
trip and a second failure mode in order to land in a slice the process already
occupies by inheritance. Cgroup membership is a property of a process tree, so
placement belongs at the root of each tree; the two roots are enumerated in a
single branch table, and a third cannot appear without editing it.

## Testing Strategy

Unit coverage pins the decisions: the argv shape including the flags that make
PID preservation hold, the slice resolution table, the never-raise direction of
the badness adjustment, the per-slice probe cache, and the failure-retry window.

Integration coverage runs against a real systemd user manager. Both spawn paths
are exercised end to end from a parent carrying the inherited adjustment, and
the assertions read `/proc/<pid>/cgroup`, `/proc/<pid>/oom_score_adj`, and the
effective slice properties. A separate real-systemd adoption test starts the
exact watchdog argv outside the target slice and proves the watchdog PID,
server PID, and listener port are unchanged after both processes move.

The acceptance test was validated by reverting the fix and confirming it
reproduces the production symptom exactly: both processes in the caller's
cgroup, and `oom_score_adj` at 200.

Placement is not observable at the instant `Start` returns, because systemd-run
registers the scope over D-Bus and only then execs, so the assertions poll
rather than sample a single read.

## Risks

A host where the user bus is reachable but slow pays the probe timeout once per
process on the managed-dolt start path. The measured probe cost on the reference
host is about 10 ms against a 5 s ceiling, and the start path already waits on
the noms lock and then on query-readiness, so the ceiling is a guard rather than
a typical cost.

On the watchdog-free path a missing `dolt` binary no longer fails at spawn.
systemd-run starts successfully and reports the failure itself, so `gc` surfaces
a readiness timeout instead of `exec: dolt: executable file not found in $PATH`.
The watchdog path is unaffected, since it wraps the `gc` binary and its inner
spawn is unwrapped.

The `oom_score_adj=200` figure is host-derived rather than universal. The system
`user@.service` ships `OOMScoreAdjust=100` and the user manager defaults its own
units relative to that, so the exact value is distro-dependent. The
implementation reads the live value and only ever lowers it, so behavior is
correct wherever the number lands.

## Open Questions

Startup verifies placement before publishing the port. The controller caches
that result by canonical city/PID/port identity, avoiding repeated systemd and
process-table work on every patrol; a PID change forces revalidation even when
the port is reused. A `gc doctor` drift check would still improve operator
visibility for external cgroup/property changes that do not replace the
process; `cmd_doctor_drift.go` already owns comparable Dolt checks.

The supervisor's ambient-port export is gated by a process-global flag rather
than by the runtime object that owns the behavior, which leaves it ambiguous
whether `gc start`, also long-lived and also spawning children, should arm it.
Threading that seam through `CityRuntime` alongside the three that already exist
there would make the answer a literal at each construction site.
