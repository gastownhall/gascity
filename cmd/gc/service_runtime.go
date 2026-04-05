package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

type serviceRuntime struct {
	cr *CityRuntime
}

var _ workspacesvc.RuntimeContext = (*serviceRuntime)(nil)

func (rt *serviceRuntime) CityPath() string {
	return rt.cr.cityPath
}

func (rt *serviceRuntime) CityName() string {
	return rt.cr.cityName
}

func (rt *serviceRuntime) PublicationStorePath() string {
	return supervisor.PublicationsPath(rt.cr.cityPath)
}

func (rt *serviceRuntime) Config() *config.City {
	rt.cr.serviceStateMu.RLock()
	defer rt.cr.serviceStateMu.RUnlock()
	return rt.cr.cfg
}

func (rt *serviceRuntime) PublicationConfig() supervisor.PublicationConfig {
	rt.cr.serviceStateMu.RLock()
	defer rt.cr.serviceStateMu.RUnlock()
	return rt.cr.publication
}

func (rt *serviceRuntime) SessionProvider() runtime.Provider {
	rt.cr.serviceStateMu.RLock()
	defer rt.cr.serviceStateMu.RUnlock()
	return rt.cr.sp
}

func (rt *serviceRuntime) BeadStore(rig string) beads.Store {
	// controllerState is installed before the runtime loop starts and is not
	// swapped afterward, so reading the pointer here is race-free.
	if rt.cr.cs != nil {
		return rt.cr.cs.BeadStore(rig)
	}
	cfg := rt.Config()
	if cfg == nil {
		return nil
	}
	for _, candidate := range cfg.Rigs {
		if candidate.Name == rig {
			return bdStoreForRig(candidate.Path, rt.cr.cityPath, cfg)
		}
	}
	return nil
}

func (rt *serviceRuntime) Poke() {
	if rt.cr.pokeCh == nil {
		return
	}
	select {
	case rt.cr.pokeCh <- struct{}{}:
	default:
	}
}
