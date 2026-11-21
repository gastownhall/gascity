package rppcheck

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
)

// writeScript creates an executable shell script in dir and returns its path.
func writeScript(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "provider")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// conformantScript returns a script body implementing the full RPP v0
// surface against a state directory: file-per-session lifecycle, metadata
// files, and both optional capabilities declared and implemented.
func conformantScript(stateDir string) string {
	return fmt.Sprintf(`
state=%q
op="$1"
name="$2"

case "$op" in
  protocol)          printf '%%s' '{"version":0,"capabilities":["report-attachment","report-activity"]}' ;;
  start)             cat > /dev/null; touch "$state/$name.running" ;;
  stop)              rm -f "$state/$name.running" ;;
  is-running)        if [ -f "$state/$name.running" ]; then echo true; else echo false; fi ;;
  is-attached)       echo false ;;
  get-last-activity) echo 2026-06-12T00:00:00Z ;;
  nudge)             cat > /dev/null ;;
  process-alive)     cat > /dev/null; echo true ;;
  set-meta)          cat > "$state/$name.meta.$3" ;;
  get-meta)          cat "$state/$name.meta.$3" 2>/dev/null || true ;;
  remove-meta)       rm -f "$state/$name.meta.$3" ;;
  peek)              echo "" ;;
  interrupt)         ;;
  list-running)
    prefix="$2"
    for f in "$state"/*.running; do
      [ -f "$f" ] || continue
      b=$(basename "$f" .running)
      case "$b" in "$prefix"*) echo "$b" ;; esac
    done
    ;;
  *) exit 2 ;;
esac
`, stateDir)
}

// minimalScript returns a script body implementing only the required
// lifecycle ops; everything else (including protocol) exits 2.
func minimalScript(stateDir string) string {
	return fmt.Sprintf(`
state=%q
op="$1"
name="$2"

case "$op" in
  start)      cat > /dev/null; touch "$state/$name.running" ;;
  stop)       rm -f "$state/$name.running" ;;
  is-running) if [ -f "$state/$name.running" ]; then echo true; else echo false; fi ;;
  *) exit 2 ;;
esac
`, stateDir)
}

func runScript(t *testing.T, body string, opts Options) Result {
	t.Helper()
	dir := t.TempDir()
	script := writeScript(t, dir, body)
	res, err := Run(context.Background(), script, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func findCheck(t *testing.T, res Result, name string) Check {
	t.Helper()
	for _, c := range res.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not found in %+v", name, res.Checks)
	return Check{}
}

func hasCheck(res Result, name string) bool {
	for _, c := range res.Checks {
		if c.Name == name {
			return true
		}
	}
	return false
}

func TestRun_ConformantExecutablePasses(t *testing.T) {
	state := t.TempDir()
	res := runScript(t, conformantScript(state), Options{})

	if res.Failed() {
		t.Fatalf("Failed() = true for conformant script: %+v", res.Checks)
	}
	if !res.Protocol.Has("report-attachment") || !res.Protocol.Has("report-activity") {
		t.Fatalf("Protocol = %+v, want both capabilities declared", res.Protocol)
	}
	for _, name := range []string{
		"protocol handshake",
		"lifecycle: start",
		"lifecycle: is-running after start",
		"lifecycle: stop",
		"lifecycle: is-running after stop",
		"lifecycle: stop idempotent",
		"capability report-attachment: is-attached",
		"capability report-activity: get-last-activity",
		"optional: process-alive",
		"optional: nudge",
		"optional: metadata round-trip",
		"optional: peek",
		"optional: list-running",
		"optional: interrupt",
	} {
		if c := findCheck(t, res, name); c.Status != StatusPass {
			t.Errorf("check %q = %s (%s), want PASS", name, c.Status, c.Detail)
		}
	}
}

func TestRun_NoProtocolOpDefaultsToVersionZero(t *testing.T) {
	state := t.TempDir()
	res := runScript(t, minimalScript(state), Options{})

	if res.Failed() {
		t.Fatalf("Failed() = true for minimal script: %+v", res.Checks)
	}
	hs := findCheck(t, res, "protocol handshake")
	if hs.Status != StatusPass {
		t.Errorf("handshake = %s (%s), want PASS", hs.Status, hs.Detail)
	}
	if res.Protocol.Version != 0 || len(res.Protocol.Capabilities) != 0 {
		t.Errorf("Protocol = %+v, want version 0 with no capabilities", res.Protocol)
	}
	// Undeclared capabilities are never exercised as capability checks.
	if hasCheck(res, "capability report-attachment: is-attached") {
		t.Error("capability check ran without a declared capability")
	}
	// Optional ops absent (exit 2) are reported as SKIP, not failures.
	for _, name := range []string{
		"optional: process-alive",
		"optional: nudge",
		"optional: metadata round-trip",
		"optional: peek",
		"optional: list-running",
		"optional: interrupt",
		"optional: is-attached",
		"optional: get-last-activity",
	} {
		if c := findCheck(t, res, name); c.Status != StatusSkip {
			t.Errorf("check %q = %s (%s), want SKIP", name, c.Status, c.Detail)
		}
	}
}

func TestRun_MalformedHandshakeFailsButLifecycleStillRuns(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`'{"version":0,"capabilities":["report-attachment","report-activity"]}'`,
		`'{not json'`, 1)
	res := runScript(t, body, Options{})

	if !res.Failed() {
		t.Fatal("Failed() = false, want true for malformed handshake")
	}
	if c := findCheck(t, res, "protocol handshake"); c.Status != StatusFail {
		t.Errorf("handshake = %s, want FAIL", c.Status)
	}
	// Lifecycle is still validated for diagnostics.
	if c := findCheck(t, res, "lifecycle: start"); c.Status != StatusPass {
		t.Errorf("lifecycle: start = %s (%s), want PASS", c.Status, c.Detail)
	}
}

func TestRun_NegativeProtocolVersionFails(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`'{"version":0,"capabilities":["report-attachment","report-activity"]}'`,
		`'{"version":-1}'`, 1)
	res := runScript(t, body, Options{})

	if c := findCheck(t, res, "protocol handshake"); c.Status != StatusFail {
		t.Errorf("handshake = %s (%s), want FAIL for negative version", c.Status, c.Detail)
	}
	if !res.Failed() {
		t.Fatal("Failed() = false, want true")
	}
}

func TestRun_UnknownDeclaredCapabilityIgnored(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`["report-attachment","report-activity"]`,
		`["report-attachment","report-activity","future-cap"]`, 1)
	res := runScript(t, body, Options{})

	if res.Failed() {
		t.Fatalf("Failed() = true with unknown capability declared: %+v", res.Checks)
	}
	if hasCheck(res, "capability future-cap") {
		t.Error("unknown capability produced a capability check")
	}
}

// idle-boundary is a capability gc honors, so the handshake check must not
// call it unknown; it promises no op of its own, so it adds no capability
// check (gc runtime conformance checks it as RPP-CONN-002).
// Kills: a token gc acts on reported as ignored.
func TestRun_IdleBoundaryCapabilityIsKnown(t *testing.T) {
	state := t.TempDir()
	const from = `["report-attachment","report-activity"]`
	script := conformantScript(state)
	if !strings.Contains(script, from) {
		t.Fatalf("conformant script declares no %s to extend", from)
	}
	body := strings.Replace(script, from, `["report-attachment","report-activity","idle-boundary"]`, 1)
	res := runScript(t, body, Options{})

	if res.Failed() {
		t.Fatalf("Failed() = true with idle-boundary declared: %+v", res.Checks)
	}
	if c := findCheck(t, res, "protocol handshake"); strings.Contains(c.Detail, "unknown") {
		t.Errorf("handshake detail = %q, want idle-boundary not reported as unknown", c.Detail)
	}
	if hasCheck(res, "capability idle-boundary") {
		t.Error("idle-boundary produced a capability check")
	}
}

