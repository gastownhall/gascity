package beads

import "runtime"

// FederatedRead runs read, a store read that is ALLOWED to miss because the row
// may live in another store: one leg of a deliberate multi-store federation, or
// a by-id residence probe that asks each candidate store in turn. It does
// nothing else; the read runs once and its error comes back unchanged.
//
// The wrapper is an annotation. On a split city every class of bead lives in
// its own store, so a read that reaches the wrong store comes back silently
// empty, and most such reads are routing bugs. The split-strict test gate
// (GC_TEST_SPLIT_STRICT=1, internal/beads/splittest) records every read that
// finds a wrong-class row behind the wrong store, unless the read runs inside
// FederatedRead: that frame on the stack is how a call site says "missing here
// is expected, another leg covers it". Wrap only the probe itself, never a
// whole operation, and keep the set small: every production call site is listed
// by splittest's marker guard, so adding one is a reviewed change.
//
//go:noinline
func FederatedRead(read func() error) error {
	return read()
}

// FederatedGet is FederatedRead for the common by-id probe: store.Get(id) on a
// store that may not hold id.
//
//go:noinline
func FederatedGet(store Store, id string) (Bead, error) {
	var bead Bead
	err := FederatedRead(func() error {
		var err error
		bead, err = store.Get(id)
		return err
	})
	return bead, err
}

// RelicRead runs read, a store read that is allowed to miss for a reason the
// split plan has ruled against: one leg of a scan that still reads a scope
// store (the city or a rig ledger) for rows of a class that belongs in the
// class binding, kept only for rows written there before the split (relics)
// or by writers the plan has not moved yet. Like FederatedRead it changes
// nothing at runtime. The split-strict test gate does not fail on reads it
// covers, but counts them per call site as debt, so the leg stays visible
// until the writers move and the leg can be deleted. It covers READS only: a
// write inside it is still checked.
//
//go:noinline
func RelicRead(read func() error) error {
	return read()
}

// RelicGet is RelicRead for a by-id probe: store.Get(id) on a scope store
// that holds the row only as a relic.
//
//go:noinline
func RelicGet(store Store, id string) (Bead, error) {
	var bead Bead
	err := RelicRead(func() error {
		var err error
		bead, err = store.Get(id)
		return err
	})
	return bead, err
}

// ReadMarker names the annotation a store read runs under.
type ReadMarker int

const (
	// NoReadMarker is a read outside any marker.
	NoReadMarker ReadMarker = iota
	// FederatedReadMarker is a read inside FederatedRead or FederatedGet.
	FederatedReadMarker
	// RelicReadMarker is a read inside RelicRead.
	RelicReadMarker
)

// federatedReadMarkers are the frames FederatedReadOnStack recognizes.
var federatedReadMarkers = []string{
	"github.com/gastownhall/gascity/internal/beads.FederatedRead",
	"github.com/gastownhall/gascity/internal/beads.FederatedGet",
}

// relicReadMarker is the frame RelicRead leaves.
const relicReadMarker = "github.com/gastownhall/gascity/internal/beads.RelicRead"

// ReadMarkerOnStack reports the innermost read marker pcs (from
// runtime.Callers) pass through.
func ReadMarkerOnStack(pcs []uintptr) ReadMarker {
	if len(pcs) == 0 {
		return NoReadMarker
	}
	frames := runtime.CallersFrames(pcs)
	for {
		frame, more := frames.Next()
		if frame.Function == relicReadMarker {
			return RelicReadMarker
		}
		for _, marker := range federatedReadMarkers {
			if frame.Function == marker {
				return FederatedReadMarker
			}
		}
		if !more {
			return NoReadMarker
		}
	}
}

// FederatedReadMarkerFuncs returns the fully qualified function names that mark
// a federated or relic read on a goroutine's stack.
func FederatedReadMarkerFuncs() []string {
	return append(append([]string(nil), federatedReadMarkers...), relicReadMarker)
}

// FederatedReadOnStack reports whether pcs (from runtime.Callers) pass through
// FederatedRead or FederatedGet. Test-side store wrappers call it to tell a
// marked federation leg from an unmarked wrong-store read.
func FederatedReadOnStack(pcs []uintptr) bool {
	return ReadMarkerOnStack(pcs) == FederatedReadMarker
}
