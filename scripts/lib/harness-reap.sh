#!/usr/bin/env bash
# harness-reap.sh — process-tree ownership for the sharded test harness.
#
# Two failure modes motivated this (gas-px0, routed from kit-k72d):
#
#   1. LOAD. A gc.test binary was measured at PPID 1 burning ~1 core for 5h34m
#      despite -test.timeout=20m0s. A test that wedges in a non-preemptible
#      loop, or deadlocks the runtime, defeats go's own timeout goroutine; if
#      the shard-runner chain then dies (ctrl-C, session teardown, driver
#      killed), the test binary orphans to init and nothing reaps it.
#   2. LITTER. Orphaned tmux servers accumulate across runs, each holding a
#      test socket, at zero CPU.
#
# The runner already owns the process tree, so it is the right place to fix
# both: gc_harness_run_supervised gives each product run its own process group
# and kills the group on exit or signal, and gc_harness_sweep_stale_orphans
# clears what earlier runs already stranded.
#
# Portability: macOS has no setsid(1), so process-group isolation is done with
# bash job control (`set -m`), which is POSIX and works under bash 3.2 — the
# oldest bash the shard runner is tested against (see findBash32 in
# scripts/test_go_test_shard_test.go).
#
# Self-test: scripts/harness_reap_test.go (run by go test ./scripts).

# PID of the supervised run's process-group leader, published so a caller's
# signal trap can tear the group down. Empty when no run is in flight.
# shellcheck disable=SC2034  # read by the sourcing entrypoints' signal traps
gc_harness_supervised_pgid=""

# gc_harness_duration_seconds converts a Go-style duration of the form
# <n>[s|m|h] (a bare <n> is seconds) to whole seconds on stdout. It fails on
# any other spelling rather than guessing, so an unrecognized GO_TEST_TIMEOUT
# disables the watchdog instead of arming it at a wrong budget.
gc_harness_duration_seconds() {
  local value="${1:-}"
  [[ -n "$value" ]] || return 1
  [[ "$value" =~ ^([0-9]+)([smh]?)$ ]] || return 1
  local number="${BASH_REMATCH[1]}"
  case "${BASH_REMATCH[2]}" in
    ""|s) printf '%s' "$number" ;;
    m) printf '%s' "$(( number * 60 ))" ;;
    h) printf '%s' "$(( number * 3600 ))" ;;
  esac
}

# gc_harness_group_alive reports whether any process remains in process group
# pgid. Signal 0 against a negative PID probes the whole group.
gc_harness_group_alive() {
  local pgid="${1:-}"
  [[ -n "$pgid" ]] || return 1
  kill -0 -- -"$pgid" 2>/dev/null
}

# gc_harness_signal_group sends sig to every process in process group pgid.
# Best-effort: a group that has already drained is not an error.
gc_harness_signal_group() {
  local pgid="${1:-}" sig="${2:-TERM}"
  [[ -n "$pgid" ]] || return 0
  kill -"$sig" -- -"$pgid" 2>/dev/null || true
}

# gc_harness_terminate_group ends process group pgid: SIGTERM, then SIGKILL
# for anything still standing after the drain window. It returns immediately
# when the group is already empty, so the happy path pays no latency.
#
# drain_ticks (of 0.2s each) sizes that window. A supervisor of supervisors —
# the parallel fan-out, whose children are themselves shard runners draining
# their own nested groups — must wait longer than its children do, or its
# SIGKILL lands before a child's trap has finished tearing its own group down
# and re-orphans exactly what this is here to prevent.
gc_harness_terminate_group() {
  local pgid="${1:-}" drain_ticks="${2:-25}"
  gc_harness_group_alive "$pgid" || return 0
  gc_harness_signal_group "$pgid" TERM
  local tick=0
  while (( tick < drain_ticks )); do
    gc_harness_group_alive "$pgid" || return 0
    sleep 0.2
    tick=$(( tick + 1 ))
  done
  gc_harness_signal_group "$pgid" KILL
}

# gc_harness_watchdog kills process group pgid once fire_after seconds have
# passed. SIGQUIT goes first on purpose: the Go runtime answers it by dumping
# every goroutine's stack, which names the wedged test in the shard log. That
# is the only artifact that answers "which test wedges" for a hang that
# defeats -test.timeout, and kit-k72d had to be diagnosed without it.
gc_harness_watchdog() {
  local pgid="${1}" fire_after="${2}" quit_grace="${3}" label="${4}"
  sleep "$fire_after"
  gc_harness_group_alive "$pgid" || return 0
  printf 'harness watchdog: %s exceeded its %ss budget and defeated go'\''s own -timeout; dumping goroutine stacks (SIGQUIT) then killing the process group\n' \
    "$label" "$fire_after" >&2
  gc_harness_signal_group "$pgid" QUIT
  sleep "$quit_grace"
  gc_harness_signal_group "$pgid" KILL
}

