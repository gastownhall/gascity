package runproj

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// hex32SessionID is the shape this city's session bead ids take: a three-letter
// store prefix plus a "session-<32 hex>" body. The old sessionIDRe rejected both
// the 3-letter prefix and the 40-char body on the assignee-only path (ga-3cs9p).
const hex32SessionID = "gcg-session-0123456789abcdef0123456789abcdef"

// TestRunSessionLinkForAcceptsStorePrefixedSessionIDs is the ga-3cs9p regression
// for the NAME/assignee fallback path: a step whose only session reference is a
// gcg-session-<32hex> assignee (no durable gc.session_id stamp) must resolve to a
// link carrying that id, exactly as the stamped path already did.
func TestRunSessionLinkForAcceptsStorePrefixedSessionIDs(t *testing.T) {
	var emptyCtx runSessionLinkContext

	t.Run("assignee-only", func(t *testing.T) {
		bead := runSnapshotBead{assignee: hex32SessionID}
		link, ok := runSessionLinkFor(bead, "done", emptyCtx)
		if !ok {
			t.Fatalf("expected a link for assignee %q", hex32SessionID)
		}
		if link.SessionID != hex32SessionID {
			t.Errorf("sessionID = %q, want %q", link.SessionID, hex32SessionID)
		}
	})

	t.Run("legacy session_id metadata", func(t *testing.T) {
		bead := runSnapshotBead{metadata: map[string]string{"session_id": hex32SessionID}}
		link, ok := runSessionLinkFor(bead, "done", emptyCtx)
		if !ok || link.SessionID != hex32SessionID {
			t.Fatalf("link = {%q ok:%v}, want %q", link.SessionID, ok, hex32SessionID)
		}
	})

	t.Run("pool-qualified assignee suffix", func(t *testing.T) {
		bead := runSnapshotBead{assignee: "polecat-" + hex32SessionID}
		link, ok := runSessionLinkFor(bead, "done", emptyCtx)
		if !ok || link.SessionID != hex32SessionID {
			t.Fatalf("link = {%q ok:%v}, want suffix %q", link.SessionID, ok, hex32SessionID)
		}
	})

	t.Run("two-letter store prefix", func(t *testing.T) {
		bead := runSnapshotBead{assignee: "mc-s1"}
		link, ok := runSessionLinkFor(bead, "done", emptyCtx)
		if !ok || link.SessionID != "mc-s1" {
			t.Fatalf("link = {%q ok:%v}, want mc-s1", link.SessionID, ok)
		}
	})

	t.Run("still rejects pool names, bare handles, uppercase, and over-long bodies", func(t *testing.T) {
		for _, assignee := range []string{
			"polecat",
			"mystery-handle",
			"alpha__worker-1",
			"GCG-SESSION-0123456789ABCDEF0123456789ABCDEF",
			"gcg-" + strings.Repeat("a", 41),
			"a-1",
			"abcde-1",
		} {
			bead := runSnapshotBead{assignee: assignee}
			if link, ok := runSessionLinkFor(bead, "done", emptyCtx); ok {
				t.Errorf("assignee %q: expected no link, got %q", assignee, link.SessionID)
			}
		}
	})
}

// TestRunSessionsRetiredIndexedByIDOnly pins the retired-session index rule: a
// session resolved individually by durable id (a closed seat the live listing no
// longer carries) is reachable ONLY through its id. Its name and template are
// never indexed, because pool slot names are deterministic and REUSED — a byName
// hit on a recycled slot would attribute a step to the wrong closed session.
func TestRunSessionsRetiredIndexedByIDOnly(t *testing.T) {
	alias := "adopt-worker"
	retired := DashboardSession{ID: "gcg-session-retired", SessionName: "gc__worker-1", Template: "adopt", Alias: &alias}
	idx := buildRunSessionIndex(RunSessions{Retired: []DashboardSession{retired}})

	if got, ok := idx.byID["gcg-session-retired"]; !ok || got.ID != retired.ID {
		t.Fatalf("retired session must be indexed by id; byID = %v", idx.byID)
	}
	for _, key := range []string{"gc__worker-1", "adopt-worker"} {
		if _, ok := idx.byName[key]; ok {
			t.Errorf("retired session must NOT be indexed byName[%q]", key)
		}
	}
	if _, ok := idx.byTemplate["adopt"]; ok {
		t.Errorf("retired session must NOT be indexed byTemplate")
	}
}

// TestRunSessionsLiveWinsOverRetiredOnIDCollision: when a live session and a
// retired one share an id (a stale by-id fetch racing a session that is still
// open), the live listing is authoritative for display fields.
func TestRunSessionsLiveWinsOverRetiredOnIDCollision(t *testing.T) {
	live := DashboardSession{ID: "gcg-session-a", Title: "live title", State: "active", Running: true}
	stale := DashboardSession{ID: "gcg-session-a", Title: "stale title"}
	idx := buildRunSessionIndex(RunSessions{Live: []DashboardSession{live}, Retired: []DashboardSession{stale}})
	if got := idx.byID["gcg-session-a"].Title; got != "live title" {
		t.Fatalf("byID title = %q, want the live listing's %q", got, "live title")
	}
}

