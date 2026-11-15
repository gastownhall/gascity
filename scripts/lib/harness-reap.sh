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
# scripts/test_go_test_shard_test.go). The watchdog's process identity reads
# /proc/PID/stat on Linux and `ps -o pgid=,lstart=` on macOS and the BSDs; a
# host with neither runs unguarded by the watchdog and says so.
#
# Watchdog identity rule (ga-plyjh4): the watchdog never signals a process
# group by number alone. A group number can be reused by a stranger once every
# member of the original group has exited, and kill -0 cannot tell the two
# apart. Before each signal the watchdog proves that a process it recorded as a
# member of the run, identified by pid and start token, is still alive in that
# group. When it cannot, it prints one refusal line and sends nothing:
#
#   harness watchdog: LABEL: not signalling process group N: REASON
#
# where REASON is one of: no identity supplied, identity unreadable, leader
# gone, leader identity changed (pid reused), leader left the group, no
# verified member survives. At arm time an unreadable identity prints
# "harness watchdog disarmed: cannot read the run's process identity (REASON)"
# instead, with the extra reason "unsupported platform".
#
# Self-tests: scripts/harness_reap_test.go and
# scripts/harness_watchdog_identity_test.go (run by bazel test //scripts:scripts_test).

# PID of the supervised run's process-group leader, published so a caller's
# signal trap can tear the group down. Empty when no run is in flight.
gc_harness_supervised_pgid=""

# PID of the run's watchdog process-group leader, published for the same
# reason. Empty when no run is in flight or the run has no watchdog.
gc_harness_watchdog_pgid=""

# A launch whose PID is not yet published: "run" or "watchdog" from just before
# its fork until gc_harness_launch_publish records the PID, otherwise empty.
# gc_harness_launch_prior_pid is $! as it stood before that fork (empty when
# nothing had been forked yet), which is how publishing tells whether the fork
# has happened yet.
#
# Both functions read $! only behind a (( ${#!} == 0 )) guard. $! is unset
# until a script forks its first background job, and under `set -u` bash 3.2
# aborts on any expansion of an unset $!, a defaulted one included; only its
# length is safe to take. The runners that source this file run `set -u` and
# need not have forked anything before their first launch.
gc_harness_launching=""
gc_harness_launch_prior_pid=""