# gc_harness_run_supervised runs a command in its own process group under a
# watchdog and returns the command's exit status.
#
#   gc_harness_run_supervised <label> <deadline-seconds|""> <quit-grace> -- cmd...
#
# An empty deadline runs without a watchdog. The caller is expected to trap
# INT/TERM/EXIT and call gc_harness_terminate_group "$gc_harness_supervised_pgid"
# so a signal to the runner tears down the run rather than orphaning it.
gc_harness_run_supervised() {
  local label="${1}" deadline="${2}" quit_grace="${3}"
  shift 3
  [[ "${1:-}" != "--" ]] || shift

  # Job control puts each background job in a fresh process group, which is
  # what makes the whole tree addressable as -PGID. Monitor mode is restored
  # to its prior setting straight after launch so the runner's own output is
  # not interleaved with job-status chatter; the group assignment persists.
  local monitor_was_on=0
  case "$-" in
    *m*) monitor_was_on=1 ;;
  esac
  set -m
  "$@" &
  local job_pgid=$!
  if (( monitor_was_on == 0 )); then
    set +m
  fi
  gc_harness_supervised_pgid="$job_pgid"

  # The watchdog gets a process group of its own, for two separate reasons.
  # It must not be inside the supervised group, or its own first signal would
  # kill it before it could escalate to SIGKILL. And it must be addressable as
  # a group when the run finishes early, because the watchdog is a subshell
  # blocked in sleep(1): signalling only the subshell's PID leaves that sleep
  # running as an orphan holding every descriptor it inherited — including the
  # caller's stdout pipe, which keeps the caller's read blocked for the full
  # remaining budget. That is the very failure this file exists to prevent,
  # reintroduced one level up.
  local watchdog_pgid=""
  if [[ -n "$deadline" ]]; then
    set -m
    gc_harness_watchdog "$job_pgid" "$deadline" "$quit_grace" "$label" &
    watchdog_pgid=$!
    if (( monitor_was_on == 0 )); then
      set +m
    fi
  fi

  local status=0
  wait "$job_pgid" || status=$?

  if [[ -n "$watchdog_pgid" ]]; then
    gc_harness_signal_group "$watchdog_pgid" KILL
    wait "$watchdog_pgid" 2>/dev/null || true
  fi
  # The group leader can exit while a descendant it spawned is still running;
  # that descendant is the orphan this whole file exists to prevent.
  gc_harness_terminate_group "$job_pgid"
  gc_harness_supervised_pgid=""
  return "$status"
}

# gc_harness_kill_pid ends a single stranded process. Every sweep kill goes
# through here: it is the one place the sweep destroys anything, which keeps
# that authority reviewable in isolation and gives the self-test a seam to
# record decisions instead of killing real processes.
gc_harness_kill_pid() {
  local pid="${1:-}"
  [[ -n "$pid" ]] || return 0
  kill -KILL "$pid" 2>/dev/null || true
}

# gc_harness_etime_seconds converts ps(1) ELAPSED ([[DD-]HH:]MM:SS) to whole
# seconds. Linux ps offers etimes directly, BSD/macOS ps does not, so the
# sweep parses the portable column instead of branching per platform.
gc_harness_etime_seconds() {
  local etime="${1:-}"
  [[ -n "$etime" ]] || return 1
  local days=0
  if [[ "$etime" == *-* ]]; then
    days="${etime%%-*}"
    etime="${etime#*-}"
  fi
  local hours=0 minutes seconds
  case "$etime" in
    *:*:*)
      hours="${etime%%:*}"
      etime="${etime#*:}"
      ;;
  esac
  minutes="${etime%%:*}"
  seconds="${etime##*:}"
  [[ "$days$hours$minutes$seconds" =~ ^[0-9]+$ ]] || return 1
  printf '%s' "$(( 10#$days * 86400 + 10#$hours * 3600 + 10#$minutes * 60 + 10#$seconds ))"
}