// TestRunSessionLinkForAdoptsRetiredDisplayFields is the user-visible outcome: a
// finished step stamped with a closed session's durable id renders that
// session's alias as the link name (not a bare id) once the session is supplied
// as a retired by-id resolution — on both the stamped and the assignee-only path.
func TestRunSessionLinkForAdoptsRetiredDisplayFields(t *testing.T) {
	alias := "adopt-worker"
	retired := DashboardSession{ID: hex32SessionID, Alias: &alias, SessionName: "gc__worker-1"}
	idx := buildRunSessionIndex(RunSessions{Retired: []DashboardSession{retired}})
	ctx := runSessionLinkContext{sessionIndex: &idx}

	t.Run("stamped", func(t *testing.T) {
		bead := runSnapshotBead{metadata: map[string]string{beadmeta.SessionIDMetadataKey: hex32SessionID}}
		link, ok := runSessionLinkFor(bead, "done", ctx)
		if !ok {
			t.Fatal("expected a link")
		}
		if link.SessionID != hex32SessionID || link.SessionName != "adopt-worker" {
			t.Fatalf("link = %+v, want id %q name %q", link, hex32SessionID, "adopt-worker")
		}
	})

	t.Run("assignee-only", func(t *testing.T) {
		bead := runSnapshotBead{assignee: hex32SessionID}
		link, ok := runSessionLinkFor(bead, "done", ctx)
		if !ok {
			t.Fatal("expected a link")
		}
		if link.SessionID != hex32SessionID || link.SessionName != "adopt-worker" {
			t.Fatalf("link = %+v, want id %q name %q", link, hex32SessionID, "adopt-worker")
		}
	})
}

// retiredFixtureBeads builds a graph.v2 run (root + steps) whose steps reference
// sessions in every way the link resolver understands, so the id extraction can
// be checked against the resolver's own acceptance rules.
func retiredFixtureBeads() []beads.Bead {
	root := beads.Bead{
		ID: "run1", Title: "mol-x", Status: "closed", Type: "molecule", Ref: "mol-x",
		Metadata: map[string]string{
			"gc.formula_contract": "graph.v2", "gc.kind": "run", "gc.formula": "mol-x",
			"gc.run_target": "rig:demo", "gc.root_store_ref": "rig:demo",
			"gc.scope_kind": "rig", "gc.scope_ref": "demo",
		},
	}
	step := func(id, stepID, status, assignee string, meta map[string]string) beads.Bead {
		m := map[string]string{"gc.kind": "step", "gc.root_bead_id": "run1", "gc.step_id": stepID, "gc.scope_ref": "demo"}
		for k, v := range meta {
			m[k] = v
		}
		return beads.Bead{ID: id, Title: stepID, Status: status, Type: "task", ParentID: "run1", Ref: "mol-x." + stepID, Assignee: assignee, Metadata: m}
	}
	return []beads.Bead{
		root,
		// Durable stamp (closed step).
		step("run1.1", "a", "closed", "", map[string]string{beadmeta.SessionIDMetadataKey: "gcg-session-aaaa"}),
		// Same stamp again on a retry: deduped.
		step("run1.2", "a", "closed", "", map[string]string{beadmeta.SessionIDMetadataKey: "gcg-session-aaaa", "gc.attempt": "2"}),
		// Assignee-only (this city's id shape).
		step("run1.3", "b", "closed", hex32SessionID, nil),
		// Pool-qualified assignee: the supervisor id suffix is what gets resolved.
		step("run1.4", "c", "in_progress", "polecat-gc-333573", nil),
		// Pending steps have no session yet: skipped even when stamped.
		step("run1.5", "d", "open", "", map[string]string{beadmeta.SessionIDMetadataKey: "gcg-session-pending"}),
		// Unresolvable handle: skipped.
		step("run1.6", "e", "closed", "mystery-handle", nil),
		// Stamp wins over a differing assignee on the same bead.
		step("run1.7", "f", "closed", "gc-ignored", map[string]string{"session_id": "mc-s9"}),
	}
}

// TestSessionIDsForSnapshot pins the id set the dashboard BFF resolves
// individually: every durable/derived session id the resolver would put on a
// link, deduped, in first-seen bead order, with pending steps and
// unresolvable handles excluded. It is exactly the set of ids a by-id lookup can
// turn from a bare link into a named one, so a lookup is never spent on an id the
// resolver would reject anyway.
func TestSessionIDsForSnapshot(t *testing.T) {
	snap, err := SnapshotForRun(retiredFixtureBeads(), "run1", 1, 100)
	if err != nil {
		t.Fatalf("SnapshotForRun: %v", err)
	}
	got := SessionIDsForSnapshot(snap)
	want := []string{"gcg-session-aaaa", hex32SessionID, "gc-333573", "mc-s9"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("SessionIDsForSnapshot = %v, want %v", got, want)
	}
}

