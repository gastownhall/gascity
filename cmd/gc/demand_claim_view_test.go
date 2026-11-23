package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// errClaimViewRead is what failingClaimViewStore returns from Get.
var errClaimViewRead = errors.New("claim view read failed (demand_claim_view_test)")

// failingClaimViewStore answers Get with an error, so a test can ask what the
// probe does when the claim view cannot answer.
type failingClaimViewStore struct {
	beads.Store
}

func (s *failingClaimViewStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errClaimViewRead
}

// seededView is a MemStore with explicit ids, holding one row in each state
// the claim view can hold a cross-store row's copy in.
func seededView(t *testing.T) *beads.MemStore {
	t.Helper()
	view := beads.NewMemStore()
	view.HonorExplicitIDs = true
	create := func(id string) {
		t.Helper()
		if _, err := view.Create(beads.Bead{ID: id, Title: "row " + id, Type: "task", Status: "open"}); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
	}
	create("open-1")
	create("claimed-1")
	who := "other-session"
	if err := view.Update("claimed-1", beads.UpdateOpts{Assignee: &who}); err != nil {
		t.Fatalf("assigning claimed-1: %v", err)
	}
	create("closed-1")
	if err := view.Close("closed-1"); err != nil {
		t.Fatalf("closing closed-1: %v", err)
	}
	return view
}

// TestClaimViewProbeAsksTheClaimsOwnQuestion is the design's verdict table
// (#5857): a row the claim view does not hold is city-delivered work the claim
// can win (ErrNotFound), a row it holds open and unassigned is winnable, a row
// it holds in any other state (assigned, in progress, closed) is refused by
// the claim CAS before any write, and a read that fails answers nothing.
func TestClaimViewProbeAsksTheClaimsOwnQuestion(t *testing.T) {
	view := seededView(t)
	probe := claimViewProbe{}

	for _, tc := range []struct {
		id       string
		want     claimViewVerdict
		wantErr  error
		wantRead bool
	}{
		{id: "missing-1", want: claimViewWin},
		{id: "open-1", want: claimViewWin, wantRead: true},
		{id: "claimed-1", want: claimViewShadow, wantRead: true},
		{id: "closed-1", want: claimViewShadow, wantRead: true},
	} {
		got, err := probe.check(view, tc.id)
		if got != tc.want {
			t.Fatalf("check(%s) verdict = %v, want %v (err=%v)", tc.id, got, tc.want, err)
		}
		if tc.wantErr == nil && err != nil {
			t.Fatalf("check(%s) err = %v, want nil", tc.id, err)
		}
	}
	if probe.stats.Probed != 4 {
		t.Fatalf("Probed = %d, want 4 (one read per asked id)", probe.stats.Probed)
	}

	got, err := probe.check(&failingClaimViewStore{Store: view}, "open-1")
	if got != claimViewUnknown {
		t.Fatalf("check(failing view) verdict = %v, want %v", got, claimViewUnknown)
	}
	if !errors.Is(err, errClaimViewRead) {
		t.Fatalf("check(failing view) err = %v, want %v", err, errClaimViewRead)
	}
	if probe.stats.Probed != 5 {
		t.Fatalf("Probed = %d, want 5 after the failing read", probe.stats.Probed)
	}

	// A nil view is a leg without a claim view: today's counting, no probe.
	if got, err := probe.check(nil, "open-1"); got != claimViewWin || err != nil {
		t.Fatalf("check(nil) = %v, %v, want %v, nil", got, err, claimViewWin)
	}
	if probe.stats.Probed != 5 {
		t.Fatalf("Probed = %d, want 5: a nil view reads nothing", probe.stats.Probed)
	}
}

