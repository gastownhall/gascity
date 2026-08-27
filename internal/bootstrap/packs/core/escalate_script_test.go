package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// escalateScriptPath is the on-disk copy of the escalation hook. The other
// tests in this package read the embedded FS because they assert on content;
// these run the script, so they need a real path.
const escalateScriptPath = "assets/scripts/escalate.sh"

// fakeGCBin writes a `gc` shim that appends its argv to a log and then behaves
// as body says. Returns the bin dir and the log path.
func fakeGCBin(t *testing.T, body string) (binDir, logPath string) {
	t.Helper()
	binDir = t.TempDir()
	logPath = filepath.Join(binDir, "gc.log")
	script := "#!/bin/sh\nprintf 'gc %s\\n' \"$*\" >> " + logPath + "\n" + body
	if err := os.WriteFile(filepath.Join(binDir, "gc"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gc: %v", err)
	}
	return binDir, logPath
}

func runEscalate(t *testing.T, binDir string, extraEnv ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", escalateScriptPath, "--subject", "Dolt backup: 1/2 databases failed to sync [MEDIUM]", "--message", "Failed databases: hq(sync failed)")
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestEscalateWakesAnAgentRecipient pins that an escalation addressed to an
// agent session carries --notify.
//
// Without it the send writes a message bead and emits an event with no
// subscriber, so a paused agent finds the escalation only on a turn boundary
// it may never reach. That is not hypothetical: one city's backup advisory
// fired every six hours for a day and reached nobody, while the bead store it
// was warning about went 18 hours without a backup.
func TestEscalateWakesAnAgentRecipient(t *testing.T) {
	binDir, logPath := fakeGCBin(t, "exit 0\n")

	out, err := runEscalate(t, binDir, "GC_ESCALATION_RECIPIENT=local-core.manager")
	if err != nil {
		t.Fatalf("escalate.sh failed: %v\n%s", err, out)
	}

	logged, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read gc log: %v", readErr)
	}
	if !strings.Contains(string(logged), "mail send local-core.manager --notify") {
		t.Fatalf("escalation to an agent must pass --notify, got:\n%s", logged)
	}
}

// TestEscalateDoesNotNotifyTheOperatorInbox is the other half of the same
// contract. `human` names an operator mail inbox with no session behind it, so
// there is nothing to wake and the flag would fail against it. The routing
// default is unchanged by this rule; only the wake is conditional.
func TestEscalateDoesNotNotifyTheOperatorInbox(t *testing.T) {
	binDir, logPath := fakeGCBin(t, "exit 0\n")

	out, err := runEscalate(t, binDir)
	if err != nil {
		t.Fatalf("escalate.sh failed: %v\n%s", err, out)
	}

	logged, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read gc log: %v", readErr)
	}
	if !strings.Contains(string(logged), "mail send human ") {
		t.Fatalf("default recipient changed, got:\n%s", logged)
	}
	if strings.Contains(string(logged), "--notify") {
		t.Fatalf("the operator inbox has no session to wake; --notify must be omitted:\n%s", logged)
	}
}

// TestEscalateSurvivesAHangingWake pins that a wake outliving its bound does
// not take the caller down with it.
//
// The wake can block well past the send it follows, and escalate.sh runs
// inline in deterministic maintenance orders. An unbounded hang there would
// stall the very run that raised the alarm. The mail is already written by the
// time the wake blocks, so a bound that trips costs the wake and not the
// message, and reporting failure would make a caller retry a message that
// landed.
func TestEscalateSurvivesAHangingWake(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skipf("timeout(1) not available: %v", err)
	}
	binDir, logPath := fakeGCBin(t, "sleep 30\nexit 0\n")

	out, err := runEscalate(t, binDir,
		"GC_ESCALATION_RECIPIENT=local-core.manager",
		"GC_ESCALATE_SEND_TIMEOUT_SECS=1")
	if err != nil {
		t.Fatalf("a hung wake must not fail the escalation: %v\n%s", err, out)
	}
	if !strings.Contains(out, "wake exceeded") {
		t.Errorf("an abandoned wake must say so rather than pass silently:\n%s", out)
	}

	logged, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read gc log: %v", readErr)
	}
	if !strings.Contains(string(logged), "mail send local-core.manager --notify") {
		t.Fatalf("the send must still have been attempted:\n%s", logged)
	}
}
