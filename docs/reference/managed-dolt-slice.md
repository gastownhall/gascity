# Managed Dolt Slice

`GC_DOLT_SLICE` controls the systemd user slice that managed `dolt sql-server`
processes are placed in. Unlike [`GC_AGENT_SLICE`](/reference/tmux-agent-slice),
which is opt-in, this placement is **on by default** and fail-closed.

## Why placement is default-on

gc auto-starts the managed server from whichever process first needs it — the
supervisor, a control-dispatcher pane, an interactive command. Without explicit
placement the server inherits the cgroup of that starter, so which resource
limits apply to a shared, long-lived bead store is decided by a race.

On one deployment the server landed in the agent slice and was killed four times
in a single morning by the kernel's memcg OOM killer once that slice reached its
`MemoryMax`. Each respawn took a different port, which desynced the supervisor's
port pin and opened the Dolt circuit breaker across the whole city.

Opt-in would not have fixed that, because the failure is precisely that the
starting process varies — any starter that missed the variable would put the
server back in the wrong cgroup.

## Behavior

| `GC_DOLT_SLICE` | Effect |
|---|---|
| unset | placed in bounded `gcdolt.slice` (the built-in default) |
| set to a slice name | placed in that bounded slice |
| set to the empty string | rejected; managed-Dolt placement is required |

Placement wraps the spawn as:

```
systemd-run --user --scope --slice=<slice> --collect --quiet -- <command>
```

`--scope` makes systemd-run exec in place, so the spawned process keeps the PID
gc observed and stays trackable and signalable. Placement is inherited across
`fork`, so gc places the scope watchdog and the server inherits it.

When `systemd-run` is missing, the systemd user manager is unreachable, or the
slice policy cannot be read back exactly, gc fails the managed-Dolt preflight.
It does not spawn an unwrapped server or publish an unsafe adopted server to the
controller.

## Slice naming

systemd derives slice nesting from the unit name: `a-b.slice` is a child of
`a.slice`. A dolt slice named after the slice holding your agents would nest
inside it and stay subject to that cgroup's memory limit, which defeats the
purpose — the kernel's memcg OOM killer only considers tasks inside the cgroup
that hit its limit. Choose a name that makes the dolt slice a sibling of the
agent slice, not a descendant. The default, `gcdolt.slice`, is top-level.

The slice is a sibling of the agent slice, not a child, so agent memory
pressure cannot select the server. It is still a descendant of the user
manager's own slice, so a user-level or system-level OOM can — this bounds the
blast radius, it does not exempt the server.

gc applies and verifies these runtime properties before spawn or adoption:

```
MemoryMax=8589934592
MemoryLow=2147483648
ManagedOOMPreference=avoid
```

The 8 GiB ceiling covers the observed 5.7 GiB peak plus watchdog and compaction
headroom. The 2 GiB low protection preserves a useful working set under
systemd-oomd pressure. It only becomes effective when the ancestor hierarchy
also participates in memory protection; ancestors with `MemoryLow=0` leave it
declarative. `ManagedOOMPreference=avoid` is defense-in-depth for systemd-oomd,
not a fix for the kernel memcg OOM killer.

Prefer `MemoryMax` over `MemoryHigh` here. `MemoryHigh` throttles by reclaim,
and a cgroup that is anon-dominated with swap disabled has nothing to reclaim,
so the throttle spins instead of bounding anything.

## OOM score

systemd's user manager applies a positive `OOMScoreAdjust` to the units beneath
it, and that adjustment is inherited by every descendant process. The kernel adds
a proportion of total RAM to a process's badness score for a positive
adjustment, which on a large host can rank a modest server as though it were an
order of magnitude bigger than it is.

The scope watchdog therefore attempts to lower its own `oom_score_adj` toward
zero before spawning the server. This is best-effort: a manager-imposed
unprivileged floor can make the write fail with `permission denied`. The server
then keeps the watchdog's actual inherited value, which is logged and verified.
Negative values are not attempted.

This hardening is gated by `GC_DOLT_SCOPE_WATCHDOG`, not by `GC_DOLT_SLICE`.
With `GC_DOLT_SCOPE_WATCHDOG=0` the server is still placed in the slice, but is
spawned by a general-purpose gc process whose own badness must not be
permanently rewritten; that path lowers the value only across the fork and
restores it immediately.

## Verifying

```sh
cat /proc/<dolt-pid>/cgroup          # expect the configured slice
cat /proc/<dolt-pid>/oom_score_adj   # record actual inherited value; zero is not required
systemctl --user show gcdolt.slice \
  -p MemoryMax -p MemoryLow -p ManagedOOMPreference
```

On the watchdog-free path (`GC_DOLT_SCOPE_WATCHDOG=0`) a missing `dolt` binary
no longer fails at spawn: `systemd-run` starts successfully and reports the
failure itself, so gc surfaces a readiness timeout rather than
`exec: dolt: executable file not found in $PATH`.

Placement is not observable the instant the process starts: `systemd-run`
registers the transient scope over D-Bus and only then execs, so a check run
immediately after spawn can still see the pre-placement cgroup. Re-read after a
moment.

## Supervisor adoption

`KillMode=process` lets the managed watchdog/server survive a controlled
supervisor restart. Before exporting the surviving port, the new controller
verifies the exact owned PID pair, start identities, process tree, command,
runtime state, and listener holder. It attaches both existing PIDs to one
transient scope, waits for cgroup convergence, then repeats those checks.
Failure is visible and fail-closed; adoption never restarts Dolt.

With `GC_DOLT_SCOPE_WATCHDOG=0`, the same adoption boundary accepts only an
exact direct `dolt sql-server --config` process carrying gc's managed-process
sentinel and no session attribution. It attaches and rechecks that one PID.
Older direct servers without the sentinel fail closed instead of being guessed
safe.

New spawns strip `GC_SESSION_ID` and related agent attribution. A canonical
pre-fix watchdog/server that still carries stale attribution is identified by
its exact managed command sentinel and excluded from ordinary session-orphan
reaping.