// A pack declaring proc.exec, report-activity and report-attachment without
// idle-boundary is in a recognizable migration state: it passes, but is told
// gc gives it no idle wait. Declaring idle-boundary, or not declaring all
// three, silences the warning.
// Kills: a pack that silently drops to timed_only sleep (gh#8903).
func TestRun_WarnsOnTheTripleWithoutIdleBoundary(t *testing.T) {
	const from = `["report-attachment","report-activity"]`
	for _, tc := range []struct {
		caps string
		warn bool
	}{
		{`["report-attachment","report-activity","proc.exec"]`, true},
		{`["report-attachment","report-activity","proc.exec","idle-boundary"]`, false},
		{`["report-attachment","report-activity"]`, false},
	} {
		script := conformantScript(t.TempDir())
		if !strings.Contains(script, from) {
			t.Fatalf("conformant script declares no %s to replace", from)
		}
		res := runScript(t, strings.Replace(script, from, tc.caps, 1), Options{})
		if res.Failed() {
			t.Fatalf("%s: Failed() = true: %+v", tc.caps, res.Checks)
		}
		detail := findCheck(t, res, "protocol handshake").Detail
		if got := strings.Contains(detail, idleBoundaryMigrationWarning); got != tc.warn {
			t.Errorf("%s: handshake detail %q, want migration warning %v", tc.caps, detail, tc.warn)
		}
		if strings.Contains(detail, "unknown") {
			t.Errorf("%s: handshake detail %q reports a capability gc honors as unknown", tc.caps, detail)
		}
	}
}

// fs.host narrows locality local: a handshake declaring it without
// "locality": "local" passes, but is told gc reads fs.host as undeclared and
// does no host work for it. With locality local, fs.host is a token gc
// honors, not an unknown one.
// Kills: a pack whose fs.host gc silently ignores.
func TestRun_WarnsOnFSHostWithoutLocalityLocal(t *testing.T) {
	const from = `{"version":0,"capabilities":["report-attachment","report-activity"]}`
	for _, tc := range []struct {
		handshake string
		warn      bool
	}{
		{`{"version":0,"capabilities":["report-attachment","report-activity","fs.host"]}`, true},
		{`{"version":0,"capabilities":["report-attachment","report-activity","fs.host"],"locality":"remote"}`, true},
		{`{"version":0,"capabilities":["report-attachment","report-activity","fs.host"],"locality":"local"}`, false},
		{`{"version":0,"capabilities":["report-attachment","report-activity"],"locality":"local"}`, false},
	} {
		script := conformantScript(t.TempDir())
		if !strings.Contains(script, from) {
			t.Fatalf("conformant script declares no %s to replace", from)
		}
		res := runScript(t, strings.Replace(script, from, tc.handshake, 1), Options{})
		if res.Failed() {
			t.Fatalf("%s: Failed() = true: %+v", tc.handshake, res.Checks)
		}
		detail := findCheck(t, res, "protocol handshake").Detail
		if got := strings.Contains(detail, fsHostWithoutLocalityWarning); got != tc.warn {
			t.Errorf("%s: handshake detail %q, want the fs.host locality warning %v", tc.handshake, detail, tc.warn)
		}
		if strings.Contains(detail, "unknown") {
			t.Errorf("%s: handshake detail %q reports a capability gc honors as unknown", tc.handshake, detail)
		}
	}
}

func TestRun_DeclaredCapabilityNotImplementedFails(t *testing.T) {
	state := t.TempDir()
	// Declares report-attachment but the is-attached op falls through to
	// exit 2: the declaration must not be trusted.
	body := strings.Replace(conformantScript(state),
		`  is-attached)       echo false ;;
`, "", 1)
	res := runScript(t, body, Options{})

	if !res.Failed() {
		t.Fatal("Failed() = false, want true for unimplemented declared capability")
	}
	c := findCheck(t, res, "capability report-attachment: is-attached")
	if c.Status != StatusFail {
		t.Errorf("capability check = %s (%s), want FAIL", c.Status, c.Detail)
	}
}

func TestRun_DeclaredCapabilityUnparseableOutputFails(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`is-attached)       echo false`,
		`is-attached)       echo attached-yes`, 1)
	res := runScript(t, body, Options{})

	c := findCheck(t, res, "capability report-attachment: is-attached")
	if c.Status != StatusFail {
		t.Errorf("capability check = %s (%s), want FAIL for unparseable output", c.Status, c.Detail)
	}
}

func TestRun_DeclaredActivityCapabilityMalformedTimestampFails(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`get-last-activity) echo 2026-06-12T00:00:00Z`,
		`get-last-activity) echo not-a-timestamp`, 1)
	res := runScript(t, body, Options{})

	c := findCheck(t, res, "capability report-activity: get-last-activity")
	if c.Status != StatusFail {
		t.Errorf("capability check = %s (%s), want FAIL for malformed timestamp", c.Status, c.Detail)
	}
}

func TestRun_StartFailureSkipsSessionChecks(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`start)             cat > /dev/null; touch "$state/$name.running"`,
		`start)             echo "boom" >&2; exit 1`, 1)
	res := runScript(t, body, Options{})

	if !res.Failed() {
		t.Fatal("Failed() = false, want true when start fails")
	}
	c := findCheck(t, res, "lifecycle: start")
	if c.Status != StatusFail {
		t.Errorf("lifecycle: start = %s, want FAIL", c.Status)
	}
	if !strings.Contains(c.Detail, "boom") {
		t.Errorf("start failure detail %q should include script stderr", c.Detail)
	}
	// Every remaining check — including the required stop steps — must
	// still appear in the report, as SKIP rather than vanishing.
	for _, name := range []string{
		"lifecycle: is-running after start",
		"capability report-attachment: is-attached",
		"optional: nudge",
		"lifecycle: stop",
		"lifecycle: is-running after stop",
		"lifecycle: stop idempotent",
		"optional: interrupt",
	} {
		if c := findCheck(t, res, name); c.Status != StatusSkip {
			t.Errorf("check %q = %s, want SKIP after start failure", name, c.Status)
		}
	}
}

func TestRun_StartUnknownOpFails(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`start)             cat > /dev/null; touch "$state/$name.running"`,
		`start)             exit 2`, 1)
	res := runScript(t, body, Options{})

	c := findCheck(t, res, "lifecycle: start")
	if c.Status != StatusFail {
		t.Errorf("lifecycle: start = %s (%s), want FAIL for exit 2 on a required op", c.Status, c.Detail)
	}
}

func TestRun_IsRunningStaysTrueAfterStopFails(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`stop)              rm -f "$state/$name.running"`,
		`stop)              :`, 1)
	res := runScript(t, body, Options{})

	c := findCheck(t, res, "lifecycle: is-running after stop")
	if c.Status != StatusFail {
		t.Errorf("is-running after stop = %s (%s), want FAIL", c.Status, c.Detail)
	}
	if !res.Failed() {
		t.Fatal("Failed() = false, want true")
	}
}

func TestRun_StopNotIdempotentFails(t *testing.T) {
	state := t.TempDir()
	// First stop removes the marker; a second stop on the now-missing
	// session errors instead of succeeding.
	body := strings.Replace(conformantScript(state),
		`stop)              rm -f "$state/$name.running"`,
		`stop)              if [ -f "$state/$name.running" ]; then rm -f "$state/$name.running"; else echo "no such session" >&2; exit 1; fi`, 1)
	res := runScript(t, body, Options{})

	c := findCheck(t, res, "lifecycle: stop idempotent")
	if c.Status != StatusFail {
		t.Errorf("stop idempotent = %s (%s), want FAIL", c.Status, c.Detail)
	}
}

