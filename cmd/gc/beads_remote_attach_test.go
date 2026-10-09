package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	bdhttp "github.com/steveyegge/beads/backend/http"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// The shape `bd connect` and bdhttp.Attach write into a scope: metadata.json
// selects the beads http backend (and keeps whatever else it carried), and
// the per-user sidecar http_target.json pins the server and project. There is
// no storage_endpoint and no storage_database. gc must treat that scope as a
// store it does not serve everywhere it dispatches, the way it treats the
// opaque storage binding, by asking the beads registry which backends are
// remote rather than naming one.

// attachRemoteScope writes the Attach shape into dir/.beads with the real
// bdhttp.Attach, after registering the beads http backend the way gc's
// composition root does (idempotent across tests).
func attachRemoteScope(t *testing.T, dir string) {
	t.Helper()
	beads.RegisterRemoteBackends("gc/test")
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse("http://127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	if err := bdhttp.Attach(beadsDir, bdhttp.Target{BaseURL: base, ExpectProjectID: "gc-remote"}); err != nil {
		t.Fatalf("bdhttp.Attach: %v", err)
	}
	if _, err := os.Stat(bdhttp.TargetPath(beadsDir)); err != nil {
		t.Fatalf("Attach wrote no sidecar: %v", err)
	}
}

func readAttachFileForTest(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestAttachWrittenScopeIsAStoreGCDoesNotServe pins the two predicates every
// dispatch site consults: the metadata loader accepts the shape, and the
// complete-binding predicate classifies it as bound without demanding the
// storage_endpoint/storage_database pair (the partial-binding refusal).
func TestAttachWrittenScopeIsAStoreGCDoesNotServe(t *testing.T) {
	scope := t.TempDir()
	attachRemoteScope(t, scope)

	state, ok, err := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(scope))
	if err != nil || !ok {
		t.Fatalf("LoadMetadataState on an Attach-written scope = (%v, %v), want accepted", ok, err)
	}
	if !contract.BackendIsRemote(state.Backend) {
		t.Fatalf("backend %q is not a registered remote backend", state.Backend)
	}
	bound, err := scopeHasCompleteStorageBinding(scopeMetadataJSONPath(scope))
	if err != nil || !bound {
		t.Fatalf("scopeHasCompleteStorageBinding = (%v, %v), want (true, nil) for the Attach shape", bound, err)
	}
}

// TestScopeSkipsManagedDoltForInitOnAttachWrittenScopes is lifecycle dispatch
// (gc start, gc rig add): an Attach-written scope of its own, and a fresh rig
// under an Attach-written city, both skip managed Dolt instead of failing
// metadata parsing.
func TestScopeSkipsManagedDoltForInitOnAttachWrittenScopes(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Run("own scope", func(t *testing.T) {
		scope := t.TempDir()
		attachRemoteScope(t, scope)
		got, err := scopeSkipsManagedDoltForInit(scope, scope)
		if err != nil || !got {
			t.Fatalf("scopeSkipsManagedDoltForInit = (%v, %v), want (true, nil)", got, err)
		}
	})
	t.Run("fresh rig under an attached city", func(t *testing.T) {
		cityPath, rigPath := writeFreshInheritedCity(t, `{"database":"beads","backend":"dolt","dolt_database":"hq"}`)
		attachRemoteScope(t, cityPath)
		got, err := scopeSkipsManagedDoltForInit(cityPath, rigPath)
		if err != nil || !got {
			t.Fatalf("scopeSkipsManagedDoltForInit = (%v, %v), want (true, nil)", got, err)
		}
	})
}