# gc_harness_launch_begin opens the launch window for kind ("run" or
# "watchdog"). Call it immediately before forking the job with `&`, and call
# gc_harness_launch_publish immediately after.
#
# The window exists because bash runs a signal trap between any two commands:
# a signal that lands after the fork but before `pgid=$!` would find no PID to
# tear down, and the just-forked job would outlive the runner holding every
# descriptor it inherited, the caller's stdout included (ga-96smfk.61).
gc_harness_launch_begin() {
  gc_harness_launch_prior_pid=""
  (( ${#!} == 0 )) || gc_harness_launch_prior_pid="$!"
  gc_harness_launching="${1}"
}

# gc_harness_launch_publish records the PID of the job forked since
# gc_harness_launch_begin, then closes the window. It is idempotent, and safe
# to call from a signal trap: bash sets $! in the same step as the fork, before
# any trap can run, so if $! has moved the fork has happened and $! is its PID;
# if not, nothing was forked and there is nothing to record.
gc_harness_launch_publish() {
  [[ -n "${gc_harness_launching}" ]] || return 0
  local last_pid=""
  (( ${#!} == 0 )) || last_pid="$!"
  if [[ "${last_pid}" != "${gc_harness_launch_prior_pid}" ]]; then
    case "${gc_harness_launching}" in
      run) gc_harness_supervised_pgid="${last_pid}" ;;
      watchdog) gc_harness_watchdog_pgid="${last_pid}" ;;
    esac
  fi
  gc_harness_launching=""
}

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

# gc_harness_kill_group ends process group pgid with SIGKILL and returns once
# the group is empty, bounded by kill_ticks (of 0.2s each). A process the
# group was forking when the signal was sent can miss it on macOS, whose
# fork does not retry for a pending group signal (Linux's does): the forked
# child lands in the group alive, orphaned, holding every descriptor it
# inherited (mc-08rl5, the watchdog's `sleep` caught mid-fork). So the
# group is signaled again until no member is left. leader, when given, is a
# child of this shell, reaped first so its zombie does not read as alive.
gc_harness_kill_group() {
  local pgid="${1:-}" leader="${2:-}" kill_ticks="${3:-25}"
  [[ -n "$pgid" ]] || return 0
  gc_harness_signal_group "$pgid" KILL
  if [[ -n "$leader" ]]; then
    wait "$leader" 2>/dev/null || true
  fi
  local tick=0
  while (( tick < kill_ticks )); do
    gc_harness_group_alive "$pgid" || return 0
    gc_harness_signal_group "$pgid" KILL
    sleep 0.2
    tick=$(( tick + 1 ))
  done
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

# gc_harness_terminate_supervised ends the supervised run in flight, if any:
# its watchdog first, then the run's own process group, drained as
# gc_harness_terminate_group does with drain_ticks. This is the teardown a
# caller's signal and EXIT traps run. Ending only the run's group is not
# enough, because the watchdog lives in a group of its own: left behind, it
# sleeps out its budget as an orphan holding the caller's descriptors, then
# signals the run's process-group id, which by then the kernel may have
# handed to an unrelated group (ga-880tzy).
gc_harness_terminate_supervised() {
  local drain_ticks="${1:-25}"
  # A signal can arrive between a fork and the assignment of its PID; adopt
  # that job so it is torn down like any other.
  gc_harness_launch_publish
  if [[ -n "${gc_harness_watchdog_pgid:-}" ]]; then
    # Its leader reaped here, as on the normal path, so bash does not report
    # the killed job in the run's log; a just-forked watchdog may be forking
    # its sleep, which the group kill re-signals.
    gc_harness_kill_group "$gc_harness_watchdog_pgid" "$gc_harness_watchdog_pgid"
    gc_harness_watchdog_pgid=""
  fi
  gc_harness_terminate_group "${gc_harness_supervised_pgid:-}" "$drain_ticks"
}

# Process identity: how the watchdog proves a process-group number still names
# the group it was started for (ga-plyjh4, ga-66qxub).
#
# A process-group number can be handed to an unrelated group once every member
# of the original has exited, so "a group with this number exists" (kill -0)
# proves nothing about whose group it is. A process is identified instead by
# (pid, start token): a pid reused by a stranger carries a different token. The
# kernel never reallocates a number while any process still has it as its pid
# or group, so a verified live member of the original group proves the number
# is still the original group's.
#
# The token is field 22 of /proc/PID/stat (start time in ticks) on Linux, and
# `ps -o lstart=` on macOS and the BSDs. Linux does not use ps lstart: its
# resolution is one second, too coarse to tell a reused pid apart.

# gc_harness_identity_source is set by gc_harness_select_identity_source to
# "procfs", "ps" or "none". The detection result is cached across calls made in
# one shell.
gc_harness_identity_source=""
gc_harness_identity_source_detected=""

# gc_harness_select_identity_source sets gc_harness_identity_source:
# procfs when /proc/self/stat yields the 22 fields, ps when the BSD ps form
# prints a line for this shell, none otherwise. GC_TEST_HARNESS_IDENTITY_SOURCE
# (procfs|ps|none) forces a branch for tests and is inert when unset.
gc_harness_select_identity_source() {
  case "${GC_TEST_HARNESS_IDENTITY_SOURCE:-}" in
    procfs|ps|none)
      gc_harness_identity_source="${GC_TEST_HARNESS_IDENTITY_SOURCE}"
      return 0
      ;;
  esac
  if [[ -z "$gc_harness_identity_source_detected" ]]; then
    local IFS=$' \t\n' line="" rest="" detected="none"
    if { IFS= read -r line; } 2>/dev/null < /proc/self/stat; then
      rest="${line##*) }"
      if [[ "$rest" != "$line" ]]; then
        # shellcheck disable=SC2086 # word splitting is the point: fields of a stat or ps line
        set -- $rest
        if (( $# >= 20 )); then
          detected=procfs
        fi
      fi
    fi
    if [[ "$detected" == none ]] && [[ -n "$(LC_ALL=C ps -o pgid=,lstart= -p "$$" 2>/dev/null)" ]]; then
      detected="ps"
    fi
    gc_harness_identity_source_detected="$detected"
  fi
  gc_harness_identity_source="$gc_harness_identity_source_detected"
}

# gc_harness_read_identity reads the process group and start token of pid into
# gc_harness_identity_pgrp and gc_harness_identity_token, in one snapshot, and
# fails when the process is absent or the platform is unsupported. It spawns
# nothing on the procfs path.
gc_harness_identity_pgrp=""
gc_harness_identity_token=""
gc_harness_read_identity() {
  local IFS=$' \t\n' pid="${1:-}" line="" rest="" result=""
  gc_harness_identity_pgrp=""
  gc_harness_identity_token=""
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  gc_harness_select_identity_source
  case "$gc_harness_identity_source" in
    procfs)
      { IFS= read -r line; } 2>/dev/null < "/proc/$pid/stat" || return 1
      # The command name (field 2) may hold spaces and parentheses, so strip
      # through the last ") " rather than splitting from the left. What is
      # left starts at field 3: the state, so PGRP is $3 and the start time
      # (field 22) is ${20}.
      rest="${line##*) }"
      [[ "$rest" != "$line" ]] || return 1
      # shellcheck disable=SC2086 # word splitting is the point: fields of a stat or ps line
      set -- $rest
      (( $# >= 20 )) || return 1
      [[ "$3" =~ ^[0-9]+$ && "${20}" =~ ^[0-9]+$ ]] || return 1
      gc_harness_identity_pgrp="$3"
      gc_harness_identity_token="${20}"
      ;;
    ps)
      result="$(LC_ALL=C ps -o pgid=,lstart= -p "$pid" 2>/dev/null)" || return 1
      # shellcheck disable=SC2086 # word splitting is the point: fields of a stat or ps line
      set -- $result
      (( $# >= 2 )) || return 1
      [[ "$1" =~ ^[0-9]+$ ]] || return 1
      gc_harness_identity_pgrp="$1"
      gc_harness_identity_token="${*:2}"
      ;;
    *)
      return 1
      ;;
  esac
}

# gc_harness_proc_identity prints "PGRP TOKEN" for pid and returns 0, or
# returns non-zero when the process is absent or its identity unreadable.
gc_harness_proc_identity() {
  gc_harness_read_identity "${1:-}" || return 1
  printf '%s %s\n' "$gc_harness_identity_pgrp" "$gc_harness_identity_token"
}

# gc_harness_group_members prints "PID TOKEN" for every process currently in
# process group pgid. One ps(1) pass lists the host; on Linux each member's
# token then comes from /proc, on BSD from the same ps call.
gc_harness_group_members() {
  local IFS=$' \t\n' pgid="${1:-}" listing="" pid="" pg="" rest=""
  [[ "$pgid" =~ ^[0-9]+$ ]] || return 1
  gc_harness_select_identity_source
  case "$gc_harness_identity_source" in
    procfs)
      listing="$(ps -A -o pid=,pgid= 2>/dev/null)" || return 1
      while read -r pid pg rest; do
        [[ "$pg" == "$pgid" ]] || continue
        gc_harness_read_identity "$pid" || continue
        [[ "$gc_harness_identity_pgrp" == "$pgid" ]] || continue
        printf '%s %s\n' "$pid" "$gc_harness_identity_token"
      done <<< "$listing"
      ;;
    ps)
      listing="$(LC_ALL=C ps -A -o pid=,pgid=,lstart= 2>/dev/null)" || return 1
      while read -r pid pg rest; do
        [[ "$pg" == "$pgid" ]] || continue
        # shellcheck disable=SC2086 # word splitting is the point: fields of a stat or ps line
        set -- $rest
        (( $# > 0 )) || continue
        printf '%s %s\n' "$pid" "$*"
      done <<< "$listing"
      ;;
    *)
      return 1
      ;;
  esac
}

# gc_harness_watchdog_refuse prints the one-line refusal for a group the
# watchdog will not signal. The wording is contract: tests and operators match
# on it.
gc_harness_watchdog_refuse() {
  printf 'harness watchdog: %s: not signalling process group %s: %s\n' "${1}" "${2}" "${3}" >&2
}

# gc_harness_watchdog_refresh renews the watchdog's lease on its process group.
# It is a helper of gc_harness_watchdog and works on that function's locals
# (proven, refresh_state, refresh_reason) through bash's dynamic scoping.
#
# proven is a set of "PID TOKEN" lines: processes known to belong to the run,
# the leader alone at the start. A refresh:
#   1. finds the group absent (kill -0), or
#   2. enumerates the group's current members (candidates), then
#   3. verifies each proven member: alive, same token, still in the group.
# If at least one verifies, the number is pinned, because a verified live
# member keeps the kernel from reusing it, and proven becomes all candidates
# (plus the verified members). Candidates are never trusted on their own:
# otherwise a stranger group that took the number would be adopted. If none
# verifies while the group exists, the group is unproven.
#
# refresh_state is set to absent, pinned or unproven; on unproven,
# refresh_reason names why. When the leader is the only member ever recorded
# the reason is the leader's own failure mode, otherwise "no verified member
# survives".
gc_harness_watchdog_refresh() {
  local IFS=$' \t\n' pgid="${1}" candidates="" verified="" pid="" token="" ident=""
  local why="" leader_reason="" count=0 first_pid="" found=0 cpid=""
  refresh_state=""
  refresh_reason=""
  if ! gc_harness_group_alive "$pgid"; then
    refresh_state=absent
    return 0
  fi
  candidates="$(gc_harness_group_members "$pgid")" || candidates=""
  while read -r pid token; do
    [[ -n "$pid" ]] || continue
    count=$(( count + 1 ))
    first_pid="$pid"
    if ident="$(gc_harness_proc_identity "$pid")"; then
      if [[ "${ident#* }" != "$token" ]]; then
        why="leader identity changed (pid reused)"
      elif [[ "${ident%% *}" != "$pgid" ]]; then
        why="leader left the group"
      else
        verified="${verified}${pid} ${token}"$'\n'
        continue
      fi
    elif kill -0 "$pid" 2>/dev/null; then
      why="identity unreadable"
    else
      why="leader gone"
    fi
    if [[ "$pid" == "$pgid" ]]; then
      leader_reason="$why"
    fi
  done <<< "$proven"
  if [[ -z "$verified" ]]; then
    refresh_state=unproven
    if (( count == 1 )) && [[ "$first_pid" == "$pgid" && -n "$leader_reason" ]]; then
      refresh_reason="$leader_reason"
    else
      refresh_reason="no verified member survives"
    fi
    return 0
  fi
  # Lease renewed: the verified members pin the number, so everything in the
  # group right now belongs to the run.
  proven="$candidates"
  [[ -z "$proven" ]] || proven="${proven}"$'\n'
  while read -r pid token; do
    [[ -n "$pid" ]] || continue
    found=0
    while read -r cpid _; do
      if [[ "$cpid" == "$pid" ]]; then
        found=1
      fi
    done <<< "$candidates"
    if (( found == 0 )); then
      proven="${proven}${pid} ${token}"$'\n'
    fi
  done <<< "$verified"
  refresh_state=pinned
}

# gc_harness_watchdog kills process group pgid once fire_after seconds have
# passed, but only while it can prove the group is still the one it was
# started for (see "Process identity" above). SIGQUIT goes first on purpose:
# the Go runtime answers it by dumping every goroutine's stack, which names the
# wedged test in the shard log. That is the only artifact that answers "which
# test wedges" for a hang that defeats -test.timeout, and kit-k72d had to be
# diagnosed without it.
#
#   gc_harness_watchdog PGID FIRE_AFTER QUIT_GRACE LABEL LEADER_IDENTITY [TICK]
#
# LEADER_IDENTITY is the "PGRP TOKEN" the group leader had when the watchdog
# was armed (gc_harness_proc_identity). An empty or absent identity refuses.
# TICK (default 30 seconds) is how often the lease is renewed while waiting;
# a group that has vanished releases the watchdog at the next tick instead of
# the end of the budget. The schedule is otherwise unchanged: QUIT at
# FIRE_AFTER, KILL QUIT_GRACE later, each preceded by a fresh proof and sent
# with nothing in between. On a failed proof the watchdog prints one refusal
# line and stops without signalling. It returns 0 on every path.
gc_harness_watchdog() {
  local pgid="${1:-}" fire_after="${2:-0}" quit_grace="${3:-0}" label="${4:-}"
  local leader_identity="${5:-}" tick="${6:-30}"
  local proven="" refresh_state="" refresh_reason="" slept=0 chunk=0
  [[ "$tick" =~ ^[1-9][0-9]*$ ]] || tick=30
  if [[ -z "$leader_identity" || "$leader_identity" != *" "* \
    || "${leader_identity%% *}" != "$pgid" || -z "${leader_identity#* }" ]]; then
    gc_harness_watchdog_refuse "$label" "$pgid" "no identity supplied"
    return 0
  fi
  proven="${pgid} ${leader_identity#* }"$'\n'

  while :; do
    chunk=$(( fire_after - slept ))
    if (( chunk > tick )); then
      chunk="$tick"
    fi
    if (( chunk > 0 )); then
      sleep "$chunk"
    fi
    slept=$(( slept + chunk ))
    gc_harness_watchdog_refresh "$pgid"
    case "$refresh_state" in
      absent)
        return 0
        ;;
      unproven)
        gc_harness_watchdog_refuse "$label" "$pgid" "$refresh_reason"
        return 0
        ;;
    esac
    if (( slept >= fire_after )); then
      break
    fi
  done

  printf 'harness watchdog: %s exceeded its %ss budget and defeated go'\''s own -timeout; dumping goroutine stacks (SIGQUIT) then killing the process group\n' \
    "$label" "$fire_after" >&2
  gc_harness_signal_group "$pgid" QUIT
  sleep "$quit_grace"
  gc_harness_watchdog_refresh "$pgid"
  case "$refresh_state" in
    absent)
      return 0
      ;;
    unproven)
      gc_harness_watchdog_refuse "$label" "$pgid" "$refresh_reason"
      return 0
      ;;
  esac
  gc_harness_signal_group "$pgid" KILL
  return 0
}

# gc_harness_arm_identity reads the identity of the just-launched run leader
# pid for the watchdog. On success it sets gc_harness_arm_identity_value to
# "PGRP TOKEN" and returns 0. Otherwise it returns 1 with
# gc_harness_arm_reason set: "gone" when the leader has already exited (nothing
# to guard, and no notice is due), or the disarm reason: "unsupported
# platform", "identity unreadable" or "leader left the group". The run leader
# must be in its own group (pgrp equal to pid); it is read up to three times,
# 20ms apart, only while that is not yet true.
gc_harness_arm_identity_value=""
gc_harness_arm_reason=""
gc_harness_arm_identity() {
  local pid="${1:-}" attempt=0
  gc_harness_arm_identity_value=""
  gc_harness_arm_reason=""
  gc_harness_select_identity_source
  if [[ "$gc_harness_identity_source" == none ]]; then
    gc_harness_arm_reason="unsupported platform"
    return 1
  fi
  while :; do
    if gc_harness_read_identity "$pid"; then
      if [[ "$gc_harness_identity_pgrp" == "$pid" ]]; then
        gc_harness_arm_identity_value="${gc_harness_identity_pgrp} ${gc_harness_identity_token}"
        return 0
      fi
      gc_harness_arm_reason="leader left the group"
    elif kill -0 "$pid" 2>/dev/null; then
      gc_harness_arm_reason="identity unreadable"
    else
      gc_harness_arm_reason="gone"
      return 1
    fi
    attempt=$(( attempt + 1 ))
    (( attempt < 3 )) || return 1
    sleep 0.02
  done
}

# gc_harness_run_supervised runs a command in its own process group under a
# watchdog and returns the command's exit status.
#
#   gc_harness_run_supervised <label> <deadline-seconds|""> <quit-grace> -- cmd...
#
# An empty deadline runs without a watchdog. The caller is expected to trap
# INT/TERM/EXIT and call gc_harness_terminate_supervised so a signal to the
# runner tears down the run and its watchdog rather than orphaning them. That
# signal trap must then exit or re-raise the signal, as both runners' traps
# do, never return: a signal that lands just before a fork finds nothing to
# tear down and closes the launch window, so a trap that returned would resume
# into a fork whose PID is never published, and nothing would tear that job
# down.
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
  # Bash resets an inherited-but-not-locally-reinstalled trap to SIG_DFL for an
  # async subshell (`cmd &`), so the job takes QUIT's default disposition —
  # terminate plus core dump — even though the caller trapped QUIT before
  # backgrounding it. gc_harness_watchdog signals this job's group directly
  # (never the caller's own pid), so it is this subshell, not the caller, that
  # needs its own trap reinstalled. `exit 131` (128+SIGQUIT) reproduces the
  # signal's own exit-status convention without leaving QUIT uncaught; the
  # group teardown below (gc_harness_terminate_group) already runs
  # unconditionally once `wait` returns, so this trap does not duplicate it.
  gc_harness_launch_begin run
  ( trap 'exit 131' QUIT; "$@" ) &
  gc_harness_launch_publish
  local job_pgid="$gc_harness_supervised_pgid"
  if (( monitor_was_on == 0 )); then
    set +m
  fi

  # The watchdog gets a process group of its own, for two separate reasons.
  # It must not be inside the supervised group, or its own first signal would
  # kill it before it could escalate to SIGKILL. And it must be addressable as
  # a group when the run finishes early, because the watchdog is a subshell
  # blocked in sleep(1): signalling only the subshell's PID leaves that sleep
  # running as an orphan holding every descriptor it inherited — including the
  # caller's stdout pipe, which keeps the caller's read blocked for the full
  # remaining budget. That is the very failure this file exists to prevent,
  # reintroduced one level up.
  #
  # It is armed with the leader's identity, read now while the leader is
  # certainly the process this run launched. A watchdog that cannot read it is
  # not armed at all (the sweep stays the backstop), and says so, rather than
  # signalling a group number it cannot vouch for.
  local watchdog_pgid=""
  if [[ -n "$deadline" ]]; then
    if gc_harness_arm_identity "$job_pgid"; then
      set -m
      gc_harness_launch_begin watchdog
      gc_harness_watchdog "$job_pgid" "$deadline" "$quit_grace" "$label" "$gc_harness_arm_identity_value" &
      gc_harness_launch_publish
      watchdog_pgid="$gc_harness_watchdog_pgid"
      if (( monitor_was_on == 0 )); then
        set +m
      fi
    elif [[ "$gc_harness_arm_reason" != gone ]]; then
      printf 'harness watchdog disarmed: cannot read the run'\''s process identity (%s)\n' \
        "$gc_harness_arm_reason" >&2
    fi
  fi

  local status=0
  wait "$job_pgid" || status=$?

  if [[ -n "$watchdog_pgid" ]]; then
    gc_harness_kill_group "$watchdog_pgid" "$watchdog_pgid"
  fi
  gc_harness_watchdog_pgid=""
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
