package main

import (
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// readOverlapGate parks each store read until a second read is in flight (or a
// timeout passes) and records the peak number of reads in flight. A serial
// caller never shows a peak above one: its lone read waits out the timeout.
type readOverlapGate struct {
	mu   sync.Mutex
	in   int
	peak int
	met  chan struct{}
	once sync.Once
}

func (g *readOverlapGate) enter() {
	g.mu.Lock()
	g.in++
	if g.in > g.peak {
		g.peak = g.in
	}
	if g.in >= 2 {
		g.once.Do(func() { close(g.met) })
	}
	g.mu.Unlock()
	select {
	case <-g.met:
	case <-time.After(time.Second):
	}
	g.mu.Lock()
	g.in--
	g.mu.Unlock()
}

func (g *readOverlapGate) peakReads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

type overlapGatedStore struct {
	*beads.MemStore
	gate *readOverlapGate
}

func (s *overlapGatedStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.gate.enter()
	return s.MemStore.Ready(query...)
}

// The default scale_check demand pass reads one ready set per store group. Each
// read is a store round trip (a bd subprocess on a Dolt-backed store), so the
// reads must overlap: read one after another, a city with many rigs paid the
// sum of them on every pass. The counts stay the same either way.
func TestDefaultScaleCheckCountsAndDemandOverlapsStoreReads(t *testing.T) {
	gate := &readOverlapGate{met: make(chan struct{})}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(5)}},
	}
	var targets []defaultScaleCheckTarget
	for i, key := range []string{"city", "rig:alpha", "rig:bravo"} {
		// Distinct sequences keep the bead IDs distinct across the stores, so
		// the cross-group ID dedup does not fold them together.
		mem := beads.NewMemStoreFrom(i*100, nil, nil)
		seedRoutedWork(t, mem, "worker", "work in "+key)
		targets = append(targets, defaultScaleCheckTarget{
			template: "worker",
			storeKey: key,
			store:    &overlapGatedStore{MemStore: mem, gate: gate},
		})
	}

	counts, demand, partials, errs := defaultScaleCheckCountsAndDemand(cfg, targets, newReadyDemandCache())

	if len(errs) != 0 || len(partials) != 0 {
		t.Fatalf("errs=%v partials=%v, want a clean pass", errs, partials)
	}
	if counts["worker"] != 3 || demand["worker"].Count != 3 {
		t.Fatalf("count=%d demand=%d, want one ready bead per store group", counts["worker"], demand["worker"].Count)
	}
	if peak := gate.peakReads(); peak < 2 {
		t.Fatalf("peak concurrent store reads = %d, want the store groups' reads to overlap", peak)
	}
}
