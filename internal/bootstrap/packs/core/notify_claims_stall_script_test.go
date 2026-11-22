package core

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestNotifyOnClaimsStalledOrders pins both orders' event contracts: each
// wakes on one of the claims-stall guard's events and runs the one script.
func TestNotifyOnClaimsStalledOrders(t *testing.T) {
	assertEventExecOrder(t, "notify-on-claims-stalled.toml", "agent.claims_stalled", "notify-claims-stall.sh")
	assertEventExecOrder(t, "notify-on-claims-resumed.toml", "agent.claims_resumed", "notify-claims-stall.sh")
}

const notifyClaimsStallScriptPath = "assets/scripts/notify-claims-stall.sh"

// claimsStallEvents writes the `gc events` answers the fake gc serves: one
// JSON event per line, by type.
func claimsStallEvents(t *testing.T, dir string, stalled, resumed []string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "stalled.jsonl"), []byte(strings.Join(stalled, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "resumed.jsonl"), []byte(strings.Join(resumed, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	stalledEvent = `{"seq":101,"type":"agent.claims_stalled","subject":"gascity/review-codex","message":"Agent gascity/review-codex stopped claiming its work on cursor-grok: 3 of its seats in a row failed to claim the work routed to them while it waited (since 2026-10-31T07:16:00Z).","payload":{"template":"gascity/review-codex","provider":"cursor-grok","episode":"gascity/review-codex@cursor-grok@2026-10-31T07:16:00Z","action":"held","cause":"seats","seats":3,"since":"2026-10-31T07:16:00Z"}}`
	probeEvent   = `{"seq":140,"type":"agent.claims_stalled","subject":"gascity/review-codex","message":"Agent gascity/review-codex stopped claiming its work on cursor-grok: its probe seat failed to claim too (5 seats in all).","payload":{"template":"gascity/review-codex","provider":"cursor-grok","episode":"gascity/review-codex@cursor-grok@2026-10-31T07:16:00Z","action":"held","cause":"probe","seats":5,"since":"2026-10-31T07:16:00Z"}}`
	resumedEvent = `{"seq":180,"type":"agent.claims_resumed","subject":"gascity/review-codex","message":"Agent gascity/review-codex claims its work again on cursor-grok.","payload":{"template":"gascity/review-codex","provider":"cursor-grok","episode":"gascity/review-codex@cursor-grok@2026-10-31T07:16:00Z","cause":"claimed","seats":5,"since":"2026-10-31T07:16:00Z"}}`
	// movedOffEvent ends the episode by a config change that moved the
	// template off the stalled provider.
	movedOffEvent = `{"seq":180,"type":"agent.claims_resumed","subject":"gascity/review-codex","message":"Agent gascity/review-codex was moved off cursor-grok by a config change; the stall that began 2026-10-31T07:16:00Z (5 seats failed) is over and its routed work counts as demand again.","payload":{"template":"gascity/review-codex","provider":"cursor-grok","episode":"gascity/review-codex@cursor-grok@2026-10-31T07:16:00Z","cause":"config_changed","seats":5,"since":"2026-10-31T07:16:00Z"}}`
)

// claimsStallGC writes the fake gc the script runs. It answers each events read
// with the file claimsStallEvents wrote for its type, and runs mail for a mail
// send to human.
func claimsStallGC(t *testing.T, work, mail string) (binDir, logPath string) {
	t.Helper()
	return claimsStallGCReading(t, `cat "`+filepath.Join(work, "stalled.jsonl")+`"`, `cat "`+filepath.Join(work, "resumed.jsonl")+`"`, mail)
}

// claimsStallGCReading is claimsStallGC answering the stall read with stalled
// and the resumption read with resumed, each a shell command.
func claimsStallGCReading(t *testing.T, stalled, resumed, mail string) (binDir, logPath string) {
	t.Helper()
	return fakeGCBin(t, `case "$1 $2 $3" in
"events --type agent.claims_stalled") `+stalled+` ;;
"events --type agent.claims_resumed") `+resumed+` ;;
"mail send human") `+mail+` ;;
esac
`)
}

// claimsStallEnv is the environment of a run in work that escalates through
// escalate, with its incidents under work.
func claimsStallEnv(work, escalate string) []string {
	return []string{
		"GC_ESCALATE_SCRIPT=" + escalate,
		"GC_ESCALATE_INCIDENT_DIR=" + filepath.Join(work, "incidents"),
		"GC_CITY_PATH=" + work,
	}
}

// coreEscalateScript is the absolute path of this pack's escalate.sh.
func coreEscalateScript(t *testing.T) string {
	t.Helper()
	escalate, err := filepath.Abs(escalateScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	return escalate
}

// claimsStallMailSends are the `gc mail send` lines of the fake gc's log.
func claimsStallMailSends(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "gc mail send ") {
			out = append(out, line)
		}
	}
	return out
}

// TestNotifyClaimsStallMailsOncePerEpisode is the script's contract: a stall
// mails the escalation recipient once per episode, however many alerts the
// episode raises and however often the window is re-read; its resumption
// resolves the incident and mails once; and the stall of an episode that is
// over is not mailed again when a later run re-reads it.
func TestNotifyClaimsStallMailsOncePerEpisode(t *testing.T) {
	work := t.TempDir()
	binDir, logPath := claimsStallGC(t, work, `echo "Sent message m-1 to human"`)
	env := claimsStallEnv(work, coreEscalateScript(t))
	run := func() {
		t.Helper()
		if out, err := runPackScript(t, notifyClaimsStallScriptPath, binDir, escalationEnvKeys, env); err != nil {
			t.Fatalf("notify-claims-stall: %v\n%s", err, out)
		}
	}

	claimsStallEvents(t, work, []string{stalledEvent}, nil)
	run()
	sends := claimsStallMailSends(t, logPath)
	if len(sends) != 1 || !strings.Contains(sends[0], "Agent gascity/review-codex stopped claiming its work on cursor-grok [HIGH]") ||
		!strings.Contains(sends[0], "gc mail send human ") {
		t.Fatalf("sends after the stall = %q, want one HIGH mail to human about the template and provider", sends)
	}

	// The probe alert of the same episode, and a re-read of the window,
	// mail nothing more.
	claimsStallEvents(t, work, []string{stalledEvent, probeEvent}, nil)
	run()
	run()
	if sends := claimsStallMailSends(t, logPath); len(sends) != 1 {
		t.Fatalf("sends = %d after the probe alert and a re-read, want still 1:\n%s", len(sends), strings.Join(sends, "\n"))
	}

	// The resumption mails once, and the over episode's stall stays quiet.
	claimsStallEvents(t, work, []string{stalledEvent, probeEvent}, []string{resumedEvent})
	run()
	run()
	sends = claimsStallMailSends(t, logPath)
	if len(sends) != 2 || !strings.Contains(sends[1], "Agent gascity/review-codex claims its work again on cursor-grok") {
		t.Fatalf("sends after the resumption = %q, want the stall then one resumption", sends)
	}
}

// TestNotifyClaimsStallFailsLoud: an undeliverable mail exits non-zero, so
// the controller logs the order's output and the next run retries.
func TestNotifyClaimsStallFailsLoud(t *testing.T) {
	work := t.TempDir()
	binDir, _ := claimsStallGC(t, work, `echo "store unavailable" >&2; exit 1`)
	claimsStallEvents(t, work, []string{stalledEvent}, nil)
	out, err := runPackScript(t, notifyClaimsStallScriptPath, binDir, escalationEnvKeys, claimsStallEnv(work, coreEscalateScript(t)))
	if err == nil {
		t.Fatalf("an undeliverable stall mail exited 0:\n%s", out)
	}
	if !strings.Contains(out, "could not escalate the stall of gascity/review-codex") {
		t.Fatalf("output %q, want the failure named", out)
	}
}

// TestNotifyClaimsStallSaysAConfigChangeEndedTheStall: a stall that ends
// because a config change moved the template off the stalled provider is not
// reported as the template claiming its work again on that provider.
func TestNotifyClaimsStallSaysAConfigChangeEndedTheStall(t *testing.T) {
	work := t.TempDir()
	binDir, logPath := claimsStallGC(t, work, `echo "Sent message m-1 to human"`)
	claimsStallEvents(t, work, []string{stalledEvent}, []string{movedOffEvent})

	out, err := runPackScript(t, notifyClaimsStallScriptPath, binDir, escalationEnvKeys, claimsStallEnv(work, coreEscalateScript(t)))
	if err != nil {
		t.Fatalf("notify-claims-stall: %v\n%s", err, out)
	}

	sends := claimsStallMailSends(t, logPath)
	if len(sends) != 1 || !strings.Contains(sends[0], "-s Agent gascity/review-codex was moved off cursor-grok; its claims stall is over -m") {
		t.Fatalf("sends = %q, want one note that the config change ended the stall", sends)
	}
}

// TestNotifyClaimsStallFailsLoudWhenTheStallIncidentDoesNotClear: a
// resumption whose stall incident escalate.sh cannot clear still sends its
// note, then fails the run with escalate.sh's own error, so the controller
// logs it.
func TestNotifyClaimsStallFailsLoudWhenTheStallIncidentDoesNotClear(t *testing.T) {
	work := t.TempDir()
	binDir, logPath := claimsStallGC(t, work, `echo "Sent message m-1 to human"`)
	env := claimsStallEnv(work, coreEscalateScript(t))
	claimsStallEvents(t, work, []string{stalledEvent}, nil)
	if out, err := runPackScript(t, notifyClaimsStallScriptPath, binDir, escalationEnvKeys, env); err != nil {
		t.Fatalf("notify-claims-stall: %v\n%s", err, out)
	}

	// The stall's incident record cannot be removed once a directory stands
	// in its place.
	states, err := filepath.Glob(filepath.Join(work, "incidents", "*.state"))
	if err != nil || len(states) != 1 {
		t.Fatalf("incident records after the stall = %v (%v), want the stall's one", states, err)
	}
	if err := os.Remove(states[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(states[0], "pinned"), 0o755); err != nil {
		t.Fatal(err)
	}

	claimsStallEvents(t, work, []string{stalledEvent}, []string{resumedEvent})
	out, err := runPackScript(t, notifyClaimsStallScriptPath, binDir, escalationEnvKeys, env)
	if err == nil {
		t.Fatalf("an incident that did not clear exited 0:\n%s", out)
	}
	for _, want := range []string{"escalate: could not clear incident claims-stalled:", "could not resolve the stall incident of gascity/review-codex"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not say %q:\n%s", want, out)
		}
	}
	sends := claimsStallMailSends(t, logPath)
	if len(sends) != 2 || !strings.Contains(sends[1], "Agent gascity/review-codex claims its work again on cursor-grok") {
		t.Fatalf("sends = %q, want the stall, then the resumption note despite the failed resolve", sends)
	}
}

// TestNotifyClaimsStallFailsLoudWhenItCannotReadTheEvents: an event read that
// fails, or event output that does not decode, is not an empty window. A
// resumption may raise no later event to run the order again, so the run mails
// nothing, names the failure, passes the read's own error through and exits
// non-zero, which makes the controller log it. A window that holds no event is
// a clean run.
func TestNotifyClaimsStallFailsLoudWhenItCannotReadTheEvents(t *testing.T) {
	const readFails = `echo "simulated event API failure" >&2; exit 17`
	cases := []struct {
		name             string
		stalled, resumed string // the fake gc's answer to each read
		want             []string
	}{
		{"the stall read fails", readFails, "true", []string{"simulated event API failure", "could not read the agent.claims_stalled events"}},
		{"the resumption read fails", "", readFails, []string{"simulated event API failure", "could not read the agent.claims_resumed events"}},
		{"the output does not decode", `echo "event stream truncated {"`, "true", []string{"could not decode"}},
		{"the window is empty", "true", "true", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			claimsStallEvents(t, work, []string{stalledEvent}, nil)
			if tc.stalled == "" {
				tc.stalled = `cat "` + filepath.Join(work, "stalled.jsonl") + `"`
			}
			binDir, logPath := claimsStallGCReading(t, tc.stalled, tc.resumed, `echo "Sent message m-1 to human"`)

			out, err := runPackScript(t, notifyClaimsStallScriptPath, binDir, escalationEnvKeys, claimsStallEnv(work, coreEscalateScript(t)))

			if sends := claimsStallMailSends(t, logPath); len(sends) != 0 {
				t.Fatalf("sends = %q, want none", sends)
			}
			if tc.want == nil {
				if err != nil {
					t.Fatalf("an empty window failed: %v\n%s", err, out)
				}
				return
			}
			if err == nil {
				t.Fatalf("exited 0, want a failure:\n%s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Fatalf("output does not say %q:\n%s", want, out)
				}
			}
		})
	}
}

