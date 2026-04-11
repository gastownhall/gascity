package main

import (
	"fmt"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
)

// buildDesiredStateLogCache suppresses repeated supervisor.log lines that
// describe identical state across consecutive reconciler ticks. The
// reconciler fires roughly every 30s and most ticks observe zero churn,
// so emitting the same "assignedWorkBeads" / "namedWorkReady" block each
// tick was producing ~90% of supervisor.log volume. This cache records
// the signature of the last-logged values and only allows a new emission
// when the signature actually changes.
//
// A cache instance is per CityRuntime / supervisor instance, never
// package-global: tests can construct independent caches and assert
// logging behavior in isolation.
type buildDesiredStateLogCache struct {
	mu sync.Mutex

	// assignedWorkBeadsSig is the last-logged signature for the
	// "assignedWorkBeads: N beads found" summary line. Empty string
	// means "never logged yet"; the zero-beads case uses a dedicated
	// sentinel so we still detect transitions to/from empty.
	assignedWorkBeadsSig   string
	assignedWorkBeadsKnown bool

	// matchedNamedPairs records identity|beadID tuples we have already
	// logged "namedWorkReady: %s matched by bead %s" for. We only emit
	// the first time a pair is observed.
	matchedNamedPairs map[string]struct{}

	// namedReadySig is the last-logged signature for the
	// "namedWorkReady: ... ready=%v" summary line.
	namedReadySig   string
	namedReadyKnown bool
}

func newBuildDesiredStateLogCache() *buildDesiredStateLogCache {
	return &buildDesiredStateLogCache{
		matchedNamedPairs: make(map[string]struct{}),
	}
}

// assignedWorkBeadsSignature derives a stable identity key from the
// slice. Two slices with the same set of (ID, status, assignee,
// gc.routed_to) tuples produce the same signature regardless of order.
func assignedWorkBeadsSignature(wbs []beads.Bead) string {
	if len(wbs) == 0 {
		return "empty"
	}
	parts := make([]string, 0, len(wbs))
	for _, wb := range wbs {
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s",
			wb.ID, wb.Status, wb.Assignee, wb.Metadata["gc.routed_to"]))
	}
	// Sort so slice ordering doesn't cause spurious signature churn.
	// Insertion is cheap for the small slices the supervisor deals
	// with (single-digit typical).
	for i := 1; i < len(parts); i++ {
		j := i
		for j > 0 && parts[j-1] > parts[j] {
			parts[j-1], parts[j] = parts[j], parts[j-1]
			j--
		}
	}
	return strings.Join(parts, ";")
}

// shouldLogAssignedWorkBeads reports whether the "assignedWorkBeads"
// summary (plus per-bead detail lines) should be emitted for the given
// slice. The cache is updated on true return.
func (c *buildDesiredStateLogCache) shouldLogAssignedWorkBeads(wbs []beads.Bead) bool {
	if c == nil {
		return true
	}
	sig := assignedWorkBeadsSignature(wbs)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.assignedWorkBeadsKnown && c.assignedWorkBeadsSig == sig {
		return false
	}
	c.assignedWorkBeadsSig = sig
	c.assignedWorkBeadsKnown = true
	return true
}

// shouldLogNamedMatch reports whether the per-match
// "namedWorkReady: %s matched by bead %s" line should be emitted for
// the given identity/bead pair. Returns true only the first time the
// pair is seen; the cache is updated on true return.
func (c *buildDesiredStateLogCache) shouldLogNamedMatch(identity, beadID string) bool {
	if c == nil {
		return true
	}
	key := identity + "|" + beadID
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, seen := c.matchedNamedPairs[key]; seen {
		return false
	}
	c.matchedNamedPairs[key] = struct{}{}
	return true
}

// namedReadySignature derives a stable identity key from the readiness
// map so logical equality is preserved regardless of Go map iteration
// order.
func namedReadySignature(ready map[string]bool) string {
	if len(ready) == 0 {
		return "empty"
	}
	keys := make([]string, 0, len(ready))
	for k, v := range ready {
		if v {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return "none-ready"
	}
	for i := 1; i < len(keys); i++ {
		j := i
		for j > 0 && keys[j-1] > keys[j] {
			keys[j-1], keys[j] = keys[j], keys[j-1]
			j--
		}
	}
	return strings.Join(keys, ",")
}

// shouldLogNamedReadySummary reports whether the
// "namedWorkReady: %d assigned beads, %d named specs, ready=%v" line
// should be emitted. Returns true when the set of ready identities
// changed relative to the previous call; the cache is updated on true
// return.
func (c *buildDesiredStateLogCache) shouldLogNamedReadySummary(ready map[string]bool) bool {
	if c == nil {
		return true
	}
	sig := namedReadySignature(ready)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.namedReadyKnown && c.namedReadySig == sig {
		return false
	}
	c.namedReadySig = sig
	c.namedReadyKnown = true
	return true
}
