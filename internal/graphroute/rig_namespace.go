package graphroute

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

// rigNamespaceStepRouteKeys are the routing keys a decorated step may carry
// into the store.
var rigNamespaceStepRouteKeys = []string{
	beadmeta.RoutedToMetadataKey,
	beadmeta.ExecutionRoutedToMetadataKey,
	beadmeta.DeferredRoutedToMetadataKey,
	beadmeta.DeferredExecutionRoutedToMetadataKey,
}

// ValidateRigNamespaceRoutes refuses a decorated recipe whose steps would
// write a route or an assignee outside the guarded rig (federation = "rig")
// that storeRef names. Every routing value must be one of the rig's own
// agents, rig-qualified with no empty segment; a bare (city-scoped) route in
// a shared store is served by every city that defines it. A direct session
// assignee is refused unless it is a rig-qualified identity inside the
// namespace: the routing layer cannot see the city's session records, so it
// fails closed. A storeRef that names no guarded rig checks nothing.
func ValidateRigNamespaceRoutes(steps []formula.RecipeStep, storeRef string, cfg *config.City) error {
	rig := agentutil.GuardedRigForStoreRef(cfg, storeRef)
	if rig == nil {
		return nil
	}
	for i := range steps {
		step := &steps[i]
		for _, key := range rigNamespaceStepRouteKeys {
			if err := agentutil.RigRouteRefusal(rig, key, step.Metadata[key]); err != nil {
				return fmt.Errorf("graph step %s: %w", strings.TrimSpace(step.ID), err)
			}
		}
		if err := agentutil.RigRouteRefusal(rig, "assignee", step.Assignee); err != nil {
			return fmt.Errorf("graph step %s: %w", strings.TrimSpace(step.ID), err)
		}
	}
	return nil
}
