package config

import (
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

// fakeBdEphemeralSnapshots returns a fake bd whose `bd query` answers the open
// and in_progress ephemeral snapshots with the given JSON arrays and whose
// every other verb (ready, list, show) answers `[]`, so the ephemeral jq
// probes are the only tiers that can serve a row.
func fakeBdEphemeralSnapshots(openRows, inProgressRows string) string {
	return `#!/bin/sh
case "$1" in
  query)
    case "$*" in
      *"status=in_progress"*) printf '%s' '[` + inProgressRows + `]' ;;
      *"status=open"*) printf '%s' '[` + openRows + `]' ;;
      *) printf '[]' ;;
    esac
    ;;
  *) printf '[]' ;;
esac
`
}

func mailStarvationSessionEnv() map[string]string {
	return map[string]string{
		"GC_SESSION_ID":   "gcg-session-5815",
		"GC_SESSION_NAME": "gascity--run-operator",
	}
}

func TestEffectiveWorkQueryOpenMailDoesNotStarveRoutedWisp(t *testing.T) {
	requireJQ(t)
	a := singleSessionConfigAgent()
	env := mailStarvationSessionEnv()

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

// TestEffectiveWorkQueryOpenMailToEveryIdentityDoesNotStarveRoutedWisp covers
// each identity the assigned tiers loop over: mail addressed to the session
// name or the claim alias must be skipped exactly like mail to the session id.
func TestEffectiveWorkQueryOpenMailToEveryIdentityDoesNotStarveRoutedWisp(t *testing.T) {
	requireJQ(t)
	a := singleSessionConfigAgent()
	env := mailStarvationSessionEnv()
	env["GC_ALIAS"] = a.QualifiedName()
	routed := `{"id":"gcg-routed-wisp","status":"open","issue_type":"task","assignee":"","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:05:00Z","metadata":{"gc.routed_to":"` + a.QualifiedName() + `"}}`

	for _, key := range []string{"GC_SESSION_ID", "GC_SESSION_NAME", "GC_ALIAS"} {
		t.Run(key, func(t *testing.T) {
			mail := `{"id":"gcg-mail-1","status":"open","issue_type":"message","assignee":"` + env[key] + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
			out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env,
				fakeBdEphemeralSnapshots(mail+","+routed, ""))
			if strings.Contains(out, "gcg-mail-1") || !strings.Contains(out, "gcg-routed-wisp") {
				t.Fatalf("mail to %s starved routed work: work query served %q, want gcg-routed-wisp", key, out)
			}
		})
	}
}
