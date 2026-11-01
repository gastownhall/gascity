package core

import (
	"os"
	"path/filepath"
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
	resumedEvent = `{"seq":180,"type":"agent.claims_resumed","subject":"gascity/review-codex","message":"Agent gascity/review-codex claims its work again on cursor-grok.","payload":{"template":"gascity/review-codex","provider":"cursor-grok","episode":"gascity/review-codex@cursor-grok@2026-10-31T07:16:00Z","seats":5,"since":"2026-10-31T07:16:00Z"}}`
)

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
	binDir, logPath := fakeGCBin(t, `case "$1 $2 $3" in
"events --type agent.claims_stalled") cat "`+filepath.Join(work, "stalled.jsonl")+`" ;;
"events --type agent.claims_resumed") cat "`+filepath.Join(work, "resumed.jsonl")+`" ;;
"mail send human") echo "Sent message m-1 to human" ;;
esac
`)
	escalate, err := filepath.Abs(escalateScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	env := []string{
		"GC_ESCALATE_SCRIPT=" + escalate,
		"GC_ESCALATE_INCIDENT_DIR=" + filepath.Join(work, "incidents"),
		"GC_CITY_PATH=" + work,
	}
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
	binDir, _ := fakeGCBin(t, `case "$1 $2 $3" in
"events --type agent.claims_stalled") cat "`+filepath.Join(work, "stalled.jsonl")+`" ;;
"events --type agent.claims_resumed") cat "`+filepath.Join(work, "resumed.jsonl")+`" ;;
"mail send human") echo "store unavailable" >&2; exit 1 ;;
esac
`)
	escalate, err := filepath.Abs(escalateScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	claimsStallEvents(t, work, []string{stalledEvent}, nil)
	out, err := runPackScript(t, notifyClaimsStallScriptPath, binDir, escalationEnvKeys, []string{
		"GC_ESCALATE_SCRIPT=" + escalate,
		"GC_ESCALATE_INCIDENT_DIR=" + filepath.Join(work, "incidents"),
		"GC_CITY_PATH=" + work,
	})
	if err == nil {
		t.Fatalf("an undeliverable stall mail exited 0:\n%s", out)
	}
	if !strings.Contains(out, "could not escalate the stall of gascity/review-codex") {
		t.Fatalf("output %q, want the failure named", out)
	}
}
