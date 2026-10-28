package config

import (
	"os/exec"
	"strings"
	"testing"
)

// Regression coverage for ga-gspkop (#5815): mail is read, not claimed as work
// (#4419), but a read, still-open mail bead is ephemeral and assigned to its
// recipient (internal/mail/beadmail), so it matches the generated work query's
// assigned tiers exactly like a pre-assigned candidate. A tier that served one
// exited the script before the routed tier ran, and the hook's #4419 Go-side
// filter (hookClaimCandidateIsMessage) then refused the only candidate in its
// batch — the hook drained no_work while routed work sat unqueried below, the
// same short-circuit starvation shape as the held-wisp bug (gas-kg6, #5114).
// The fix withholds message rows in the jq filters over the ephemeral
// snapshots, before each tier's truncation, matching what `bd ready` /
// Ready() already exclude natively; the Go-side check stays as the backstop
// for custom work_query overrides.

// fakeBdOpenEphemeralStore answers the generated work query's `bd query`
// ephemeral snapshot reads: the open snapshot holds openRows, the in_progress
// snapshot is empty, and every other verb is empty. That is pre-1.0.5 ready
// semantics — `bd ready` never returns ephemeral rows — so a wisp is
// discoverable only through the jq probes over these snapshots, which
// isolates the leak the live incident showed (#5815 repro).
func fakeBdOpenEphemeralStore(openRows string) string {
	return `#!/bin/sh
case "$1" in
  query)
    case "$*" in
      *"ephemeral=true AND status=open"*) printf '%s' '` + openRows + `' ;;
      *) printf '[]' ;;
    esac
    ;;
  *) printf '[]' ;;
esac
`
}

