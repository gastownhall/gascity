package config

import (
	"os/exec"
	"strings"
	"testing"
)

// Regression coverage for ga-gspkop (#5815): a read, still-open mail bead is
// ephemeral and assigned to its recipient, so the assigned tiers of the
// generated work query serve it and exit before the routed tier runs. The hook
// then refuses the message (#4419) and drains no_work while routed work sits
// unqueried below.

// fakeBdOpenMailAndRoutedWisp returns a fake bd (pre-1.0.5 ready semantics:
// `bd ready` never returns ephemeral rows) whose open ephemeral tier holds a
// message bead assigned to sessionID and, when withMail is false, nothing but
// an unassigned wisp task routed to target.
func fakeBdOpenMailAndRoutedWisp(sessionID, target string, withMail bool) string {
	rows := `{"id":"gcg-routed-wisp","status":"open","issue_type":"task","assignee":"","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:05:00Z","metadata":{"gc.routed_to":"` + target + `"}}`
	if withMail {
		rows = `{"id":"gcg-mail-1","status":"open","issue_type":"message","assignee":"` + sessionID + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}},` + rows
	}
	return `#!/bin/sh
case "$1" in
  query) printf '%s' '[` + rows + `]' ;;
  *) printf '[]' ;;
esac
`
}

func TestEffectiveWorkQueryOpenMailDoesNotStarveRoutedWisp(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	a := singleSessionConfigAgent()
	env := map[string]string{
		"GC_SESSION_ID":   "gcg-session-5815",
		"GC_SESSION_NAME": "gascity--run-operator",
	}

	// Control: with no mail open, the fake's routed wisp is served.
	control := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env,
		fakeBdOpenMailAndRoutedWisp(env["GC_SESSION_ID"], a.QualifiedName(), false))
	if !strings.Contains(control, "gcg-routed-wisp") {
		t.Fatalf("control: routed wisp not served with no mail open: %q", control)
	}

	out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env,
		fakeBdOpenMailAndRoutedWisp(env["GC_SESSION_ID"], a.QualifiedName(), true))
	if strings.Contains(out, "gcg-mail-1") || !strings.Contains(out, "gcg-routed-wisp") {
		t.Fatalf("open mail bead starved routed work: work query served %q, want the routed wisp gcg-routed-wisp and no message bead", out)
	}
}

// TestEffectiveWorkQueryReadyReadsDoNotExcludeMessageType pins the fix's
// boundary from the other side: mail is withheld at the jq ephemeral probes,
// so NO generated `bd ready` / `gc ready` read may grow an
// --exclude-type=message flag. The ready path already excludes infra types
// natively (readyExcludeTypes; bd's own ready policy), so the flag would
// churn every deployed command without changing what a seat is served.
func TestEffectiveWorkQueryReadyReadsDoNotExcludeMessageType(t *testing.T) {
	a := singleSessionConfigAgent()
	for name, topo := range map[string]QueryTopology{
		"bd104":     {},
		"bd105":     {Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}},
		"federated": {FederatedReady: true},
	} {
		got := a.EffectiveWorkQueryFor(topo)
		if strings.Contains(got, "--exclude-type=message") {
			t.Errorf("%s: generated work query renders --exclude-type=message on a ready read: %q", name, got)
		}
	}
}
