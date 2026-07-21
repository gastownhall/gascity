# Managed Dolt Slice

`GC_DOLT_SLICE` controls the systemd user slice that managed `dolt sql-server`
processes are placed in. Unlike [`GC_AGENT_SLICE`](/reference/tmux-agent-slice),
which is opt-in, this placement is **on by default** and degrades gracefully.

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
| unset | placed in `gcdolt.slice` (the built-in default) |
| set to a slice name | placed in that slice |
| set to the empty string | no cgroup placement |

Placement wraps the spawn as:

```
systemd-run --user --scope --slice=<slice> --collect --quiet -- <command>
```

`--scope` makes systemd-run exec in place, so the spawned process keeps the PID
gc observed and stays trackable and signalable. Placement is inherited across
`fork`, so gc places the scope watchdog and the server inherits it.

When `systemd-run` is missing or the systemd user manager is unreachable — a
container, a non-systemd host, no user bus — gc warns once and spawns the server
unwrapped. Placement is hardening; it is never a reason for the bead store to
fail to start.

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

Give the slice a ceiling with a drop-in, for example:

```ini
[Slice]
MemoryMax=4G
MemorySwapMax=0
```

Size the ceiling for both processes: the scope watchdog shares the slice with
the server by design (it must share the server's fate), and costs roughly 100 MB
resident on top of the server's own working set.

Prefer `MemoryMax` over `MemoryHigh` here. `MemoryHigh` throttles by reclaim,
and a cgroup that is anon-dominated with swap disabled has nothing to reclaim,
so the throttle spins instead of bounding anything.

## OOM score

systemd's user manager applies a positive `OOMScoreAdjust` to the units beneath
it, and that adjustment is inherited by every descendant process. The kernel adds
a proportion of total RAM to a process's badness score for a positive
adjustment, which on a large host can rank a modest server as though it were an
order of magnitude bigger than it is.

The scope watchdog therefore clears its own `oom_score_adj` to 0 before spawning
the server, which inherits the neutral value. Lowering toward 0 is permitted for
unprivileged processes; negative values require `CAP_SYS_RESOURCE` and are not
attempted.

This hardening is gated by `GC_DOLT_SCOPE_WATCHDOG`, not by `GC_DOLT_SLICE`.
With `GC_DOLT_SCOPE_WATCHDOG=0` the server is still placed in the slice, but is
spawned by a general-purpose gc process whose own badness must not be
permanently rewritten; that path lowers the value only across the fork and
restores it immediately.

## Verifying

```sh
cat /proc/<dolt-pid>/cgroup          # expect the configured slice
cat /proc/<dolt-pid>/oom_score_adj   # expect 0
```

On the watchdog-free path (`GC_DOLT_SCOPE_WATCHDOG=0`) a missing `dolt` binary
no longer fails at spawn: `systemd-run` starts successfully and reports the
failure itself, so gc surfaces a readiness timeout rather than
`exec: dolt: executable file not found in $PATH`.

Placement is not observable the instant the process starts: `systemd-run`
registers the transient scope over D-Bus and only then execs, so a check run
immediately after spawn can still see the pre-placement cgroup. Re-read after a
moment.
