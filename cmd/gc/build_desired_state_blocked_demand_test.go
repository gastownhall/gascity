package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestCollectOpenUnassignedRoutedWorkExcludesBlocked covers gc-ft31x, the
// build_desired_state.go sibling of gc-4zb/#4395: the controller-demand read at
// collectOpenUnassignedRoutedWork must not count a blocked-but-routed bead as
// spawn capacity. mapBdStatus folds bd's blocked/deferred/review/testing into
// Gas City's "open", so a blocked routed bead decodes with Status "open" and a
// cached (non-Live) List hands it back; only a Live read reaches bd's raw
// --status=open filter and drops it. Before the fix the cached read counted the
// blocked bead as controller-dispatcher demand; after it, only genuinely-open
// routed work is demand.
func TestCollectOpenUnassignedRoutedWorkExcludesBlocked(t *testing.T) {
	const pool = "worker"
	blocked := beads.Bead{ID: "BLK-1", Type: "task", Status: "open", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: pool,
	}}
	open := beads.Bead{ID: "OPN-1", Type: "task", Status: "open", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: pool,
	}}
	store := collapsedBlockedStatusStore{
		Store:          beads.NewMemStore(),
		cachedSnapshot: []beads.Bead{blocked, open}, // non-Live: blocked collapsed to "open", present
		liveSnapshot:   []beads.Bead{open},          // Live: bd's raw --status=open filter dropped the blocked row
	}
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}

	work, _, _ := collectOpenUnassignedRoutedWork(cfg, store, nil, nil, io.Discard)

	got := make(map[string]bool, len(work))
	for _, b := range work {
		got[b.ID] = true
	}
	if !got["OPN-1"] {
		t.Errorf("genuinely-open routed bead OPN-1 missing from demand: %v", ids(work))
	}
	if got["BLK-1"] {
		t.Errorf("blocked routed bead BLK-1 counted as spawn demand: %v — a Live read must exclude it (gc-ft31x)", ids(work))
	}
}

// blockedDemandStore models the production controller-demand List reads for a
// bead that is blocked in the backing store. mapBdStatus collapses it to Status
// "open", so a non-Live Status:"open" read returns it (openCollapsed); a Live
// Status:"open" read reaches bd's raw filter and excludes it (openLive). Status
// is honored so the in-progress demand read stays empty and every other read
// delegates to the embedded store (Ready/Get/DepList/writes).
type blockedDemandStore struct {
	beads.Store
	openCollapsed []beads.Bead // Status:"open", non-Live: blocked rows present, collapsed
	openLive      []beads.Bead // Status:"open", Live: raw filter excluded blocked
}

func (s blockedDemandStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	switch q.Status {
	case "in_progress":
		return nil, nil
	case "open":
		if q.Live {
			return append([]beads.Bead(nil), s.openLive...), nil
		}
		return append([]beads.Bead(nil), s.openCollapsed...), nil
	default:
		return s.Store.List(q)
	}
}

// TestCollectAssignedWorkBeadsExcludesBlockedFromDemandButReaperStillSeesIt
// covers the second gc-ft31x call site (collectAssignedWorkBeads open-routed
// pass). One read fed both a demand consumer (appendOpenAssignedMoleculeWorkUnique,
// which markReadyAssigned) and the blocked-routed reaper
// (appendOpenRoutedWorkUnique -> releaseOrphanedPoolAssignments). The fix splits
// it: the demand consumer reads the Live tier so a blocked assigned molecule root
// is NOT counted as wake demand, while the reaper keeps the collapsed-status read
// so its input is unchanged and still captures the blocked-routed orphan — "do
// not remove the blocked-routed reaper until a reviewed binary is deployed"
// (gc-ft31x). (releaseOrphanedPoolAssignments' own live re-read then decides
// whether to act on it; that gate is out of scope here.)
func TestCollectAssignedWorkBeadsExcludesBlockedFromDemandButReaperStillSeesIt(t *testing.T) {
	const deadAssignee = "worker--pool__coder-gc-session-deadbeef"
	live := beads.NewMemStore()
	blocker, err := live.Create(beads.Bead{Title: "workflow finalize", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	// A blocked graph.v2 root orphaned by a dead session: an assigned molecule
	// root (demand candidate) that is also routed (reaper candidate), decoded as
	// Status "open" by mapBdStatus. The blocking dep keeps it out of the Ready
	// path so the ONLY demand route is the molecule pass under test.
	orphan, err := live.Create(beads.Bead{
		Title:    "orphaned blocked workflow root",
		Type:     "wisp",
		Status:   "open",
		Assignee: deadAssignee,
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:     beadmeta.KindWorkflow,
			beadmeta.RoutedToMetadataKey: "worker",
		},
	})
	if err != nil {
		t.Fatalf("create orphan: %v", err)
	}
	if err := live.DepAdd(orphan.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("block orphan: %v", err)
	}
	collapsed := beads.Bead{
		ID: orphan.ID, Title: "orphaned blocked workflow root", Type: "wisp", Status: "open",
		Assignee: deadAssignee,
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:     beadmeta.KindWorkflow,
			beadmeta.RoutedToMetadataKey: "worker",
		},
	}
	store := blockedDemandStore{
		Store:         live,
		openCollapsed: []beads.Bead{collapsed}, // non-Live: blocked orphan present, collapsed to "open"
		openLive:      nil,                     // Live: bd's raw --status=open filter dropped it
	}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}

	found, _, _, readyAssigned, partial := collectAssignedWorkBeadsWithStores(cfg, store, nil, nil, nil)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}
	// Demand: the blocked assigned molecule root must NOT be wake demand.
	for k := range readyAssigned {
		if k.ID == orphan.ID {
			t.Fatalf("blocked assigned molecule root %s counted as demand (readyAssigned=%v) — the Live demand read must exclude it", orphan.ID, readyAssigned)
		}
	}
	// Reaper: the collapsed-status read is unchanged, so the blocked-routed
	// orphan is still captured (do not remove the blocked-routed reaper).
	if len(found) != 1 || found[0].ID != orphan.ID {
		t.Fatalf("reaper lost the blocked-routed orphan: found=%v, want [%s]", ids(found), orphan.ID)
	}
}

func ids(bs []beads.Bead) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.ID
	}
	return out
}
