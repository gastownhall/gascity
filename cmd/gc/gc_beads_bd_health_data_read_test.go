package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runHealthGate runs one op_health gate function from gc-beads-bd.sh with die,
// run_bd_pinned and the loaded health variables stubbed, so the gate's decision
// is tested without a Dolt server.
func runHealthGate(t *testing.T, fn, prelude string) (stdout, stderr string, err error) {
	t.Helper()
	if _, lookErr := exec.LookPath("sh"); lookErr != nil {
		t.Skip("sh not available; skipping shell-function test")
	}
	scriptBytes, readErr := os.ReadFile(gcBinGuardScriptPath(t))
	if readErr != nil {
		t.Fatalf("read script: %v", readErr)
	}
	src := extractShellFunction(t, string(scriptBytes), fn)
	script := "die() { printf '%s\\n' \"$*\" >&2; exit 1; }\n" +
		"GC_CITY_PATH=\"$(mktemp -d)\"; mkdir -p \"$GC_CITY_PATH/.beads\"\n" +
		prelude + "\n" + src + "\n" + fn + "\necho gate-passed\n"
	return runGCBeadsBdCommand(t, os.Environ(), "sh", "-c", script)
}

func TestHealthDataReadGateDiesOnDataReadError(t *testing.T) {
	t.Parallel()
	stdout, stderr, err := runHealthGate(t, "health_data_read_gate",
		`GC_HEALTH_DATA_READ_ERROR='gascity: empty chunk returned from ChunkStore'; GC_HEALTH_DATA_READ_WARNING=''`)
	if err == nil || strings.Contains(stdout, "gate-passed") {
		t.Fatalf("gate accepted a data read error: stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "empty chunk returned from ChunkStore") {
		t.Fatalf("stderr = %q, want the underlying chunk error text, not a generic failure", stderr)
	}
}

func TestHealthDataReadGateWarnsButPassesOnInconclusiveRead(t *testing.T) {
	t.Parallel()
	stdout, stderr, err := runHealthGate(t, "health_data_read_gate",
		`GC_HEALTH_DATA_READ_ERROR=''; GC_HEALTH_DATA_READ_WARNING='gm: timed out after 5s'`)
	if err != nil || !strings.Contains(stdout, "gate-passed") {
		t.Fatalf("gate failed on a warning: err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
	if !strings.Contains(stderr, "timed out after 5s") {
		t.Fatalf("stderr = %q, want the warning surfaced", stderr)
	}
}

func TestHealthDataReadGatePassesSilentlyWhenClean(t *testing.T) {
	t.Parallel()
	stdout, stderr, err := runHealthGate(t, "health_data_read_gate",
		`GC_HEALTH_DATA_READ_ERROR=''; GC_HEALTH_DATA_READ_WARNING=''`)
	if err != nil || !strings.Contains(stdout, "gate-passed") || stderr != "" {
		t.Fatalf("clean gate: err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
}

// beads-health must exercise bd's own store-open path: a server that answers SQL
// can still hand bd a store it cannot open.
func TestHealthStoreOpenGateRunsBdPingAndFailsWithItsOutput(t *testing.T) {
	t.Parallel()
	stdout, stderr, err := runHealthGate(t, "health_store_open_gate",
		`run_bd_pinned() { [ "$2" = ping ] || exit 9; echo "Error: chunk deadbeef required but not found" >&2; return 1; }`)
	if err == nil || strings.Contains(stdout, "gate-passed") {
		t.Fatalf("gate accepted a failing bd ping: stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "required but not found") {
		t.Fatalf("stderr = %q, want bd's own error text carried through", stderr)
	}
}

func TestHealthStoreOpenGatePassesWhenBdPingSucceeds(t *testing.T) {
	t.Parallel()
	stdout, stderr, err := runHealthGate(t, "health_store_open_gate",
		`run_bd_pinned() { [ "$2" = ping ] || exit 9; return 0; }`)
	if err != nil || !strings.Contains(stdout, "gate-passed") {
		t.Fatalf("gate failed on a healthy ping: err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
}

func TestHealthStoreOpenGateSkipsCityWithoutBeadsDir(t *testing.T) {
	t.Parallel()
	stdout, stderr, err := runHealthGate(t, "health_store_open_gate",
		`rmdir "$GC_CITY_PATH/.beads"; run_bd_pinned() { echo "ping ran" >&2; return 1; }`)
	if err != nil || !strings.Contains(stdout, "gate-passed") || strings.Contains(stderr, "ping ran") {
		t.Fatalf("gate ran bd ping without a .beads dir: err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
}

func TestLoadHealthCheckFromGCParsesDataReadFields(t *testing.T) {
	t.Parallel()
	scriptBytes, err := os.ReadFile(gcBinGuardScriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	src := extractShellFunction(t, string(scriptBytes), "load_health_check_from_gc")
	script := `connect_host() { echo 127.0.0.1; }
resolve_gc_helper_bin() { echo "$FAKE_GC"; }
DOLT_PORT=1; DOLT_USER=root
` + src + `
load_health_check_from_gc || exit 3
printf 'err=[%s]\nwarn=[%s]\nready=[%s]\n' "$GC_HEALTH_DATA_READ_ERROR" "$GC_HEALTH_DATA_READ_WARNING" "$GC_HEALTH_QUERY_READY"
`
	fake := t.TempDir() + "/gc"
	body := "#!/bin/sh\nprintf 'query_ready\\ttrue\\nread_only\\tfalse\\nconnection_count\\t2\\ndata_read_error\\tgascity: empty chunk returned from ChunkStore\\ndata_read_warning\\tgm: timed out\\n'\n"
	if err := os.WriteFile(fake, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, runErr := runGCBeadsBdCommand(t, append(os.Environ(), "FAKE_GC="+fake), "sh", "-c", script)
	if runErr != nil {
		t.Fatalf("run: %v stderr=%q", runErr, stderr)
	}
	for _, want := range []string{
		"err=[gascity: empty chunk returned from ChunkStore]",
		"warn=[gm: timed out]",
		"ready=[true]",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, missing %q", stdout, want)
		}
	}
}
