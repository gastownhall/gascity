---
title: CPU Profiling a gc Invocation
description: How to profile a one-shot gc command with GC_CPUPROFILE, without a local build.
---

## When To Use This

Use `GC_CPUPROFILE` when a one-shot `gc` command (`gc mail inbox`, a hook
line, `gc session list`) burns more CPU than it should and you need to see
where the time goes. It works on any `gc` binary that includes it, so the
profile can be taken on the host that shows the problem.

For a long-lived process (the supervisor, `gc nudge poll`) use `GC_PPROF=1`
instead: it serves `net/http/pprof` so a profile can be pulled while the
process runs. `GC_CPUPROFILE` writes its file only when the command returns.

## Taking A Profile

Set `GC_CPUPROFILE` to a path prefix and run the command:

```bash
GC_CPUPROFILE=/tmp/gc-cpu gc mail inbox <seat> --json
ls /tmp/gc-cpu.*
```

The profile is written to `<prefix>.<pid>`. The pid suffix is what lets one
exported prefix serve many processes: a hook that runs `gc` as a child, or a
shell that exports the variable for a whole session, gives every process its
own file instead of having them overwrite each other.

The directory must exist and be writable. The file is created with mode
`0600`.

## Reading It

Use the same `gc` binary that produced the profile, so symbols resolve:

```bash
go tool pprof -top -cum "$(command -v gc)" /tmp/gc-cpu.<pid>
```

Several profiles of the same command merge by listing them together, which
averages out the noise of a single short run:

```bash
go tool pprof -top -cum "$(command -v gc)" /tmp/gc-cpu.101 /tmp/gc-cpu.102 /tmp/gc-cpu.103
```

A one-shot `gc` run is short, so a single profile has few samples. Take
three to five before reading anything into a small slice.

## What To Expect

- **Output and exit code do not change.** If the profile cannot be created or
  started (unwritable path, a profile already running), `gc` prints one
  `gc: GC_CPUPROFILE: ...` line on stderr and runs the command unprofiled.
- **The profile is complete only when the command returns.** A process that
  is killed by a signal, or by a `gc hook run --timeout` expiring, leaves a
  partial or empty file. Raise the timeout while profiling a hook line.
- **It covers the whole command**, from dispatch through exit: config load,
  store opens, and the work `gc` does around its child processes. CPU spent
  inside a forked child (`bd`, `tmux`, `git`) does not appear; measure that
  with `/usr/bin/time` or `strace -f`.
- **Only one CPU profile exists per process.** If something else in the
  process already started one, `GC_CPUPROFILE` stands down rather than stop
  the other.

## Measuring A Change

For a before/after comparison, report CPU-seconds (user plus system,
including children) per call, with the median of several sequential runs on
an otherwise quiet host:

```bash
/usr/bin/time -f '%U %S %e' gc mail inbox <seat> --json
```

Use `GC_CPUPROFILE` to explain a number; use `/usr/bin/time` to report it.
