package scripts_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test snapshot is intentionally independent of the library function.
// It lets refusal tests fail because a signal was sent, rather than because
// gc_harness_proc_identity has not been implemented yet.
const testIdentitySnapshot = `
test_identity() {
  local line rest
  if [[ "${GC_TEST_HARNESS_IDENTITY_SOURCE:-}" != ps && -r "/proc/$1/stat" ]]; then
    IFS= read -r line < "/proc/$1/stat" || return 1
    rest="${line##*) }"
    set -- $rest
    printf '%s %s' "$3" "${20}"
  else
    LC_ALL=C ps -o pgid=,lstart= -p "$1" | awk '{p=$1; $1=""; sub(/^ +/, ""); print p, $0}'
  fi
}
test_group() {
  set -m
  bash -c "$1" &
  test_pgid=$!
  set +m
  test_identity="$(test_identity "$test_pgid")"
  [[ "${test_identity%% *}" == "$test_pgid" ]]
  echo "TEST_GROUP $test_pgid"
}
test_cleanup() {
  if [[ -n "${test_pgid:-}" ]]; then
    kill -KILL -- -"$test_pgid" 2>/dev/null || true
    wait "$test_pgid" 2>/dev/null || true
  fi
}
test_live_members() {
  ps -A -o pgid=,stat= | awk -v pgid="$1" '$1 == pgid && $2 !~ /^Z/ { found=1 } END { exit !found }'
}
trap test_cleanup EXIT
`

func watchdogScript(t *testing.T, body string, env ...string) (string, error) {
	t.Helper()
	script := fmt.Sprintf("source %q\n%s\n%s", filepath.Join(repoRoot(t), "scripts/lib/harness-reap.sh"), testIdentitySnapshot, body)
	return runHarnessScript(t, script, env...)
}

func requireWatchdogScript(t *testing.T, body string, env ...string) string {
	t.Helper()
	out, err := watchdogScript(t, body, env...)
	if err != nil {
		t.Fatalf("watchdog fixture failed: %v\n%s", err, out)
	}
	return out
}

func assertRefusal(t *testing.T, output string, pgid string, reason string) {
	t.Helper()
	want := "harness watchdog: identity-test: not signaling process group " + pgid + ": " + reason
	if strings.Count(output, want) != 1 {
		t.Fatalf("want exactly one refusal %q; output:\n%s", want, output)
	}
	if strings.Contains(output, "exceeded its") {
		t.Fatalf("refused watchdog printed the escalation budget line:\n%s", output)
	}
}

// V1: the old four-argument call must never retain a liveness-only signal path.
func TestHarnessWatchdogRejectsMissingIdentity(t *testing.T) {
	out := requireWatchdogScript(t, `
set -eu
test_group 'sleep 12'
gc_harness_signal_group() { printf 'SIGNAL %s %s\n' "$2" "$1"; }
gc_harness_watchdog "$test_pgid" 1 1 identity-test
sleep 2
kill -0 -- -"$test_pgid"
`)
	assertRefusal(t, out, groupIDFromOutput(t, out), "no identity supplied")
	if strings.Contains(out, "SIGNAL ") {
		t.Fatalf("legacy watchdog signaled a live stand-in group:\n%s", out)
	}
}

// groupIDFromOutput reads the fixture's explicit owner line. This makes every
// diagnostic assertion refer to a PGID created by this test, never a guessed
// number that might belong to another host process.
func groupIDFromOutput(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "TEST_GROUP ") {
			id := strings.TrimPrefix(line, "TEST_GROUP ")
			if _, err := strconv.Atoi(id); err == nil {
				return id
			}
		}
	}
	t.Fatalf("fixture did not report its owned process group:\n%s", out)
	return ""
}

