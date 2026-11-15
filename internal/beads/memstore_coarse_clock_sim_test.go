//go:build gc_coarse_clock

package beads

import (
	"testing"
	"time"
)

// The simulation is live: real-clock creates in one millisecond tie on the
// wall clock, and the stamps still come out strictly increasing.
func TestCoarseClockSimulationIsActive(t *testing.T) {
	if got := memStoreWallClock(); got.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("gc_coarse_clock wall clock %v is not millisecond-aligned", got)
	}
	m := NewMemStore()
	var created []Bead
	for i := 0; i < 20; i++ {
		b, err := m.Create(Bead{Labels: []string{"tick"}})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, b)
	}
	wantStrictlyIncreasing(t, created)
	wantCreationOrder(t, m, "tick", ids(created))
}