// TestBuildRunDetailFromSnapshotEnrichesRetiredLinks drives the full detail build
// with a retired by-id resolution and checks the execution instances carry the
// retired session's display name, while a step whose id was not resolved keeps
// its correct bare-id link (never dropped).
func TestBuildRunDetailFromSnapshotEnrichesRetiredLinks(t *testing.T) {
	snap, err := SnapshotForRun(retiredFixtureBeads(), "run1", 1, 100)
	if err != nil {
		t.Fatalf("SnapshotForRun: %v", err)
	}
	alias := "adopt-worker"
	sessions := RunSessions{
		Retired: []DashboardSession{{ID: hex32SessionID, Alias: &alias}},
	}
	detail, err := BuildRunDetailFromSnapshot(snap, sessions, nil, FormulaDetailUpstreamError)
	if err != nil {
		t.Fatalf("BuildRunDetailFromSnapshot: %v", err)
	}
	linkByBead := map[string]RunSessionAttachment{}
	for _, node := range detail.Nodes {
		for _, inst := range node.ExecutionInstances {
			linkByBead[inst.BeadID] = inst.Session
		}
	}
	enriched := linkByBead["run1.3"]
	if enriched.Kind != "attached" || enriched.Link.SessionID != hex32SessionID || enriched.Link.SessionName != "adopt-worker" {
		t.Fatalf("run1.3 session = %+v, want attached link to %q named %q", enriched, hex32SessionID, "adopt-worker")
	}
	bare := linkByBead["run1.1"]
	if bare.Kind != "attached" || bare.Link.SessionID != "gcg-session-aaaa" || bare.Link.SessionName != "gcg-session-aaaa" {
		t.Fatalf("run1.1 session = %+v, want the bare stamped link preserved", bare)
	}
}

// TestSessionIDForLookupMirrorsLinkFallback: the id SessionIDsForSnapshot offers
// for a by-id read must be the id runSessionLinkFor would put on the link when
// no index matches, on every path — otherwise a step gets a link the retired
// enrichment can never name. In particular a durable stamp that fails the id
// gate does not end the search: the link path falls through to the assignee-
// derived supervisor id, so the lookup path must too.
func TestSessionIDForLookupMirrorsLinkFallback(t *testing.T) {
	var emptyCtx runSessionLinkContext
	cases := map[string]runSnapshotBead{
		"stamped":                      {metadata: map[string]string{beadmeta.SessionIDMetadataKey: hex32SessionID}},
		"assignee only":                {assignee: hex32SessionID},
		"pool-qualified assignee":      {assignee: "polecat-gc-333573"},
		"gated stamp, usable assignee": {metadata: map[string]string{beadmeta.SessionIDMetadataKey: "Polecat-GC-1"}, assignee: "polecat-gc-333573"},
		"pool-qualified stamp":         {metadata: map[string]string{beadmeta.SessionIDMetadataKey: "polecat-" + hex32SessionID}},
		"gated stamp, gated assignee":  {metadata: map[string]string{beadmeta.SessionIDMetadataKey: "Polecat-GC-1"}, assignee: "mystery"},
		"nothing":                      {},
	}
	for name, bead := range cases {
		t.Run(name, func(t *testing.T) {
			bead.status = "closed"
			want := ""
			if link, ok := runSessionLinkFor(bead, "closed", emptyCtx); ok {
				want = link.SessionID
			}
			if got := sessionIDForLookup(bead); got != want {
				t.Fatalf("sessionIDForLookup = %q, want the link's id %q", got, want)
			}
		})
	}
}

// TestSessionIDGateAcceptsMintedShapes pins sessionIDRe's body bound to the
// longest id shape a store mints — "session-" plus 32 hex digits, 40 characters
// exactly — so a change on either side (a longer minted form, a tighter gate)
// fails here instead of silently regressing those ids to bare links.
func TestSessionIDGateAcceptsMintedShapes(t *testing.T) {
	body := hex32SessionID[len("gcg-"):]
	if len(body) != len("session-")+32 {
		t.Fatalf("fixture body is %d chars, want %d", len(body), len("session-")+32)
	}
	if !sessionIDRe.MatchString(hex32SessionID) {
		t.Fatalf("gate rejects the minted shape %q", hex32SessionID)
	}
	if sessionIDRe.MatchString(hex32SessionID + "0") {
		t.Fatalf("gate accepts a %d-char body; the bound is meant to be exactly %d", len(body)+1, len(body))
	}
}