// V2: a different process's start token cannot authorize QUIT of a live group.
func TestHarnessWatchdogRejectsReusedLeaderIdentity(t *testing.T) {
	out := requireWatchdogScript(t, `
set -eu
sleep 1 & old=$!
old_identity="$(test_identity "$old")"
wait "$old"
test_group 'sleep 12'
gc_harness_signal_group() { printf 'SIGNAL %s %s\n' "$2" "$1"; }
gc_harness_watchdog "$test_pgid" 1 1 identity-test "$test_pgid ${old_identity#* }"
kill -0 -- -"$test_pgid"
`)
	assertRefusal(t, out, groupIDFromOutput(t, out), "leader identity changed (pid reused)")
	if strings.Contains(out, "SIGNAL ") {
		t.Fatalf("stale token authorized a signal:\n%s", out)
	}
}

// A live numeric group cannot authorize QUIT when its leader's snapshot is
// unreadable or shows that the leader moved to another group.
func TestHarnessWatchdogRejectsUnprovenLeader(t *testing.T) {
	for _, tc := range []struct {
		name, reader, reason string
	}{
		{"identity-unreadable", `gc_harness_proc_identity() { return 1; }`, "identity unreadable"},
		{"leader-left-group", `gc_harness_proc_identity() { printf '0 %s\n' "${test_identity#* }"; }`, "leader left the group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := requireWatchdogScript(t, fmt.Sprintf(`
set -eu
test_group 'sleep 12'
%s
gc_harness_signal_group() { printf 'SIGNAL %%s %%s\n' "$2" "$1"; }
gc_harness_watchdog "$test_pgid" 1 1 identity-test "$test_identity"
kill -0 -- -"$test_pgid"
`, tc.reader))
			assertRefusal(t, out, groupIDFromOutput(t, out), tc.reason)
			if strings.Contains(out, "SIGNAL ") {
				t.Fatalf("unproven leader authorized a signal:\n%s", out)
			}
		})
	}
}

// V3: a verified original group still gets QUIT at the deadline and KILL
// after the grace, even though its members ignore catchable signals.
func TestHarnessWatchdogEscalatesVerifiedGroup(t *testing.T) {
	out := requireWatchdogScript(t, `
set -eu
test_group 'trap "" QUIT TERM; sleep 12 & wait'
gc_harness_watchdog "$test_pgid" 1 1 identity-test "$test_identity"
wait "$test_pgid" 2>/dev/null || true
if test_live_members "$test_pgid"; then echo GROUP_SURVIVED; fi
`)
	if !strings.Contains(out, "identity-test exceeded its 1s budget") || strings.Contains(out, "GROUP_SURVIVED") {
		t.Fatalf("verified wedged group did not drain after escalation:\n%s", out)
	}
}

// V4: surviving members do not prove a killed leader's identity at QUIT.
func TestHarnessWatchdogRejectsLeaderlessGroupAtQuit(t *testing.T) {
	out := requireWatchdogScript(t, `
set -eu
test_group 'sleep 12 & echo $! > "$FIXTURE_DIR/child"; wait'
for i in $(seq 1 100); do [[ -s "$FIXTURE_DIR/child" ]] && break; sleep 0.02; done
[[ -s "$FIXTURE_DIR/child" ]]
kill -KILL "$test_pgid"
wait "$test_pgid" 2>/dev/null || true
gc_harness_signal_group() { printf 'SIGNAL %s %s\n' "$2" "$1"; }
gc_harness_watchdog "$test_pgid" 1 1 identity-test "$test_identity"
test_live_members "$test_pgid"
`, "FIXTURE_DIR="+t.TempDir())
	assertRefusal(t, out, groupIDFromOutput(t, out), "leader gone")
	if strings.Contains(out, "SIGNAL ") {
		t.Fatalf("leaderless group was signaled:\n%s", out)
	}
}

// V5: the P2 proof must be fresh. A deterministic reader seam changes every
// witness token only after QUIT; the recording seam makes all signals harmless.
func TestHarnessWatchdogRejectsChangedWitnessAtKill(t *testing.T) {
	out := requireWatchdogScript(t, `
set -eu
test_group 'sleep 12 & wait'
changed=0
gc_harness_proc_identity() {
  if [[ "$changed" == 1 ]]; then printf '%s 0\n' "$test_pgid"; else test_identity "$1"; fi
}
gc_harness_signal_group() {
  printf 'SIGNAL %s %s\n' "$2" "$1"
  [[ "$2" != QUIT ]] || changed=1
}
gc_harness_watchdog "$test_pgid" 1 1 identity-test "$test_identity"
kill -0 -- -"$test_pgid"
`)
	pgid := groupIDFromOutput(t, out)
	if strings.Count(out, "SIGNAL QUIT "+pgid) != 1 || strings.Contains(out, "SIGNAL KILL ") {
		t.Fatalf("want QUIT only after witness identity changed:\n%s", out)
	}
	if !strings.Contains(out, "not signaling process group "+pgid+": no verified member survives") {
		t.Fatalf("missing P2 refusal:\n%s", out)
	}
}

// V6: P2 accepts a surviving QUIT-time member after the leader dies.
func TestHarnessWatchdogKillsSurvivingWitness(t *testing.T) {
	out := requireWatchdogScript(t, `
set -eu
test_group 'bash -c '\''trap "" QUIT TERM; sleep 12'\'' & wait'
gc_harness_watchdog "$test_pgid" 1 1 identity-test "$test_identity"
wait "$test_pgid" 2>/dev/null || true
if test_live_members "$test_pgid"; then echo GROUP_SURVIVED; fi
`)
	if !strings.Contains(out, "identity-test exceeded its 1s budget") || strings.Contains(out, "GROUP_SURVIVED") {
		t.Fatalf("surviving QUIT-time member did not get KILL:\n%s", out)
	}
}

// V7: an unsupported identity source disarms before forking a watchdog.
func TestHarnessWatchdogDisarmsWhenIdentityUnsupported(t *testing.T) {
	out := requireWatchdogScript(t, `
set -u
gc_harness_watchdog() { echo ARMED; }
status=0
gc_harness_run_supervised identity-test 1 1 -- bash -c 'exit 7' || status=$?
printf 'STATUS %s WATCHDOG_PGID <%s>\n' "$status" "$gc_harness_watchdog_pgid"
`, "GC_TEST_HARNESS_IDENTITY_SOURCE=none")
	if !strings.Contains(out, "harness watchdog disarmed: cannot read the run's process identity (unsupported platform)") ||
		!strings.Contains(out, "STATUS 7 WATCHDOG_PGID <>") || strings.Contains(out, "ARMED") {
		t.Fatalf("unsupported identity did not disarm while preserving command status:\n%s", out)
	}
}

// V8: forcing the ps source on Linux must apply the same stale and valid
// identity rules. macOS exercises its unmodified production ps path.
func TestHarnessWatchdogPSIdentitySource(t *testing.T) {
	var env []string
	if runtime.GOOS == "linux" {
		ps, err := exec.LookPath("ps")
		if err != nil {
			t.Fatal(err)
		}
		bin := t.TempDir()
		writeExecutable(t, filepath.Join(bin, "ps"), fmt.Sprintf("#!/bin/sh\nprintf 'PS_CALL %%s\\n' \"$*\" >&2\nexec %q \"$@\"\n", ps))
		env = append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	env = append(env, "GC_TEST_HARNESS_IDENTITY_SOURCE=ps")
	out := requireWatchdogScript(t, `
set -eu
sleep 1 & old=$!
old_identity="$(test_identity "$old")"
wait "$old"
test_group 'sleep 12'
gc_harness_signal_group() { printf 'SIGNAL %s %s\n' "$2" "$1"; }
gc_harness_watchdog "$test_pgid" 1 1 identity-test "$test_pgid ${old_identity#* }"
echo PHASE_VALID
gc_harness_watchdog "$test_pgid" 1 1 identity-test "$test_identity"
kill -0 -- -"$test_pgid"
`, env...)
	pgid := groupIDFromOutput(t, out)
	before, after, found := strings.Cut(out, "PHASE_VALID")
	if !found || !strings.Contains(before, "not signaling process group "+pgid+": leader identity changed (pid reused)") || strings.Contains(before, "SIGNAL ") ||
		!strings.Contains(after, "SIGNAL QUIT "+pgid) || !strings.Contains(after, "SIGNAL KILL "+pgid) {
		t.Fatalf("ps identity branch did not distinguish stale and original identities:\n%s", out)
	}
	if runtime.GOOS == "linux" && !strings.Contains(out, "PS_CALL ") {
		t.Fatalf("forced ps source never called fake ps:\n%s", out)
	}
}

// V9: /proc stat's comm can contain both spaces and right parentheses.
func TestHarnessProcIdentityParsesParenthesizedCommand(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc/PID/stat parser is Linux-only")
	}
	bin, err := os.ReadFile("/usr/bin/sleep")
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "a) (b c")
	if err := os.WriteFile(exe, bin, 0o755); err != nil {
		t.Fatal(err)
	}
	out := requireWatchdogScript(t, fmt.Sprintf(`
set -eu
set -m
%q 12 & test_pgid=$!
set +m
echo "TEST_GROUP $test_pgid"
line="$(cat "/proc/$test_pgid/stat")"
rest="${line##*) }"; set -- $rest
expected="$3 ${20}"
naive="$(awk '{print $5, $22}' "/proc/$test_pgid/stat")"
actual="$(gc_harness_proc_identity "$test_pgid")"
printf 'EXPECTED <%%s> ACTUAL <%%s>\n' "$expected" "$actual"
[[ "$naive" != "$expected" ]]
[[ "$actual" == "$expected" ]]
`, exe))
	if !strings.Contains(out, "EXPECTED <") || !strings.Contains(out, "ACTUAL <") {
		t.Fatalf("missing parser comparison:\n%s", out)
	}
}

// V10: an absent group simply returns without an escalation or refusal line.
func TestHarnessWatchdogSilentWhenGroupGone(t *testing.T) {
	out := requireWatchdogScript(t, `
set -eu
test_group 'sleep 1'
wait "$test_pgid"
gc_harness_signal_group() { printf 'SIGNAL %s %s\n' "$2" "$1"; }
gc_harness_watchdog "$test_pgid" 1 1 identity-test "$test_identity"
echo DONE
`)
	if !strings.Contains(out, "DONE") || strings.Contains(out, "harness watchdog:") || strings.Contains(out, "SIGNAL ") {
		t.Fatalf("absent group did not return silently:\n%s", out)
	}
}

// V11 is a real kernel PID-reuse reproduction, entirely inside a user and
// PID namespace. Every group signal below is confined to that namespace.
func TestHarnessWatchdogDoesNotSignalRecycledGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("user and PID namespaces are Linux-only")
	}
	root := repoRoot(t)
	work := t.TempDir()
	runner := filepath.Join(work, "runner.sh")
	strangerScript := filepath.Join(work, "stranger.sh")
	innerPath := filepath.Join(work, "inside.sh")
	for path, body := range map[string]string{
		runner: `#!/usr/bin/env bash
source "$1"
gc_harness_run_supervised identity-test 4 1 -- bash -c 'echo $$ > "$1/job.pid"; sleep 2' bash "$2"
`,
		strangerScript: `#!/usr/bin/env bash
trap 'echo QUIT >> "$1/stranger.log"; exit 73' QUIT
sleep 5
echo EXIT >> "$1/stranger.log"
`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	inner := fmt.Sprintf(`#!/usr/bin/env bash
set -uo pipefail
echo 400 > /proc/sys/kernel/pid_max || exit 81
ulimit -c 0
work=%q
set -m
bash %q %q "$work" > "$work/runner.log" 2>&1 & runner=$!
set +m
for i in $(seq 1 100); do [[ -s "$work/job.pid" ]] && break; sleep 0.02; done
[[ -s "$work/job.pid" ]] || exit 82
old="$(cat "$work/job.pid")"
kill -KILL "$runner"; wait "$runner" 2>/dev/null || true
for i in $(seq 1 150); do kill -0 -- -"$old" 2>/dev/null || break; sleep 0.02; done
if kill -0 -- -"$old" 2>/dev/null; then echo ORIGINAL_GROUP_REMAINS; exit 83; fi
echo $((old-1)) > /proc/sys/kernel/ns_last_pid || exit 84
set -m
bash %q "$work" & stranger=$!
set +m
echo "REUSED old=$old stranger=$stranger"
[[ "$stranger" == "$old" ]] || exit 85
status=0; wait "$stranger" || status=$?
echo "STRANGER_STATUS $status"
cat "$work/stranger.log" 2>/dev/null || true
cat "$work/runner.log"
[[ "$status" == 0 && "$(cat "$work/stranger.log")" == EXIT ]]
`, work, runner, filepath.Join(root, "scripts/lib/harness-reap.sh"), strangerScript)
	if err := os.WriteFile(innerPath, []byte(inner), 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`
if ! unshare --user --map-root-user --pid --fork --mount-proc bash -c 'true' >/dev/null 2>&1; then
  echo "UNSHARE_UNAVAILABLE: user and PID namespace creation denied"
  exit 77
fi
unshare --user --map-root-user --pid --fork --mount-proc bash %q
`, innerPath)
	out, err := runHarnessScript(t, script)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 77 {
		t.Skip(strings.TrimSpace(out))
	}
	if err != nil {
		t.Fatalf("contained reuse fixture failed: %v\n%s", err, out)
	}
	var recycledPGID string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "REUSED old=") {
			parts := strings.Fields(line)
			recycledPGID = strings.TrimPrefix(parts[1], "old=")
		}
	}
	if !strings.Contains(out, "REUSED old=") || !strings.Contains(out, "STRANGER_STATUS 0") || !strings.Contains(out, "\nEXIT\n") ||
		strings.Contains(out, "\nQUIT\n") ||
		!strings.Contains(out, "not signaling process group "+recycledPGID+": leader identity changed (pid reused)") ||
		strings.Contains(out, "exceeded its") {
		t.Fatalf("recycled stranger was signaled or refusal was missing:\n%s", out)
	}
	t.Logf("contained PID reuse ran: %s", strings.TrimSpace(out))
}

// V12: SIGKILL bypasses the runner's trap. The surviving watchdog still
// escalates against its original wedged run within the fixed 30s grace.
func TestGoTestShardWatchdogSurvivesRunnerSIGKILL(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "wedged.pid")
	fixture := newReapFixture(t, fmt.Sprintf(`
    trap '' TERM INT QUIT HUP
    echo $$ > %q
    end=$(( $(date +%%s) + %d ))
    while [ "$(date +%%s)" -lt "$end" ]; do sleep 1; done
`, pidFile, fixtureLifetimeSeconds))
	cmd := fixture.start(t, "GO_TEST_TIMEOUT=2s", "GO_TEST_WATCHDOG_GRACE=2s")
	wedged := waitForPIDFile(t, pidFile)
	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = waitWithin(t, cmd, 10*time.Second)
	if !processGone(wedged, 45*time.Second) {
		t.Fatalf("runner died but watchdog did not reap original wedged run %d; output:\n%s", wedged, fixture.output())
	}
	if !strings.Contains(fixture.output(), "exceeded its") {
		t.Fatalf("surviving watchdog omitted budget diagnostic:\n%s", fixture.output())
	}
}
