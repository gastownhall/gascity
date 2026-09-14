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

// escalationEnvKeys are the escalate.sh settings each test supplies itself. A
// live Gas City session exports GC_ESCALATION_RECIPIENT, so an inherited copy
// makes the default-recipient assertion read the host's routing rather than
// the script's own default.
var escalationEnvKeys = map[string]struct{}{
	"GC_ESCALATION_RECIPIENT":       {},
	"GC_ESCALATE_SEND_TIMEOUT_SECS": {},
	"GC_ESCALATE_INCIDENT_DIR":      {},
	"GC_ESCALATE_NOW_EPOCH":         {},
	"GC_ESCALATE_REMINDER_SECONDS":  {},
}

func runEscalate(t *testing.T, binDir string, extraEnv ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", escalateScriptPath, "--subject", "Dolt backup: 1/2 databases failed to sync [MEDIUM]", "--message", "Failed databases: hq(sync failed)")
	inherited := os.Environ()
	env := make([]string, 0, len(inherited)+1+len(extraEnv))
	for _, entry := range inherited {
		if key, _, ok := strings.Cut(entry, "="); ok {
			if _, skip := escalationEnvKeys[key]; skip {
				continue
			}
		}
		env = append(env, entry)
	}
	env = append(env, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	env = append(env, extraEnv...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runIncidentEscalate(t *testing.T, binDir, incidentDir, now, fingerprint, message string, extraEnv ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", escalateScriptPath,
		"--subject", "Dolt backup failed [MEDIUM]",
		"--message", message,
		"--incident-key", "dolt-backup:hq",
		"--fingerprint", fingerprint)
	inherited := os.Environ()
	env := make([]string, 0, len(inherited)+4+len(extraEnv))
	for _, entry := range inherited {
		if key, _, ok := strings.Cut(entry, "="); ok {
			if _, skip := escalationEnvKeys[key]; skip {
				continue
			}
		}
		env = append(env, entry)
	}
	env = append(env,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_ESCALATE_INCIDENT_DIR="+incidentDir,
		"GC_ESCALATE_NOW_EPOCH="+now,
		"GC_ESCALATE_REMINDER_SECONDS=86400")
	env = append(env, extraEnv...)
	cmd.Env = env
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

func TestEscalateDeduplicatesStableIncidents(t *testing.T) {
	binDir, logPath := fakeGCBin(t, "exit 0\n")
	incidentDir := t.TempDir()
	runs := []struct {
		now         string
		fingerprint string
		message     string
	}{
		{now: "1000", fingerprint: "hq:timeout", message: "first observation"},
		{now: "2800", fingerprint: "hq:timeout", message: "same failure 30 minutes later"},
		{now: "2900", fingerprint: "hq:auth", message: "material state changed"},
		{now: "89301", fingerprint: "hq:auth", message: "daily reminder"},
	}
	for _, run := range runs {
		if out, err := runIncidentEscalate(t, binDir, incidentDir, run.now, run.fingerprint, run.message); err != nil {
			t.Fatalf("incident escalation failed at %s: %v\n%s", run.now, err, out)
		}
	}

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	if got := strings.Count(string(logged), "gc mail send"); got != 3 {
		t.Fatalf("mail sends = %d, want 3 (initial, material change, daily reminder):\n%s", got, logged)
	}
	entries, err := os.ReadDir(incidentDir)
	if err != nil {
		t.Fatalf("read incident directory: %v", err)
	}
	stateFiles := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".state") {
			stateFiles++
		}
	}
	if stateFiles != 1 {
		t.Fatalf("stable incident created %d state files, want 1", stateFiles)
	}
}

func TestEscalateRetriesChangedIncidentAfterSendFailure(t *testing.T) {
	binDir, logPath := fakeGCBin(t, `
case "$*" in
*"material state changed"*)
    if [ ! -f "$GC_FAIL_MARK" ]; then
        : >"$GC_FAIL_MARK"
        exit 1
    fi
    ;;
esac
exit 0
`)
	incidentDir := t.TempDir()
	failMark := filepath.Join(t.TempDir(), "failed-once")
	env := "GC_FAIL_MARK=" + failMark

	if out, err := runIncidentEscalate(t, binDir, incidentDir, "1000", "hq:timeout", "initial failure", env); err != nil {
		t.Fatalf("initial send failed: %v\n%s", err, out)
	}
	if out, err := runIncidentEscalate(t, binDir, incidentDir, "1001", "hq:auth", "material state changed", env); err == nil {
		t.Fatalf("changed-incident send unexpectedly succeeded:\n%s", out)
	}
	if out, err := runIncidentEscalate(t, binDir, incidentDir, "1002", "hq:auth", "retry material state changed", env); err != nil {
		t.Fatalf("retry after changed-incident send failure failed: %v\n%s", err, out)
	}

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	if got := strings.Count(string(logged), "gc mail send"); got != 3 {
		t.Fatalf("mail sends = %d, want initial send plus two changed-incident attempts:\n%s", got, logged)
	}
}