func TestRun_BrokenMetadataRoundTripFails(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`get-meta)          cat "$state/$name.meta.$3" 2>/dev/null || true`,
		`get-meta)          echo wrong-value`, 1)
	res := runScript(t, body, Options{})

	c := findCheck(t, res, "optional: metadata round-trip")
	if c.Status != StatusFail {
		t.Errorf("metadata round-trip = %s (%s), want FAIL", c.Status, c.Detail)
	}
}

func TestRun_ListRunningMissingSessionFails(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`  list-running)
    prefix="$2"
    for f in "$state"/*.running; do
      [ -f "$f" ] || continue
      b=$(basename "$f" .running)
      case "$b" in "$prefix"*) echo "$b" ;; esac
    done
    ;;`,
		`  list-running) ;;`, 1)
	res := runScript(t, body, Options{})

	c := findCheck(t, res, "optional: list-running")
	if c.Status != StatusFail {
		t.Errorf("list-running = %s (%s), want FAIL when the running session is missing", c.Status, c.Detail)
	}
}

func TestRun_UndeclaredImplementedOpsReportedAsOptional(t *testing.T) {
	state := t.TempDir()
	// Implements is-attached and get-last-activity but declares nothing.
	body := strings.Replace(conformantScript(state),
		`'{"version":0,"capabilities":["report-attachment","report-activity"]}'`,
		`'{"version":0}'`, 1)
	res := runScript(t, body, Options{})

	if res.Failed() {
		t.Fatalf("Failed() = true: %+v", res.Checks)
	}
	for _, name := range []string{"optional: is-attached", "optional: get-last-activity"} {
		c := findCheck(t, res, name)
		if c.Status != StatusPass {
			t.Errorf("check %q = %s (%s), want PASS", name, c.Status, c.Detail)
		}
		if !strings.Contains(c.Detail, "not declared") {
			t.Errorf("check %q detail %q should note the capability is not declared", name, c.Detail)
		}
	}
}

func TestRun_OpTimeoutFails(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`is-running)        if [ -f "$state/$name.running" ]; then echo true; else echo false; fi`,
		`is-running)        exec sleep 5`, 1)
	res := runScript(t, body, Options{OpTimeout: 100 * time.Millisecond})

	c := findCheck(t, res, "lifecycle: is-running after start")
	if c.Status != StatusFail {
		t.Errorf("is-running after start = %s (%s), want FAIL on timeout", c.Status, c.Detail)
	}
}

func TestRun_SessionNameOptionUsed(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`start)             cat > /dev/null; touch "$state/$name.running"`,
		`start)             cat > /dev/null; touch "$state/$name.running"; echo "$name" >> "$state/names.log"`, 1)
	res := runScript(t, body, Options{SessionName: "custom-check-session"})

	if res.Failed() {
		t.Fatalf("Failed() = true: %+v", res.Checks)
	}
	names, err := os.ReadFile(filepath.Join(state, "names.log"))
	if err != nil {
		t.Fatalf("reading names log: %v", err)
	}
	if got := strings.TrimSpace(string(names)); got != "custom-check-session" {
		t.Errorf("start received session name %q, want %q", got, "custom-check-session")
	}
	if _, err := os.Stat(filepath.Join(state, "custom-check-session.running")); !os.IsNotExist(err) {
		t.Errorf("session marker should be cleaned up after the round-trip: %v", err)
	}
}

func TestRun_IsRunningUnknownOpFails(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`  is-running)        if [ -f "$state/$name.running" ]; then echo true; else echo false; fi ;;
`, "", 1)
	res := runScript(t, body, Options{})

	for _, name := range []string{"lifecycle: is-running after start", "lifecycle: is-running after stop"} {
		c := findCheck(t, res, name)
		if c.Status != StatusFail || !strings.Contains(c.Detail, "required op not implemented (exit 2)") {
			t.Errorf("check %q = %s (%s), want FAIL for exit 2 on a required op", name, c.Status, c.Detail)
		}
	}
	if !res.Failed() {
		t.Fatal("Failed() = false, want true")
	}
}

func TestRun_StopUnknownOpFails(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`  stop)              rm -f "$state/$name.running" ;;
`, "", 1)
	res := runScript(t, body, Options{})

	for _, name := range []string{"lifecycle: stop", "lifecycle: stop idempotent"} {
		c := findCheck(t, res, name)
		if c.Status != StatusFail || !strings.Contains(c.Detail, "required op not implemented (exit 2)") {
			t.Errorf("check %q = %s (%s), want FAIL for exit 2 on a required op", name, c.Status, c.Detail)
		}
	}
	if !res.Failed() {
		t.Fatal("Failed() = false, want true")
	}
}

func TestRun_SessionFatalInterruptCannotMaskBrokenStop(t *testing.T) {
	state := t.TempDir()
	// A backend where interrupt kills the session (SIGINT forwarded to
	// the session command) and stop is a silent no-op: the round-trip
	// must still catch the broken stop, not credit interrupt's side
	// effect.
	body := strings.Replace(conformantScript(state),
		`stop)              rm -f "$state/$name.running"`,
		`stop)              :`, 1)
	body = strings.Replace(body,
		`interrupt)         ;;`,
		`interrupt)         rm -f "$state/$name.running" ;;`, 1)
	res := runScript(t, body, Options{})

	c := findCheck(t, res, "lifecycle: is-running after stop")
	if c.Status != StatusFail {
		t.Errorf("is-running after stop = %s (%s), want FAIL despite session-fatal interrupt", c.Status, c.Detail)
	}
}

func TestRun_ProcessAliveUnparseableOutputFails(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`process-alive)     cat > /dev/null; echo true`,
		`process-alive)     cat > /dev/null; echo maybe`, 1)
	res := runScript(t, body, Options{})

	c := findCheck(t, res, "optional: process-alive")
	if c.Status != StatusFail {
		t.Errorf("process-alive = %s (%s), want FAIL for unparseable output", c.Status, c.Detail)
	}
}

func TestRun_GetMetaUnknownOpAfterRemoveFails(t *testing.T) {
	state := t.TempDir()
	// get-meta degrades to exit 2 once the key is gone: the unset-key
	// contract is empty output, so the round-trip must fail.
	body := strings.Replace(conformantScript(state),
		`get-meta)          cat "$state/$name.meta.$3" 2>/dev/null || true`,
		`get-meta)          if [ -f "$state/$name.meta.$3" ]; then cat "$state/$name.meta.$3"; else exit 2; fi`, 1)
	res := runScript(t, body, Options{})

	c := findCheck(t, res, "optional: metadata round-trip")
	if c.Status != StatusFail || !strings.Contains(c.Detail, "exit 2") {
		t.Errorf("metadata round-trip = %s (%s), want FAIL for exit 2 on unset get-meta", c.Status, c.Detail)
	}
}

func TestRun_StartConfigCarriesCommandAndWorkDir(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`start)             cat > /dev/null; touch "$state/$name.running"`,
		`start)             cat > "$state/start-config.json"; touch "$state/$name.running"`, 1)
	workDir := t.TempDir()
	res := runScript(t, body, Options{Command: "probe-cmd-12345", WorkDir: workDir})

	if res.Failed() {
		t.Fatalf("Failed() = true: %+v", res.Checks)
	}
	cfg, err := os.ReadFile(filepath.Join(state, "start-config.json"))
	if err != nil {
		t.Fatalf("reading captured start config: %v", err)
	}
	if !strings.Contains(string(cfg), `"command":"probe-cmd-12345"`) {
		t.Errorf("start config %q missing command field", cfg)
	}
	if !strings.Contains(string(cfg), fmt.Sprintf(`"work_dir":%q`, workDir)) {
		t.Errorf("start config %q missing work_dir field", cfg)
	}
}

