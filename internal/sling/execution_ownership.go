package sling

import (
	"context"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

func inputOwnership(deps SlingDeps, a config.Agent) sourceworkflow.InputOwnership {
	return sourceworkflow.InputOwnership{
		Work: deps.Store, Graph: deps.graphStore(), CityPath: deps.CityPath,
		LockScope: sourceWorkflowLockScope(deps),
		Target:    agentutil.NormalizePoolRouteTarget(deps.Cfg, agentutil.RoutedToIdentity(&a)),
	}
}

func withWorkflowInputOwnership(ctx context.Context, deps SlingDeps, recipe *formula.Recipe, a config.Agent, launch func() (*molecule.Result, error)) (*molecule.Result, error) {
	root := recipe.RootStep()
	if root == nil {
		return launch()
	}
	var result *molecule.Result
	err := inputOwnership(deps, a).WithWorkflow(ctx, root.Metadata[beadmeta.InputConvoyIDMetadataKey], func() error {
		var innerErr error
		result, innerErr = launch()
		return innerErr
	})
	return result, err
}
