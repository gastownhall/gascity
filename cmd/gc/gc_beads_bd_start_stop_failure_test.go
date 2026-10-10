package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Keep the real CLI dispatch, op_start and graceful_stop_owned_pid together.
// Only process inspection, signals and elapsed time are substituted: the fake
// server survives SIGTERM and retains its exclusive store lock. No PID is
// signaled and the thirty-second grace advances without wall-clock sleeps.
func gcBeadsBdLockedStartHarness(t *testing.T) string {
	t.Helper()
	prelude, main, ok := strings.Cut(readGCBeadsBdScript(t), "# --- Main ---")
	if !ok {
		t.Fatal("gc-beads-bd.sh is missing its main dispatch boundary")
	}
	return prelude + `
FAKE_PID=424242
FAKE_NOW_MS=0
trace() { printf '%s\n' "$*" >> "$GC_TEST_START_TRACE"; }
is_remote() { return 1; }
scope_is_proxied() { return 1; }
ensure_dolt_identity() { :; }
load_runtime_layout_from_gc() { return 1; }
allocate_port() { printf '17777\n'; }
flock() { return 0; }
dolt() { trace "start:dolt:$*"; return 97; }
resolve_gc_helper_bin() {
    case "$GC_TEST_STOP_CASE" in
        existing-managed|owned-probe) printf '/fake/gc\n' ;;
    esac
}
load_existing_managed_from_gc() {
    [ "$GC_TEST_STOP_CASE" = existing-managed ] || return 1
    GC_EXISTING_MANAGED_PID="$FAKE_PID"
    GC_EXISTING_MANAGED_OWNED=true
    GC_EXISTING_REUSABLE=false
    GC_EXISTING_STATE_PORT=""
}
load_managed_process_inspection_from_gc() {
    GC_MANAGED_PID=""
    GC_MANAGED_OWNED=false
    GC_MANAGED_DELETED=false
    GC_PORT_HOLDER_PID=""
    GC_PORT_HOLDER_OWNED=false
    GC_PORT_HOLDER_DELETED=false
    holder=""
    case "$GC_TEST_STOP_CASE" in
        unready-shell)
            GC_MANAGED_PID="$FAKE_PID"
            GC_MANAGED_OWNED=true
            ;;
        stale-shell)
            GC_PORT_HOLDER_PID="$FAKE_PID"
            GC_PORT_HOLDER_OWNED=true
            GC_PORT_HOLDER_DELETED=true
            ;;
    esac
    return 0
}
load_probe_managed_from_gc() {
    [ "$GC_TEST_STOP_CASE" = owned-probe ] || return 1
    GC_PROBE_PORT_HOLDER_PID="$FAKE_PID"
    GC_PROBE_PORT_HOLDER_OWNED=true
    GC_PROBE_RUNNING=false
}
find_port_holder() { printf '%s\n' "$FAKE_PID"; }
verify_our_server() { [ "$1" = "$FAKE_PID" ]; }
wait_deleted_data_inodes() { [ "$GC_TEST_STOP_CASE" = stale-shell ]; }
load_state_field() { return 0; }
save_state() { trace "save:$*"; }
load_start_managed_from_gc() { trace start:helper; exit 97; }
kill() {
    case "$1" in
        -0) [ "$2" = "$FAKE_PID" ] ;;
        -9|-KILL) trace "SIGKILL:$2" ;;
        *) trace "SIGTERM:$*" ;;
    esac
}
sleep() { FAKE_NOW_MS=$((FAKE_NOW_MS + 500)); }
sleep_ms() { FAKE_NOW_MS=$((FAKE_NOW_MS + $1)); }
current_time_ms() { printf '%s\n' "$FAKE_NOW_MS"; }
lock_release_timeout_ms() { printf '500\n'; }
dolt_data_lock_holder() { printf 'locked-by:%s\n' "$FAKE_PID"; }
` + "\n# --- Main ---\n" + main
}

// TestGcBeadsBdStartFailsClosedWhenOwnedServerRetainsStoreLock verifies that
// every owned-server restart path stops before a forced kill or replacement.
func TestGcBeadsBdStartFailsClosedWhenOwnedServerRetainsStoreLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{"existing-managed", "could not stop existing dolt server"},
		{"unready-shell", "could not stop unready dolt server"},
		{"owned-probe", "could not stop dolt server"},
		{"stale-shell", "could not stop stale dolt server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			tracePath := filepath.Join(city, "start.trace")
			script := filepath.Join(city, "gc-beads-bd.sh")
			writeExecutable(t, script, gcBeadsBdLockedStartHarness(t))
			stdout, stderr, err := runGCBeadsBdCommand(t, sanitizedBaseEnv(
				"GC_BIN=",
				"GC_CITY_PATH="+city,
				"GC_TEST_STOP_CASE="+tc.name,
				"GC_TEST_START_TRACE="+tracePath,
			), "sh", script, "start")
			if err == nil {
				t.Errorf("start succeeded while the owned server retained its store lock: %s%s", stdout, stderr)
			}
			for _, want := range []string{tc.want, "PID 424242", "without risking journal corruption", "refusing SIGKILL mid-journal-write"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("start stderr missing %q:\n%s", want, stderr)
				}
			}
			traceBytes, readErr := os.ReadFile(tracePath)
			if readErr != nil {
				t.Fatalf("read start trace: %v\nstdout: %s\nstderr: %s", readErr, stdout, stderr)
			}
			trace := string(traceBytes)
			if !strings.Contains(trace, "SIGTERM:424242\n") {
				t.Errorf("start did not attempt graceful shutdown: %s", trace)
			}
			for _, forbidden := range []string{"SIGKILL:", "start:", "save:"} {
				if strings.Contains(trace, forbidden) {
					t.Errorf("start performed %q after an unsafe shutdown: %s", forbidden, trace)
				}
			}
			if _, statErr := os.Stat(filepath.Join(city, ".gc", "runtime", "packs", "dolt", "dolt.pid")); !os.IsNotExist(statErr) {
				t.Errorf("failed start published a pid file: %v", statErr)
			}
		})
	}
}
