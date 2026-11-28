package main

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// doctorScopeSweepConcurrency caps the scope reads a sweep keeps in flight.
// On a bd-backed scope each read is a bd subprocess, so it matches the cap
// doctor already applies to concurrent bd calls
// (proxiedBackupCoverageParallelism in internal/doctor).
const doctorScopeSweepConcurrency = 4

// doctorWorkScope is one bead store a doctor check reads: the city store or
// a rig store, named by the label its findings and skip lines carry.
type doctorWorkScope struct {
	label string
	path  string
}

// doctorWorkScopes returns the city scope followed, in config order, by every
// rig that is not suspended and has a path.
func doctorWorkScopes(cfg *config.City, cityPath string) []doctorWorkScope {
	scopes := []doctorWorkScope{{label: "city", path: cityPath}}
	if cfg == nil {
		return scopes
	}
	for _, rig := range cfg.Rigs {
		if rig.Suspended || strings.TrimSpace(rig.Path) == "" {
			continue
		}
		scopes = append(scopes, doctorWorkScope{label: "rig " + rig.Name, path: rig.Path})
	}
	return scopes
}

// doctorScopeLister is the one store operation the scope sweep needs. The
// sweep is generic over it so it neither holds nor hands out a store type of
// its own: each check keeps the store its factory returns.
type doctorScopeLister interface {
	List(query beads.ListQuery) ([]beads.Bead, error)
}

// doctorScopeList is the outcome of reading one scope. read reports that the
// scope's store was opened and listed; skipped, when non-empty, is the
// scope's skip line. A scope with neither was not read at all (no factory or
// no path).
type doctorScopeList[S doctorScopeLister] struct {
	label   string
	store   S
	items   []beads.Bead
	skipped string
	read    bool
}

// sweepDoctorScopeLists opens every scope through newStore and lists it with
// query, keeping at most doctorScopeSweepConcurrency scope reads in flight at
// once. Results are returned in scope
// order whatever order the reads finish in.
//
// Once the runner abandons the check (ctx.Canceled), the sweep issues no new
// open or List: every scope not yet opened or listed is reported as
// abandoned, and the sweep returns when the calls already in flight finish.
// A store's List takes no context, so a call in flight runs to completion.
//
// A panic in a scope's open or List stops the sweep the same way. Once the
// calls in flight finish, the sweep panics again with the same value on its
// caller's goroutine, where the doctor runner's per-check recovery reports it
// as that check's failure; a panic left on a sweep goroutine would end the
// whole doctor run.
func sweepDoctorScopeLists[S doctorScopeLister](ctx *doctor.CheckContext, scopes []doctorWorkScope, newStore func(string) (S, error), query beads.ListQuery) []doctorScopeList[S] {
	return sweepDoctorScopeListsLimit(ctx, scopes, newStore, query, doctorScopeSweepConcurrency)
}

// sweepDoctorScopeListsLimit is sweepDoctorScopeLists with at most limit
// scope reads in flight at once; tests read scopes one at a time with a limit of 1.
func sweepDoctorScopeListsLimit[S doctorScopeLister](ctx *doctor.CheckContext, scopes []doctorWorkScope, newStore func(string) (S, error), query beads.ListQuery, limit int) []doctorScopeList[S] {
	results := make([]doctorScopeList[S], len(scopes))
	var g errgroup.Group
	g.SetLimit(limit)
	var panicked atomic.Bool
	// stopped reports that the sweep starts no new open or List. A scope
	// stopped by a panic gets the abandoned skip line, which nothing reads:
	// the sweep panics instead of returning its results.
	stopped := func() bool { return ctx.Canceled() || panicked.Load() }
	for i, sc := range scopes {
		results[i].label = sc.label
		if newStore == nil || strings.TrimSpace(sc.path) == "" {
			continue
		}
		if stopped() {
			results[i].skipped = doctorScopeAbandoned(sc.label)
			continue
		}
		g.Go(func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					panicked.Store(true)
					err = doctorScopeReadPanic{value: r}
				}
			}()
			readDoctorScope(&results[i], sc, newStore, query, stopped)
			return nil
		})
	}
	// A worker returns an error only for a panic it recovered.
	var p doctorScopeReadPanic
	if errors.As(g.Wait(), &p) {
		panic(p.value)
	}
	return results
}

// readDoctorScope opens one scope and lists it into res, unless stopped
// reports before the open or before the List that the sweep has stopped. A
// scope that fails to open or to list gets its skip line.
func readDoctorScope[S doctorScopeLister](res *doctorScopeList[S], sc doctorWorkScope, newStore func(string) (S, error), query beads.ListQuery, stopped func() bool) {
	if stopped() {
		res.skipped = doctorScopeAbandoned(sc.label)
		return
	}
	store, err := newStore(sc.path)
	if err != nil {
		res.skipped = fmt.Sprintf("%s skipped: opening bead store: %v", sc.label, err)
		return
	}
	res.store = store
	if stopped() {
		res.skipped = doctorScopeAbandoned(sc.label)
		return
	}
	items, err := store.List(query)
	if err != nil {
		res.skipped = fmt.Sprintf("%s skipped: listing beads: %v", sc.label, err)
		return
	}
	res.items = items
	res.read = true
}

// doctorScopeReadPanic carries a panic recovered on a sweep goroutine back to
// the sweep, which panics again with value on its caller's goroutine.
type doctorScopeReadPanic struct{ value any }

func (p doctorScopeReadPanic) Error() string { return fmt.Sprintf("scope read panicked: %v", p.value) }

// doctorScopeAbandoned is the skip line of a scope a check stopped reading
// because the runner abandoned it at its timeout.
func doctorScopeAbandoned(label string) string {
	return label + " skipped: check abandoned at its timeout"
}
