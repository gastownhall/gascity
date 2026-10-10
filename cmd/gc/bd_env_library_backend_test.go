package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	beadsbackend "github.com/steveyegge/beads/backend"
)

// registerLibraryBackendForTest registers name with the linked beads library
// for the duration of the test, standing in for a build's distribution wiring
// (the enterprise build's bdhttp.Register).
func registerLibraryBackendForTest(t *testing.T, name string) {
	t.Helper()
	refuse := func(context.Context, string) (beadsbackend.DoltStorage, error) {
		return nil, errors.New("test backend does not open")
	}
	beadsbackend.Register(name, beadsbackend.Backend{Open: refuse, OpenReadOnly: refuse})
	t.Cleanup(func() { beadsbackend.Deregister(name) })
}

// writeLibraryBackendScope writes an authoritative scope in the shape `bd
// connect <url> --convert-workspace` leaves: metadata names the backend and
// carries no storage-binding fields, because the target lives beside it.
func writeLibraryBackendScope(t *testing.T, backend string) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "issue_prefix: demo\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	metadata := `{"database":"dolt","backend":"` + backend + `","dolt_mode":"server","dolt_database":"hq"}`
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

func TestScopeHasCompleteStorageBindingRecognizesLibraryExtensionBackend(t *testing.T) {
	const name = "gctest-ext"
	cityPath := writeLibraryBackendScope(t, name)

	got, err := scopeHasCompleteStorageBinding(scopeMetadataJSONPath(cityPath))
	if err != nil || got {
		t.Fatalf("before registration scopeHasCompleteStorageBinding() = (%v, %v), want (false, nil)", got, err)
	}

	registerLibraryBackendForTest(t, name)
	got, err = scopeHasCompleteStorageBinding(scopeMetadataJSONPath(cityPath))
	if err != nil || !got {
		t.Fatalf("scopeHasCompleteStorageBinding() = (%v, %v), want (true, nil) for a library extension backend", got, err)
	}
}

// A registered extension does not loosen the partial-binding refusal: storage
// fields that are present must still be complete.
func TestScopeHasCompleteStorageBindingPartialFieldsFailClosedForLibraryExtension(t *testing.T) {
	const name = "gctest-ext"
	registerLibraryBackendForTest(t, name)
	cityPath := t.TempDir()
	path := scopeMetadataJSONPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"backend":"`+name+`","storage_endpoint":"remote"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := scopeHasCompleteStorageBinding(path); err == nil {
		t.Fatal("scopeHasCompleteStorageBinding() accepted a partial binding")
	}
}

// gc projects no backend environment for a library extension backend: bd
// reads the workspace and its own credentials, so every projected backend key
// is withheld rather than pointing bd at a Dolt endpoint.
func TestLibraryExtensionBackendWithholdsProjectedBackendEnv(t *testing.T) {
	const name = "gctest-ext"
	registerLibraryBackendForTest(t, name)
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	for _, tc := range []struct {
		name  string
		apply func(env map[string]string, cityPath string) (bool, error)
	}{
		{name: "scope", apply: func(env map[string]string, cityPath string) (bool, error) {
			return applyCanonicalScopeBackendEnv(env, cityPath, cityPath)
		}},
		{name: "city", apply: applyCityStorageBindingEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := writeLibraryBackendScope(t, name)
			env := map[string]string{"GC_DOLT_HOST": "leftover.example.test", "BEADS_DOLT_SERVER_PORT": "3307"}
			used, err := tc.apply(env, cityPath)
			if err != nil || !used {
				t.Fatalf("projection = (%v, %v), want (true, nil)", used, err)
			}
			for _, key := range []string{
				"GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT",
				"GC_BEADS_BACKEND", "BEADS_BACKEND",
			} {
				if got, ok := env[key]; !ok || got != "" {
					t.Errorf("env[%q] = (%q, present=%v), want it withheld as empty", key, got, ok)
				}
			}
		})
	}
}
