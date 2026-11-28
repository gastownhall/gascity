package main

import (
	"fmt"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

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
// query, reading distinct scopes concurrently. Results are returned in scope
// order whatever order the reads finish in.
//
// Once the runner abandons the check (ctx.Canceled), the sweep issues no new
// open or List: every scope not yet opened or listed is reported as
// abandoned, and the sweep returns when the calls already in flight finish.
// A store's List takes no context, so a call in flight runs to completion.
func sweepDoctorScopeLists[S doctorScopeLister](ctx *doctor.CheckContext, scopes []doctorWorkScope, newStore func(string) (S, error), query beads.ListQuery) []doctorScopeList[S] {
	return sweepDoctorScopeListsLimit(ctx, scopes, newStore, query, -1)
}

// sweepDoctorScopeListsLimit is sweepDoctorScopeLists with at most limit
// scope reads in flight at once; a negative limit is unbounded.
func sweepDoctorScopeListsLimit[S doctorScopeLister](ctx *doctor.CheckContext, scopes []doctorWorkScope, newStore func(string) (S, error), query beads.ListQuery, limit int) []doctorScopeList[S] {
	results := make([]doctorScopeList[S], len(scopes))
	var g errgroup.Group
	g.SetLimit(limit)
	for i, sc := range scopes {
		results[i].label = sc.label
		if newStore == nil || strings.TrimSpace(sc.path) == "" {
			continue
		}
		if ctx.Canceled() {
			results[i].skipped = doctorScopeAbandoned(sc.label)
			continue
		}
		g.Go(func() error {
			res := &results[i]
			if ctx.Canceled() {
				res.skipped = doctorScopeAbandoned(sc.label)
				return nil
			}
			store, err := newStore(sc.path)
			if err != nil {
				res.skipped = fmt.Sprintf("%s skipped: opening bead store: %v", sc.label, err)
				return nil
			}
			res.store = store
			if ctx.Canceled() {
				res.skipped = doctorScopeAbandoned(sc.label)
				return nil
			}
			items, err := store.List(query)
			if err != nil {
				res.skipped = fmt.Sprintf("%s skipped: listing beads: %v", sc.label, err)
				return nil
			}
			res.items = items
			res.read = true
			return nil
		})
	}
	_ = g.Wait() // workers record their failures in their own slot and never return an error
	return results
}

// doctorScopeAbandoned is the skip line of a scope a check stopped reading
// because the runner abandoned it at its timeout.
func doctorScopeAbandoned(label string) string {
	return label + " skipped: check abandoned at its timeout"
}
