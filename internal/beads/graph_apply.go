package beads

import (
	"context"
	"fmt"
	"strings"

	beadslib "github.com/steveyegge/beads"
)

// GraphApplyStore is an optional store capability for atomically creating a
// precomputed graph of beads, dependency edges, and post-create assignments.
type GraphApplyStore interface {
	ApplyGraphPlan(ctx context.Context, plan *GraphApplyPlan) (*GraphApplyResult, error)
}

// GraphApplyHandleProvider exposes a graph-apply handle for stores whose
// capability depends on wrapped runtime state.
type GraphApplyHandleProvider interface {
	GraphApplyHandle() (GraphApplyStore, bool)
}

// GraphApplyFor returns the graph-apply capability for store when one is
// available. It preserves ordinary GraphApplyStore implementations and lets
// wrappers expose a delegated handle without claiming the interface globally.
func GraphApplyFor(store Store) (GraphApplyStore, bool) {
	if store == nil {
		return nil, false
	}
	if applier, ok := store.(GraphApplyStore); ok {
		return applier, true
	}
	if provider, ok := store.(GraphApplyHandleProvider); ok {
		return provider.GraphApplyHandle()
	}
	return nil, false
}

// GraphApplyPlan describes a symbolic bead graph to create atomically.
// Keys are caller-defined stable identifiers (for example recipe step IDs).
type GraphApplyPlan struct {
	CommitMessage string           `json:"commit_message,omitempty"`
	Nodes         []GraphApplyNode `json:"nodes"`
	Edges         []GraphApplyEdge `json:"edges,omitempty"`
}

// EphemeralGraphApplyStore reports whether a GraphApplyStore can create an
// entire graph in ephemeral storage.
type EphemeralGraphApplyStore interface {
	SupportsEphemeralGraphApply() bool
}

// GraphApplyNode describes a single bead to create.
type GraphApplyNode struct {
	Key               string            `json:"key"`
	Title             string            `json:"title"`
	Type              string            `json:"type,omitempty"`
	Priority          *int              `json:"priority,omitempty"`
	Description       string            `json:"description,omitempty"`
	Assignee          string            `json:"assignee,omitempty"`
	AssignAfterCreate bool              `json:"assign_after_create,omitempty"`
	From              string            `json:"from,omitempty"`
	Labels            []string          `json:"labels,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	MetadataRefs      map[string]string `json:"metadata_refs,omitempty"`
	ParentKey         string            `json:"parent_key,omitempty"`
	ParentID          string            `json:"parent_id,omitempty"`
}

// GraphApplyEdge describes a dependency edge. At least one of FromKey/FromID
// and one of ToKey/ToID must be set.
type GraphApplyEdge struct {
	FromKey  string `json:"from_key,omitempty"`
	FromID   string `json:"from_id,omitempty"`
	ToKey    string `json:"to_key,omitempty"`
	ToID     string `json:"to_id,omitempty"`
	Type     string `json:"type,omitempty"`
	Metadata string `json:"metadata,omitempty"`
}

// GraphApplyResult returns the concrete bead IDs assigned to each symbolic key.
type GraphApplyResult struct {
	IDs map[string]string `json:"ids"`
}

// validateGraphApplyPlan enforces the graph-plan contract shared by every
// in-process graph applier. Backends may add capability checks, but may not
// accept a structurally invalid plan with backend-dependent semantics.
func validateGraphApplyPlan(plan *GraphApplyPlan) error {
	if plan == nil {
		return fmt.Errorf("plan is nil")
	}
	if len(plan.Nodes) == 0 {
		return fmt.Errorf("plan has no nodes")
	}

	knownKeys := make(map[string]bool, len(plan.Nodes))
	for i, node := range plan.Nodes {
		if strings.TrimSpace(node.Key) == "" {
			return fmt.Errorf("node %d has empty key", i)
		}
		if knownKeys[node.Key] {
			return fmt.Errorf("duplicate node key %q", node.Key)
		}
		knownKeys[node.Key] = true
		if strings.TrimSpace(node.Title) == "" {
			return fmt.Errorf("node %q has empty title", node.Key)
		}
	}
	for _, node := range plan.Nodes {
		for metaKey, refKey := range node.MetadataRefs {
			if !knownKeys[refKey] {
				return fmt.Errorf("node %q: metadata ref %q references unknown key %q", node.Key, metaKey, refKey)
			}
		}
		if node.ParentKey != "" && !knownKeys[node.ParentKey] {
			return fmt.Errorf("node %q: parent key %q not found in plan", node.Key, node.ParentKey)
		}
	}
	for i, edge := range plan.Edges {
		if edge.FromKey != "" && !knownKeys[edge.FromKey] {
			return fmt.Errorf("edge %d: from key %q not found in plan", i, edge.FromKey)
		}
		if edge.ToKey != "" && !knownKeys[edge.ToKey] {
			return fmt.Errorf("edge %d: to key %q not found in plan", i, edge.ToKey)
		}
		if edge.FromKey == "" && edge.FromID == "" {
			return fmt.Errorf("edge %d: must specify from_key or from_id", i)
		}
		if edge.ToKey == "" && edge.ToID == "" {
			return fmt.Errorf("edge %d: must specify to_key or to_id", i)
		}
		depType := beadslib.DependencyType(edge.Type)
		if depType == "" {
			depType = beadslib.DepBlocks
		}
		if !depType.IsValid() {
			return fmt.Errorf("edge %d: invalid dependency type %q", i, edge.Type)
		}
	}
	return nil
}

// ValidateGraphApplyResult checks that every requested node key resolved to a
// concrete bead ID in the apply result.
func ValidateGraphApplyResult(plan *GraphApplyPlan, result *GraphApplyResult) error {
	if plan == nil {
		return fmt.Errorf("graph apply plan is nil")
	}
	if result == nil {
		return fmt.Errorf("graph apply result is nil")
	}
	if len(plan.Nodes) == 0 {
		return nil
	}
	missing := make([]string, 0)
	for _, node := range plan.Nodes {
		if strings.TrimSpace(result.IDs[node.Key]) == "" {
			missing = append(missing, node.Key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("graph apply result missing IDs for keys: %s", strings.Join(missing, ", "))
	}
	return nil
}
