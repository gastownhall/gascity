package config

import (
	"strings"
	"testing"
)

// This file pins the query-layer half of the #4419 contract: mail is read,
// not claimed as work. #4419 taught `gc hook --claim` to skip
// issue_type="message" candidates, but the generated assigned-ready reads
// could still SERVE a mail bead as the tier's only row and exit the script
// before the routed-pool tier ever ran. The hook's Go-side filter then
// stripped the message and reported no_work while real routed work sat
// unqueried below — the same starvation shape as the held-wisp bug
// (gas-kg6, #5114), wearing a mail bead instead of a hold label.
//
// Live incident (2026-08-31): an on_demand agent with a read mail wisp
// awaiting its GC sweep reported "no claimable work" for a full mail-TTL
// hour while an order-dispatched wisp routed to it sat ready.

// assignedMessageStarvationFixture models the incident store: a read mail
// wisp addressed to the agent (assignee = recipient, the #4419 shape) created
// BEFORE a real task wisp assigned to the same identity. Field shape mirrors
// `bd query --json` rows (dependency_count scalar, no dependencies array).
const assignedMessageStarvationFixture = `[
  {
    "id": "ga-wisp-mail",
    "title": "memory proposal: something to arbitrate",
    "status": "open",
    "priority": 2,
    "issue_type": "message",
    "assignee": "hindsight.archivist",
    "created_at": "2026-08-31T02:15:22Z",
    "metadata": {"mail.read": "true"},
    "labels": ["read"],
    "ephemeral": true,
    "dependency_count": 0
  },
  {
    "id": "ga-wisp-task",
    "title": "real assigned work",
    "status": "open",
    "priority": 2,
    "issue_type": "task",
    "assignee": "hindsight.archivist",
    "created_at": "2026-08-31T02:39:57Z",
    "ephemeral": true,
    "dependency_count": 0
  }
]`

// TestAssignedReadyTierCommandExcludesMessageBeads pins that every generated
// assigned-ready `bd ready`/`gc ready` read carries --exclude-type=message.
// Recent bd excludes messages from ready natively; the explicit flag keeps
// the read safe on older bd versions (bd >= 1.0.4 parses --exclude-type —
// the routed pool tier has leaned on --exclude-type=epic there all along)
// and keeps the serving contract visible in the query itself.
func TestAssignedReadyTierCommandExcludesMessageBeads(t *testing.T) {
	topologies := map[string]QueryTopology{
		"bd104":           {},
		"bd105":           {Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}},
		"bd104_federated": {FederatedReady: true},
		"bd105_federated": {FederatedReady: true, Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}},
	}
	for name, topo := range topologies {
		got := assignedReadyTierCommand("id", topo)
		if !strings.Contains(got, "--exclude-type=message") {
			t.Errorf("assignedReadyTierCommand(%s) = %q, missing --exclude-type=message: "+
				"a mail bead addressed to this identity satisfies the assignee-scoped "+
				"ready read and, at --limit=1, consumes the tier's only row", name, got)
		}
	}
}

// TestEphemeralAssignedReadyProbeExcludesMessageBeads pins the jq behavior of
// the bd-1.0.4 wisp tier: a message wisp addressed to the identity must not
// be served, and — because the exclusion runs before the `.[:1]` truncation —
// a real assigned wisp created after the mail must be served instead of being
// shadowed by it. Same filter-before-truncate reasoning as the held-wisp fix
// in ephemeralAssignedInProgressProbeScript (gas-kg6, #5114).
func TestEphemeralAssignedReadyProbeExcludesMessageBeads(t *testing.T) {
	filter := legacyEphemeralReadyFilterJQ(assignedReadySelectorJQ(), 1, false)

	got := runJQFilter(t, filter, assignedMessageStarvationFixture, "--arg", "id", "hindsight.archivist")

	if strings.Contains(got, "ga-wisp-mail") {
		t.Errorf("ephemeral assigned-ready probe served a mail message bead as work:\n%s\n\n"+
			"mail is read, not claimed (#4419); serving it here short-circuits the "+
			"work query before the routed-pool tier runs", got)
	}
	if !strings.Contains(got, "ga-wisp-task") {
		t.Errorf("ephemeral assigned-ready probe did not serve the real assigned wisp "+
			"waiting behind the mail bead:\nwant ga-wisp-task, got: %s", got)
	}
}

// TestEphemeralAssignedReadyDependencyProbeExcludesMessageBeads applies the
// same pin to the slow-path (dependency_count > 0) candidate filter, so a
// message bead can never ride the enrichment branch into the serve slot
// either.
func TestEphemeralAssignedReadyDependencyProbeExcludesMessageBeads(t *testing.T) {
	fixture := strings.ReplaceAll(assignedMessageStarvationFixture, `"dependency_count": 0`, `"dependency_count": 1`)
	filter := ephemeralReadyDependencyCandidateFilterJQ(assignedReadySelectorJQ(), 1, false)

	got := runJQFilter(t, filter, fixture, "--arg", "id", "hindsight.archivist")

	if strings.Contains(got, "ga-wisp-mail") {
		t.Errorf("ephemeral assigned-ready dependency probe served a mail message bead:\n%s", got)
	}
	if !strings.Contains(got, "ga-wisp-task") {
		t.Errorf("ephemeral assigned-ready dependency probe did not serve the real "+
			"assigned wisp behind the mail bead:\nwant ga-wisp-task, got: %s", got)
	}
}

// TestEphemeralAssignedReadyProbeScriptStillHoldTransparent re-asserts the
// ga-5736js boundary alongside the message exclusion: excluding mail from the
// assigned-ready probe must not sneak in a hold-label filter — a held
// assignment still keeps its owner visible to demand and recovery accounting.
func TestEphemeralAssignedReadyProbeScriptStillHoldTransparent(t *testing.T) {
	got := ephemeralAssignedReadyProbeScript("cand", QueryTopology{})
	if !strings.Contains(got, `"message"`) {
		t.Errorf("ephemeralAssignedReadyProbeScript() = %q, missing the message-type exclusion", got)
	}
	if strings.Contains(got, "--exclude-label") || strings.Contains(got, ".labels") {
		t.Errorf("ephemeralAssignedReadyProbeScript() = %q, assignee-scoped tier must stay hold-transparent", got)
	}
}
