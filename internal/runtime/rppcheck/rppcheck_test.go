package rppcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
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

func capableHelperScript(stateDir string) string {
	return capableHelperScriptWithMode(stateDir, "")
}

func capableHelperScriptWithMode(stateDir, mode string) string {
	return fmt.Sprintf(`
export GC_RPP_CHECK_HELPER=1
export GC_RPP_CHECK_STATE=%q
export GC_RPP_CHECK_MODE=%q
exec %q -test.run='^TestRPPCheckerHelperProcess$' -- "$@"
`, stateDir, mode, os.Args[0])
}

// TestRPPCheckerHelperProcess is invoked behind a shell wrapper so the checker
// can exercise a deterministic provider without depending on jq, screen, or a
// platform-specific SHA-256 command in the unit-test environment.
func TestRPPCheckerHelperProcess(_ *testing.T) {
	if os.Getenv("GC_RPP_CHECK_HELPER") != "1" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		helperProcessFail("missing op")
	}
	op := args[0]
	name := ""
	if len(args) > 1 {
		name = args[1]
	}
	state := os.Getenv("GC_RPP_CHECK_STATE")
	marker := filepath.Join(state, name+".running")
	switch op {
	case "protocol":
		fmt.Printf(`{"version":0,"capabilities":[%q]}`, runtime.ProtocolCapabilityReconcilerOwnedMergeablePaths)
		os.Exit(0)
	case "start":
		var cfg startConfig
		if err := json.NewDecoder(os.Stdin).Decode(&cfg); err != nil {
			helperProcessFail(err.Error())
		}
		if os.Getenv("GC_RPP_CHECK_MODE") == "fail-positive-after-allocation" && strings.HasSuffix(name, "-ro-owned") {
			if err := os.WriteFile(marker, nil, 0o644); err != nil {
				helperProcessFail(err.Error())
			}
			helperProcessFail("positive staging failed after allocation")
		}
		if os.Getenv("GC_RPP_CHECK_MODE") == "fail-after-allocation" &&
			len(cfg.ReconcilerOwnedMergeablePaths) > 0 &&
			(strings.HasSuffix(name, "-ro-stale") || strings.HasSuffix(name, "-ro-missing")) {
			if err := os.WriteFile(marker, nil, 0o644); err != nil {
				helperProcessFail(err.Error())
			}
			helperProcessFail("staging rejected after allocation")
		}
		acknowledgement := ""
		if len(cfg.ReconcilerOwnedMergeablePaths) > 0 {
			helperStageOwnedConfig(cfg)
			if os.Getenv("GC_RPP_CHECK_MODE") != "exit-after-first-poll" {
				acknowledgement = ownedProbeAcknowledgement(cfg)
				if err := os.WriteFile(marker+".ack", []byte(acknowledgement), 0o644); err != nil {
					helperProcessFail(err.Error())
				}
			}
		}
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			helperProcessFail(err.Error())
		}
		os.Exit(0)
	case "stop":
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			helperProcessFail(err.Error())
		}
		if err := os.Remove(marker + ".ack"); err != nil && !os.IsNotExist(err) {
			helperProcessFail(err.Error())
		}
		if err := os.Remove(marker + ".polled"); err != nil && !os.IsNotExist(err) {
			helperProcessFail(err.Error())
		}
		os.Exit(0)
	case "is-running":
		if _, err := os.Stat(marker); err == nil {
			if os.Getenv("GC_RPP_CHECK_MODE") == "exit-after-first-poll" && strings.HasSuffix(name, "-ro-owned") {
				if _, err := os.Stat(marker + ".polled"); err == nil {
					if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
						helperProcessFail(err.Error())
					}
					fmt.Print("false\n")
					os.Exit(0)
				}
				if err := os.WriteFile(marker+".polled", nil, 0o644); err != nil {
					helperProcessFail(err.Error())
				}
			}
			fmt.Print("true\n")
		} else {
			fmt.Print("false\n")
		}
		os.Exit(0)
	case "process-alive":
		probe, err := io.ReadAll(os.Stdin)
		if err != nil {
			helperProcessFail(err.Error())
		}
		ack, err := os.ReadFile(marker + ".ack")
		if err == nil && strings.TrimSpace(string(probe)) == string(ack) {
			fmt.Print("true\n")
		} else {
			fmt.Print("false\n")
		}
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

func helperStageOwnedConfig(cfg startConfig) {
	if len(cfg.ReconcilerOwnedMergeablePaths) != 1 || cfg.ReconcilerOwnedMergeablePaths[0] != ".codex/hooks.json" || len(cfg.CopyFiles) != 1 {
		helperProcessFail("invalid owned-path shape")
	}
	entry := cfg.CopyFiles[0]
	if entry.RelDst != ".codex/hooks.json" || !entry.Probed || entry.ContentHash == "" {
		helperProcessFail("invalid owned copy entry")
	}
	data, err := os.ReadFile(entry.Src)
	if err != nil {
		helperProcessFail(err.Error())
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != entry.ContentHash {
		helperProcessFail("stale owned digest")
	}
	// The capability promises an exact-path exclusion, not an overlay-wide
	// exclusion. Stage every unowned fixture while deliberately ignoring the
	// conflicting overlay hook. The verifier must not rely on an implicit mirror
	// of the controller-local work directory.
	if _, err := os.Stat(filepath.Join(cfg.WorkDir, ".gc-rpp-owned-check.sh")); !os.IsNotExist(err) {
		helperProcessFail("verifier unexpectedly pre-existed in work_dir")
	}
	for _, rel := range []string{".gc-rpp-overlay-sibling", ".gc-rpp-expected-hooks", ".gc-rpp-owned-check.sh", ownedProbeAcknowledgement(cfg)} {
		data, err := os.ReadFile(filepath.Join(cfg.OverlayDir, rel))
		if err != nil {
			helperProcessFail(err.Error())
		}
		mode := os.FileMode(0o644)
		if rel == ".gc-rpp-owned-check.sh" || rel == ownedProbeAcknowledgement(cfg) {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(cfg.WorkDir, rel), data, mode); err != nil {
			helperProcessFail(err.Error())
		}
	}
	dst := filepath.Join(cfg.WorkDir, ".codex", "hooks.json")
	if filepath.Clean(entry.Src) != filepath.Clean(dst) {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			helperProcessFail(err.Error())
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			helperProcessFail(err.Error())
		}
	}
	expected, err := os.ReadFile(filepath.Join(cfg.WorkDir, ".gc-rpp-expected-hooks"))
	if err != nil {
		helperProcessFail(err.Error())
	}
	actual, err := os.ReadFile(dst)
	if err != nil {
		helperProcessFail(err.Error())
	}
	if !bytes.Equal(actual, expected) {
		helperProcessFail("canonical handoff mismatch")
	}
}