// TestEffectiveWorkQueryOpenMailDoesNotStarveRoutedWisp is the primary pin
// (AC-1): with an open mail bead in the seat's inbox, the assigned-ready tier
// must not serve it and exit — the routed wisp behind the probe must still be
// reached.
func TestEffectiveWorkQueryOpenMailDoesNotStarveRoutedWisp(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	a := singleSessionConfigAgent()
	env := map[string]string{
		"GC_SESSION_ID":   "gcg-session-5815",
		"GC_SESSION_NAME": "gascity--run-operator",
	}
	routedWisp := `{"id":"gcg-routed-wisp","status":"open","issue_type":"task","assignee":"","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:05:00Z","metadata":{"gc.routed_to":"` + a.QualifiedName() + `"}}`
	openMail := `{"id":"gcg-mail-1","status":"open","issue_type":"message","assignee":"` + env["GC_SESSION_ID"] + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`

	// Control: with no mail open, the fake's routed wisp is served.
	t.Run("control without mail serves the routed wisp", func(t *testing.T) {
		out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env,
			fakeBdOpenEphemeralStore(`[`+routedWisp+`]`))
		if !strings.Contains(out, "gcg-routed-wisp") {
			t.Fatalf("control: routed wisp not served with no mail open: %q", out)
		}
	})

	// The defect: an open, already-read mail bead is still an open wisp
	// assigned to the recipient until the sweep GCs it, so it rides the
	// assigned-ready probe and exits the script ahead of the routed tier.
	t.Run("open mail does not shadow routed work", func(t *testing.T) {
		out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env,
			fakeBdOpenEphemeralStore(`[`+openMail+`,`+routedWisp+`]`))
		if strings.Contains(out, "gcg-mail-1") || !strings.Contains(out, "gcg-routed-wisp") {
			t.Fatalf("open mail bead starved routed work: work query served %q, want the routed wisp gcg-routed-wisp and no message bead", out)
		}
	})

	// The identity loop probes GC_SESSION_ID, then GC_SESSION_NAME, then
	// GC_ALIAS: mail addressed to any of the seat's identities must be
	// withheld for every $id, not just the first.
	t.Run("mail addressed to a second identity is skipped", func(t *testing.T) {
		aliasMail := `{"id":"gcg-mail-2","status":"open","issue_type":"message","assignee":"` + env["GC_SESSION_NAME"] + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
		out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env,
			fakeBdOpenEphemeralStore(`[`+aliasMail+`,`+routedWisp+`]`))
		if strings.Contains(out, "gcg-mail-2") || !strings.Contains(out, "gcg-routed-wisp") {
			t.Fatalf("mail to a second identity starved routed work: %q", out)
		}
	})

	// The clause normalizes the type the way the hook's Go-side check does
	// (hookClaimCandidateIsMessage: TrimSpace + EqualFold), so a message
	// spelling the hook would refuse is never served by the query either.
	t.Run("capitalized and padded message types are withheld", func(t *testing.T) {
		capsMail := `{"id":"gcg-mail-3","status":"open","issue_type":"Message","assignee":"` + env["GC_SESSION_ID"] + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
		paddedMail := `{"id":"gcg-mail-4","status":"open","issue_type":" message ","assignee":"` + env["GC_SESSION_ID"] + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:01:00Z","metadata":{}}`
		out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env,
			fakeBdOpenEphemeralStore(`[`+capsMail+`,`+paddedMail+`,`+routedWisp+`]`))
		if strings.Contains(out, "gcg-mail-3") || strings.Contains(out, "gcg-mail-4") || !strings.Contains(out, "gcg-routed-wisp") {
			t.Fatalf("non-exact message spellings starved routed work: %q", out)
		}
	})

	// A legacy row that spells the type in `.type` instead of `.issue_type`
	// (the base selector reads both) is withheld too.
	t.Run("legacy .type spelling is withheld", func(t *testing.T) {
		legacyMail := `{"id":"gcg-mail-5","status":"open","type":"message","assignee":"` + env["GC_SESSION_ID"] + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
		out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env,
			fakeBdOpenEphemeralStore(`[`+legacyMail+`,`+routedWisp+`]`))
		if strings.Contains(out, "gcg-mail-5") || !strings.Contains(out, "gcg-routed-wisp") {
			t.Fatalf("legacy-typed message starved routed work: %q", out)
		}
	})

	// The clause must not error the filter on a non-string JSON type: jq
	// errors in these probes are swallowed by 2>/dev/null and read as an
	// empty tier, so a candidate with a numeric issue_type would silently
	// vanish. It must be served, not swallowed.
	t.Run("non-string issue_type does not empty the tier", func(t *testing.T) {
		numericType := `{"id":"gcg-numeric-type","status":"open","issue_type":42,"assignee":"` + env["GC_SESSION_ID"] + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
		out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env,
			fakeBdOpenEphemeralStore(`[`+numericType+`]`))
		if !strings.Contains(out, "gcg-numeric-type") {
			t.Fatalf("non-string issue_type was dropped by the message clause (jq error read as an empty tier): %q", out)
		}
	})

	// A mail reply carrying dependencies rides the slow-path filter and the
	// bd show enrichment, not the fast path — it must be withheld there too,
	// while a real dependent task still reaches the enrichment and is served.
	t.Run("slow-path reply with dependencies is withheld", func(t *testing.T) {
		reply := `{"id":"gcg-mail-reply","status":"open","issue_type":"message","assignee":"` + env["GC_SESSION_ID"] + `","ephemeral":true,"dependency_count":1,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
		dependentTask := `{"id":"gcg-blocked-task","status":"open","issue_type":"task","assignee":"` + env["GC_SESSION_ID"] + `","ephemeral":true,"dependency_count":1,"created_at":"2026-08-31T10:01:00Z","metadata":{}}`
		out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env,
			fakeBdOpenEphemeralStore(`[`+reply+`,`+dependentTask+`]`))
		if strings.Contains(out, "gcg-mail-reply") {
			t.Fatalf("dependent mail reply was served by the slow path: %q", out)
		}
		if !strings.Contains(out, "gcg-blocked-task") {
			t.Fatalf("dependent task was not served by the slow path: %q", out)
		}
	})

	// The control-dispatcher shape carries its own copy of the assigned-ready
	// loop (legacyControlAssignedReadyWorkQueryScript), so a fix applied to
	// only the standard loop would leave it jammed.
	t.Run("legacy control assigned-ready tier skips mail", func(t *testing.T) {
		script := legacyControlAssignedReadyWorkQueryScript(QueryTopology{}) + `printf "[]"`
		out := runShellWithFakeBd(t, script, map[string]string{
			"GC_SESSION_ID": "gcg-session-5815",
		}, fakeBdOpenEphemeralStore(`[`+openMail+`]`))
		if strings.Contains(out, "gcg-mail-1") {
			t.Fatalf("legacy-control assigned-ready tier served a mail bead: %q", out)
		}
	})
}

// TestEffectiveWorkQueryBD104ServesAssignedTaskBehindOlderMailWisp pins AC-2:
// the assigned-ready tier truncates to one row, so with an older mail wisp in
// front of a real assigned task, withholding the message is what lets the task
// be served rather than shadowed.
func TestEffectiveWorkQueryBD104ServesAssignedTaskBehindOlderMailWisp(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	a := singleSessionConfigAgent()
	env := map[string]string{"GC_SESSION_ID": "gcg-session-5815"}
	// Older created_at, so the tier's sort_by puts the mail first: at the
	// truncation it is the row served.
	mail := `{"id":"gcg-mail-1","status":"open","issue_type":"message","assignee":"` + env["GC_SESSION_ID"] + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
	task := `{"id":"gcg-assigned-task","status":"open","issue_type":"task","assignee":"` + env["GC_SESSION_ID"] + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:02:00Z","metadata":{}}`

	out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env,
		fakeBdOpenEphemeralStore(`[`+mail+`,`+task+`]`))
	if strings.Contains(out, "gcg-mail-1") || !strings.Contains(out, "gcg-assigned-task") {
		t.Fatalf("assigned-ready tier did not serve the task behind an older mail wisp: %q", out)
	}
}

// TestEffectiveWorkQueryReadyReadsDoNotExcludeMessageType pins the fix's
// scope: the message withholding lives in the jq probes over the ephemeral
// snapshots, never as --exclude-type flags on the generated ready reads. bd's
// ready path already excludes message rows natively (DefaultInfraTypes), and
// a flag would churn every deployed command without reaching the jq probe
// that served the mail bead (#5815). It must stay absent across all three
// topologies.
func TestEffectiveWorkQueryReadyReadsDoNotExcludeMessageType(t *testing.T) {
	a := singleSessionConfigAgent()
	topologies := []struct {
		name string
		topo QueryTopology
	}{
		{"bd-1.0.4", QueryTopology{}},
		{"bd-1.0.5", QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}}},
		{"federated", QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}, FederatedReady: true}},
	}
	for _, tt := range topologies {
		t.Run(tt.name, func(t *testing.T) {
			got := a.EffectiveWorkQueryFor(tt.topo)
			if strings.Contains(got, "--exclude-type=message") {
				t.Fatalf("generated ready read carries --exclude-type=message; the message exclusion belongs to the jq probes, not the ready flags: %q", got)
			}
		})
	}
}
