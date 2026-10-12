package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// hotfix/1.5.1 adaptation of the #7480 backport: these test helpers live on
// main in session_reconciler_killed_seat_test.go (#7362, killed pool seats)
// and build_desired_state_test.go (#7196, demand-leg panic recovery), neither
// of which is on hotfix/1.5.1. They are copied here, trimmed to what the
// split-store work-leg tests use.

// killRaceStore fails the Lists whose status listErr names, standing in for
// a dark leg, and lets a test watch the reads: afterList runs after every List
// the wrapped store answers. Conditional writes resolve to the wrapped store,
// so releases keep their CAS.
type killRaceStore struct {
	beads.Store
	afterList func(q beads.ListQuery, got []beads.Bead)
	listErr   map[string]error // by query status
}

func (s *killRaceStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if err := s.listErr[q.Status]; err != nil {
		return nil, err
	}
	got, err := s.Store.List(q)
	if err == nil && s.afterList != nil {
		s.afterList(q, got)
	}
	return got, err
}

func (s *killRaceStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// persistentWorker is a persistent pool with a floor of one seat.
func persistentWorker() config.Agent {
	return config.Agent{Name: "worker", Dir: "repo", StartCommand: "true", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2)}
}

// panickingLegStore panics on one read, standing in for a bug inside a leg's
// store read (a bd JSON decode edge, a nil wrapper deref).
type panickingLegStore struct {
	beads.Store
	panicOnList bool
}

func (s panickingLegStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if s.panicOnList {
		panic("injected leg panic: List")
	}
	return s.Store.List(query)
}