func helperProcessFail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}

func TestRun_ReconcilerOwnedStagingCapabilityPassesConformance(t *testing.T) {
	state := t.TempDir()
	res := runScript(t, capableHelperScript(state), Options{})

	if res.Failed() {
		t.Fatalf("Failed() = true for capable staging provider: %+v", res.Checks)
	}
	for _, name := range []string{
		reconcilerOwnedStagingCheckPrefix + "canonical handoff",
		reconcilerOwnedStagingCheckPrefix + "stale digest rejection",
		reconcilerOwnedStagingCheckPrefix + "missing source rejection",
	} {
		if check := findCheck(t, res, name); check.Status != StatusPass {
			t.Errorf("check %q = %s (%s), want PASS", name, check.Status, check.Detail)
		}
	}
	if check := findCheck(t, res, reconcilerOwnedStagingCheckPrefix+"canonical handoff"); !strings.Contains(check.Detail, "verifier process acknowledged") {
		t.Fatalf("canonical handoff detail = %q, want fact-based verifier acknowledgement", check.Detail)
	}
}

func TestRun_ReconcilerOwnedRejectedStartFailsIfSessionBecameRunnable(t *testing.T) {
	state := t.TempDir()
	res := runScript(t, capableHelperScriptWithMode(state, "fail-after-allocation"), Options{})

	if !res.Failed() {
		t.Fatal("Failed() = false when invalid starts became runnable before returning an error")
	}
	for _, name := range []string{
		reconcilerOwnedStagingCheckPrefix + "stale digest rejection",
		reconcilerOwnedStagingCheckPrefix + "missing source rejection",
	} {
		check := findCheck(t, res, name)
		if check.Status != StatusFail || !strings.Contains(check.Detail, "became runnable") {
			t.Errorf("check %q = %s (%s), want runnable-contract FAIL", name, check.Status, check.Detail)
		}
	}
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".running") {
			t.Fatalf("negative staging probe leaked running marker %q", entry.Name())
		}
	}
}

func TestRun_ReconcilerOwnedPositiveStartCleansUpAfterAllocationError(t *testing.T) {
	state := t.TempDir()
	res := runScript(t, capableHelperScriptWithMode(state, "fail-positive-after-allocation"), Options{})

	check := findCheck(t, res, reconcilerOwnedStagingCheckPrefix+"canonical handoff")
	if check.Status != StatusFail || !strings.Contains(check.Detail, "positive staging failed after allocation") {
		t.Fatalf("canonical handoff = %s (%s), want allocation-error FAIL", check.Status, check.Detail)
	}
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "-ro-owned.running") {
			t.Fatalf("positive staging probe leaked running marker %q", entry.Name())
		}
	}
}

func TestRun_ReconcilerOwnedStagingVerifierMustRemainRunning(t *testing.T) {
	state := t.TempDir()
	res := runScript(t, capableHelperScriptWithMode(state, "exit-after-first-poll"), Options{})

	check := findCheck(t, res, reconcilerOwnedStagingCheckPrefix+"canonical handoff")
	if check.Status != StatusFail {
		t.Fatalf("canonical handoff = %s (%s), want FAIL for verifier that exits after the first poll", check.Status, check.Detail)
	}
	if !strings.Contains(check.Detail, "before staging acknowledgement") {
		t.Fatalf("canonical handoff detail = %q, want missing-acknowledgement failure", check.Detail)
	}
}

func TestRun_ReconcilerOwnedStagingCapabilityCannotBeHandshakeOnly(t *testing.T) {
	state := t.TempDir()
	body := strings.Replace(minimalScript(state),
		`case "$op" in`,
		fmt.Sprintf("case \"$op\" in\n  protocol) printf '%%s' '{\"version\":0,\"capabilities\":[\"%s\"]}' ;;", runtime.ProtocolCapabilityReconcilerOwnedMergeablePaths), 1)
	res := runScript(t, body, Options{})

	if !res.Failed() {
		t.Fatal("Failed() = false for provider that advertises but ignores the staging contract")
	}
	for _, name := range []string{
		reconcilerOwnedStagingCheckPrefix + "stale digest rejection",
		reconcilerOwnedStagingCheckPrefix + "missing source rejection",
	} {
		if check := findCheck(t, res, name); check.Status != StatusFail {
			t.Errorf("check %q = %s (%s), want FAIL", name, check.Status, check.Detail)
		}
	}
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
