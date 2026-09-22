package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestProviderOwnedCustomTypesEnvMatchesTheLifecycleProjection pins the env the
// types.custom registration runs under.
//
// It is the only registration a provider-owned scope gets — the owned branch of
// initAndHookDir writes no config.yaml — and it is best-effort, so a wrong
// binary produces no failure, just a scope whose every gc bead type fails
// validation. The `bd init` it follows ran through the workspace's pinned BD_BIN
// and the scope's proxied selectors; running the follow-up against whatever bd
// PATH resolves to would talk to a different binary than the one that created
// the store, and for a proxied scope a different one than owns the proxy.
func TestProviderOwnedCustomTypesEnvMatchesTheLifecycleProjection(t *testing.T) {
	city := t.TempDir()
	pinnedBd := filepath.Join(city, "pinned-bd")
	if err := os.WriteFile(pinnedBd, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"pinned\"\n\n[workspace.env]\nBD_BIN = \"" + pinnedBd + "\"\n" +
		"[beads]\nprovider = \"exec:" + gcBeadsBdScriptPath(city) + "\"\n"
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(city, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "proxied-server",
		DoltDatabase: "hq",
	}); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, city); err != nil {
		t.Fatal(err)
	}

	env, err := providerOwnedScopeCustomTypesEnv(city, city)
	if err != nil {
		t.Fatalf("providerOwnedScopeCustomTypesEnv: %v", err)
	}

	want, err := providerLifecycleProcessEnvForScopeInitWithError(city, city, beadsProvider(city))
	if err != nil {
		t.Fatalf("lifecycle env: %v", err)
	}
	wantMap := runtimeEnvEntriesToMap(want)
	// Everything the lifecycle projects, projected the same way. BEADS_DIR is
	// the scope's own and BD_BIN is the workspace pin, both asserted below;
	// export suppression is layered on top and is additive.
	overridden := map[string]bool{"BEADS_DIR": true, "BD_BIN": true}
	for key, value := range wantMap {
		if overridden[key] {
			continue
		}
		if got, ok := env[key]; !ok || got != value {
			t.Errorf("%s = %q (present=%t), want the lifecycle projection's %q", key, got, ok, value)
		}
	}
	if env["BEADS_DOLT_PROXIED_SERVER"] != "1" {
		t.Errorf("BEADS_DOLT_PROXIED_SERVER = %q, want the proxied scope's selector", env["BEADS_DOLT_PROXIED_SERVER"])
	}
	if env["BD_BIN"] != pinnedBd {
		t.Errorf("BD_BIN = %q, want the workspace pin %q", env["BD_BIN"], pinnedBd)
	}
	if env["BEADS_DIR"] != filepath.Join(city, ".beads") {
		t.Errorf("BEADS_DIR = %q, want the scope's own .beads", env["BEADS_DIR"])
	}
	if env["BD_EXPORT_AUTO"] == "" {
		t.Error("export suppression was dropped from the custom-types env")
	}
}

// TestRegisterProviderOwnedScopeCustomTypesRunsTheWorkspacePinnedBd drives the
// real registration rather than its env builder: a city.toml `[workspace.env]
// BD_BIN` pin is the supported versioned-pin shape, and the registration used to
// ignore it and run whatever `bd` PATH resolved to — against a store the pinned
// binary had just created.
func TestRegisterProviderOwnedScopeCustomTypesRunsTheWorkspacePinnedBd(t *testing.T) {
	city := t.TempDir()
	marker := filepath.Join(city, "pinned-bd-invoked")
	pinnedBd := filepath.Join(city, "pinned-bd")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + marker + "\n" +
		"if [ \"$2\" = get ]; then printf '{\"value\":\"\"}\\n'; fi\nexit 0\n"
	if err := os.WriteFile(pinnedBd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"pinned\"\n\n[workspace.env]\nBD_BIN = \"" + pinnedBd + "\"\n" +
		"[beads]\nprovider = \"exec:" + gcBeadsBdScriptPath(city) + "\"\n"
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(city, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "proxied-server",
		DoltDatabase: "hq",
	}); err != nil {
		t.Fatal(err)
	}

	registerProviderOwnedScopeCustomTypes(city, city)

	invocations, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the workspace-pinned bd was never invoked: %v", err)
	}
	for _, want := range []string{"config get --json types.custom", "config set types.custom"} {
		if !strings.Contains(string(invocations), want) {
			t.Errorf("pinned bd invocations = %q, want one containing %q", invocations, want)
		}
	}
}
