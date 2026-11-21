package graphroute

import (
	"fmt"
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

// IsExpandedWorkflowRoot reports whether meta belongs to an expanded graph.v2
// workflow root: a structural latch whose executable work lives in its member
// beads, never in the root itself.
func IsExpandedWorkflowRoot(meta map[string]string) bool {
	return meta[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow &&
		meta[beadmeta.WorkflowExpandedMetadataKey] == "true"
}

// SlingTargetBinding derives the default execution binding for sling target a,
// persisted under routedTo. A single-session target also carries its session
// name. Both the --formula launch (ApplyGraphRouting) and the plain sling of a
// cooked workflow root derive the target binding here.
func SlingTargetBinding(a config.Agent, routedTo string, store beads.SessionStore, cityName string, cfg *config.City) (GraphRouteBinding, error) {
	binding := GraphRouteBindingForAgent(a)
	// routedTo is the caller's already-normalized persisted identity. Preserve
	// it rather than recomputing in case the caller resolved a compatibility
	// spelling that agentutil intentionally keeps stable on the wire.
	binding.QualifiedName = routedTo
	if !binding.MetadataOnly {
		sessionTemplate := ""
		if cfg != nil {
			sessionTemplate = cfg.Workspace.SessionTemplate
		}
		binding.SessionName = agentutil.LookupSessionName(store, cityName, a.QualifiedName(), sessionTemplate)
		if binding.SessionName == "" {
			return GraphRouteBinding{}, fmt.Errorf("could not resolve session name for %q", a.QualifiedName())
		}
	}
	return binding, nil
}

// UnroutedMemberPatch returns the metadata that routes workflow member b to
// binding when b carries no route of its own, or nil when b needs none. It
// only fills empty routes: it never rewrites an existing route and never
// deletes a key, so applying the patch cannot move work between agents.
func UnroutedMemberPatch(b beads.Bead, binding GraphRouteBinding) (map[string]string, error) {
	kind := b.Metadata[beadmeta.KindMetadataKey]
	if IsWorkflowTopologyKind(kind) {
		return nil, nil
	}
	if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
		return nil, nil
	}
	step := formula.RecipeStep{ID: b.ID, Assignee: b.Assignee, Metadata: maps.Clone(b.Metadata)}
	if step.Metadata == nil {
		step.Metadata = make(map[string]string)
	}
	if err := ApplyGraphRouteBinding(&step, binding); err != nil {
		return nil, fmt.Errorf("routing member %s: %w", b.ID, err)
	}
	patch := make(map[string]string)
	for key, value := range step.Metadata {
		if value != "" && value != b.Metadata[key] {
			patch[key] = value
		}
	}
	if len(patch) == 0 {
		return nil, nil
	}
	return patch, nil
}
