package splittest

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/convoy"
)

// readShape is one way a production read reaches a gate store: under a read
// marker or none, through the store itself or a layer over it.
type readShape struct {
	mark func(func() error) error
	over func(beads.Store) beads.Store
}

func unmarked(read func() error) error { return read() }

func itself(s beads.Store) beads.Store { return s }

var (
	federatedLegOver = func(s beads.Store) beads.Store { return beadstest.ResidentLeg(beadstest.FederatedLeg(s)) }
	relicLegOver     = func(s beads.Store) beads.Store { return beadstest.ResidentLeg(beadstest.RelicLeg(s)) }
	residentOver     = func(s beads.Store) beads.Store { return beadstest.Resident(s) }
)

// dualModePairs are the shapes the gate must judge alike while it honors
// both markers and leg roles (until T7j): each marker and its leg, and an
// unmarked read through the store, a class handle and a ResidentStore over
// no leg (OD-T7-3: strict). A marker decides over a leg beneath it.
var dualModePairs = map[string][2]readShape{
	"federated":                 {{beads.FederatedRead, itself}, {unmarked, federatedLegOver}},
	"relic":                     {{beads.RelicRead, itself}, {unmarked, relicLegOver}},
	"strict resident":           {{unmarked, itself}, {unmarked, residentOver}},
	"strict class handle":       {{unmarked, itself}, {unmarked, func(s beads.Store) beads.Store { return beadstest.Sessions(s) }}},
	"relic marker, federated":   {{beads.RelicRead, itself}, {beads.RelicRead, federatedLegOver}},
	"federated marker, relic":   {{beads.FederatedRead, itself}, {beads.FederatedRead, relicLegOver}},
	"federated, through a view": {{beads.FederatedRead, itself}, {unmarked, func(s beads.Store) beads.Store { return beadstest.Graph(federatedLegOver(s)) }}},
}

// projectionSightings runs a by-id read (convoy.ConvoyClose), a dep read
// (convoy.HasTrack) and a write (convoy.SetConvoyFields) of a session row
// through the work view, in shape, and returns what the gate saw.
func projectionSightings(t *testing.T, shape readShape) []sighting {
	t.Helper()
	p, leaf, sink := newTestProjection(t)
	session := seed(t, leaf, sessionBead("s"))
	view := shape.over(p.WorkView())
	_ = shape.mark(func() error { return convoyCloseThrough(view, session.ID) })
	_ = shape.mark(func() error {
		_, err := convoy.HasTrack(beadstest.Work(view), session.ID, "x")
		return err
	})
	_ = shape.mark(func() error {
		return convoy.SetConvoyFields(beadstest.Work(view), session.ID, convoy.ConvoyFields{Owner: "x"})
	})
	return sink.sightings()
}

// TestGateDualModeParityOnTheProjection: the projection charges a read made
// through a leg exactly as one made under the matching marker: the same
// sightings, sites, lines and kinds; a write is charged whatever the shape.
func TestGateDualModeParityOnTheProjection(t *testing.T) {
	for name, pair := range dualModePairs {
		marked, legged := projectionSightings(t, pair[0]), projectionSightings(t, pair[1])
		if !reflect.DeepEqual(marked, legged) {
			t.Errorf("%s: the leg's sightings differ from the marker's:\n leg    %+v\n marker %+v", name, legged, marked)
		}
	}
	want := map[string]struct{ violations, relics int }{
		"federated":           {1, 0}, // the write only
		"relic":               {1, 2}, // the write, and both reads as debt
		"strict resident":     {3, 0},
		"strict class handle": {3, 0},
	}
	for name, n := range want {
		got := projectionSightings(t, dualModePairs[name][1])
		if v, r := len(summarize(got, kindViolation)), len(summarize(got, kindRelic)); v != n.violations || r != n.relics {
			t.Errorf("%s through a leg: %d violation and %d relic sites, want %d and %d: %+v", name, v, r, n.violations, n.relics, got)
		}
	}
}

// strictViolations runs a by-id read of a graph id (convoy.ConvoyClose)
// through a strict work leaf, in shape, and returns what the leaf recorded.
func strictViolations(t *testing.T, shape readShape) []SplitViolation {
	t.Helper()
	work := NewWorkStore(t, "gc")
	_ = shape.mark(func() error { return convoyCloseThrough(shape.over(work), "gcg-7") })
	return TakeSplitViolations(work)
}

// TestGateDualModeParityOnAStrictLeaf: a strict leaf records a read made
// through a leg exactly as one made under the matching marker.
func TestGateDualModeParityOnAStrictLeaf(t *testing.T) {
	for name, pair := range dualModePairs {
		marked, legged := strictViolations(t, pair[0]), strictViolations(t, pair[1])
		if !reflect.DeepEqual(marked, legged) {
			t.Errorf("%s: the leg's violations differ from the marker's:\n leg    %+v\n marker %+v", name, legged, marked)
		}
	}
	for name, want := range map[string]int{"federated": 0, "relic": 0, "strict resident": 1, "strict class handle": 1} {
		if got := strictViolations(t, dualModePairs[name][1]); len(got) != want {
			t.Errorf("%s through a leg: %d violations, want %d: %+v", name, len(got), want, got)
		}
	}
}