// Argument parsers an escalate.sh override written to the 1.5.0 contract,
// before incident keys, can have, as the reaper's tests of overrides name them
// (examples/gastown/maintenance_scope_test.go). Each is the shell an override
// runs for an argument it does not take, which it sees as $1.
const (
	// The 1.5.0 escalate.sh's own parser, which overrides were copied from.
	overrideRefusalV150 = `echo "escalate: unknown argument $1" >&2; exit 2`
	// A GNU getopt parser names the option it does not know and exits 1.
	overrideRefusalGetopt = `echo "escalate: unrecognized option '$1'" >&2; exit 1`
	// A usage message that names no flag, under the usage-error status.
	overrideRefusalUsage = `echo "usage: escalate.sh --subject S [--message M]" >&2; exit 2`
	// A lenient parser skips what it does not take.
	overrideSkipsUnknown = `shift`
)

// What an override does once it has read its arguments.
const (
	overrideDelivers  = `printf 'SENT %s\n' "$subject" >> "$ESCALATE_OVERRIDE_LOG"`
	overrideSendFails = `echo "escalate: pager unreachable" >&2; exit 1`
)

// writeEscalateOverride installs an escalate.sh override in a run's env and
// returns its path and log. The override takes the 1.5.0 contract's flags
// (--subject, --message, --severity), runs refusal for any other argument,
// then runs deliver. Its log holds a "CALL <flags>" line per call and a
// "SENT <subject>" line per delivered alert.
func writeEscalateOverride(t *testing.T, env []string, refusal, deliver string) (outEnv []string, script, log string) {
	t.Helper()
	dir := t.TempDir()
	script = filepath.Join(dir, "escalate.sh")
	log = filepath.Join(dir, "escalate.log")
	body := `#!/usr/bin/env bash
flags=""
for arg in "$@"; do
    case "$arg" in --*) flags="$flags $arg" ;; esac
done
printf 'CALL%s\n' "$flags" >> "$ESCALATE_OVERRIDE_LOG"
subject=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        --subject) subject="$2"; shift 2 ;;
        --message|-m|--severity) shift 2 ;;
        *) ` + refusal + ` ;;
    esac
done
` + deliver + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return append(slices.Clone(env), "GC_ESCALATE_SCRIPT="+script, "ESCALATE_OVERRIDE_LOG="+log), script, log
}