// TestStartBeadsLifecycleDelegatesAttachWrittenCity is gc start on a city
// whose own store was attached with `bd connect`: the managed provider never
// runs and neither file Attach wrote is touched.
func TestStartBeadsLifecycleDelegatesAttachWrittenCity(t *testing.T) {
	cityPath := t.TempDir()
	callLog := filepath.Join(cityPath, "provider-calls.log")
	script := writeManagedBdTestScript(t, "#!/bin/sh\necho \"$1\" >> "+callLog+"\nexit 99\n")
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	attachRemoteScope(t, cityPath)
	metadataBefore := readAttachFileForTest(t, scopeMetadataJSONPath(cityPath))
	targetPath := bdhttp.TargetPath(filepath.Join(cityPath, ".beads"))
	targetBefore := readAttachFileForTest(t, targetPath)
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}

	if err := healthBeadsProviderContext(context.Background(), cityPath, false); err != nil {
		t.Fatalf("healthBeadsProviderContext: %v", err)
	}
	if err := startBeadsLifecycle(cityPath, "test-city", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}
	if _, err := os.Stat(callLog); !os.IsNotExist(err) {
		t.Fatalf("managed provider ran for an Attach-written city, stat err = %v", err)
	}
	if got := readAttachFileForTest(t, scopeMetadataJSONPath(cityPath)); string(got) != string(metadataBefore) {
		t.Fatalf("metadata changed: got %s, want %s", got, metadataBefore)
	}
	if got := readAttachFileForTest(t, targetPath); string(got) != string(targetBefore) {
		t.Fatalf("activation sidecar changed: got %s, want %s", got, targetBefore)
	}
}

