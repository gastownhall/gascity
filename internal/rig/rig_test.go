package rig

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// stubDeps returns a Deps with all required infra + required funcs filled with
// no-op stubs, so validation passes and Provision reaches its core.
func stubDeps(cityPath string) Deps {
	return Deps{
		FS:       fsys.OSFS{},
		CityPath: cityPath,
		Cfg:      &config.City{},
		ComposePacks: func(string, []config.BoundImport) ([]config.BoundImport, func() error, error) {
			return nil, nil, nil
		},
		InitStore:   func(string, string, string) (bool, error) { return false, nil },
		InitAndHook: func(string, string, string) error { return nil },
		WriteRoutes: func(string, *config.City) error { return nil },
	}
}

func TestValidateDepsRequiresInfra(t *testing.T) {
	if err := validateDeps(stubDeps("/city")); err != nil {
		t.Fatalf("full deps should validate, got: %v", err)
	}
	without := func(mut func(*Deps)) Deps { d := stubDeps("/city"); mut(&d); return d }
	cases := map[string]Deps{
		"missing FS":           without(func(d *Deps) { d.FS = nil }),
		"missing CityPath":     without(func(d *Deps) { d.CityPath = "" }),
		"missing Cfg":          without(func(d *Deps) { d.Cfg = nil }),
		"missing ComposePacks": without(func(d *Deps) { d.ComposePacks = nil }),
		"missing InitStore":    without(func(d *Deps) { d.InitStore = nil }),
		"missing InitAndHook":  without(func(d *Deps) { d.InitAndHook = nil }),
		"missing WriteRoutes":  without(func(d *Deps) { d.WriteRoutes = nil }),
	}
	for name, d := range cases {
		if err := validateDeps(d); err == nil {
			t.Errorf("%s: expected a validation error, got nil", name)
		}
	}
}

func TestProvisionValidatesBeforeRunning(t *testing.T) {
	// Empty Deps must fail at deps validation, never reach the core.
	_, _, err := Provision(Deps{}, ProvisionRequest{Name: "x", Path: "/x"})
	if err == nil {
		t.Fatal("Provision with empty deps should error at validation")
	}
}

func TestProvisionValidatesRequest(t *testing.T) {
	deps := stubDeps("/city")
	for name, req := range map[string]ProvisionRequest{
		"missing name":  {Path: "/x"},
		"missing path":  {Name: "x"},
		"relative path": {Name: "x", Path: "rel/dir"},
	} {
		_, _, err := Provision(deps, req)
		if err == nil {
			t.Errorf("%s: expected a request-validation error, got nil", name)
		}
	}
}

func TestProvisionReachesCoreWhenValid(t *testing.T) {
	// With valid deps + request, Provision runs the flow: --adopt against a
	// missing directory is a core (StatRigPath) error, proving the core ran.
	deps := stubDeps(t.TempDir())
	missing := filepath.Join(t.TempDir(), "missing")
	_, _, err := Provision(deps, ProvisionRequest{Name: "x", Path: missing, Adopt: true})
	if err == nil {
		t.Fatal("expected a core error for --adopt against a missing directory")
	}
	if got, want := err.Error(), "--adopt requires an existing directory: "+missing; got != want {
		t.Fatalf("unexpected core error: got %q, want %q", got, want)
	}
}