// readOverrideLog is an override's log; empty when it was never called.
func readOverrideLog(t *testing.T, log string) string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

// TestNotifyClaimsStallAlertsThroughAnEscalateOverrideThatPredatesIncidentKeys
// covers a city whose escalate.sh override was written before incident keys
// and refuses --incident-key as a usage error. The stall is sent again in the
// 1.5.0 form, still HIGH, so it still arrives, undeduplicated, and the run says
// so in one line that names the override.
func TestNotifyClaimsStallAlertsThroughAnEscalateOverrideThatPredatesIncidentKeys(t *testing.T) {
	cases := []struct {
		name    string
		refusal string
	}{
		{"1.5.0 escalate.sh parser, exit 2", overrideRefusalV150},
		{"getopt parser naming the flag, exit 1", overrideRefusalGetopt},
		{"usage error naming no flag, exit 2", overrideRefusalUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			binDir, _ := claimsStallGC(t, work, `echo "Sent message m-1 to human"`)
			claimsStallEvents(t, work, []string{stalledEvent}, nil)
			env, script, log := writeEscalateOverride(t, claimsStallEnv(work, ""), tc.refusal, overrideDelivers)

			out, err := runPackScript(t, notifyClaimsStallScriptPath, binDir, escalationEnvKeys, env)
			if err != nil {
				t.Fatalf("notify-claims-stall: %v\n%s", err, out)
			}

			want := "CALL --subject --message --severity --incident-key --fingerprint\n" +
				"CALL --subject --message --severity\n" +
				"SENT Agent gascity/review-codex stopped claiming its work on cursor-grok\n"
			if got := readOverrideLog(t, log); got != want {
				t.Fatalf("override calls:\n%s\nwant the refused keyed call, then one HIGH call in the 1.5.0 form that delivers:\n%s", got, want)
			}
			if got := strings.Count(out, "predates incident keys"); got != 1 {
				t.Fatalf("%d warnings that the override predates incident keys, want 1:\n%s", got, out)
			}
			if !strings.Contains(out, script+" predates incident keys") {
				t.Fatalf("the warning must name the override:\n%s", out)
			}
		})
	}
}

