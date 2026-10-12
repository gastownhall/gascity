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

// runPackScript runs one of this package's scripts under bash and returns its
// combined output. It drops the inherited settings named in skipEnv, which
// each test supplies itself, puts binDir first on PATH so the script reaches
// the fake gc, and then appends extraEnv.
//
//nolint:unparam // on main other pack-script tests run through this helper too (#7264); on hotfix/1.5.1 only escalate.sh does
func runPackScript(t *testing.T, script, binDir string, skipEnv map[string]struct{}, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	inherited := os.Environ()
	env := make([]string, 0, len(inherited)+1+len(extraEnv))
	for _, entry := range inherited {
		if key, _, ok := strings.Cut(entry, "="); ok {
			if _, skip := skipEnv[key]; skip {
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
	return runPackScript(t, escalateScriptPath, binDir, escalationEnvKeys, extraEnv,
		"--subject", "Dolt backup: 1/2 databases failed to sync [MEDIUM]",
		"--message", "Failed databases: hq(sync failed)")
}

func runIncidentEscalate(t *testing.T, binDir, incidentDir, now, fingerprint, message string, extraEnv ...string) (string, error) {
	t.Helper()
	env := append([]string{
		"GC_ESCALATE_INCIDENT_DIR=" + incidentDir,
		"GC_ESCALATE_NOW_EPOCH=" + now,
		"GC_ESCALATE_REMINDER_SECONDS=86400",
	}, extraEnv...)
	return runPackScript(t, escalateScriptPath, binDir, escalationEnvKeys, env,
		"--subject", "Dolt backup failed [MEDIUM]",
		"--message", message,
		"--incident-key", "dolt-backup:hq",
		"--fingerprint", fingerprint)
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
// stall the very run that raised the alarm. gc reports the mail sent before
// it wakes the recipient, so a bound that trips after that report costs the
// wake and not the message, and reporting failure would make a caller retry a
// message that landed.
func TestEscalateSurvivesAHangingWake(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skipf("timeout(1) not available: %v", err)
	}
	binDir, logPath := fakeGCBin(t, `printf 'Sent message gc-1 to %s\n' "$3"
sleep 30
exit 0
`)

	out, err := runEscalate(t, binDir,
		"GC_ESCALATION_RECIPIENT=local-core.manager",
		"GC_ESCALATE_SEND_TIMEOUT_SECS=1")
	if err != nil {
		t.Fatalf("a hung wake must not fail the escalation: %v\n%s", err, out)
	}
	if !strings.Contains(out, "sent; gc exceeded 1s after sending it") {
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

// TestEscalateDeduplicatesStableIncidents pins the incident contract. A keyed
// escalation notifies when first seen, stays quiet while its fingerprint is
// unchanged, notifies as soon as the fingerprint changes, and repeats an
// unchanged incident once per reminder interval, all from one state file per
// key.
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

// TestEscalateDefaultsTheFingerprintToSubjectAndMessage pins the fingerprint
// an incident gets without --fingerprint: its subject and message. An exact
// repeat stays quiet, and a changed message alerts at once.
func TestEscalateDefaultsTheFingerprintToSubjectAndMessage(t *testing.T) {
	binDir, logPath := fakeGCBin(t, "exit 0\n")
	incidentDir := t.TempDir()
	runs := []struct {
		now     string
		message string
	}{
		{now: "1000", message: "first observation"},
		{now: "1001", message: "first observation"},
		{now: "1002", message: "changed observation"},
	}
	for _, run := range runs {
		out, err := runPackScript(t, escalateScriptPath, binDir, escalationEnvKeys,
			[]string{
				"GC_ESCALATE_INCIDENT_DIR=" + incidentDir,
				"GC_ESCALATE_NOW_EPOCH=" + run.now,
				"GC_ESCALATE_REMINDER_SECONDS=86400",
			},
			"--subject", "Dolt backup failed [MEDIUM]",
			"--message", run.message,
			"--incident-key", "dolt-backup:hq")
		if err != nil {
			t.Fatalf("incident escalation failed at %s: %v\n%s", run.now, err, out)
		}
	}
	if got := mailSends(t, logPath); got != 2 {
		t.Fatalf("mail sends = %d, want 2 (first observation, then the changed message)", got)
	}
}

// TestEscalateRetriesChangedIncidentAfterSendFailure pins that only a
// delivered escalation is recorded. When the send for a changed fingerprint
// fails, the state still holds the previous one, so the next run retries
// rather than treating the change as already reported.
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

// mailSends counts the sends the fake gc logged. A log that was never written
// means gc was never called.
func mailSends(t *testing.T, logPath string) int {
	t.Helper()
	logged, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	return strings.Count(string(logged), "gc mail send")
}

func runResolveIncident(t *testing.T, binDir, incidentDir, key string) (string, error) {
	t.Helper()
	return runPackScript(t, escalateScriptPath, binDir, escalationEnvKeys,
		[]string{"GC_ESCALATE_INCIDENT_DIR=" + incidentDir}, "--resolve-incident", key)
}

// TestEscalateSendsWhenIncidentStateIsUnusable pins that incident state is
// best-effort. Dedupe saves noise and must never cost the alert: when the
// incident directory cannot be created, the escalation still goes out, exits
// 0, and says that this run was not deduplicated. A regular file in the path
// stands in for any I/O failure because it stops root too.
func TestEscalateSendsWhenIncidentStateIsUnusable(t *testing.T) {
	binDir, logPath := fakeGCBin(t, "exit 0\n")
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}

	out, err := runIncidentEscalate(t, binDir, filepath.Join(blocker, "incidents"), "1000", "hq:timeout", "first observation")
	if err != nil {
		t.Fatalf("unusable incident state must not fail the escalation: %v\n%s", err, out)
	}
	if got := mailSends(t, logPath); got != 1 {
		t.Fatalf("mail sends = %d, want 1: the alert goes out without dedupe\n%s", got, out)
	}
	if !strings.Contains(out, "incident state unavailable") {
		t.Errorf("a run that skipped dedupe must say so:\n%s", out)
	}
}

// TestEscalateJudgesAnOverrunSendByGCsReport pins the timeout rule for every
// recipient. gc prints "Sent message" once the mail is written, so that report
// decides what a tripped bound cost. After it, only the tail of the send
// overran (the wake, or gc's exit), so the escalation landed and is recorded,
// and the next run stays quiet. Before it, gc can still be opening the store
// or resolving the recipient, so the mail may never have been written; that
// stays a failure and records nothing, so the next run sends again instead of
// suppressing the incident for a whole reminder interval. Whether a wake was
// requested decides neither.
func TestEscalateJudgesAnOverrunSendByGCsReport(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skipf("timeout(1) not available: %v", err)
	}
	// The first send hangs past the bound, after the report when
	// GC_REPORT_FIRST is set; every later send reports and exits at once.
	const hangOnce = `
if [ ! -f "$GC_HANG_MARK" ]; then
    : >"$GC_HANG_MARK"
    if [ -n "$GC_REPORT_FIRST" ]; then
        printf 'Sent message gc-1 to %s\n' "$3"
    fi
    sleep 30
fi
printf 'Sent message gc-2 to %s\n' "$3"
exit 0
`
	cases := []struct {
		name        string
		recipient   string
		reportFirst bool
	}{
		{name: "agent overran after the report", recipient: "local-core.manager", reportFirst: true},
		{name: "human overran after the report", recipient: "human", reportFirst: true},
		{name: "agent stalled before the write", recipient: "local-core.manager"},
		{name: "human stalled before the write", recipient: "human"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir, logPath := fakeGCBin(t, hangOnce)
			incidentDir := t.TempDir()
			env := []string{
				"GC_HANG_MARK=" + filepath.Join(t.TempDir(), "hung-once"),
				"GC_ESCALATE_SEND_TIMEOUT_SECS=1",
				"GC_ESCALATION_RECIPIENT=" + tc.recipient,
			}
			if tc.reportFirst {
				env = append(env, "GC_REPORT_FIRST=1")
			}

			out, err := runIncidentEscalate(t, binDir, incidentDir, "1000", "hq:timeout", "first observation", env...)
			if tc.reportFirst {
				if err != nil {
					t.Fatalf("a send gc reported before it overran must succeed: %v\n%s", err, out)
				}
				if !strings.Contains(out, "sent; gc exceeded 1s after sending it") {
					t.Errorf("an overrun after the report must say the mail was sent:\n%s", out)
				}
				if !strings.Contains(out, "Sent message gc-1 to "+tc.recipient) {
					t.Errorf("gc's own report must still reach the caller's log:\n%s", out)
				}
			} else {
				if err == nil {
					t.Fatalf("a send that overran before gc reported it must fail:\n%s", out)
				}
				if !strings.Contains(out, "exceeded 1s before gc reported it sent") || strings.Contains(out, "sent; gc exceeded") {
					t.Errorf("an overrun before the report must be reported as abandoned, not as sent:\n%s", out)
				}
			}
			for _, now := range []string{"1001", "1002"} {
				if out, err := runIncidentEscalate(t, binDir, incidentDir, now, "hq:timeout", "first observation", env...); err != nil {
					t.Fatalf("escalation at %s failed: %v\n%s", now, err, out)
				}
			}
			want := 2
			if tc.reportFirst {
				want = 1
			}
			if got := mailSends(t, logPath); got != want {
				t.Fatalf("mail sends = %d, want %d: a reported send is recorded, an unreported one is retried once and then recorded", got, want)
			}
		})
	}
}

// TestEscalateExplainsASuppressedRepeat pins that suppression is visible. The
// order log is where an operator looks for a missing alert, so a suppressed
// repeat prints, on stdout (callers discard stderr), when the incident was
// last delivered and when it will remind. The state file names its key, so
// the operator can find the file to reset it.
func TestEscalateExplainsASuppressedRepeat(t *testing.T) {
	binDir, _ := fakeGCBin(t, "exit 0\n")
	incidentDir := t.TempDir()

	if out, err := runIncidentEscalate(t, binDir, incidentDir, "1000", "hq:timeout", "first observation"); err != nil {
		t.Fatalf("initial escalation failed: %v\n%s", err, out)
	}
	out, err := runIncidentEscalate(t, binDir, incidentDir, "2800", "hq:timeout", "same failure 30 minutes later")
	if err != nil {
		t.Fatalf("suppressed escalation failed: %v\n%s", err, out)
	}
	const want = "escalate: incident dolt-backup:hq unchanged since 1970-01-01T00:16:40Z; next reminder after 1970-01-02T00:16:40Z"
	if !strings.Contains(out, want) {
		t.Errorf("suppressed repeat output missing %q:\n%s", want, out)
	}

	states, err := filepath.Glob(filepath.Join(incidentDir, "*.state"))
	if err != nil || len(states) != 1 {
		t.Fatalf("incident state files = %v (%v), want one", states, err)
	}
	state, err := os.ReadFile(states[0])
	if err != nil {
		t.Fatalf("read incident state: %v", err)
	}
	if !strings.Contains(string(state), "key=dolt-backup:hq\n") {
		t.Errorf("incident state must name its key:\n%s", state)
	}
}

// TestEscalateResolvedIncidentAlertsOnRecurrence pins the recovery path. A
// caller whose condition cleared passes --resolve-incident, which sends
// nothing and forgets the incident, so a recurrence inside the reminder
// interval alerts at once. That fail, recover, fail sequence is the flapping
// an operator most needs to hear about.
func TestEscalateResolvedIncidentAlertsOnRecurrence(t *testing.T) {
	binDir, logPath := fakeGCBin(t, "exit 0\n")
	incidentDir := t.TempDir()

	if out, err := runIncidentEscalate(t, binDir, incidentDir, "1000", "hq:timeout", "first observation"); err != nil {
		t.Fatalf("initial escalation failed: %v\n%s", err, out)
	}
	if out, err := runResolveIncident(t, binDir, incidentDir, "dolt-backup:hq"); err != nil {
		t.Fatalf("resolve failed: %v\n%s", err, out)
	}
	if got := mailSends(t, logPath); got != 1 {
		t.Fatalf("mail sends after resolve = %d, want 1: resolving sends nothing", got)
	}
	if out, err := runIncidentEscalate(t, binDir, incidentDir, "1060", "hq:timeout", "recurred a minute later"); err != nil {
		t.Fatalf("recurrence escalation failed: %v\n%s", err, out)
	}
	if got := mailSends(t, logPath); got != 2 {
		t.Fatalf("mail sends = %d, want 2: a recurrence after recovery is a new incident", got)
	}
}

// TestEscalateResolveWithoutStateSendsNothing pins that callers may resolve
// on every clean run. With no incident on record, and no incident directory
// at all, resolving exits 0, sends nothing, and creates nothing.
func TestEscalateResolveWithoutStateSendsNothing(t *testing.T) {
	binDir, logPath := fakeGCBin(t, "exit 0\n")
	missing := filepath.Join(t.TempDir(), "never-created")

	if out, err := runResolveIncident(t, binDir, missing, "dolt-backup:hq"); err != nil {
		t.Fatalf("resolving an unknown incident must succeed: %v\n%s", err, out)
	}
	if logged, err := os.ReadFile(logPath); !os.IsNotExist(err) {
		t.Fatalf("resolving must not call gc (read err %v):\n%s", err, logged)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("resolving must not create the incident directory (stat err %v)", err)
	}
}

// TestEscalateResendsAfterTheClockStepsBack pins that only a delivery in the
// past suppresses. A clock stepped backwards, or a state file stamped from a
// clock that ran ahead, gives a negative elapsed time that would otherwise
// read as inside the reminder interval until the clock caught up.
func TestEscalateResendsAfterTheClockStepsBack(t *testing.T) {
	binDir, logPath := fakeGCBin(t, "exit 0\n")
	incidentDir := t.TempDir()

	for _, now := range []string{"500000", "1000"} {
		if out, err := runIncidentEscalate(t, binDir, incidentDir, now, "hq:timeout", "same failure"); err != nil {
			t.Fatalf("escalation at %s failed: %v\n%s", now, err, out)
		}
	}
	if got := mailSends(t, logPath); got != 2 {
		t.Fatalf("mail sends = %d, want 2: a last delivery in the future must not suppress", got)
	}
}
