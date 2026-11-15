package main

import (
	"errors"
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

// A data-read failure must reach healthBeadsProvider's caller. Today health
// fails, recover runs and, when it succeeds, the original health error is
// dropped: the helper's recovery path reports success without re-running the
// data-read probe, so unreadable data looked healthy. A restart cannot restore a
// lost chunk, so a failure carrying the probe's marker is returned as it is and
// recover is not started (the breaker-open skip works the same way).
//
// The fixture cannot let recover finish: a successful publish needs a live
// managed dolt, so only the skip is observable here. A re-check after recover
// would still need its own test against a live server.
func TestHealthBeadsProviderReturnsDataReadFailureWithoutRecover(t *testing.T) {
	cityPath := t.TempDir()
	writeMinimalCityToml(t, cityPath)
	const text = "dolt data read probe failed: gascity: empty chunk returned from ChunkStore"
	opsFile := writeBreakerAwarePreflightFakes(t, cityPath, text)

	cityKey := normalizePathForCompare(cityPath)
	t.Cleanup(func() { lastBeadsProviderRecover.Delete(cityKey) })

	err := healthBeadsProvider(cityPath)
	if err == nil || !strings.Contains(err.Error(), "empty chunk returned from ChunkStore") {
		t.Fatalf("healthBeadsProvider() = %v, want the original chunk text", err)
	}
	ops, readErr := os.ReadFile(opsFile)
	if readErr != nil {
		t.Fatalf("read provider ops: %v", readErr)
	}
	if got := strings.Fields(string(ops)); len(got) != 1 || got[0] != "health" {
		t.Fatalf("provider ops = %v, want only health (no recover for a data-read failure)", got)
	}
	if _, loaded := lastBeadsProviderRecover.Load(cityKey); loaded {
		t.Fatalf("a data-read failure must not start the recover cooldown for %q", cityPath)
	}
}

// runHealthOp runs the script's own op_health, as wired. The gate tests above pin
// each gate on its own; nothing else runs op_health, so a gate dropped from it, or
// a gate that only one of its two branches reaches, would pass every one of them.
// The real health_data_read_gate and health_store_open_gate are extracted from
// the script next to it, and errexit is on as it is in gc-beads-bd.sh. Only the
// seams around op_health are stubbed: the TCP check, the gc helper's report, the
// legacy shell probes and bd itself.
//
// helper picks the branch: true makes the gc helper available, reporting
// dataReadError (and no warning); false makes it unavailable, so op_health
// takes the shell-native probes. A non-empty pingError makes bd ping fail with
// that text. The two strings are placed inside single quotes, so they carry
// none.
func runHealthOp(t *testing.T, helper bool, dataReadError, pingError string) (stdout, stderr string, err error) {
	t.Helper()
	scriptBytes, readErr := os.ReadFile(gcBinGuardScriptPath(t))
	if readErr != nil {
		t.Fatalf("read script: %v", readErr)
	}
	var prelude strings.Builder
	prelude.WriteString("set -e\nDOLT_PORT=1\n")
	prelude.WriteString("tcp_check() { return 0; }\nconnect_host() { echo 127.0.0.1; }\nis_remote() { return 1; }\n")
	if helper {
		prelude.WriteString("load_health_check_from_gc() { GC_HEALTH_QUERY_READY=true; GC_HEALTH_READ_ONLY=false; GC_HEALTH_CONNECTION_COUNT=2;" +
			" GC_HEALTH_DATA_READ_ERROR='" + dataReadError + "'; GC_HEALTH_DATA_READ_WARNING=''; return 0; }\n")
	} else {
		prelude.WriteString("load_health_check_from_gc() { return 1; }\ndo_query_probe() { return 0; }\n" +
			"check_read_only() { return 1; }\nget_connection_count() { echo 2; }\n")
	}
	if pingError == "" {
		prelude.WriteString("run_bd_pinned() { [ \"$2\" = ping ] || exit 9; return 0; }\n")
	} else {
		prelude.WriteString("run_bd_pinned() { [ \"$2\" = ping ] || exit 9; echo '" + pingError + "' >&2; return 1; }\n")
	}
	for _, fn := range []string{"health_data_read_gate", "health_store_open_gate"} {
		prelude.WriteString(extractShellFunction(t, string(scriptBytes), fn) + "\n")
	}
	return runHealthGate(t, "op_health", prelude.String())
}

// A data-read failure the gc helper reports must fail the health op, and the text
// it fails with must be the one the Go provider recognizes. The provider op runner
// returns "exec beads <op>: <stderr>", and a message the provider does not match
// sends it into recover, which cannot give back a lost chunk.
func TestHealthOpDiesOnDataReadErrorAndCarriesTheProviderMarker(t *testing.T) {
	t.Parallel()
	stdout, stderr, err := runHealthOp(t, true, "gascity: empty chunk returned from ChunkStore", "")
	if err == nil || strings.Contains(stdout, "gate-passed") {
		t.Fatalf("op_health accepted a data read error: stdout=%q stderr=%q", stdout, stderr)
	}
	const want = "dolt data read probe failed: gascity: empty chunk returned from ChunkStore"
	if !strings.Contains(stderr, want) {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
	if provider := errors.New("exec beads health: " + strings.TrimSpace(stderr)); !isDataReadFailureError(provider) {
		t.Fatalf("isDataReadFailureError(%q) = false: the shell message and the Go marker have drifted apart", provider)
	}
}

// beads-health must exercise bd's own store-open path on both branches of
// op_health, not only where the gc helper is available.
func TestHealthOpFailsWhenBdPingFailsOnTheHelperBranch(t *testing.T) {
	t.Parallel()
	stdout, stderr, err := runHealthOp(t, true, "", "Error: chunk deadbeef required but not found")
	if err == nil || strings.Contains(stdout, "gate-passed") {
		t.Fatalf("op_health accepted a failing bd ping: stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "bd store-open probe failed (bd ping): ") || !strings.Contains(stderr, "required but not found") {
		t.Fatalf("stderr = %q, want the store-open gate's message carrying bd's own error text", stderr)
	}
}

func TestHealthOpFailsWhenBdPingFailsOnTheShellFallbackBranch(t *testing.T) {
	t.Parallel()
	stdout, stderr, err := runHealthOp(t, false, "", "Error: chunk deadbeef required but not found")
	if err == nil || strings.Contains(stdout, "gate-passed") {
		t.Fatalf("op_health accepted a failing bd ping: stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "bd store-open probe failed (bd ping): ") || !strings.Contains(stderr, "required but not found") {
		t.Fatalf("stderr = %q, want the store-open gate's message carrying bd's own error text", stderr)
	}
}

// The controls: with nothing wrong, op_health passes on each branch without a
// word, so the failures above come from the gates and not from the stubs.
func TestHealthOpPassesSilentlyWhenTheHelperReportIsClean(t *testing.T) {
	t.Parallel()
	stdout, stderr, err := runHealthOp(t, true, "", "")
	if err != nil || !strings.Contains(stdout, "gate-passed") || stderr != "" {
		t.Fatalf("clean helper branch: err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
}

func TestHealthOpPassesSilentlyOnTheShellFallbackWhenNothingFails(t *testing.T) {
	t.Parallel()
	stdout, stderr, err := runHealthOp(t, false, "", "")
	if err != nil || !strings.Contains(stdout, "gate-passed") || stderr != "" {
		t.Fatalf("clean shell fallback branch: err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
}
