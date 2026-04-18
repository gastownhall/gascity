package main

import (
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func resolveDesiredScopeEndpointState(cityPath, scopeRoot, issuePrefix, scopeLabel string, desired contract.ConfigState) (contract.ConfigState, bool, error) {
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, scopeRoot, issuePrefix)
	if err != nil {
		return contract.ConfigState{}, false, wrapInvalidEndpointStateError(scopeLabel, err)
	}
	if resolved.Kind == contract.ScopeConfigAuthoritative {
		return resolved.State, true, nil
	}
	return desired, false, nil
}

func resolveDesiredCityEndpointState(cityPath string, cityDolt config.DoltConfig, cityPrefix string) (contract.ConfigState, bool, error) {
	return resolveDesiredScopeEndpointState(cityPath, cityPath, cityPrefix, "city", desiredCityDoltConfigState(cityPath, cityDolt, cityPrefix))
}

func resolveDesiredRigEndpointState(cityPath string, rig config.Rig, cityState contract.ConfigState) (contract.ConfigState, error) {
	rig = normalizedRigConfig(cityPath, rig)
	desired := desiredRigDoltConfigState(cityPath, rig, cityState)
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, rig.Path, rig.EffectivePrefix())
	if err != nil {
		if cfg, ok, readErr := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(rig.Path, ".beads", "config.yaml")); readErr == nil && ok && cfg.EndpointOrigin == contract.EndpointOriginInheritedCity {
			return desired, nil
		}
		return contract.ConfigState{}, wrapInvalidEndpointStateError("rig", err)
	}
	if resolved.Kind == contract.ScopeConfigAuthoritative {
		return resolved.State, nil
	}
	return desired, nil
}
