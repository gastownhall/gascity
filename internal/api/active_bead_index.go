package api

import (
	"fmt"
	"log"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// activeBeadIndex answers the active-bead lookups of one list build: each row
// asks which in_progress bead its identities hold, and the index reads each
// bead store it reaches once, however many rows and identities ask. It
// captures the city's work stores and the rigs suspended by effective state
// when the build creates it, so every row of the build sees one scope.
//
// A store is read on the first lookup that reaches it, not up front: a
// lookup stops at its first match, as the per-row lookup did, so a store no
// lookup reaches is never read. The index is not safe for concurrent use; a
// build uses it from its own goroutine.
type activeBeadIndex struct {
	all  map[string]beads.WorkStore
	cold map[string]bool
	// scopes memoizes activeBeadSearchScope by the rig a lookup names.
	scopes map[string]activeBeadScope
	// loaded holds each read store's first in_progress bead per assignee,
	// keyed by beads.PhysicalStoreKey so rigs sharing a store read it once.
	// A store whose read failed is here too, with the rows that decoded.
	loaded map[string]map[string]string
	// errs holds one partial-list message per store whose read failed.
	errs []string
}

// activeBeadScope is one rig's search scope: the stores a lookup may read and
// the names to read them through, in precedence order.
type activeBeadScope struct {
	stores map[string]beads.WorkStore
	names  []string
}

// newActiveBeadIndex returns an empty index over the city's current work
// stores and effective rig suspension.
func (s *Server) newActiveBeadIndex() *activeBeadIndex {
	return &activeBeadIndex{
		all:    workStoresByName(s.state.Stores(), s.state.CityName()),
		cold:   s.loadCacheColdRigs(),
		scopes: make(map[string]activeBeadScope),
		loaded: make(map[string]map[string]string),
	}
}

// lookup returns the in_progress bead of the highest-priority identity, with
// the precedence the live lookup keeps: identity order first, then store
// order (activeBeadSearchScope), then newest first within a store. rig
// narrows the search as activeBeadSearchScope does. Returns "" if no
// identity holds work.
func (x *activeBeadIndex) lookup(rig string, assignees ...string) string {
	scope, ok := x.scopes[rig]
	if !ok {
		scope.stores, scope.names = activeBeadSearchScope(x.all, x.cold, rig)
		x.scopes[rig] = scope
	}
	for _, identity := range uniqueActiveBeadIdentities(assignees) {
		for _, name := range scope.names {
			if id := x.storeWork(name, scope.stores[name])[identity]; id != "" {
				return id
			}
		}
	}
	return ""
}

// partialErrors returns one message per store whose read failed, in the
// order the reads ran, for the list's partial_errors.
func (x *activeBeadIndex) partialErrors() []string {
	return x.errs
}

// storeWork returns store's newest in_progress bead per assignee, reading the
// store on its first use. The read takes the store's cached answer when its
// cache serves one and lists the store only when the cache declines. A failed
// read is logged and reported once and never retried within the build; a
// partial result keeps the rows that decoded.
func (x *activeBeadIndex) storeWork(name string, store beads.WorkStore) map[string]string {
	key := beads.PhysicalStoreKey(store)
	if work, ok := x.loaded[key]; ok {
		return work
	}
	query := beads.ListQuery{Status: "in_progress", Sort: beads.SortCreatedDesc}
	var rows []beads.Bead
	cacheOK := false
	if cached, ok := beads.As[beads.CachedLister](store); ok {
		rows, cacheOK = cached.CachedList(query)
	}
	if !cacheOK {
		var err error
		rows, err = store.List(query)
		if err != nil {
			log.Printf("api: looking up active work in rig %q: %v", name, err)
			x.errs = append(x.errs, fmt.Sprintf("rig %s: looking up active work: %v", name, err))
			if !beads.IsPartialResult(err) {
				rows = nil
			}
		}
	}
	work := make(map[string]string, len(rows))
	for _, b := range rows {
		if b.Assignee == "" {
			continue
		}
		if _, ok := work[b.Assignee]; !ok {
			work[b.Assignee] = b.ID
		}
	}
	x.loaded[key] = work
	return work
}

// uniqueActiveBeadIdentities trims assignees and drops blanks and repeats,
// keeping the first occurrence's priority.
func uniqueActiveBeadIdentities(assignees []string) []string {
	seen := make(map[string]bool, len(assignees))
	unique := make([]string, 0, len(assignees))
	for _, assignee := range assignees {
		assignee = strings.TrimSpace(assignee)
		if assignee == "" || seen[assignee] {
			continue
		}
		seen[assignee] = true
		unique = append(unique, assignee)
	}
	return unique
}