# gc_harness_sweep_stale_orphans kills test processes stranded by earlier
# runs, and reports each kill so a recurrence stays attributable from run logs
# instead of a fresh host census.
#
# Two populations, each identified by a marker no non-test process carries:
#
#   * Go test binaries (argv[0] ending in ".test") reparented to init. Their
#     parent `go test` is gone, so nothing else will ever reap them. These are
#     the kit-k72d core burners.
#   * Processes holding a Unix socket under a "<root>/gct-<PID>-<random>"
#     test socket-parent dir (test/tmuxtest's NewSocketParentDir shape) whose
#     owning run is gone. In practice these are tmux servers, which setsid()
#     out of the shard's process group when they daemonize and so survive the
#     group kill above by design.
#
# The socket-parent dir name carries its creator's PID, which is what decides
# liveness here: a dir that is gone, or whose creator is gone, cannot have a
# live owner. That reuses test/tmuxtest's existing ownership convention rather
# than inventing a second one. A dir that is gone is also the only signal
# left, because removing it unlinks the socket out from under a still-running
# server: the server keeps the open inode but its path no longer resolves, so
# no socket-scoped cleanup ("tmux -S <path> kill-server") can ever address it
# again. Measured on gas-px0: 8 of 9 stranded servers were already in that
# state, which is why they accumulate across runs.
#
# Safety: only sockets under a "gct-<PID>-<random>" path are ever considered,
# so a developer's own tmux server (socket ".../tmux-<uid>/default") is
# structurally unreachable from here — see the tmux-safety rule in AGENTS.md.
# Only the invoking user's processes are examined, and nothing younger than
# min_age_seconds is touched, which keeps a concurrently starting sibling run
# out of scope. Set GC_TEST_NO_ORPHAN_SWEEP=1 to skip the sweep entirely.
gc_harness_sweep_stale_orphans() {
  local min_age_seconds="${1:-3600}"
  [[ "${GC_TEST_NO_ORPHAN_SWEEP:-0}" != "1" ]] || return 0

  local reaped=0
  local pid ppid etime command age

  # Population 1: orphaned Go test binaries.
  while read -r pid ppid etime command; do
    [[ -n "${pid:-}" && -n "${command:-}" ]] || continue
    [[ "$ppid" == "1" ]] || continue
    [[ "$command" == *.test ]] || continue
    age="$(gc_harness_etime_seconds "$etime")" || continue
    (( age >= min_age_seconds )) || continue
    printf 'harness sweep: killing orphaned test binary pid %s (%s, age %ss, reparented to init)\n' \
      "$pid" "$command" "$age" >&2
    gc_harness_kill_pid "$pid"
    reaped=$(( reaped + 1 ))
  done < <(ps -u "$(id -u)" -o pid=,ppid=,etime=,comm= 2>/dev/null || true)

  # Population 2: processes parked on a test socket whose run is gone.
  if command -v lsof >/dev/null 2>&1; then
    local socket_pid socket_path owner_dir owner_pid
    while read -r socket_pid socket_path; do
      [[ -n "${socket_pid:-}" && -n "${socket_path:-}" ]] || continue
      # Trim the path back to the "<root>/gct-<PID>-<random>" component.
      owner_dir="${socket_path%%/tmux/*}"
      [[ "$owner_dir" != "$socket_path" ]] || continue
      [[ "${owner_dir##*/}" =~ ^gct-([0-9]+)-[0-9]+$ ]] || continue
      owner_pid="${BASH_REMATCH[1]}"
      # A live owner means a live run: leave it strictly alone.
      if [[ -d "$owner_dir" ]] && kill -0 "$owner_pid" 2>/dev/null; then
        continue
      fi
      etime="$(ps -o etime= -p "$socket_pid" 2>/dev/null | tr -d ' ')" || continue
      age="$(gc_harness_etime_seconds "$etime")" || continue
      (( age >= min_age_seconds )) || continue
      printf 'harness sweep: killing socket-parked pid %s (age %ss, run %s is gone)\n' \
        "$socket_pid" "$age" "${owner_dir##*/}" >&2
      gc_harness_kill_pid "$socket_pid"
      rm -rf "$owner_dir" 2>/dev/null || true
      reaped=$(( reaped + 1 ))
    done < <(gc_harness_test_socket_holders)
  fi

  (( reaped == 0 )) || printf 'harness sweep: reaped %s stale test process(es)\n' "$reaped" >&2
  return 0
}

# gc_harness_test_socket_holders prints "<pid> <socket-path>" for every Unix
# socket the invoking user holds under a "gct-<PID>-<random>" test
# socket-parent dir. lsof's field output is parsed rather than its columns
# because socket paths can contain spaces.
gc_harness_test_socket_holders() {
  lsof -U -n -P -F pn -u "$(id -u)" 2>/dev/null | awk '
    /^p/ { pid = substr($0, 2); next }
    /^n/ {
      path = substr($0, 2)
      if (pid != "" && path ~ /\/gct-[0-9]+-[0-9]+\//) {
        print pid, path
      }
    }
  '
}
