package config

import (
	"regexp"
	"slices"
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

	mail := `{"id":"gcg-mail-old","status":"open","issue_type":"message","assignee":"` + mailTestSessionID + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`

	// A non-string type must not make the jq clause error out: these filters
	// discard jq's stderr and an error reads as an empty tier, so a servable
	// row would be dropped along with the mail.
	t.Run("non-string issue_type keeps the tier intact", func(t *testing.T) {
		numeric := strings.Replace(task, `"issue_type":"task"`, `"issue_type":5`, 1)
		out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), mailTestEnv(), fakeBdEphemeralStore(mail+","+numeric))
		if strings.Contains(out, "gcg-mail-old") || !strings.Contains(out, "gcg-assigned-task") {
			t.Fatalf("non-string issue_type broke the tier (or served mail): served %q, want gcg-assigned-task and no message bead", out)
		}
	})

	// A dependency_count > 0 message cannot hide behind the fast path's
	// dependency gate: the slow filter surfaces it for bd show enrichment,
	// which would serve it and exit before the routed tier runs.
	t.Run("slow-path mail reply is withheld", func(t *testing.T) {
		reply := `{"id":"gcg-mail-reply","status":"open","issue_type":"message","assignee":"` + mailTestSessionID + `","ephemeral":true,"dependency_count":2,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
		routed := `{"id":"gcg-routed-wisp","status":"open","issue_type":"task","assignee":"","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:05:00Z","metadata":{"gc.routed_to":"` + a.QualifiedName() + `"}}`
		out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), mailTestEnv(), fakeBdEphemeralStore(reply+","+routed))
		if strings.Contains(out, "gcg-mail-reply") || !strings.Contains(out, "gcg-routed-wisp") {
			t.Fatalf("slow-path mail reply starved routed work: served %q, want gcg-routed-wisp and no message bead", out)
		}
	})
}

// AC-3: an in_progress mail wisp must not occupy the in_progress
// crash-recovery slot, on either single-store compatibility mode, in the
// combined work query or the standalone assigned_in_progress_query. The split
// city's federated tier is TestFederatedInProgressTierSkipsAssignedMailWisp.
func TestEffectiveWorkQueryInProgressSkipsAssignedMailWisp(t *testing.T) {
	requireJQ(t)
	a := singleSessionConfigAgent()
	mail := `{"id":"gcg-mail-claimed","status":"in_progress","issue_type":"message","assignee":"` + mailTestSessionID + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
	task := `{"id":"gcg-progress-task","status":"in_progress","issue_type":"task","assignee":"` + mailTestSessionID + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:05:00Z","metadata":{}}`
	// The combined work query defers an assigned graph workflow root and reads
	// the snapshot again, 20 rows deep, for an ordinary claim to serve first;
	// the mail behind the root must not be that claim.
	anchor := `{"id":"gcg-graph-root","status":"in_progress","assignee":"` + mailTestSessionID + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T09:55:00Z","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2"}}`
	for _, tc := range []struct {
		name string
		topo QueryTopology
	}{
		{"bd-1.0.4", QueryTopology{}},
		{"bd-1.0.5", QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}}},
	} {
		for _, q := range []struct {
			name    string
			command string
		}{
			{"work_query", a.EffectiveWorkQueryFor(tc.topo)},
			{"assigned_in_progress_query", a.EffectiveAssignedInProgressQueryFor(tc.topo)},
		} {
			t.Run(tc.name+"/"+q.name, func(t *testing.T) {
				out := runShellWithFakeBd(t, q.command, mailTestEnv(), fakeBdEphemeralStore(mail+","+task))
				if strings.Contains(out, "gcg-mail-claimed") || !strings.Contains(out, "gcg-progress-task") {
					t.Fatalf("in_progress tier served %q, want the in_progress task gcg-progress-task and no message bead", out)
				}
			})
		}
		t.Run(tc.name+"/work_query with mail behind a graph anchor", func(t *testing.T) {
			out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(tc.topo), mailTestEnv(), fakeBdEphemeralStore(anchor+","+mail+","+task))
			if strings.Contains(out, "gcg-mail-claimed") || !strings.Contains(out, "gcg-progress-task") {
				t.Fatalf("work query served %q behind the graph anchor, want the in_progress task gcg-progress-task and no message bead", out)
			}
		})
	}
}