// TestNotifyClaimsStallReportsAResumptionThroughAnEscalateOverrideThatPredatesIncidentKeys:
// such an override refuses the resumption's --resolve-incident too. It keeps no
// incidents, so there is nothing to clear: the note is still sent, in the 1.5.0
// form, and each refusal is reported in one line instead of its usage error.
func TestNotifyClaimsStallReportsAResumptionThroughAnEscalateOverrideThatPredatesIncidentKeys(t *testing.T) {
	work := t.TempDir()
	binDir, _ := claimsStallGC(t, work, `echo "Sent message m-1 to human"`)
	claimsStallEvents(t, work, []string{stalledEvent}, []string{resumedEvent})
	env, _, log := writeEscalateOverride(t, claimsStallEnv(work, ""), overrideRefusalV150, overrideDelivers)

	out, err := runPackScript(t, notifyClaimsStallScriptPath, binDir, escalationEnvKeys, env)
	if err != nil {
		t.Fatalf("notify-claims-stall: %v\n%s", err, out)
	}

	want := "CALL --resolve-incident\n" +
		"CALL --subject --message --incident-key --fingerprint\n" +
		"CALL --subject --message\n" +
		"SENT Agent gascity/review-codex claims its work again on cursor-grok\n"
	if got := readOverrideLog(t, log); got != want {
		t.Fatalf("override calls:\n%s\nwant the refused resolve, the refused keyed note, then the note in the 1.5.0 form:\n%s", got, want)
	}
	if got := strings.Count(out, "predates incident keys"); got != 2 {
		t.Fatalf("%d warnings that the override predates incident keys, want one per refusal:\n%s", got, out)
	}
	if strings.Contains(out, "unknown argument") {
		t.Fatalf("the override's usage error must be replaced by the warning:\n%s", out)
	}
}

