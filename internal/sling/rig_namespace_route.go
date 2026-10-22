package sling

import (
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/config"
)

// rigNamespaceRouteRefusal refuses a sling that would route beadID to
// target a in a guarded rig's store (federation = "rig") when the route is
// not one of that rig's own agents (agentutil.RigRouteAdmitted). The rig is
// the one the workflow store ref names and, for a bead, the one its prefix
// maps to. It applies to the typed Router, the shell Runner and custom
// sling_query alike, and --force does not override it.
//
// beadID is "" when the sling names a formula, whose name says nothing
// about a store.
func rigNamespaceRouteRefusal(deps SlingDeps, beadID string, a config.Agent) error {
	if deps.Cfg == nil {
		return nil
	}
	target := agentutil.RoutedToIdentity(&a)
	if err := agentutil.RigRouteRefusal(agentutil.GuardedRigForStoreRef(deps.Cfg, deps.StoreRef), "", target); err != nil {
		return err
	}
	if beadID = strings.TrimSpace(beadID); beadID == "" {
		return nil
	}
	return agentutil.RigRouteRefusal(agentutil.GuardedRigForPrefix(deps.Cfg, BeadPrefixForCity(deps.Cfg, beadID)), "", target)
}

// slingGuardBeadID is the bead a sling routes, or "" for a formula sling.
func slingGuardBeadID(opts SlingOpts) string {
	if opts.IsFormula {
		return ""
	}
	return opts.BeadOrFormula
}
