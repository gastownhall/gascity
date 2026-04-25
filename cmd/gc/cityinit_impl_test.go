package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func mustNewCityInitService(t *testing.T) *cityinit.Service {
	t.Helper()
	svc, err := newCityInitService()
	if err != nil {
		t.Fatalf("newCityInitService: %v", err)
	}
	return svc
}

type fakeSupervisorRegistry struct {
	entries       []supervisor.CityEntry
	listErr       error
	registerErr   error
	unregisterErr error
}

func (f *fakeSupervisorRegistry) List() ([]supervisor.CityEntry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]supervisor.CityEntry(nil), f.entries...), nil
}

func (f *fakeSupervisorRegistry) Register(string, string) error {
	return f.registerErr
}

func (f *fakeSupervisorRegistry) Unregister(string) error {
	return f.unregisterErr
}

func TestCityInitServiceScaffoldCreatesCityRegistersAndEmitsCreated(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cityPath := filepath.Join(t.TempDir(), "api-city")
	reloadSawCreated := 0

	oldReloadSupervisorNoWaitHook := reloadSupervisorNoWaitHook
	reloadSupervisorNoWaitHook = func() {
		evts, err := events.ReadFiltered(filepath.Join(cityPath, ".gc", "events.jsonl"), events.Filter{Type: events.CityCreated})
		if err == nil {
			reloadSawCreated = len(evts)
		}
	}
	t.Cleanup(func() {
		reloadSupervisorNoWaitHook = oldReloadSupervisorNoWaitHook
	})

	result, err := mustNewCityInitService(t).Scaffold(context.Background(), cityinit.InitRequest{
		Dir:              cityPath,
		Provider:         "codex",
		BootstrapProfile: bootstrapProfileSingleHostCompat,
		NameOverride:     "api-city",
	})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if result.CityName != "api-city" || result.CityPath != cityPath || result.ProviderUsed != "codex" {
		t.Fatalf("Scaffold result = %+v, want api-city/%s/codex", result, cityPath)
	}
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		t.Fatalf("city.toml missing after Scaffold: %v", err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("registry entries = %+v, want one", entries)
	}
	if entries[0].EffectiveName() != "api-city" {
		t.Fatalf("registry effective name = %q, want api-city", entries[0].EffectiveName())
	}
	assertSameTestPath(t, entries[0].Path, cityPath)

	evts, err := events.ReadFiltered(filepath.Join(cityPath, ".gc", "events.jsonl"), events.Filter{Type: events.CityCreated})
	if err != nil {
		t.Fatalf("ReadFiltered city.created: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("city.created events = %d, want 1: %+v", len(evts), evts)
	}
	var payload api.CityCreatedPayload
	if err := json.Unmarshal(evts[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal city.created payload: %v", err)
	}
	if payload.Name != "api-city" {
		t.Fatalf("payload name = %q, want api-city", payload.Name)
	}
	assertSameTestPath(t, payload.Path, cityPath)
	if reloadSawCreated != 1 {
		t.Fatalf("reload saw %d city.created events, want 1 before wake", reloadSawCreated)
	}

	_, err = mustNewCityInitService(t).Scaffold(context.Background(), cityinit.InitRequest{
		Dir:      cityPath,
		Provider: "codex",
	})
	if !errors.Is(err, cityinit.ErrAlreadyInitialized) {
		t.Fatalf("second Scaffold error = %v, want ErrAlreadyInitialized", err)
	}
}

func TestCityInitServiceScaffoldDoesNotEmitCreatedWhenRegisterFails(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cityPath := filepath.Join(t.TempDir(), "api-city")

	oldNewSupervisorRegistry := newSupervisorRegistry
	newSupervisorRegistry = func() supervisorRegistry {
		return &fakeSupervisorRegistry{registerErr: errors.New("boom")}
	}
	t.Cleanup(func() {
		newSupervisorRegistry = oldNewSupervisorRegistry
	})

	_, err := mustNewCityInitService(t).Scaffold(context.Background(), cityinit.InitRequest{
		Dir:              cityPath,
		Provider:         "codex",
		BootstrapProfile: bootstrapProfileSingleHostCompat,
		NameOverride:     "api-city",
	})
	if err == nil || !strings.Contains(err.Error(), "register with supervisor") {
		t.Fatalf("Scaffold error = %v, want register with supervisor failure", err)
	}
	if _, statErr := os.Stat(cityPath); !os.IsNotExist(statErr) {
		t.Fatalf("cityPath stat after failed registration = %v, want not exists", statErr)
	}

	evts, readErr := events.ReadFiltered(filepath.Join(cityPath, ".gc", "events.jsonl"), events.Filter{Type: events.CityCreated})
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("ReadFiltered city.created: %v", readErr)
	}
	if len(evts) != 0 {
		t.Fatalf("city.created events = %d, want 0: %+v", len(evts), evts)
	}
}

func TestCityInitServiceScaffoldPreservesExistingDirectoryWhenRegisterFails(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cityPath := filepath.Join(t.TempDir(), "api-city")
	keepPath := filepath.Join(cityPath, "keep.txt")
	hooksKeepPath := filepath.Join(cityPath, "hooks", "custom.json")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keepPath, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(hooksKeepPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksKeepPath, []byte(`{"custom":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	oldNewSupervisorRegistry := newSupervisorRegistry
	newSupervisorRegistry = func() supervisorRegistry {
		return &fakeSupervisorRegistry{registerErr: errors.New("boom")}
	}
	t.Cleanup(func() {
		newSupervisorRegistry = oldNewSupervisorRegistry
	})

	_, err := mustNewCityInitService(t).Scaffold(context.Background(), cityinit.InitRequest{
		Dir:              cityPath,
		Provider:         "codex",
		BootstrapProfile: bootstrapProfileSingleHostCompat,
		NameOverride:     "api-city",
	})
	if err == nil || !strings.Contains(err.Error(), "register with supervisor") {
		t.Fatalf("Scaffold error = %v, want register with supervisor failure", err)
	}

	data, readErr := os.ReadFile(keepPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%q): %v", keepPath, readErr)
	}
	if string(data) != "keep" {
		t.Fatalf("keep.txt = %q, want keep", string(data))
	}
	hooksData, hooksReadErr := os.ReadFile(hooksKeepPath)
	if hooksReadErr != nil {
		t.Fatalf("ReadFile(%q): %v", hooksKeepPath, hooksReadErr)
	}
	if string(hooksData) != `{"custom":true}` {
		t.Fatalf("custom hook file = %q, want preserved content", string(hooksData))
	}
	if _, statErr := os.Stat(filepath.Join(cityPath, "city.toml")); !os.IsNotExist(statErr) {
		t.Fatalf("city.toml stat after failed registration = %v, want not exists", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(cityPath, ".gc")); !os.IsNotExist(statErr) {
		t.Fatalf(".gc stat after failed registration = %v, want not exists", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(cityPath, "hooks", "claude.json")); !os.IsNotExist(statErr) {
		t.Fatalf("hooks/claude.json stat after failed registration = %v, want not exists", statErr)
	}

	newSupervisorRegistry = oldNewSupervisorRegistry
	result, err := mustNewCityInitService(t).Scaffold(context.Background(), cityinit.InitRequest{
		Dir:              cityPath,
		Provider:         "codex",
		BootstrapProfile: bootstrapProfileSingleHostCompat,
		NameOverride:     "api-city",
	})
	if err != nil {
		t.Fatalf("Scaffold retry: %v", err)
	}
	if result.CityName != "api-city" {
		t.Fatalf("Scaffold retry city name = %q, want api-city", result.CityName)
	}
}

func TestCityInitServiceInitScaffoldsAndFinalizes(t *testing.T) {
	skipSlowCmdGCTest(t, "runs the full local init scaffold/finalize path; run make test-cmd-gc-process for full coverage")
	configureTestDoltIdentityEnv(t)
	configureRealBdAndDoltPath(t)
	cityPath := filepath.Join(t.TempDir(), "init-city")

	result, err := mustNewCityInitService(t).Init(context.Background(), cityinit.InitRequest{
		Dir:                   cityPath,
		StartCommand:          "true",
		NameOverride:          "init-city",
		SkipProviderReadiness: true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if result.CityName != "init-city" || result.CityPath != cityPath {
		t.Fatalf("Init result = %+v, want init-city/%s", result, cityPath)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc")); err != nil {
		t.Fatalf(".gc missing after Init finalization: %v", err)
	}

	_, err = mustNewCityInitService(t).Init(context.Background(), cityinit.InitRequest{
		Dir:          cityPath,
		StartCommand: "true",
	})
	if !errors.Is(err, cityinit.ErrAlreadyInitialized) {
		t.Fatalf("second Init error = %v, want ErrAlreadyInitialized", err)
	}
}

func TestCityInitServiceUnregisterRemovesRegistryAndEmitsEvent(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	result, err := mustNewCityInitService(t).Unregister(context.Background(), cityinit.UnregisterRequest{
		CityName: " bright-lights ",
	})
	if err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if result.CityName != "bright-lights" {
		t.Fatalf("CityName = %q, want bright-lights", result.CityName)
	}
	assertSameTestPath(t, result.CityPath, cityPath)

	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("registry entries after unregister = %+v, want empty", entries)
	}

	evts, err := events.ReadFiltered(filepath.Join(cityPath, ".gc", "events.jsonl"), events.Filter{Type: events.CityUnregisterRequested})
	if err != nil {
		t.Fatalf("ReadFiltered: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("unregister_requested events = %d, want 1: %+v", len(evts), evts)
	}
	if evts[0].Actor != "gc" || evts[0].Subject != "bright-lights" {
		t.Fatalf("event actor/subject = %q/%q, want gc/bright-lights", evts[0].Actor, evts[0].Subject)
	}
	var payload api.CityUnregisterRequestedPayload
	if err := json.Unmarshal(evts[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Name != "bright-lights" {
		t.Fatalf("payload name = %q, want bright-lights", payload.Name)
	}
	assertSameTestPath(t, payload.Path, cityPath)
}

func TestCityInitServiceUnregisterDoesNotEmitEventWhenRegistryWriteFails(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	oldNewSupervisorRegistry := newSupervisorRegistry
	newSupervisorRegistry = func() supervisorRegistry {
		return &fakeSupervisorRegistry{
			entries: []supervisor.CityEntry{{
				Path: cityPath,
				Name: "bright-lights",
			}},
			unregisterErr: errors.New("boom"),
		}
	}
	t.Cleanup(func() {
		newSupervisorRegistry = oldNewSupervisorRegistry
	})

	_, err := mustNewCityInitService(t).Unregister(context.Background(), cityinit.UnregisterRequest{
		CityName: "bright-lights",
	})
	if err == nil || !strings.Contains(err.Error(), "removing \"bright-lights\" from supervisor registry") {
		t.Fatalf("Unregister error = %v, want registry removal failure", err)
	}

	evts, readErr := events.ReadFiltered(filepath.Join(cityPath, ".gc", "events.jsonl"), events.Filter{Type: events.CityUnregisterRequested})
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("ReadFiltered city.unregister_requested: %v", readErr)
	}
	if len(evts) != 0 {
		t.Fatalf("city.unregister_requested events = %d, want 0: %+v", len(evts), evts)
	}
}

func TestCityInitServiceUnregisterMissingCity(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	_, err := mustNewCityInitService(t).Unregister(context.Background(), cityinit.UnregisterRequest{CityName: "missing"})
	if !errors.Is(err, cityinit.ErrNotRegistered) {
		t.Fatalf("Unregister missing error = %v, want ErrNotRegistered", err)
	}

	_, err = mustNewCityInitService(t).Unregister(context.Background(), cityinit.UnregisterRequest{})
	if !errors.Is(err, cityinit.ErrNotRegistered) {
		t.Fatalf("Unregister blank error = %v, want ErrNotRegistered", err)
	}
}
