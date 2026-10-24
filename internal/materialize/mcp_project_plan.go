package materialize

import (
	"bytes"
	"fmt"

	"github.com/gastownhall/gascity/internal/fsys"
)

// MCPPlanAction is what reconciling a projection would do to its target.
type MCPPlanAction string

const (
	// MCPPlanNone means there is nothing to do: an empty catalog on a target
	// gc does not manage, or a managed target that is already gone.
	MCPPlanNone MCPPlanAction = "none"
	// MCPPlanUnchanged means the target already holds the projection.
	MCPPlanUnchanged MCPPlanAction = "unchanged"
	// MCPPlanCreate means the target would be created.
	MCPPlanCreate MCPPlanAction = "create"
	// MCPPlanUpdate means the target would be rewritten.
	MCPPlanUpdate MCPPlanAction = "update"
	// MCPPlanRemove means the target would be deleted: the catalog is empty
	// and nothing gc preserves is left in it.
	MCPPlanRemove MCPPlanAction = "remove"
)

// MCPProjectionPlan describes what Apply would do to one target.
type MCPProjectionPlan struct {
	Provider string
	Target   string
	// Servers is the number of servers the projection carries.
	Servers int
	Action  MCPPlanAction
	// Adopt is set when the target exists and gc does not manage it yet:
	// Apply would save its current content under SnapshotDir, then replace
	// it.
	Adopt       bool
	SnapshotDir string
}

// Plan reports what Apply would do to the target. It reads the filesystem
// and writes nothing: no target, managed marker, lock, or adoption snapshot.
// It returns the validation errors Apply would return.
func (p MCPProjection) Plan(fs fsys.FS) (MCPProjectionPlan, error) {
	plan := MCPProjectionPlan{Provider: p.Provider, Target: p.Target, Servers: len(p.Servers), Action: MCPPlanNone}
	if err := p.validateProviderContract(); err != nil {
		return plan, err
	}
	if err := ensureNotSymlink(fs, p); err != nil {
		return plan, err
	}
	change, err := p.targetChange(fs)
	if err != nil {
		return plan, err
	}
	if change.noop {
		return plan, nil
	}
	existing, err := fs.ReadFile(p.Target)
	exists := err == nil
	if err != nil && !errorsIsNotExist(err) {
		return plan, fmt.Errorf("reading %s: %w", p.Target, err)
	}
	switch {
	case change.remove:
		if exists {
			plan.Action = MCPPlanRemove
		}
	case !exists:
		plan.Action = MCPPlanCreate
	case bytes.Equal(existing, change.content):
		plan.Action = MCPPlanUnchanged
	default:
		plan.Action = MCPPlanUpdate
	}
	if exists && len(p.Servers) > 0 && !p.isManaged(fs) {
		plan.Adopt = true
		plan.SnapshotDir = p.adoptionSnapshotDir()
	}
	return plan, nil
}
