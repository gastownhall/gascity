package api

import (
	"log"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// activeBeadLookup is the active-bead lookup a list build hands its rows: a
// rig scope ("" reads every reachable store) plus the row's assignee
// identities in priority order. List views pass the build's index lookup
// (activeBeadIndex.lookup); detail views pass the live lookup
// (findLiveActiveBeadForAssignees).
type activeBeadLookup func(rig string, assignees ...string) string

// activeBeadIndex answers a list build's per-row active-bead lookups from one
// in_progress read per reached bead store, however many rows or identities
// the list carries. The agent list used to run one cached List per identity
// per store per row (findActiveBeadForAssignees), which multiplied the
// slowest store's read by the roster on a large city. An index instead loads
// each store once — the cached snapshot when the store serves one through a
// typed handle (beads.As[beads.CachedLister]), one unfiltered in_progress
// List otherwise — and keeps the first bead id per assignee in backend order.
//
// Precedence is the one findActiveBeadForAssigneesWithFreshness pinned:
// identity order first, then store order, then newest-first within a store.
// Scope (which stores a lookup may read; a store only cold rigs reach is
// excluded, a scoped lookup reads its rig's store alone) is
// activeBeadSearchScope, resolved per lookup, so a store is read only once
// some row reaches it. Loads are memoized per store, failures included: a
// store that failed is not retried within the build, and a partial result
// keeps the rows that did decode, like liveActiveWork.
//
// An index serves one build: the build's goroutine uses it sequentially and
// discards it with the build, so its maps need no lock.
type activeBeadIndex struct {
	stores map[string]beads.WorkStore
	cold   map[string]bool

	loaded map[string]bool              // per store name: load attempted
	first  map[string]map[string]string // per store name: assignee -> first bead id
}

// newActiveBeadIndex captures the build's store map and cold-rig set once.
// Both are read when the build starts; a store the build reaches later is
// read through the captured handle, so every row of one list answers from
// the same snapshot.
func (s *Server) newActiveBeadIndex() *activeBeadIndex {
	return &activeBeadIndex{
		stores: workStoresByName(s.state.Stores(), s.state.CityName()),
		cold:   s.loadCacheColdRigs(),
		loaded: make(map[string]bool),
		first:  make(map[string]map[string]string),
	}
}

// lookup returns the ID of the first in_progress bead assigned to any of the
// given identities: identity order first, then store order, then newest
// first within a store. If rig is non-empty, only that rig's store is
// searched; otherwise all stores are. A store only rigs suspended by
// effective state reach is never searched (activeBeadSearchScope).
func (idx *activeBeadIndex) lookup(rig string, assignees ...string) string {
	_, rigNames := activeBeadSearchScope(idx.stores, idx.cold, rig)
	seen := make(map[string]bool, len(assignees))
	var unique []string
	for _, assignee := range assignees {
		assignee = strings.TrimSpace(assignee)
		if assignee == "" || seen[assignee] {
			continue
		}
		seen[assignee] = true
		unique = append(unique, assignee)
	}
	for _, assignee := range unique {
		for _, rn := range rigNames {
			idx.loadStore(rn)
			if id := idx.first[rn][assignee]; id != "" {
				return id
			}
		}
	}
	return ""
}

// loadStore reads one store's in_progress beads once per build: the cached
// snapshot when the store serves one, one unfiltered in_progress List
// otherwise. A failed read is logged with rig context and never retried
// within the build; a partial result keeps the rows that did decode, so one
// unreadable bead does not hide the session's own work (liveActiveWork).
func (idx *activeBeadIndex) loadStore(rn string) {
	if idx.loaded[rn] {
		return
	}
	idx.loaded[rn] = true
	store := idx.stores[rn]
	if store == (beads.WorkStore{}) {
		return
	}
	query := beads.ListQuery{Status: "in_progress", Sort: beads.SortCreatedDesc}
	if cached, ok := beads.As[beads.CachedLister](store); ok {
		if rows, ok := cached.CachedList(query); ok {
			idx.absorb(rn, rows)
			return
		}
	}
	rows, err := store.List(query)
	if err != nil {
		log.Printf("api: listing active work in rig %q: %v", rn, err)
		if !beads.IsPartialResult(err) {
			return
		}
	}
	idx.absorb(rn, rows)
}

// absorb keeps each bead's id as its assignee's first match. Rows arrive
// newest-first within the store (SortCreatedDesc), and the lookup answers
// from the first store in scope order that carries the identity.
func (idx *activeBeadIndex) absorb(rn string, rows []beads.Bead) {
	byAssignee := make(map[string]string, len(rows))
	for _, b := range rows {
		if b.Assignee == "" {
			continue
		}
		if _, ok := byAssignee[b.Assignee]; !ok {
			byAssignee[b.Assignee] = b.ID
		}
	}
	idx.first[rn] = byAssignee
}
