package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

func newCityInitService() (*cityinit.Service, error) {
	return cityinit.NewService(cityinit.ServiceDeps{
		FS:              fsys.OSScaffoldFS{},
		Initializer:     initializerAdapter{},
		Registry:        registryAdapter{},
		Reloader:        reloaderAdapter{},
		LifecycleEvents: cityInitLifecycleEvents{stderr: io.Discard},
	})
}

type initializerAdapter struct{}

func (initializerAdapter) Scaffold(ctx context.Context, req cityinit.InitRequest) error {
	return cityInitDoInit(ctx, req)
}

func (initializerAdapter) Finalize(ctx context.Context, req cityinit.InitRequest) error {
	return cityInitFinalize(ctx, req)
}

type registryAdapter struct{}

func (registryAdapter) Register(_ context.Context, dir, nameOverride string) error {
	return registerCityForAPI(dir, nameOverride)
}

func (registryAdapter) Find(ctx context.Context, name string) (cityinit.RegisteredCity, error) {
	return cityInitFindRegisteredCity(ctx, name)
}

func (registryAdapter) Unregister(ctx context.Context, city cityinit.RegisteredCity) error {
	return cityInitUnregisterCity(ctx, city)
}

type reloaderAdapter struct{}

func (reloaderAdapter) Reload() error {
	return reloadSupervisorNoWaitHook()
}

func (reloaderAdapter) ReloadAfterUnregister() error {
	return reloadSupervisorNoWaitHook()
}

type cityInitLifecycleEvents struct {
	stderr io.Writer
}

type cityInitLifecyclePayload struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func (e cityInitLifecycleEvents) EnsureCityLog(cityPath string) error {
	fr, err := events.NewFileRecorder(filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl"), e.stderrOrDiscard())
	if err != nil {
		return err
	}
	if err := fr.Close(); err != nil {
		return fmt.Errorf("closing event log: %w", err)
	}
	return nil
}

func (e cityInitLifecycleEvents) CityCreated(cityPath, name string) error {
	return e.record(cityPath, events.CityCreated, name, cityInitLifecyclePayload{Name: name, Path: cityPath})
}

func (e cityInitLifecycleEvents) CityUnregisterRequested(city cityinit.RegisteredCity) error {
	return e.record(city.Path, events.CityUnregisterRequested, city.Name, cityInitLifecyclePayload{Name: city.Name, Path: city.Path})
}

func (e cityInitLifecycleEvents) record(cityPath, eventType, subject string, payload cityInitLifecyclePayload) error {
	fr, err := events.NewFileRecorder(filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl"), e.stderrOrDiscard())
	if err != nil {
		return err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		if closeErr := fr.Close(); closeErr != nil {
			return errors.Join(err, fmt.Errorf("closing event log: %w", closeErr))
		}
		return err
	}
	fr.Record(events.Event{
		Type:    eventType,
		Actor:   "gc",
		Subject: subject,
		Payload: raw,
	})
	if err := fr.Close(); err != nil {
		return fmt.Errorf("closing event log: %w", err)
	}
	return nil
}

func (e cityInitLifecycleEvents) stderrOrDiscard() io.Writer {
	if e.stderr != nil {
		return e.stderr
	}
	return io.Discard
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