// TestNotifyClaimsStallDoesNotResendWhenAnEscalateOverrideFailsToSend: an
// override that fails for a reason other than the incident flags is not sent
// the alert again; its own error reaches the order log and the run fails.
func TestNotifyClaimsStallDoesNotResendWhenAnEscalateOverrideFailsToSend(t *testing.T) {
	work := t.TempDir()
	binDir, _ := claimsStallGC(t, work, `echo "Sent message m-1 to human"`)
	claimsStallEvents(t, work, []string{stalledEvent}, nil)
	env, _, log := writeEscalateOverride(t, claimsStallEnv(work, ""), overrideSkipsUnknown, overrideSendFails)

	out, err := runPackScript(t, notifyClaimsStallScriptPath, binDir, escalationEnvKeys, env)
	if err == nil {
		t.Fatalf("a failed send exited 0:\n%s", out)
	}

	if got, want := readOverrideLog(t, log), "CALL --subject --message --severity --incident-key --fingerprint\n"; got != want {
		t.Fatalf("override calls:\n%s\nwant the one keyed call:\n%s", got, want)
	}
	if strings.Contains(out, "predates incident keys") {
		t.Fatalf("a failed send is not a refusal of the incident flags:\n%s", out)
	}
	for _, want := range []string{"escalate: pager unreachable", "could not escalate the stall of gascity/review-codex"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not say %q:\n%s", want, out)
		}
	}
}

// TestNotifyClaimsStallNeverResendsThroughItsOwnEscalateScript: the core
// escalate.sh takes the incident flags, so its failure, even under the
// usage-error status, is a failed send and the alert is not sent again
// without them.
func TestNotifyClaimsStallNeverResendsThroughItsOwnEscalateScript(t *testing.T) {
	work := t.TempDir()
	binDir, logPath := claimsStallGC(t, work, `exit 2`)
	claimsStallEvents(t, work, []string{stalledEvent}, nil)
	// No override and no installed packs: the script falls back to the
	// escalate.sh next to it.
	env := append(claimsStallEnv(work, ""), "GC_SYSTEM_PACKS_DIR="+filepath.Join(work, "packs"))

	out, err := runPackScript(t, notifyClaimsStallScriptPath, binDir, escalationEnvKeys, env)
	if err == nil {
		t.Fatalf("a failed send exited 0:\n%s", out)
	}

	if sends := claimsStallMailSends(t, logPath); len(sends) != 1 {
		t.Fatalf("sends = %q, want the one failed keyed escalation", sends)
	}
	if strings.Contains(out, "predates incident keys") {
		t.Fatalf("the core escalate.sh takes the incident flags:\n%s", out)
	}
}
