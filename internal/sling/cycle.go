package sling

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// DepLister can enumerate the direct dependencies of a bead.
// The "down" direction returns what the given bead depends on;
// the "up" direction returns what depends on it.
// beads.Store satisfies this interface.
type DepLister interface {
	DepList(id, direction string) ([]beads.Dep, error)
}

// CycleError is returned when a dependency cycle is detected at sling time.
// Path contains the cycle, with the first and last element being the same
// bead ID (e.g. ["a", "b", "c", "a"]).
type CycleError struct {
	Path []string
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("dependency cycle detected: %s", strings.Join(e.Path, " → "))
}

// DetectCycle performs a depth-first search from startID, following "down"
// (depends-on) edges. It returns a CycleError if a cycle is reachable from
// startID, or nil if the reachable subgraph is acyclic.
//
// Only scheduling-relevant dependency types are cycle-sensitive
// ("blocks", "waits-for", "conditional-blocks", "parent-child", and the
// empty default); informational types ("relates-to", "tracks") are skipped.
func DetectCycle(startID string, dl DepLister) error {
	// Three-color DFS: white (unvisited), gray (in stack), black (done).
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	parent := map[string]string{}

	var dfs func(id string) error
	dfs = func(id string) error {
		color[id] = gray
		deps, err := dl.DepList(id, "down")
		if err != nil {
			return fmt.Errorf("reading dependencies of %s: %w", id, err)
		}
		for _, d := range deps {
			if !isCycleSensitiveDep(d.Type) {
				continue
			}
			next := d.DependsOnID
			if color[next] == gray {
				// Cycle found — reconstruct the path.
				return &CycleError{Path: buildCyclePath(parent, id, next)}
			}
			if color[next] == white {
				parent[next] = id
				if err := dfs(next); err != nil {
					return err
				}
			}
		}
		color[id] = black
		return nil
	}

	return dfs(startID)
}

// isCycleSensitiveDep reports whether a dependency type creates a scheduling
// obligation that would deadlock if cyclic. Informational relation types
// ("relates-to", "tracks") are excluded.
func isCycleSensitiveDep(depType string) bool {
	switch depType {
	case "blocks", "waits-for", "conditional-blocks", "parent-child", "":
		return true
	}
	return false
}

// buildCyclePath reconstructs the cycle as a human-readable slice.
// It walks the parent map from cycleEntry back to cycleEntry, then appends
// cycleEntry again so the first and last elements are the same.
func buildCyclePath(parent map[string]string, fromID, cycleEntry string) []string {
	// Walk from fromID back to cycleEntry using parent pointers.
	var chain []string
	cur := fromID
	for cur != cycleEntry {
		chain = append(chain, cur)
		p, ok := parent[cur]
		if !ok {
			break
		}
		cur = p
	}
	chain = append(chain, cycleEntry)

	// Reverse so the path reads start → ... → cycleEntry.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	// Append cycleEntry again to close the loop visually.
	return append(chain, cycleEntry)
}
