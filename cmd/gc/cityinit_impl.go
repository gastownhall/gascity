package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/fsys"
)

func newCityInitService() *cityinit.Service {
	return cityinit.NewService(cityinit.ServiceDeps{
		DoInit:   cityInitDoInit,
		Finalize: cityInitFinalize,
		RegisterCity: func(_ context.Context, dir, nameOverride string) error {
			return registerCityForAPI(dir, nameOverride)
		},
		ReloadSupervisor:                reloadSupervisorNoWaitHook,
		ReloadSupervisorAfterUnregister: reloadSupervisorNoWait,
		FindCity:                        cityInitFindRegisteredCity,
		UnregisterCity:                  cityInitUnregisterCity,
		EventWriter:                     io.Discard,
	})
}

func cityInitDoInit(_ context.Context, req cityinit.InitRequest) error {
	wiz := wizardConfig{
		configName:       req.ConfigName,
		provider:         req.Provider,
		startCommand:     req.StartCommand,
		bootstrapProfile: req.BootstrapProfile,
	}
	if code := doInit(fsys.OSFS{}, req.Dir, wiz, req.NameOverride, io.Discard, io.Discard); code != 0 {
		if code == initExitAlreadyInitialized {
			return cityinit.ErrAlreadyInitialized
		}
		return fmt.Errorf("scaffold failed (exit %d)", code)
	}
	return nil
}

func cityInitFinalize(_ context.Context, req cityinit.InitRequest) error {
	if code := finalizeInit(req.Dir, io.Discard, io.Discard, initFinalizeOptions{
		skipProviderReadiness: req.SkipProviderReadiness,
		showProgress:          false,
		commandName:           "gc init",
	}); code != 0 {
		return fmt.Errorf("finalize failed (exit %d)", code)
	}
	return nil
}

func cityInitFindRegisteredCity(_ context.Context, name string) (cityinit.RegisteredCity, error) {
	reg := newSupervisorRegistry()
	entries, err := reg.List()
	if err != nil {
		return cityinit.RegisteredCity{}, err
	}
	for _, entry := range entries {
		if entry.EffectiveName() == name {
			return cityinit.RegisteredCity{
				Name: entry.EffectiveName(),
				Path: entry.Path,
			}, nil
		}
	}
	return cityinit.RegisteredCity{}, fmt.Errorf("%w: %q", cityinit.ErrNotRegistered, name)
}

func cityInitUnregisterCity(_ context.Context, city cityinit.RegisteredCity) error {
	err := newSupervisorRegistry().Unregister(city.Path)
	if errors.Is(err, cityinit.ErrNotRegistered) {
		return fmt.Errorf("%w: %s", cityinit.ErrNotRegistered, city.Name)
	}
	return err
}
