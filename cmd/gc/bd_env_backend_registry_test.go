package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// writeUnregisteredBackendScope writes an authoritative scope whose metadata
// names a backend no assembly of gc registers.
func writeUnregisteredBackendScope(t *testing.T, backend string) string {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "issue_prefix: demo\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	metadata := `{"database":"beads","backend":"` + backend + `"}`
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

// TestScopeBackendEnvRefusalEnumeratesRegisteredBackends is Invariant 0 on the
// env-projection path: an unregistered backend stops the projection by name,
// and the refusal enumerates what this build registers rather than a list typed
// into a format string.
//
// It also pins that nothing was projected. The failure this guards against is
// not a missing error — it is a partially populated env reaching a bd
// subprocess, which would then resolve a store from whatever the parent process
// happened to be carrying.
func TestScopeBackendEnvRefusalEnumeratesRegisteredBackends(t *testing.T) {
	cityPath := writeUnregisteredBackendScope(t, "sqlite")

	env := map[string]string{}
	used, err := applyCanonicalScopeBackendEnv(env, cityPath, cityPath)
	if err == nil {
		t.Fatal("applyCanonicalScopeBackendEnv accepted an unregistered backend")
	}
	if !used {
		t.Error("used = false, want true — the scope is authoritative and the failure is semantic")
	}
	if !errors.Is(err, contract.ErrUnknownBackend) {
		t.Fatalf("refusal = %v, want it to wrap ErrUnknownBackend", err)
	}
	if len(env) != 0 {
		t.Fatalf("a refused projection left %d env keys behind: %v", len(env), env)
	}

	registered, regErr := contract.RegisteredBackends()
	if regErr != nil {
		t.Fatal(regErr)
	}
	if !strings.Contains(err.Error(), `"sqlite"`) {
		t.Errorf("refusal %q does not name the backend", err)
	}
	for _, name := range registered {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refusal %q omits registered backend %q", err, name)
		}
	}
	if !strings.Contains(err.Error(), contract.BackendNotOpenedGuarantee) {
		t.Errorf("refusal %q drops the data-safety guarantee", err)
	}
}

// TestCityPostgresBackendEnvRefusalEnumeratesRegisteredBackends covers the
// second projection entry point — the one an inherited-city scope takes — which
// has its own switch and therefore its own chance to fall through silently.
func TestCityPostgresBackendEnvRefusalEnumeratesRegisteredBackends(t *testing.T) {
	cityPath := writeUnregisteredBackendScope(t, "mysql")

	env := map[string]string{}
	used, err := applyCityPostgresBackendEnv(env, cityPath)
	if err == nil {
		t.Fatal("applyCityPostgresBackendEnv accepted an unregistered backend")
	}
	if !used {
		t.Error("used = false, want true — an unregistered backend must not fall through to the dolt path")
	}
	if !errors.Is(err, contract.ErrUnknownBackend) {
		t.Fatalf("refusal = %v, want it to wrap ErrUnknownBackend", err)
	}
	if len(env) != 0 {
		t.Fatalf("a refused projection left %d env keys behind: %v", len(env), env)
	}
	if !strings.Contains(err.Error(), `"mysql"`) || !strings.Contains(err.Error(), contract.BackendNotOpenedGuarantee) {
		t.Errorf("refusal %q must name the backend and state the data-safety guarantee", err)
	}
}

// TestEveryRegisteredBackendHasAnEnvironmentProjection closes the gap the
// projector's default arm exists to catch, from the other side.
//
// Both switches sit behind LoadMetadataState, so an unregistered backend can
// never reach their default arm — which means the arm an operator actually
// meets is the one a registrar opens by adding a backend name without adding a
// projection arm for it. This walks the registered set and refuses to let that
// pair drift. Backends may still fail here for their own reasons (an
// unreachable host, an unresolvable credential); the one outcome that is a
// defect is reaching the default arm at all.
func TestEveryRegisteredBackendHasAnEnvironmentProjection(t *testing.T) {
	registered, err := contract.RegisteredBackends()
	if err != nil {
		t.Fatal(err)
	}
	if len(registered) == 0 {
		t.Fatal("this build registers no backends")
	}

	// Metadata each backend accepts, so the call reaches its projection arm
	// rather than stopping at the metadata contract. A name with no entry gets
	// the bare shape, which is exactly the case this test is here to catch.
	valid := map[string]string{
		"dolt":     `{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"hq"}`,
		"doltlite": `{"database":"doltlite","backend":"doltlite","dolt_database":"hq"}`,
		"postgres": `{"database":"beads","backend":"postgres","postgres_host":"db.example.com","postgres_port":"5432","postgres_user":"bd","postgres_database":"beads"}`,
	}
	for _, backend := range registered {
		t.Run(backend, func(t *testing.T) {
			metadata, ok := valid[backend]
			if !ok {
				metadata = `{"database":"beads","backend":"` + backend + `"}`
			}
			cityPath := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
				t.Fatal(err)
			}
			config := "issue_prefix: demo\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"
			if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(metadata), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := applyCanonicalScopeBackendEnv(map[string]string{}, cityPath, cityPath)
			if err != nil && strings.Contains(err.Error(), "has no environment projection") {
				t.Fatalf("backend %q is registered but the env projector has no arm for it: %v", backend, err)
			}
		})
	}
}

// TestUnprojectableBackendErrorNeverReturnsNil exercises the projector's own
// default arm directly.
//
// Both switches sit behind LoadMetadataState, which refuses an unregistered
// backend first, so no metadata file can reach the arm today — it is
// defense-in-depth, and defense-in-depth that is never executed is where a
// silent fall-through hides. The two shapes it must distinguish are an
// unregistered name (the operator's metadata) and a registered name with no
// projection arm (a composition defect), and neither may produce a nil error.
func TestUnprojectableBackendErrorNeverReturnsNil(t *testing.T) {
	registered, err := contract.RegisteredBackends()
	if err != nil {
		t.Fatal(err)
	}
	if len(registered) == 0 {
		t.Fatal("this build registers no backends")
	}

	unregistered := unprojectableBackendError("sqlite", "/city/rigs/demo")
	if unregistered == nil {
		t.Fatal("an unregistered backend produced no error")
	}
	if !errors.Is(unregistered, contract.ErrUnknownBackend) {
		t.Errorf("refusal = %v, want it to wrap ErrUnknownBackend", unregistered)
	}
	for _, want := range []string{`"sqlite"`, "/city/rigs/demo", contract.BackendNotOpenedGuarantee} {
		if !strings.Contains(unregistered.Error(), want) {
			t.Errorf("refusal %q omits %q", unregistered, want)
		}
	}

	// A registered backend reaching the default arm is a composition defect,
	// not bad metadata, and the message must not blame the operator's file.
	defect := unprojectableBackendError(registered[0], "/city")
	if defect == nil {
		t.Fatal("a registered but unprojectable backend produced no error")
	}
	if errors.Is(defect, contract.ErrUnknownBackend) {
		t.Errorf("refusal %q calls a registered backend unknown", defect)
	}
	for _, want := range []string{registered[0], "/city", "registered by this build", contract.BackendNotOpenedGuarantee} {
		if !strings.Contains(defect.Error(), want) {
			t.Errorf("refusal %q omits %q", defect, want)
		}
	}
}