// TestInitAndHookDirOnAttachWrittenScopes is the rig-add path: neither a rig
// attached on its own nor a fresh rig under an attached city runs provider
// init, and the fresh rig is left unpinned (it inherits the city's store).
func TestInitAndHookDirOnAttachWrittenScopes(t *testing.T) {
	newRecorder := func(t *testing.T) (string, string) {
		t.Helper()
		callsFile := filepath.Join(t.TempDir(), "provider-calls.log")
		script := filepath.Join(t.TempDir(), "gc-beads-bd")
		body := fmt.Sprintf("#!/bin/sh\nset -eu\nprintf '%%s\\n' \"$*\" >> %q\nexit 99\n", callsFile)
		if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return callsFile, script
	}
	assertNoCalls := func(t *testing.T, callsFile string) {
		t.Helper()
		if data, err := os.ReadFile(callsFile); err == nil {
			t.Fatalf("provider init ran for an Attach-written scope; calls:\n%s", data)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}

	t.Run("rig attached on its own", func(t *testing.T) {
		cityPath, rigPath := writeFreshInheritedCity(t, `{"database":"beads","backend":"dolt","dolt_database":"hq"}`)
		attachRemoteScope(t, rigPath)
		metadataBefore := readAttachFileForTest(t, scopeMetadataJSONPath(rigPath))
		callsFile, script := newRecorder(t)
		t.Setenv("GC_BEADS", "exec:"+script)
		t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
		if err := initAndHookDir(cityPath, rigPath, "fresh"); err != nil {
			t.Fatalf("initAndHookDir: %v", err)
		}
		assertNoCalls(t, callsFile)
		if got := readAttachFileForTest(t, scopeMetadataJSONPath(rigPath)); string(got) != string(metadataBefore) {
			t.Fatalf("rig metadata changed: got %s, want %s", got, metadataBefore)
		}
	})
	t.Run("fresh rig under an attached city", func(t *testing.T) {
		cityPath, rigPath := writeFreshInheritedCity(t, `{"database":"beads","backend":"dolt","dolt_database":"hq"}`)
		attachRemoteScope(t, cityPath)
		callsFile, script := newRecorder(t)
		t.Setenv("GC_BEADS", "exec:"+script)
		t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
		if err := initAndHookDir(cityPath, rigPath, "fresh"); err != nil {
			t.Fatalf("initAndHookDir: %v", err)
		}
		assertNoCalls(t, callsFile)
		if _, err := os.Stat(scopeMetadataJSONPath(rigPath)); !os.IsNotExist(err) {
			t.Fatalf("fresh rig was pinned with local metadata, stat err = %v", err)
		}
	})
}

// TestBdStoreEnvOnAttachWrittenScopes is the off-lane BdStore projection: the
// city, a rig attached on its own with no config.yaml, and a rig with no
// metadata under an attached city all get the opaque projection (no Dolt
// variable, the operator's credentials file kept), with BEADS_DIR naming the
// activation the bd CLI must read. None of them is refused as an
// unprojectable backend.
func TestBdStoreEnvOnAttachWrittenScopes(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	binDir := t.TempDir()
	bdPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(bdPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityTOML := fmt.Sprintf("[workspace]\nname = \"demo\"\n[workspace.env]\nPATH = %q\n", binDir+string(os.PathListSeparator)+"$PATH")
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	attachRemoteScope(t, cityPath)

	projectedKeys := append([]string{}, projectedDoltEnvKeys...)
	projectedKeys = append(projectedKeys, projectedBeadsBackendEnvKeys...)
	for _, key := range projectedKeys {
		t.Setenv(key, "stale-projection")
	}
	credentialsPath := filepath.Join(t.TempDir(), "custom-credentials")
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)
	assertOpaque := func(t *testing.T, env map[string]string, wantBeadsDir string) {
		t.Helper()
		for _, key := range projectedKeys {
			if key == "BEADS_CREDENTIALS_FILE" {
				continue
			}
			if got := env[key]; got != "" {
				t.Errorf("env[%q] = %q, want withheld for a remote scope", key, got)
			}
		}
		if got := env["BEADS_CREDENTIALS_FILE"]; got != credentialsPath {
			t.Errorf("BEADS_CREDENTIALS_FILE = %q, want %q", got, credentialsPath)
		}
		if got := env["BD_BIN"]; got != bdPath {
			t.Errorf("BD_BIN = %q, want workspace-pinned %q", got, bdPath)
		}
		if wantBeadsDir != "" && env["BEADS_DIR"] != wantBeadsDir {
			t.Errorf("BEADS_DIR = %q, want %q", env["BEADS_DIR"], wantBeadsDir)
		}
	}

	cityEnv, err := bdRuntimeEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("bdRuntimeEnvWithError(attached city): %v", err)
	}
	assertOpaque(t, cityEnv, filepath.Join(cityPath, ".beads"))

	attachedRig := filepath.Join(cityPath, "rigs", "attached")
	attachRemoteScope(t, attachedRig)
	rigEnv, err := bdRuntimeEnvForRigWithError(cityPath, nil, attachedRig)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError(attached rig): %v", err)
	}
	assertOpaque(t, rigEnv, filepath.Join(attachedRig, ".beads"))

	inheritingRig := filepath.Join(cityPath, "rigs", "inheriting")
	if err := os.MkdirAll(inheritingRig, 0o700); err != nil {
		t.Fatal(err)
	}
	inheritEnv, err := bdRuntimeEnvForRigWithError(cityPath, nil, inheritingRig)
	if err != nil {
		t.Fatalf("bdRuntimeEnvForRigWithError(inheriting rig): %v", err)
	}
	assertOpaque(t, inheritEnv, filepath.Join(cityPath, ".beads"))
}

// TestCanonicalScopeBackendEnvAcceptsAnAuthoritativeAttachedScope is the
// projector itself (applyCanonicalScopeBackendEnv) on a gc-canonicalized
// scope that was later attached with `bd connect`: the config.yaml still
// resolves authoritative, metadata.json now names the http backend, and the
// projection succeeds as an opaque binding instead of the
// unprojectable-backend refusal.
func TestCanonicalScopeBackendEnvAcceptsAnAuthoritativeAttachedScope(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := writeUnregisteredBackendScope(t, "dolt")
	attachRemoteScope(t, cityPath)
	env := map[string]string{}
	used, err := applyCanonicalScopeBackendEnv(env, cityPath, cityPath)
	if err != nil {
		t.Fatalf("applyCanonicalScopeBackendEnv on an attached scope: %v", err)
	}
	if !used {
		t.Fatal("used = false, want the authoritative scope's projection to answer")
	}
	if got := env["GC_DOLT_PORT"]; got != "" {
		t.Fatalf("GC_DOLT_PORT = %q, want withheld", got)
	}
}