// AC-3 on a split city, where `gc hook` runs the federated work query as its
// only claim read. Its crash-recovery tier reads `gc ready --status
// in_progress`, a list across both storage tiers that holds the session's mail
// wisps, ended at its first unblocked row. A claimed mail wisp has no blocker
// and sorts ahead of a newer claim at the same priority, so the read itself
// must drop it: a filter on the read's answer comes after the cut, when the
// answer already ends at the mail and the claim behind it was never read.
// fakeGCInProgressWindow answers the way the real reader does, cut included.
func TestFederatedInProgressTierSkipsAssignedMailWisp(t *testing.T) {
	requireJQ(t)
	const mailID = "gcg-mail-claimed"
	mail := `{"id":"` + mailID + `","status":"in_progress","issue_type":"message","assignee":"sess-1"}`
	for _, topo := range []struct {
		name string
		topo QueryTopology
	}{
		{"bd-1.0.4", QueryTopology{FederatedReady: true}},
		{"bd-1.0.5", federatedTopology()},
	} {
		for _, q := range federatedInProgressQueriesFor(topo.topo) {
			for _, tc := range []struct {
				name string
				rows string
				want []string
				// anchored marks a window led by an assigned graph root, which
				// the standalone assigned_in_progress_query serves itself.
				anchored bool
			}{
				{name: "mail ahead of a claim", rows: federatedInProgressRows(mail, federatedServableRow), want: []string{"gcs-servable"}},
				// The combined work query defers the root and reads the window
				// again, uncut, serving its first non-anchor row.
				{name: "mail ahead of a step behind a graph anchor", rows: federatedInProgressRows(federatedAnchorRow, mail, federatedServableRow), want: []string{"gcs-servable"}, anchored: true},
				{name: "mail only", rows: federatedInProgressRows(mail), want: []string{}},
			} {
				t.Run(topo.name+"/"+q.name+"/"+tc.name, func(t *testing.T) {
					want := tc.want
					if tc.anchored && q.servesAnchor {
						want = []string{"gc-root"}
					}
					if got := runFederatedInProgressWindow(t, q.command, tc.rows); !slices.Equal(got, want) {
						t.Fatalf("served %v, want %v and no message bead", got, want)
					}
				})
			}
			// The read matches the type exactly, so a padded or differently
			// cased spelling still reaches the tier, and still ends the window
			// there. The tier must fall through rather than serve a row the
			// hook refuses.
			t.Run(topo.name+"/"+q.name+"/differently spelled mail", func(t *testing.T) {
				variant := strings.Replace(mail, `"issue_type":"message"`, `"issue_type":" Message "`, 1)
				if got := runFederatedInProgressWindow(t, q.command, federatedInProgressRows(variant, federatedServableRow)); slices.Contains(got, mailID) {
					t.Fatalf("served %v, which includes the message bead %s the hook refuses", got, mailID)
				}
			})
		}
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

// The identity loop probes GC_SESSION_ID, GC_SESSION_NAME and GC_ALIAS, so a
// message addressed to the seat's alias identity must be skipped too.
func TestEffectiveWorkQuerySkipsMailAddressedToAliasIdentity(t *testing.T) {
	requireJQ(t)
	a := singleSessionConfigAgent()
	env := mailTestEnv()
	env["GC_ALIAS"] = a.QualifiedName()
	mail := `{"id":"gcg-mail-alias","status":"open","issue_type":"message","assignee":"` + a.QualifiedName() + `","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
	routed := `{"id":"gcg-routed-wisp","status":"open","issue_type":"task","assignee":"","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:05:00Z","metadata":{"gc.routed_to":"` + a.QualifiedName() + `"}}`
	out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env, fakeBdEphemeralStore(mail+","+routed))
	if strings.Contains(out, "gcg-mail-alias") || !strings.Contains(out, "gcg-routed-wisp") {
		t.Fatalf("mail to the alias identity starved routed work: served %q, want gcg-routed-wisp and no message bead", out)
	}
}

// The legacy control-dispatcher loop probes each identity's workflow-control
// candidate too, so a message addressed to that candidate identity must not
// exit the query ahead of the dispatcher's own routed work.
func TestEffectiveWorkQueryLegacyControlSkipsMailAddressedToCandidateIdentity(t *testing.T) {
	requireJQ(t)
	a := Agent{Name: ControlDispatcherAgentName, Dir: "gascity"}
	env := map[string]string{
		"GC_SESSION_NAME": "gascity--control-dispatcher",
		"GC_ALIAS":        "gascity/control-dispatcher",
	}
	mail := `{"id":"gcg-mail-legacy","status":"open","issue_type":"message","assignee":"gascity/workflow-control","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:00:00Z","metadata":{}}`
	routed := `{"id":"gcg-dispatch-wisp","status":"open","issue_type":"task","assignee":"","ephemeral":true,"dependency_count":0,"created_at":"2026-08-31T10:05:00Z","metadata":{"gc.routed_to":"gascity/control-dispatcher"}}`
	out := runShellWithFakeBd(t, a.EffectiveWorkQueryFor(QueryTopology{}), env, fakeBdEphemeralStore(mail+","+routed))
	if strings.Contains(out, "gcg-mail-legacy") || !strings.Contains(out, "gcg-dispatch-wisp") {
		t.Fatalf("mail to the legacy candidate identity starved dispatcher work: served %q, want gcg-dispatch-wisp and no message bead", out)
	}
}

// The ready path already excludes infra types natively (readyExcludeTypes; bd's
// own ready policy), so no generated `bd ready` / `gc ready` ready read may grow
// an --exclude-type=message flag: it would churn every deployed command for
// nothing. A `gc ready --status` read is a list, with no such exclusion, so it
// is not checked here: the federated in_progress read carries the flag
// (assignedInProgressRead).
func TestEffectiveWorkQueryReadyReadsDoNotExcludeMessageType(t *testing.T) {
	a := singleSessionConfigAgent()
	for name, topo := range map[string]QueryTopology{
		"bd104":     {},
		"bd105":     {Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}},
		"federated": {FederatedReady: true},
	} {
		readyReads := 0
		for _, read := range generatedReadyReaderReads.FindAllString(a.EffectiveWorkQueryFor(topo), -1) {
			if strings.Contains(read, " --status ") {
				continue
			}
			readyReads++
			if strings.Contains(read, "--exclude-type=message") {
				t.Errorf("%s: generated work query renders --exclude-type=message on a ready read: %q", name, read)
			}
		}
		if readyReads == 0 {
			t.Errorf("%s: found no ready read in the generated work query, so the check would pass vacuously", name)
		}
	}
}

// generatedReadyReaderReads matches each `bd ready` / `gc ready` read of a
// generated query: the command substitution holding it, up to its close.
var generatedReadyReaderReads = regexp.MustCompile(`\$\((?:bd|gc) ready [^)]*`)
