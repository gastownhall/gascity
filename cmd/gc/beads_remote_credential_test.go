package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// writeCredentialCity writes a city whose workspace pins a fake bd, whose
// city.toml carries beadsTOML, and whose scope is attached to the beads http
// backend (127.0.0.1:9) the way `bd connect` writes it.
func writeCredentialCity(t *testing.T, beadsTOML, rigsTOML string) string {
	t.Helper()
	cityPath := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityTOML := fmt.Sprintf("[workspace]\nname = %q\n[workspace.env]\nPATH = %q\n%s\n%s", filepath.Base(cityPath),
		binDir+string(os.PathListSeparator)+"$PATH", beadsTOML, rigsTOML)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	attachRemoteScope(t, cityPath)
	return cityPath
}

// captureBdChildEnv replaces the bd exec runner with a fake that records the
// env overrides each bd child would receive, keyed by its working directory.
func captureBdChildEnv(t *testing.T) func(dir string) map[string]string {
	t.Helper()
	var mu sync.Mutex
	captured := map[string]map[string]string{}
	capture := func(env map[string]string) beads.CommandRunner {
		snapshot := map[string]string{}
		for k, v := range env {
			snapshot[k] = v
		}
		return func(dir, _ string, _ ...string) ([]byte, error) {
			mu.Lock()
			captured[dir] = snapshot
			mu.Unlock()
			return []byte("[]"), nil
		}
	}
	origRunner, origHosted := beadsExecCommandRunnerWithEnv, beadsExecCommandRunnerWithEnvWithoutAmbientBeads
	beadsExecCommandRunnerWithEnv = capture
	beadsExecCommandRunnerWithEnvWithoutAmbientBeads = capture
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv, beadsExecCommandRunnerWithEnvWithoutAmbientBeads = origRunner, origHosted
	})
	return func(dir string) map[string]string {
		mu.Lock()
		defer mu.Unlock()
		return captured[dir]
	}
}

func installCityRemoteCredentialLookup(t *testing.T) {
	t.Helper()
	resetHostedCredentialProbeCache()
	beads.SetRemoteCredentialLookup(cityRemoteCredentialLookup)
	t.Cleanup(func() {
		beads.SetRemoteCredentialLookup(nil)
		resetHostedCredentialProbeCache()
	})
}

// TestBdStoreSubprocessEnvCarriesOnlyItsOwnScopesCredential: two cities (and
// a rig with its own beads_credential) on ONE server, with bd's ambient
// ladder configured for that very server. Each scope's BdStore bd child gets
// its own token in BEADS_HTTP_TOKEN (scoped to the server's host:port), the
// ambient token command blanked, and never another scope's token; nothing
// goes on argv. A city with no credential keeps today's ambient env.
func TestBdStoreSubprocessEnvCarriesOnlyItsOwnScopesCredential(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BEADS_HTTP_TOKEN", "127.0.0.1:9=ambient-token")
	t.Setenv("BEADS_HTTP_TOKEN_COMMAND", "127.0.0.1:9=echo ambient-command-token")
	t.Setenv("CITY_A_TOKEN", "token-a")
	t.Setenv("RIG_TOKEN", "token-rig")
	installCityRemoteCredentialLookup(t)
	childEnv := captureBdChildEnv(t)

	cityA := writeCredentialCity(t, "[beads]\ncredential = \"env:CITY_A_TOKEN\"\n",
		"[[rigs]]\nname = \"own\"\npath = \"rigs/own\"\nbeads_credential = \"env:RIG_TOKEN\"\n")
	rigOwn := filepath.Join(cityA, "rigs", "own")
	attachRemoteScope(t, rigOwn)
	cityB := writeCredentialCity(t, "[beads]\ncredential = \"file:.gc/beads-token\"\n", "")
	if err := os.MkdirAll(filepath.Join(cityB, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityB, ".gc", "beads-token"), []byte("token-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ambientCity := writeCredentialCity(t, "", "")

	for _, run := range []struct {
		name   string
		dir    string
		runner beads.CommandRunner
		want   string
	}{
		{"city A", cityA, bdCommandRunnerForCity(cityA), "token-a"},
		{"city A control", cityA, controlBdCommandRunnerForCity(cityA), "token-a"},
		{"city B", cityB, bdCommandRunnerForCity(cityB), "token-b"},
		{"rig with its own credential", rigOwn, bdCommandRunnerForRig(cityA, nil, rigOwn), "token-rig"},
		{"rig control", rigOwn, controlBdCommandRunnerForRig(cityA, nil, rigOwn), "token-rig"},
	} {
		if _, err := run.runner(run.dir, "bd", "list", "--json"); err != nil {
			t.Fatalf("%s: bd runner error = %v", run.name, err)
		}
		env := childEnv(run.dir)
		if got := env["BEADS_HTTP_TOKEN"]; got != "127.0.0.1:9="+run.want {
			t.Errorf("%s: BEADS_HTTP_TOKEN = %q, want its own token scoped to 127.0.0.1:9", run.name, got)
		}
		if got, ok := env["BEADS_HTTP_TOKEN_COMMAND"]; !ok || got != "" {
			t.Errorf("%s: BEADS_HTTP_TOKEN_COMMAND = %q (set %v), want blanked", run.name, got, ok)
		}
		for key, value := range env {
			for _, foreign := range []string{"token-a", "token-b", "token-rig", "ambient-token"} {
				if foreign != run.want && strings.Contains(value, foreign) {
					t.Errorf("%s: env[%s] carries another scope's credential", run.name, key)
				}
			}
		}
	}

	if _, err := bdCommandRunnerForCity(ambientCity)(ambientCity, "bd", "list", "--json"); err != nil {
		t.Fatalf("ambient city: %v", err)
	}
	if got, ok := childEnv(ambientCity)["BEADS_HTTP_TOKEN"]; ok {
		t.Errorf("city without a credential: BEADS_HTTP_TOKEN override = %q, want bd's ambient ladder untouched", got)
	}
}

// TestBdStoreSubprocessRefusesAFailedScopeCredential: a configured source that
// fails stops the bd child before it runs (no fallback to the ambient
// ladder), with a typed error that names the source but not a value.
func TestBdStoreSubprocessRefusesAFailedScopeCredential(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BEADS_HTTP_TOKEN", "127.0.0.1:9=ambient-token")
	installCityRemoteCredentialLookup(t)
	childEnv := captureBdChildEnv(t)
	city := writeCredentialCity(t, "[beads]\ncredential = \"env:UNSET_CITY_TOKEN\"\n", "")

	_, err := bdCommandRunnerForCity(city)(city, "bd", "list", "--json")
	if !beads.IsRemoteCredentialError(err) || !strings.Contains(err.Error(), "env:UNSET_CITY_TOKEN") {
		t.Fatalf("error = %v, want a *RemoteCredentialError naming env:UNSET_CITY_TOKEN", err)
	}
	if strings.Contains(err.Error(), "ambient-token") {
		t.Fatalf("error %q leaks the ambient token", err)
	}
	if env := childEnv(city); env != nil {
		t.Fatalf("bd child ran with env %v after a credential failure", env)
	}
}