// TestClaimViewProbeCachesPerStoreAndID pins the design's cost bound: one
// claim-view read per (store, id) per pass, so a row two of a pool's demand
// legs both ask about costs one `bd show`, not two.
func TestClaimViewProbeCachesPerStoreAndID(t *testing.T) {
	viewA := &recordingClaimViewStore{Store: seededView(t)}
	viewB := &recordingClaimViewStore{Store: seededView(t)}
	probe := claimViewProbe{}

	for i := 0; i < 2; i++ {
		if _, err := probe.check(viewA, "open-1"); err != nil {
			t.Fatalf("check(viewA, open-1) #%d: %v", i+1, err)
		}
	}
	if got := viewA.getIDs(); len(got) != 1 || got[0] != "open-1" {
		t.Fatalf("viewA read %v for the same (store, id) twice, want one read: %v", got, got)
	}
	if probe.stats.Probed != 1 {
		t.Fatalf("Probed = %d, want 1: the second ask of the same (store, id) is served from the cache", probe.stats.Probed)
	}

	// The cache is per (store, id): another view, or another id on the same
	// view, is a new question with a new read.
	if _, err := probe.check(viewB, "open-1"); err != nil {
		t.Fatalf("check(viewB, open-1): %v", err)
	}
	if _, err := probe.check(viewA, "closed-1"); err != nil {
		t.Fatalf("check(viewA, closed-1): %v", err)
	}
	if want := []string{"open-1", "closed-1"}; len(viewA.getIDs()) != 2 || viewA.getIDs()[0] != "open-1" {
		t.Fatalf("viewA reads = %v, want %v", viewA.getIDs(), want)
	}
	if got := viewB.getIDs(); len(got) != 1 {
		t.Fatalf("viewB reads = %v, want one read", got)
	}
	if probe.stats.Probed != 3 {
		t.Fatalf("Probed = %d, want 3", probe.stats.Probed)
	}
}

// TestDemandClaimViewReadFailureWithholdsTheRowAndMarksPartial is the design's
// error row (#5857): a cross-store row whose claim-view read fails with an
// error that is not not-found is a row the pass cannot prove the claim can win,
// so it is not counted — and the template's count is marked partial with an
// error naming the template, the row, the store the row was counted from, and
// the cause, so the decide retains capacity instead of silently shrinking it.
func TestDemandClaimViewReadFailureWithholdsTheRowAndMarksPartial(t *testing.T) {
	cfg := agreementConfig()
	cityStore := beads.NewMemStore()
	cityStore.HonorExplicitIDs = true
	rigStore := beads.NewMemStore()
	rigStore.HonorExplicitIDs = true

	// E: open, unassigned and routed in the city store, with a claim view that
	// cannot answer for it (a read failure that is not not-found).
	if _, err := cityStore.Create(beads.Bead{
		ID: "e", Title: "row e", Type: "task", Status: "open",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate},
	}); err != nil {
		t.Fatalf("seeding e: %v", err)
	}

	targets := []defaultScaleCheckTarget{
		{template: agreementTemplate, store: rigStore, storeKey: "rig:rig"},
	}
	targets = append(targets, crossStoreDemandLegs([]defaultScaleCheckTarget{
		{template: agreementTemplate, store: cityStore, storeKey: "city"},
	}, &failingClaimViewStore{Store: rigStore})...)

	counts, demand, partialTemplates, errs, _ := defaultScaleCheckCountsAndDemand(cfg, targets, newReadyDemandCache())

	if got := counts[agreementTemplate]; got != 0 {
		t.Fatalf("counts[%s] = %d, want 0: an unproven cross-store row is not demand", agreementTemplate, got)
	}
	entry := demand[agreementTemplate]
	for _, id := range []string{"e"} {
		if slices.Contains(entry.WorkBeadIDs, id) {
			t.Fatalf("WorkBeadIDs lists %s: the pass could not prove the claim can win it", id)
		}
		if _, listed := entry.StoreRefs[id]; listed {
			t.Fatalf("StoreRefs lists %s: an unproven cross-store row is not demand", id)
		}
	}
	if !partialTemplates[agreementTemplate] {
		t.Fatalf("partialTemplates = %v, want %s marked partial: its count is a partial of one demand source", partialTemplates, agreementTemplate)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly the claim-view read failure", errs)
	}
	if !errors.Is(errs[0], errClaimViewRead) {
		t.Fatalf("errs[0] = %v, want it to wrap the read failure %v", errs[0], errClaimViewRead)
	}
	for _, want := range []string{agreementTemplate, "e", "city", errClaimViewRead.Error()} {
		if !strings.Contains(errs[0].Error(), want) {
			t.Fatalf("errs[0] = %q, want it to name %q", errs[0], want)
		}
	}
}