// Kills a canceled run whose cleanup stop does not run, or runs outside the
// private city the session started in, or leaves that city behind.
func TestRun_CanceledRunStopsSessionViaDetachedContext(t *testing.T) {
	t.Setenv("GC_EXEC_CITY_PATH", "")
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`is-running)        if [ -f "$state/$name.running" ]; then echo true; else echo false; fi`,
		`is-running)        touch "$state/is-running-called"; exec sleep 30`, 1)
	body = strings.Replace(body,
		`stop)              rm -f "$state/$name.running"`,
		`stop)              echo "${GC_EXEC_CITY_PATH:-}|$name" >> "$state/stop.log"; rm -f "$state/$name.running"`, 1)
	script := writeScript(t, t.TempDir(), body)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Cancel as soon as the post-start is-running probe blocks,
		// simulating Ctrl-C mid-run.
		for range 500 {
			if _, err := os.Stat(filepath.Join(state, "is-running-called")); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()

	res, err := Run(ctx, script, Options{SessionName: "cancel-session"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Failed() {
		t.Fatal("Failed() = false, want true for a canceled run")
	}
	c := findCheck(t, res, "lifecycle: is-running after start")
	if c.Status != StatusFail || !strings.Contains(c.Detail, "canceled") {
		t.Errorf("is-running after start = %s (%s), want FAIL attributed to cancellation, not a timeout", c.Status, c.Detail)
	}
	stops, err := os.ReadFile(filepath.Join(state, "stop.log"))
	if err != nil {
		t.Fatalf("cleanup stop never ran after cancellation: %v", err)
	}
	first, _, _ := strings.Cut(string(stops), "\n")
	city, name, _ := strings.Cut(first, "|")
	if name != "cancel-session" {
		t.Errorf("cleanup stop log %q missing the session name", stops)
	}
	if city == "" {
		t.Errorf("cleanup stop log %q: the stop named no city, not the private city the session started in", stops)
	} else if _, err := os.Stat(city); !os.IsNotExist(err) {
		t.Errorf("private city %s after the canceled run: stat err = %v, want it removed", city, err)
	}
}

func TestRun_ExecutableNotFound(t *testing.T) {
	_, err := Run(context.Background(), filepath.Join(t.TempDir(), "missing"), Options{})
	if err == nil {
		t.Fatal("Run with missing executable should error")
	}
}

// The following RequireNudge tests express FR5 (ga-s5y62b.2): a pack can
// declare prompt_delivery = "nudge-fallback" on a runtime, but nothing
// verifies the claim is true. RequireNudge lets a caller that has
// independently determined nudge support is required — e.g. cmd/gc, for a
// runtime declaring that strategy — escalate the nudge probe from an
// optional SKIP-on-absence check to a FAIL-on-absence one.

func TestRun_RequireNudgeFailsWhenNudgeUnimplemented(t *testing.T) {
	state := t.TempDir()
	res := runScript(t, minimalScript(state), Options{RequireNudge: true})

	if !res.Failed() {
		t.Fatal("Failed() = false, want true when RequireNudge is set and nudge is unimplemented")
	}
	c := findCheck(t, res, "required: nudge")
	if c.Status != StatusFail {
		t.Errorf("required: nudge = %s (%s), want FAIL", c.Status, c.Detail)
	}
	if hasCheck(res, "optional: nudge") {
		t.Error("optional: nudge should not appear when RequireNudge is set")
	}
}

func TestRun_RequireNudgeFailsWhenNudgeErrors(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		"nudge)             cat > /dev/null ;;",
		`nudge)             echo "boom" >&2; exit 1 ;;`, 1)
	res := runScript(t, body, Options{RequireNudge: true})

	if !res.Failed() {
		t.Fatal("Failed() = false, want true when RequireNudge is set and nudge errors")
	}
	c := findCheck(t, res, "required: nudge")
	if c.Status != StatusFail {
		t.Errorf("required: nudge = %s (%s), want FAIL", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "boom") {
		t.Errorf("nudge failure detail %q should include script stderr", c.Detail)
	}
}

func TestRun_RequireNudgePassesWhenNudgeWorks(t *testing.T) {
	state := t.TempDir()
	res := runScript(t, conformantScript(state), Options{RequireNudge: true})

	if res.Failed() {
		t.Fatalf("Failed() = true for conformant script with RequireNudge: %+v", res.Checks)
	}
	c := findCheck(t, res, "required: nudge")
	if c.Status != StatusPass {
		t.Errorf("required: nudge = %s (%s), want PASS", c.Status, c.Detail)
	}
	if hasCheck(res, "optional: nudge") {
		t.Error("optional: nudge should not appear when RequireNudge is set")
	}
}

func TestRun_RequireNudgeStartFailureSkipsWithRequiredName(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`start)             cat > /dev/null; touch "$state/$name.running"`,
		`start)             echo "boom" >&2; exit 1`, 1)
	res := runScript(t, body, Options{RequireNudge: true})

	c := findCheck(t, res, "required: nudge")
	if c.Status != StatusSkip {
		t.Errorf("required: nudge = %s (%s), want SKIP when start fails", c.Status, c.Detail)
	}
	if hasCheck(res, "optional: nudge") {
		t.Error("optional: nudge should not appear when RequireNudge is set, even after a start failure")
	}
}

// observeReference writes the runtimecontract golden reference, which
// declares observe.v1, with bodies overriding its ops.
func observeReference(t *testing.T, bodies map[string]string) string {
	t.Helper()
	all := reference.Bodies()
	for op, body := range bodies {
		all[op] = body
	}
	path := filepath.Join(t.TempDir(), "golden")
	if err := os.WriteFile(path, []byte(reference.Script(t.TempDir(), all)), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Kills a smoke check that does not run, or does not gate, the observe.v1
// ops a script declares: the golden passes all four; a script whose
// observe, inventory, kill-object or post-stop observe misbehaves fails
// exactly that check.
func TestRun_ObserveV1Smoke(t *testing.T) {
	checks := []string{observeCheck, inventoryCheck, killObjectCheck, observeAbsentCheck}
	res, err := Run(context.Background(), observeReference(t, nil), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range checks {
		if c := findCheck(t, res, name); c.Status != StatusPass {
			t.Errorf("golden: %s = %s (%s), want PASS", name, c.Status, c.Detail)
		}
	}
	for check, bodies := range map[string]map[string]string{
		observeCheck:                       {"observe": `exit 2`},
		observeCheck + " (no object)":      {"observe": `cat >/dev/null; if [ -d "$box" ]; then echo '{"state":"running","identity":null}'; else echo '{"state":"absent"}'; fi`},
		inventoryCheck + " (not complete)": {"inventory": `printf '{"complete":false,"sessions":[{"name":"%s","state":"running","object":{"id":"a","created":"b"}}]}' "$(basename "$B"/*)"`},
		inventoryCheck:                     {"inventory": `echo '{"complete":true,"sessions":[]}'`},
		killObjectCheck:                    {"kill-object": `cat >/dev/null; echo killed`},
		observeAbsentCheck:                 {"stop": `if [ -d "$box" ]; then : > "$box/stopped"; fi`, "is-running": `if [ -d "$box" ] && [ ! -f "$box/stopped" ]; then echo true; else echo false; fi`},
	} {
		res, err := Run(context.Background(), observeReference(t, bodies), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if c := findCheck(t, res, strings.SplitN(check, " (", 2)[0]); c.Status != StatusFail {
			t.Errorf("mutant for %s: %s (%s), want FAIL", check, c.Status, c.Detail)
		}
	}
}

// A script declaring observe.v1 whose start fails reports the observe.v1
// checks as skipped, so the report carries the full check set.
func TestRun_ObserveV1ChecksSkippedWhenStartFails(t *testing.T) {
	res, err := Run(context.Background(), observeReference(t, map[string]string{"start": `cat >/dev/null; exit 1`}), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{observeCheck, inventoryCheck, killObjectCheck, observeAbsentCheck} {
		if c := findCheck(t, res, name); c.Status != StatusSkip {
			t.Errorf("%s = %s, want SKIP after a failed start", name, c.Status)
		}
	}
	if hasCheck(minimalResult(t), observeCheck) {
		t.Error("a script without observe.v1 reports observe.v1 checks")
	}
}

func minimalResult(t *testing.T) Result {
	t.Helper()
	return runScript(t, minimalScript(t.TempDir()), Options{})
}

// Kills a smoke check that does not run, or does not gate, the server-stop a
// script declaring server-lifecycle owes: the golden passes both checks; a
// server-stop that is not implemented, leaves the session running, or fails
// the second time fails its check; and an undeclared script is not sent it.
func TestRun_ServerLifecycleSmoke(t *testing.T) {
	res, err := Run(context.Background(), observeReference(t, nil), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{serverStopCheck, serverStopIdempotentCheck} {
		if c := findCheck(t, res, name); c.Status != StatusPass {
			t.Errorf("golden: %s = %s (%s), want PASS", name, c.Status, c.Detail)
		}
	}
	for check, body := range map[string]string{
		serverStopCheck + " (exit 2)":           `exit 2`,
		serverStopCheck + " (leaves a session)": `:`,
		serverStopIdempotentCheck:               `if [ -f "$S/server-stopped" ]; then exit 1; fi; : > "$S/server-stopped"; rm -rf "$B"`,
	} {
		res, err := Run(context.Background(), observeReference(t, map[string]string{"server-stop": body}), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if c := findCheck(t, res, strings.SplitN(check, " (", 2)[0]); c.Status != StatusFail {
			t.Errorf("mutant for %s: %s (%s), want FAIL", check, c.Status, c.Detail)
		}
	}
	if hasCheck(minimalResult(t), serverStopCheck) {
		t.Error("a script without server-lifecycle reports a server-stop check")
	}
}

// Kills a server-stop smoke check run in the city the check inherited: gc
// runtime check run from a city must leave that city's sessions running,
// though the golden's server-stop, like a city-scoped pack's, ends every
// session of the city it runs in.
func TestRun_ServerStopSparesTheCallersCity(t *testing.T) {
	script := observeReference(t, nil)
	t.Setenv("GC_EXEC_CITY_NAME", "live")
	t.Setenv("GC_EXEC_CITY_PATH", "/live/city")
	const sentinel = "live-city-sentinel"
	if out, err := scriptOp(script, `{"command":"sleep 300"}`, "start", sentinel); err != nil {
		t.Fatalf("starting %s in the caller's city: %v (%s)", sentinel, err, out)
	}
	res, err := Run(context.Background(), script, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, res, serverStopCheck); c.Status != StatusPass {
		t.Fatalf("%s = %s (%s), want PASS: the check must run server-stop", serverStopCheck, c.Status, c.Detail)
	}
	if out, err := scriptOp(script, "", "is-running", sentinel); err != nil || out != "true" {
		t.Fatalf("%s in the caller's city: is-running = %q (%v) after the check, want true: the check's server-stop ended a session of the city it was run from", sentinel, out, err)
	}
}

// scriptOp runs one op of script in the test's environment, the city a check
// started from it inherits.
func scriptOp(script, stdin string, args ...string) (string, error) {
	cmd := exec.Command(script, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// outcomeReference writes the input-outcome golden with bodies overridden.
func outcomeReference(t *testing.T, bodies map[string]string) string {
	t.Helper()
	all := reference.InputOutcomeBodies(reference.Bodies())
	for op, body := range bodies {
		all[op] = body
	}
	path := filepath.Join(t.TempDir(), "golden")
	if err := os.WriteFile(path, []byte(reference.Script(t.TempDir(), all)), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Kills an input-outcome smoke check that does not run, or does not gate,
// the nudge to an absent name a script declaring the token owes: the golden
// passes it, and every other check too (its post-stop interrupt answers a
// typed gone, not the undeclared exit 0); a nudge answering an absent name
// with exit 0, an empty exit 80, another word or exit 1 fails it; and an
// undeclared script is not checked.
func TestRun_InputOutcomeSmoke(t *testing.T) {
	res, err := Run(context.Background(), outcomeReference(t, nil), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed() {
		for _, c := range res.Checks {
			if c.Status == StatusFail {
				t.Errorf("golden: %s failed: %s", c.Name, c.Detail)
			}
		}
	}
	if c := findCheck(t, res, inputOutcomeCheck); c.Status != StatusPass {
		t.Errorf("golden: %s = %s (%s), want PASS", inputOutcomeCheck, c.Status, c.Detail)
	}
	if c := findCheck(t, res, "optional: interrupt"); c.Status != StatusPass || !strings.Contains(c.Detail, "gone") {
		t.Errorf("golden: post-stop interrupt = %s (%s), want PASS on a typed gone", c.Status, c.Detail)
	}
	golden := reference.InputOutcomeBodies(reference.Bodies())["nudge"]
	for name, body := range map[string]string{
		"exit 0":       strings.Replace(golden, `[ -d "$box" ] || out gone;`, `[ -d "$box" ] || exit 0;`, 1),
		"empty 80":     strings.Replace(golden, `[ -d "$box" ] || out gone;`, `[ -d "$box" ] || exit 80;`, 1),
		"another word": strings.Replace(golden, `[ -d "$box" ] || out gone;`, `[ -d "$box" ] || out not_sent;`, 1),
		"exit 1":       strings.Replace(golden, `[ -d "$box" ] || out gone;`, `[ -d "$box" ] || exit 1;`, 1),
		"past the cap": strings.Replace(golden, `[ -d "$box" ] || out gone;`, `[ -d "$box" ] || `+goneBeyondCap+`;`, 1),
	} {
		if body == golden {
			t.Fatalf("mutant %s does not apply", name)
		}
		res, err := Run(context.Background(), outcomeReference(t, map[string]string{"nudge": body}), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if c := findCheck(t, res, inputOutcomeCheck); c.Status != StatusFail {
			t.Errorf("mutant %s: %s = %s (%s), want FAIL", name, inputOutcomeCheck, c.Status, c.Detail)
		}
	}
	res, err = Run(context.Background(), outcomeReference(t, map[string]string{"interrupt": `[ -d "$box" ] || ` + goneBeyondCap + `; out not_sent`}), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, res, "optional: interrupt"); c.Status == StatusPass && strings.Contains(c.Detail, "gone") {
		t.Errorf("post-stop interrupt answering gone past gc's stdout cap = %s (%s), want it not read as a typed gone", c.Status, c.Detail)
	}
	if hasCheck(observeResult(t), inputOutcomeCheck) {
		t.Error("a script without input-outcome reports an input-outcome check")
	}
}

// goneBeyondCap is a shell exit-80 answer of one gone line padded with
// trailing newlines past gc's stdout cap (sessionexec.InputOutcomeStdoutCap),
// which gc reads as malformed.
const goneBeyondCap = `{ echo '{"outcome":"gone"}'; head -c 70000 /dev/zero | tr '\0' '\n'; exit 80; }`

// observeResult runs the check over the base golden, which does not declare
// input-outcome.
func observeResult(t *testing.T) Result {
	t.Helper()
	res, err := Run(context.Background(), observeReference(t, nil), Options{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Kills a check that reads a typed refusal from a driving op to the session
// rppcheck just started and verified running as a pass: a nudge that never
// delivers fails "required: nudge" whatever word it answers, a carrier.ops
// key op that always answers rejected fails its carrier.ops check, and the
// post-stop interrupt accepts only gone.
func TestRun_InputOutcomeRefusalOnARunningSessionFails(t *testing.T) {
	for _, word := range []string{"not_sent", "rejected", "gone", "unavailable", "busy", "pending_interaction"} {
		path := outcomeReference(t, map[string]string{"nudge": `cat >/dev/null; out ` + word})
		res, err := Run(context.Background(), path, Options{RequireNudge: true})
		if err != nil {
			t.Fatal(err)
		}
		if c := findCheck(t, res, "required: nudge"); c.Status != StatusFail {
			t.Errorf("nudge always %s: required: nudge = %s (%s), want FAIL", word, c.Status, c.Detail)
		}
		if !res.Failed() {
			t.Errorf("nudge always %s: the run did not fail", word)
		}
	}

	ops := reference.InputOutcomeBodies(reference.NonTmuxBodies())
	ops["send-keys"] = `out rejected`
	path := filepath.Join(t.TempDir(), "golden")
	if err := os.WriteFile(path, []byte(reference.Script(t.TempDir(), ops)), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, res, "capability carrier.ops: send-keys"); c.Status != StatusFail {
		t.Errorf("send-keys always rejected: %s (%s), want FAIL", c.Status, c.Detail)
	}

	res, err = Run(context.Background(), outcomeReference(t, map[string]string{"interrupt": `out not_sent`}), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, res, "optional: interrupt"); c.Status != StatusFail {
		t.Errorf("post-stop interrupt answering not_sent = %s (%s), want FAIL: only gone answers a stopped session", c.Status, c.Detail)
	}
}

// cityOnly wraps the executable at runtime in reference.CityOnlyScript and
// returns the wrapper and its ops log.
func cityOnly(t *testing.T, runtime string) (script, opsLog string) {
	t.Helper()
	dir := t.TempDir()
	opsLog = filepath.Join(dir, "ops.log")
	script = filepath.Join(dir, "city-only")
	if err := os.WriteFile(script, []byte(reference.CityOnlyScript(runtime, opsLog)), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, opsLog
}

// cityOps reads the ops a cityOnly wrapper logged.
func cityOps(t *testing.T, opsLog string) []reference.CityOp {
	t.Helper()
	ops, err := reference.ReadCityOps(opsLog)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

// reportFailures logs every failed check of res as a test error.
func reportFailures(t *testing.T, res Result) {
	t.Helper()
	for _, c := range res.Checks {
		if c.Status == StatusFail {
			t.Errorf("%s failed: %s", c.Name, c.Detail)
		}
	}
}

// Kills a check run outside a city that runs its ops in no city: a runtime
// that serves only a city, as gc's own ops always name one, refuses them and
// fails the check. Run outside a city, every op names a private city, which
// is gone after the run, and whose server is ended by a final server-stop.
func TestRun_OutsideACityRunsInAPrivateCity(t *testing.T) {
	t.Setenv("GC_EXEC_CITY_NAME", "")
	t.Setenv("GC_EXEC_CITY_PATH", "")
	script, opsLog := cityOnly(t, outcomeReference(t, nil))

	res, err := Run(context.Background(), script, Options{})
	if err != nil {
		t.Fatal(err)
	}
	reportFailures(t, res)
	for _, name := range []string{serverStopCheck, serverStopIdempotentCheck, inputOutcomeCheck} {
		if c := findCheck(t, res, name); c.Status != StatusPass {
			t.Errorf("%s = %s (%s), want PASS", name, c.Status, c.Detail)
		}
	}
	ops := cityOps(t, opsLog)
	if len(ops) == 0 || ops[0].Args != "protocol" {
		t.Fatalf("ops = %v, want protocol first", ops)
	}
	private := ops[0].City
	var last string
	for _, op := range ops {
		if op.City == "" {
			t.Errorf("op %q named no city", op.Args)
		}
		if op.City == private {
			last = op.Args
		}
	}
	if private == "" {
		t.Fatal("protocol named no city")
	}
	if _, err := os.Stat(private); !os.IsNotExist(err) {
		t.Errorf("private city %s after the run: stat err = %v, want it removed", private, err)
	}
	if last != "server-stop" {
		t.Errorf("last op in the private city = %q, want server-stop: the check left its server running", last)
	}
}

// Kills a check run outside a city that sends server-stop to a runtime that
// does not declare server-lifecycle.
func TestRun_OutsideACitySendsNoServerStopToAnUndeclaredRuntime(t *testing.T) {
	t.Setenv("GC_EXEC_CITY_NAME", "")
	t.Setenv("GC_EXEC_CITY_PATH", "")
	script, opsLog := cityOnly(t, observeReference(t, map[string]string{
		"protocol": `echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}'`,
	}))

	res, err := Run(context.Background(), script, Options{})
	if err != nil {
		t.Fatal(err)
	}
	reportFailures(t, res)
	for _, op := range cityOps(t, opsLog) {
		if op.Args == "server-stop" {
			t.Errorf("server-stop sent in %q to a runtime not declaring server-lifecycle", op.City)
		}
	}
}

// Kills a check run from a city that leaves that city: with
// GC_EXEC_CITY_PATH set, the check's ops run there, its server checks run in
// a city of their own, and it sends the caller's city no server-stop.
func TestRun_FromACityRunsThere(t *testing.T) {
	script, opsLog := cityOnly(t, outcomeReference(t, nil))
	t.Setenv("GC_EXEC_CITY_NAME", "live")
	t.Setenv("GC_EXEC_CITY_PATH", "/live/city")

	res, err := Run(context.Background(), script, Options{SessionName: "from-city"})
	if err != nil {
		t.Fatal(err)
	}
	reportFailures(t, res)
	var protocol, start, serverStart bool
	for _, op := range cityOps(t, opsLog) {
		switch {
		case op.Args == "protocol" && op.City == "/live/city":
			protocol = true
		case op.Args == "start from-city" && op.City == "/live/city":
			start = true
		case op.Args == "start from-city-server" && op.City != "/live/city" && op.City != "":
			serverStart = true
		case op.Args == "server-stop" && op.City == "/live/city":
			t.Error("server-stop sent to the caller's city")
		}
	}
	if !protocol || !start {
		t.Errorf("protocol in the caller's city = %v, start from-city there = %v, want both", protocol, start)
	}
	if !serverStart {
		t.Error("the server check's session did not start in a city of its own")
	}
}

// lastOpIn returns the last op logged in city, empty for none.
func lastOpIn(ops []reference.CityOp, city string) string {
	var last string
	for _, op := range ops {
		if op.City == city {
			last = op.Args
		}
	}
	return last
}

// Kills an interrupted check run outside a city that removes its private
// city without the final server-stop, leaving the server a session brought
// up there keyed to a path that no longer exists: the stop must run on a
// cancellation-detached context.
func TestRun_CanceledBareRunSendsServerStopToItsPrivateCity(t *testing.T) {
	t.Setenv("GC_EXEC_CITY_NAME", "")
	t.Setenv("GC_EXEC_CITY_PATH", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel, as a Ctrl-C would, once the first is-running blocks.
	blocking, release, err := reference.OnFirstOp(outcomeReference(t, map[string]string{
		"is-running": `exec sleep 30`,
	}), "is-running", t.TempDir(), cancel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	script, opsLog := cityOnly(t, blocking)

	res, err := Run(ctx, script, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Protocol.Has(runtime.ProtocolCapabilityServerLifecycle) {
		t.Fatalf("handshake %+v does not declare server-lifecycle; the test needs it", res.Protocol)
	}
	ops := cityOps(t, opsLog)
	if len(ops) == 0 || ops[0].City == "" {
		t.Fatalf("ops = %v, want protocol first, in a private city", ops)
	}
	private := ops[0].City
	if last := lastOpIn(ops, private); last != "server-stop" {
		t.Errorf("last op in the private city of the canceled run = %q, want server-stop", last)
	}
	if _, err := os.Stat(private); !os.IsNotExist(err) {
		t.Errorf("private city %s after the canceled run: stat err = %v, want it removed", private, err)
	}
}

// Kills a check run outside a city whose server checks run in the run's own
// private city rather than one of their own, as when the run's city is
// appended after the op's (the last value of a key wins): server-stop there
// would end the run's sessions too.
func TestRun_OutsideACityServerChecksUseACityOfTheirOwn(t *testing.T) {
	t.Setenv("GC_EXEC_CITY_NAME", "")
	t.Setenv("GC_EXEC_CITY_PATH", "")
	script, opsLog := cityOnly(t, outcomeReference(t, nil))

	res, err := Run(context.Background(), script, Options{SessionName: "bare"})
	if err != nil {
		t.Fatal(err)
	}
	reportFailures(t, res)
	ops := cityOps(t, opsLog)
	if len(ops) == 0 || ops[0].City == "" {
		t.Fatalf("ops = %v, want protocol first, in a private city", ops)
	}
	private := ops[0].City
	var serverCity string
	for _, op := range ops {
		if op.Args == "start bare-server" {
			serverCity = op.City
		}
	}
	switch serverCity {
	case "":
		t.Fatal("the server check's session started in no city")
	case private:
		t.Fatalf("the server check's session started in the run's private city %s, want a city of its own", private)
	}
	var runStops, serverStops int
	for _, op := range ops {
		if op.Args != "server-stop" {
			continue
		}
		switch op.City {
		case private:
			runStops++
		case serverCity:
			serverStops++
		}
	}
	if runStops != 1 {
		t.Errorf("server-stops in the run's private city = %d, want 1, the final one", runStops)
	}
	if serverStops < 3 {
		t.Errorf("server-stops in the server checks' city = %d, want the two checked ones and a final one", serverStops)
	}
	if last := lastOpIn(ops, serverCity); last != "server-stop" {
		t.Errorf("last op in the server checks' city = %q, want the final server-stop", last)
	}
	if _, err := os.Stat(serverCity); !os.IsNotExist(err) {
		t.Errorf("server checks' city %s after the run: stat err = %v, want it removed", serverCity, err)
	}
}

// Kills a check run that swallows a final server-stop that fails before it
// removes a private city: the server may still run, keyed to a deleted path,
// so the run reports it as a warning naming the city.
func TestRun_FailedFinalServerStopIsAWarning(t *testing.T) {
	t.Setenv("GC_EXEC_CITY_NAME", "")
	t.Setenv("GC_EXEC_CITY_PATH", "")
	script, opsLog := cityOnly(t, outcomeReference(t, map[string]string{
		"server-stop": `echo "server still up" >&2; exit 1`,
	}))

	res, err := Run(context.Background(), script, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ops := cityOps(t, opsLog)
	if len(ops) == 0 || ops[0].City == "" {
		t.Fatalf("ops = %v, want protocol first, in a private city", ops)
	}
	private := ops[0].City
	var named bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "server-stop in private city "+private+" ") && strings.Contains(w, "server still up") {
			named = true
		}
	}
	if !named {
		t.Errorf("warnings %q: want one naming the run's private city %s and server-stop's stderr", res.Warnings, private)
	}
	if len(res.Warnings) != 2 {
		t.Errorf("warnings %q: want 2, the run's city's and the server checks' city's", res.Warnings)
	}
}

// launchBodies returns the proc.provision and proc.launch golden's bodies.
func launchBodies() map[string]string {
	return reference.LaunchBodies(reference.Bodies())
}

// launchReference writes the proc.provision and proc.launch golden with
// bodies overridden.
func launchReference(t *testing.T, bodies map[string]string) string {
	t.Helper()
	all := launchBodies()
	for op, body := range bodies {
		all[op] = body
	}
	path := filepath.Join(t.TempDir(), "golden")
	if err := os.WriteFile(path, []byte(reference.Script(t.TempDir(), all)), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// proc.provision and proc.launch are tokens gc honors, so the handshake
// check must not call them unknown.
// Kills: a token gc acts on reported as ignored.
func TestRun_ProvisionAndLaunchAreKnown(t *testing.T) {
	res, err := Run(context.Background(), launchReference(t, nil), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c := findCheck(t, res, handshakeCheck); strings.Contains(c.Detail, "unknown") {
		t.Errorf("handshake detail %q reports a declared token as unknown", c.Detail)
	}
}

// Kills a smoke check that does not run, or does not gate, the un-welded
// start a script declaring proc.provision and proc.launch is sent: the golden
// passes all three checks; a launch answering exit 2 (which fails the pack's
// first start), a launch that starts no agent, a fresh_only launch replacing
// the running agent, a provision answering exit 2 and a provision that
// starts the agent each fail their check; a set-meta answering exit 2 fails
// the metadata round-trip rather than skipping it, as gc fails the launch
// whose identity it cannot publish (publishLaunchIdentity).
func TestRun_ProvisionLaunchSmoke(t *testing.T) {
	res, err := Run(context.Background(), launchReference(t, nil), Options{})
	if err != nil {
		t.Fatal(err)
	}
	reportFailures(t, res)
	for _, name := range []string{provisionCheck, launchCheck, launchFreshOnlyCheck, metadataCheck} {
		if c := findCheck(t, res, name); c.Status != StatusPass {
			t.Errorf("golden: %s = %s (%s), want PASS", name, c.Status, c.Detail)
		}
	}

	golden := launchBodies()
	const freshRefusal = "if [ -n \"$fresh\" ]; then exit 73; fi; reap"
	ignoresFresh := strings.Replace(golden["launch"], freshRefusal, "reap", 1)
	if ignoresFresh == golden["launch"] {
		t.Fatalf("golden launch %q has no fresh_only refusal to remove", golden["launch"])
	}
	for _, m := range []struct {
		name                             string
		bodies                           map[string]string
		provision, launch, freshly, meta Status
	}{
		{"launch exit 2", map[string]string{"launch": `cat >/dev/null; exit 2`}, StatusPass, StatusFail, StatusSkip, StatusPass},
		{"launch starts nothing", map[string]string{"launch": `cat >/dev/null`}, StatusPass, StatusFail, StatusSkip, StatusPass},
		{"launch ignores fresh_only", map[string]string{"launch": ignoresFresh}, StatusPass, StatusPass, StatusFail, StatusPass},
		{"provision exit 2", map[string]string{"provision": `cat >/dev/null; exit 2`}, StatusFail, StatusSkip, StatusSkip, StatusPass},
		{"provision starts the agent", map[string]string{"provision": golden["start"]}, StatusFail, StatusPass, StatusPass, StatusPass},
		{"set-meta exit 2", map[string]string{"set-meta": `cat >/dev/null; exit 2`}, StatusPass, StatusPass, StatusPass, StatusFail},
	} {
		res, err := Run(context.Background(), launchReference(t, m.bodies), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Failed() {
			t.Errorf("mutant %s: Failed() = false, want true", m.name)
		}
		for name, want := range map[string]Status{provisionCheck: m.provision, launchCheck: m.launch, launchFreshOnlyCheck: m.freshly, metadataCheck: m.meta} {
			if c := findCheck(t, res, name); c.Status != want {
				t.Errorf("mutant %s: %s = %s (%s), want %s", m.name, name, c.Status, c.Detail, want)
			}
		}
	}
}

// Kills a smoke check gated on the tokens rather than on the start gc sends:
// provision runs for every handshake that un-welds start (proc.provision with
// proc.launch, or with proc.exec and without carrier.ops), launch only under
// proc.launch, and an inert declaration is sent neither, though its tokens are
// known.
func TestRun_ProvisionLaunchSmokeFollowsTheUnweldedStart(t *testing.T) {
	const golden = `"server-lifecycle","proc.provision","proc.launch"`
	for _, tc := range []struct {
		caps      string
		provision bool
	}{
		{`"server-lifecycle","proc.provision","proc.exec"`, true},
		{`"server-lifecycle","proc.provision"`, false},
		{`"server-lifecycle","proc.provision","proc.exec","carrier.ops"`, false},
		{`"server-lifecycle","proc.launch"`, false},
	} {
		protocol := launchBodies()["protocol"]
		if !strings.Contains(protocol, golden) {
			t.Fatalf("golden protocol %q does not declare %s", protocol, golden)
		}
		res, err := Run(context.Background(), launchReference(t, map[string]string{
			"protocol": strings.Replace(protocol, golden, tc.caps, 1),
		}), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got := hasCheck(res, provisionCheck); got != tc.provision {
			t.Errorf("%s: provision check run = %v, want %v", tc.caps, got, tc.provision)
		}
		if hasCheck(res, launchCheck) || hasCheck(res, launchFreshOnlyCheck) {
			t.Errorf("%s: a launch check ran for a handshake gc sends no launch", tc.caps)
		}
		if c := findCheck(t, res, handshakeCheck); strings.Contains(c.Detail, "unknown") {
			t.Errorf("%s: handshake detail %q reports a declared token as unknown", tc.caps, c.Detail)
		}
	}
}

// Kills a provision and launch smoke check run in the city the check
// inherited, or one that leaves its box or its server behind: its ops run in
// a private city of their own, whose last op is the final server-stop, and
// which is removed.
func TestRun_ProvisionLaunchSmokeUsesACityOfItsOwn(t *testing.T) {
	script, opsLog := cityOnly(t, launchReference(t, nil))
	t.Setenv("GC_EXEC_CITY_NAME", "live")
	t.Setenv("GC_EXEC_CITY_PATH", "/live/city")

	res, err := Run(context.Background(), script, Options{SessionName: "from-city"})
	if err != nil {
		t.Fatal(err)
	}
	reportFailures(t, res)
	ops := cityOps(t, opsLog)
	var city string
	for _, op := range ops {
		if op.Args == "provision from-city-launch" {
			city = op.City
		}
	}
	switch city {
	case "":
		t.Fatal("provision from-city-launch ran in no city")
	case "/live/city":
		t.Fatal("provision from-city-launch ran in the caller's city, want a city of its own")
	}
	var launches int
	for _, op := range ops {
		if strings.HasPrefix(op.Args, "launch ") {
			launches++
			if op.City != city {
				t.Errorf("%q ran in %q, want the provision's city %s", op.Args, op.City, city)
			}
		}
	}
	if launches != 2 {
		t.Errorf("launches = %d, want the launch and the fresh_only launch", launches)
	}
	if last := lastOpIn(ops, city); last != "server-stop" {
		t.Errorf("last op in the provision and launch city = %q, want the final server-stop", last)
	}
	if _, err := os.Stat(city); !os.IsNotExist(err) {
		t.Errorf("provision and launch city %s after the run: stat err = %v, want it removed", city, err)
	}
}

// Kills an interrupted provision and launch smoke check that leaves the
// launched session running: its stop runs on a cancellation-detached
// context, in the check's city, before the final server-stop there.
func TestRun_CanceledProvisionLaunchStopsItsSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel, as a Ctrl-C would, once the launch blocks.
	blocking, release, err := reference.OnFirstOp(launchReference(t, map[string]string{
		"launch": `cat >/dev/null; exec sleep 30`,
	}), "launch", t.TempDir(), cancel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	script, opsLog := cityOnly(t, blocking)
	t.Setenv("GC_EXEC_CITY_NAME", "live")
	t.Setenv("GC_EXEC_CITY_PATH", "/live/city")

	if _, err := Run(ctx, script, Options{SessionName: "canceled"}); err != nil {
		t.Fatal(err)
	}
	ops := cityOps(t, opsLog)
	var city string
	for _, op := range ops {
		if op.Args == "provision canceled-launch" {
			city = op.City
		}
	}
	if city == "" || city == "/live/city" {
		t.Fatalf("provision canceled-launch ran in %q, want a private city of its own", city)
	}
	var stopped bool
	for _, op := range ops {
		if op.Args == "stop canceled-launch" && op.City == city {
			stopped = true
		}
	}
	if !stopped {
		t.Error("the canceled check did not stop canceled-launch in its city")
	}
	if last := lastOpIn(ops, city); last != "server-stop" {
		t.Errorf("last op in the canceled check's city = %q, want the final server-stop", last)
	}
	if _, err := os.Stat(city); !os.IsNotExist(err) {
		t.Errorf("the canceled check's city %s: stat err = %v, want it removed", city, err)
	}
}

// Kills an is-running that exits 2 after provision or launch reported as
// "did not answer: <nil>": the detail names the exit 2 of a required op.
func TestRun_ProvisionLaunchIsRunningExitTwoSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name, isRunning, check string
	}{
		{"after provision", `if [ -d "$box" ]; then echo true; else case "$name" in *-launch) exit 2 ;; esac; echo false; fi`, provisionCheck},
		{"after launch", `if [ -d "$box" ]; then case "$name" in *-launch) exit 2 ;; esac; echo true; else echo false; fi`, launchCheck},
	} {
		res, err := Run(context.Background(), launchReference(t, map[string]string{"is-running": tc.isRunning}), Options{})
		if err != nil {
			t.Fatal(err)
		}
		c := findCheck(t, res, tc.check)
		if c.Status != StatusFail {
			t.Errorf("%s: %s = %s (%s), want FAIL", tc.name, tc.check, c.Status, c.Detail)
		}
		if strings.Contains(c.Detail, "<nil>") || !strings.Contains(c.Detail, "exit 2") {
			t.Errorf("%s: %s detail = %q, want it to name the exit 2", tc.name, tc.check, c.Detail)
		}
	}
}

// input-fence is a token gc honors, so the handshake check must not call it
// unknown; it promises no op of its own, so it adds no capability check.
// Kills: a token gc acts on reported as ignored.
func TestRun_InputFenceCapabilityIsKnown(t *testing.T) {
	state := t.TempDir()
	const from = `["report-attachment","report-activity"]`
	script := conformantScript(state)
	if !strings.Contains(script, from) {
		t.Fatalf("conformant script declares no %s to extend", from)
	}
	body := strings.Replace(script, from, `["report-attachment","report-activity","`+runtime.ProtocolCapabilityInputFence+`"]`, 1)
	res := runScript(t, body, Options{})

	if c := findCheck(t, res, handshakeCheck); strings.Contains(c.Detail, "unknown") {
		t.Errorf("handshake detail = %q, want %s not reported as unknown", c.Detail, runtime.ProtocolCapabilityInputFence)
	}
	if hasCheck(res, "capability "+runtime.ProtocolCapabilityInputFence) {
		t.Errorf("%s produced a capability check", runtime.ProtocolCapabilityInputFence)
	}
}
