package rig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestDryRunDoesNotCreateRigOrReachMutations(t *testing.T) {
	deps := stubDeps(t.TempDir())
	req := ProvisionRequest{Name: "preview", Path: filepath.Join(t.TempDir(), "new-rig"), DryRun: true}
	deps.InitStore = func(string, string, string) (bool, error) { t.Fatal("dry run initialized store"); return false, nil }
	deps.InitAndHook = func(string, string, string) error { t.Fatal("dry run installed hooks"); return nil }
	deps.WriteRoutes = func(string, *config.City) error { t.Fatal("dry run wrote routes"); return nil }
	deps.PostProvision = func(ProvisionContext) error { t.Fatal("dry run reloaded runtime"); return nil }
	got, result, err := Provision(deps, req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "preview" || len(result.Steps) == 0 {
		t.Fatalf("missing proposed rig: %+v %+v", got, result)
	}
	if _, err := deps.FS.Stat(req.Path); err == nil {
		t.Fatal("dry run created rig directory")
	}
	if len(deps.Cfg.Rigs) != 0 {
		t.Fatal("dry run mutated input config")
	}
}

func TestAdoptNativeDatabasePrefixBeforePlanning(t *testing.T) {
	for _, tc := range []struct {
		name, explicit, stored, wantErr string
		yaml                            string
		readErr                         error
	}{
		{name: "infer existing namespace", stored: "prod"},
		{name: "matching explicit namespace", stored: "prod", explicit: "prod"},
		{name: "mismatching explicit namespace", stored: "prod", explicit: "other", wantErr: "already has bead prefix"},
		{name: "collision with HQ", stored: "gc", wantErr: "collides with HQ"},
		{name: "unavailable namespace", readErr: errors.New("proxy is stopped"), wantErr: "proxy is stopped"},
		{name: "empty namespace", wantErr: "issue_prefix"},
		{name: "matching YAML mirror", stored: "prod", yaml: "prod"},
		{name: "conflicting YAML mirror", stored: "prod", yaml: "other", wantErr: "conflicts with stored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := stubDeps(t.TempDir())
			deps.Cfg.Workspace.Prefix = "gc"
			path := t.TempDir()
			if err := os.Mkdir(filepath.Join(path, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{"metadata.json": `{"dolt_mode":"proxied-server"}`, "config.yaml": "# native bd keeps its prefix in the database\n"} {
				if err := os.WriteFile(filepath.Join(path, ".beads", name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.yaml != "" {
				if err := os.WriteFile(filepath.Join(path, ".beads", "config.yaml"), []byte("issue_prefix: "+tc.yaml+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			reads := 0
			deps.ReadStorePrefix = func(got string, readOnly bool) (string, error) {
				reads++
				if got != path || !readOnly {
					t.Fatalf("prefix lookup=(%q,%v)", got, readOnly)
				}
				return tc.stored, tc.readErr
			}
			deps.InitStore = func(string, string, string) (bool, error) { t.Fatal("preview initialized store"); return false, nil }
			deps.PrepareAdopt = func(string, string) error { t.Fatal("preview prepared store"); return nil }
			deps.WriteRoutes = func(string, *config.City) error { t.Fatal("preview wrote routes"); return nil }
			deps.PostProvision = func(ProvisionContext) error { t.Fatal("preview reloaded city"); return nil }
			got, _, err := Provision(deps, ProvisionRequest{Name: "renamed-project", Path: path, Adopt: true, DryRun: true, Prefix: tc.explicit})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error=%v; want %s", err, tc.wantErr)
				}
			} else if err != nil || got.EffectivePrefix() != tc.stored {
				t.Fatalf("rig=%+v, error=%v", got, err)
			}
			if reads != 1 {
				t.Fatalf("prefix reads=%d; want1", reads)
			}
			if len(deps.Cfg.Rigs) != 0 {
				t.Fatal("preview mutated input config")
			}
		})
	}
}
