package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// convoyMemberRelic seeds a split city whose class binding owns a
// work-shaped (relic) id, and a work store holding the frozen pre-migration
// copy of that id — the shape `gc storage migrate` leaves behind.
func convoyMemberRelic(t *testing.T) (cityDir string, work *beads.MemStore, relicID string) {
	t.Helper()
	_, cityDir = bdSQLRefusalCityDir(t, bdSQLRefusalSplitStorage)
	resetCLIStorageRoutes(t)
	captureCLIStorageStderr(t)
	relic, _ := classResidentWorkShapedBead(t, cityDir, "demo-relic1", "migrated graph root")
	work = beads.NewMemStore()
	work.HonorExplicitIDs = true
	if _, err := work.Create(beads.Bead{ID: relic.ID, Title: "frozen pre-migration copy", Type: "task"}); err != nil {
		t.Fatalf("seeding the frozen copy: %v", err)
	}
	return cityDir, work, relic.ID
}

func convoyBeadsIn(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	var convoys []beads.Bead
	for _, b := range all {
		if b.Type == "convoy" {
			convoys = append(convoys, b)
		}
	}
	return convoys
}

// TestConvoyCreateRefusesABindingOwnedMember pins that `gc convoy create`
// asks the class binding first: a member the binding owns is refused before
// the convoy bead is created, so no orphaned convoy is left and no tracks edge
// lands on the frozen work copy.
func TestConvoyCreateRefusesABindingOwnedMember(t *testing.T) {
	cityDir, work, relicID := convoyMemberRelic(t)
	var stdout, stderr bytes.Buffer
	code := doConvoyCreateWithOptionsJSON(work, nil, cityDir, events.Discard, []string{"batch", relicID}, convoyCreateOptions{}, false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("convoy create tracked binding-owned %s; stdout=%q", relicID, stdout.String())
	}
	for _, want := range []string{relicID, "is owned by the relocated class binding"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("refusal missing %q; stderr=%q", want, stderr.String())
		}
	}
	if convoys := convoyBeadsIn(t, work); len(convoys) != 0 {
		t.Fatalf("refused create left %d convoy bead(s) behind: %+v", len(convoys), convoys)
	}
}

// TestConvoyAddRefusesABindingOwnedMember is the same contract for
// `gc convoy add`: no tracks edge onto a binding-owned member.
func TestConvoyAddRefusesABindingOwnedMember(t *testing.T) {
	cityDir, work, relicID := convoyMemberRelic(t)
	convoy, err := work.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doConvoyAddJSON(work, cityDir, []string{convoy.ID, relicID}, false, &stdout, &stderr); code == 0 {
		t.Fatalf("convoy add tracked binding-owned %s; stdout=%q", relicID, stdout.String())
	}
	if !strings.Contains(stderr.String(), relicID+" is owned by the relocated class binding") {
		t.Errorf("refusal does not name the owner; stderr=%q", stderr.String())
	}
	deps, err := work.DepList(convoy.ID, "up")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Fatalf("refused add left tracks edges: %+v", deps)
	}
}

// TestConvoyCreateAndAddOnASingleStoreCityAreUnchanged pins the non-split
// path: no binding answers, so members are read and tracked on the work store.
func TestConvoyCreateAndAddOnASingleStoreCityAreUnchanged(t *testing.T) {
	cityPath := writeMetadataCASTestCity(t)
	resetCLIStorageRoutes(t)
	work := beads.NewMemStore()
	member, err := work.Create(beads.Bead{Title: "member", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doConvoyCreateWithOptionsJSON(work, nil, cityPath, events.Discard, []string{"batch", member.ID}, convoyCreateOptions{}, false, &stdout, &stderr); code != 0 {
		t.Fatalf("convoy create = %d; stderr=%q", code, stderr.String())
	}
	other, err := work.Create(beads.Bead{Title: "other", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	convoys := convoyBeadsIn(t, work)
	if len(convoys) != 1 {
		t.Fatalf("want one convoy, got %+v", convoys)
	}
	stderr.Reset()
	if code := doConvoyAddJSON(work, cityPath, []string{convoys[0].ID, other.ID}, false, &stdout, &stderr); code != 0 {
		t.Fatalf("convoy add = %d; stderr=%q", code, stderr.String())
	}
}
