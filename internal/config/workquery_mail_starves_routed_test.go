package config

import (
	"strings"
	"testing"
)

// Regression coverage for ga-gspkop (#5815): a read, still-open mail bead is
// ephemeral and assigned to its recipient, so the bd-1.0.4 assigned wisp probe
// of the generated work query served it as the tier's only row and exited
// before the routed tier ran. The hook then refuses the message (#4419) and
// drains no_work while routed work sits unqueried below. The query layer must
// withhold message rows before each tier's truncation.

const (
	mailTestSessionID   = "gcg-session-5815"
	mailTestSessionName = "gascity--run-operator"
)

func mailTestEnv() map[string]string {
	return map[string]string{
		"GC_SESSION_ID":   mailTestSessionID,
		"GC_SESSION_NAME": mailTestSessionName,
	}
}

// fakeBdEphemeralStore returns a fake bd holding rows (a comma-separated list
// of JSON beads) in its ephemeral tier: `bd query` answers each
// `ephemeral=true AND status=<s>` snapshot with the rows whose status is s,
// and every other verb (ready, list, show) prints `[]`, since pre-1.0.5
// `bd ready` never returns ephemeral rows. Mail beads are ephemeral and
// assigned to their recipient (internal/mail/beadmail), so they live here.
func fakeBdEphemeralStore(rows string) string {
	return `#!/bin/sh
case "$1" in
  query)
    case "$*" in
      *"status=in_progress"*) s=in_progress ;;
      *"status=open"*) s=open ;;
      *) printf '[]'; exit 0 ;;
    esac
    printf '%s' '[` + rows + `]' | jq -c --arg s "$s" '[.[] | select(.status == $s)]'
    ;;
  *) printf '[]' ;;
esac
`
}

// AC-1: an open mail wisp assigned to the seat must not short-circuit the
// routed tier, whichever of the seat's identities it is addressed to.
func TestEffectiveWorkQueryOpenMailDoesNotStarveRoutedWisp(t *testing.T) {
	requireJQ(t)
	a := singleSessionConfigAgent()
	routed := `{"id":"gcg-routed-wisp","status":"open","issue_type":"task","assignee":"","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:05:00Z","metadata":{"gc.routed_to":"` + a.QualifiedName() + `"}}`
	query := a.EffectiveWorkQueryFor(QueryTopology{})

	control := runShellWithFakeBd(t, query, mailTestEnv(), fakeBdEphemeralStore(routed))
	if !strings.Contains(control, "gcg-routed-wisp") {
		t.Fatalf("control: routed wisp not served with no mail open: %q", control)
	}

	for _, recipient := range []string{mailTestSessionID, mailTestSessionName} {
		t.Run("recipient="+recipient, func(t *testing.T) {
			mail := `{"id":"gcg-mail-1","status":"open","issue_type":"message","assignee":"` + recipient + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
			out := runShellWithFakeBd(t, query, mailTestEnv(), fakeBdEphemeralStore(mail+","+routed))
			if strings.Contains(out, "gcg-mail-1") || !strings.Contains(out, "gcg-routed-wisp") {
				t.Fatalf("open mail bead starved routed work: work query served %q, want the routed wisp gcg-routed-wisp and no message bead", out)
			}
		})
	}
}

// AC-2: an older open mail wisp must not hide a newer assigned wisp task from
// the assigned-ready tier. The type is matched the way the hook matches it:
// trimmed, case-folded, and read from the legacy "type" key when
// "issue_type" is absent.
func TestEffectiveWorkQueryBD104ServesAssignedTaskBehindOlderMailWisp(t *testing.T) {
	requireJQ(t)
	a := singleSessionConfigAgent()
	task := `{"id":"gcg-assigned-task","status":"open","issue_type":"task","assignee":"` + mailTestSessionID + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:05:00Z","metadata":{}}`
	for _, tc := range []struct {
		name    string
		typeKey string
	}{
		{"issue_type=message", `"issue_type":"message"`},
		{"issue_type=Message", `"issue_type":"Message"`},
		{"issue_type padded", `"issue_type":" message "`},
		{"legacy type key", `"type":"message"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mail := `{"id":"gcg-mail-old","status":"open",` + tc.typeKey + `,"assignee":"` + mailTestSessionID + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
			out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), mailTestEnv(), fakeBdEphemeralStore(mail+","+task))
			if strings.Contains(out, "gcg-mail-old") || !strings.Contains(out, "gcg-assigned-task") {
				t.Fatalf("assigned-ready tier served %q, want the assigned task gcg-assigned-task and no message bead", out)
			}
		})
	}
}

// AC-3: an in_progress mail wisp must not occupy the in_progress
// crash-recovery slot, on either single-store compatibility mode.
func TestEffectiveWorkQueryInProgressSkipsAssignedMailWisp(t *testing.T) {
	requireJQ(t)
	a := singleSessionConfigAgent()
	mail := `{"id":"gcg-mail-claimed","status":"in_progress","issue_type":"message","assignee":"` + mailTestSessionID + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
	task := `{"id":"gcg-progress-task","status":"in_progress","issue_type":"task","assignee":"` + mailTestSessionID + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:05:00Z","metadata":{}}`
	for _, tc := range []struct {
		name string
		topo QueryTopology
	}{
		{"bd-1.0.4", QueryTopology{}},
		{"bd-1.0.5", QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(tc.topo), mailTestEnv(), fakeBdEphemeralStore(mail+","+task))
			if strings.Contains(out, "gcg-mail-claimed") || !strings.Contains(out, "gcg-progress-task") {
				t.Fatalf("in_progress tier served %q, want the in_progress task gcg-progress-task and no message bead", out)
			}
		})
	}
}

// AC-4: a seat whose only open wisp is mail has an empty frontier.
func TestEffectiveWorkQueryMailOnlyStoreYieldsEmptyFrontier(t *testing.T) {
	requireJQ(t)
	a := singleSessionConfigAgent()
	mail := `{"id":"gcg-mail-1","status":"open","issue_type":"message","assignee":"` + mailTestSessionID + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
	out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), mailTestEnv(), fakeBdEphemeralStore(mail))
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("mail-only store: work query printed %q, want an empty frontier []", out)
	}
}
