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

// fakeBdStatusSnapshots returns a fake bd whose ephemeral snapshots are
// status-keyed: the open rows answer only `bd query ... status=open`, the
// in-progress rows only `status=in_progress`. Every other verb answers [].
func fakeBdStatusSnapshots(openRows, inProgressRows string) string {
	return `#!/bin/sh
case "$1" in
  query)
    case "$*" in
      *"ephemeral=true AND status=open"*) printf '%s' '[` + openRows + `]' ;;
      *"ephemeral=true AND status=in_progress"*) printf '%s' '[` + inProgressRows + `]' ;;
      *) printf '[]' ;;
    esac
    ;;
  *) printf '[]' ;;
esac
`
}

// TestEffectiveWorkQueryBD104ServesAssignedTaskBehindOlderMailWisp pins
// AC-2: the assigned-ready tier sorts by created_at and truncates to one row,
// so an older mail wisp shadowed a newer assigned task outright. The message
// must be dropped BEFORE the `.[:1]` truncation — after it, the tier's only
// row is a bead the hook is certain to refuse (#4419) and the task stays
// unservable for the mail TTL.
func TestEffectiveWorkQueryBD104ServesAssignedTaskBehindOlderMailWisp(t *testing.T) {
	const taskRow = `{"id":"gcg-task-newer","status":"open","issue_type":"task","assignee":"gcg-session-5815","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:05:00Z","metadata":{}}`
	env := map[string]string{
		"GC_SESSION_ID":   "gcg-session-5815",
		"GC_SESSION_NAME": "gascity--run-operator",
	}

	for name, mail := range map[string]string{
		"message type": `{"id":"gcg-mail-older","status":"open","issue_type":"message","assignee":"gcg-session-5815","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`,
		// The clause normalizes the type the way the hook does
		// (hookClaimCandidateIsMessage: TrimSpace + EqualFold), so casing and
		// padding cannot smuggle a message past the query layer.
		"cased type":   `{"id":"gcg-mail-older","status":"open","issue_type":"Message","assignee":"gcg-session-5815","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`,
		"padded type":  `{"id":"gcg-mail-older","status":"open","issue_type":" message ","assignee":"gcg-session-5815","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`,
		"legacy .type": `{"id":"gcg-mail-older","status":"open","type":"message","assignee":"gcg-session-5815","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			a := singleSessionConfigAgent()
			out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env,
				fakeBdStatusSnapshots(mail+`,`+taskRow, ""))
			if strings.Contains(out, "gcg-mail-older") || !strings.Contains(out, "gcg-task-newer") {
				t.Fatalf("assigned-ready tier let a mail wisp shadow the assigned task: served %q, want gcg-task-newer and no message bead", out)
			}
		})
	}

	t.Run("non-string issue_type keeps the tier intact", func(t *testing.T) {
		// A non-string type must not make the jq clause error out: these
		// filters discard jq's stderr and an error reads as an empty tier,
		// so a servable row would be dropped along with the mail.
		a := singleSessionConfigAgent()
		numeric := strings.Replace(taskRow, `"issue_type":"task"`, `"issue_type":5`, 1)
		mail := `{"id":"gcg-mail-older","status":"open","issue_type":"message","assignee":"gcg-session-5815","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
		out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env,
			fakeBdStatusSnapshots(mail+`,`+numeric, ""))
		if strings.Contains(out, "gcg-mail-older") || !strings.Contains(out, "gcg-task-newer") {
			t.Fatalf("non-string issue_type broke the tier (or served mail): served %q, want gcg-task-newer and no message bead", out)
		}
	})

	t.Run("slow-path mail reply is withheld", func(t *testing.T) {
		// A dependency_count > 0 message cannot hide behind the fast path's
		// dependency gate: the slow filter surfaces it for bd-show enrichment,
		// and the enrichment (`bd show` of an unrelated candidate) would
		// serve it and exit before the routed tier runs.
		a := singleSessionConfigAgent()
		reply := `{"id":"gcg-mail-reply","status":"open","issue_type":"message","assignee":"gcg-session-5815","ephemeral":true,"dependency_count":2,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
		routed := `{"id":"gcg-routed-wisp","status":"open","issue_type":"task","assignee":"","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:05:00Z","metadata":{"gc.routed_to":"` + a.QualifiedName() + `"}}`
		out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env,
			fakeBdStatusSnapshots(reply+`,`+routed, ""))
		if strings.Contains(out, "gcg-mail-reply") || !strings.Contains(out, "gcg-routed-wisp") {
			t.Fatalf("slow-path mail reply starved routed work: served %q, want gcg-routed-wisp and no message bead", out)
		}
	})
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
