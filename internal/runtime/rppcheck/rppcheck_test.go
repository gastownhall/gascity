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

func TestRun_CanceledRunStopsSessionViaDetachedContext(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(conformantScript(state),
		`is-running)        if [ -f "$state/$name.running" ]; then echo true; else echo false; fi`,
		`is-running)        touch "$state/is-running-called"; exec sleep 30`, 1)
	body = strings.Replace(body,
		`stop)              rm -f "$state/$name.running"`,
		`stop)              echo "$name" >> "$state/stop.log"; rm -f "$state/$name.running"`, 1)
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
	if !strings.Contains(string(stops), "cancel-session") {
		t.Errorf("cleanup stop log %q missing the session name", stops)
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
