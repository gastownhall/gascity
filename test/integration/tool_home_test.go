//go:build integration

package integration

import (
	"debug/buildinfo"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/test/toolhome"
)

// File-store controls and direct tool commands use realBDBinary, the tool-home
// wrapper, directly or through the file-store shim (GC_INTEGRATION_REAL_BD).
// Split defaults use the raw Go binary only under the isolated HOME contract.
func TestIntegrationRealBdIsTheToolHomeWrapper(t *testing.T) {
	data, err := os.ReadFile(realBDBinary)
	if err != nil {
		t.Fatalf("read realBDBinary %s: %v", realBDBinary, err)
	}
	script := string(data)
	if !strings.HasPrefix(script, "#!/bin/sh\n") || !strings.Contains(script, "\nHOME=") || !strings.Contains(script, "\nexec ") {
		t.Fatalf("realBDBinary %s is not the tool-home wrapper:\n%s", realBDBinary, script)
	}
	if got := parseEnvList(integrationEnv())[integrationRealBDBinaryEnv]; got != realBDBinary {
		t.Fatalf("%s = %q, want the wrapper %q", integrationRealBDBinaryEnv, got, realBDBinary)
	}
}

// The suite's envs carry no host XDG directory or BEADS_*/BD_* value, and pin
// bd's shared-server mode off.
func TestIntegrationEnvCarriesNoHostBdState(t *testing.T) {
	for name, env := range map[string][]string{
		"integrationEnv":     integrationEnv(),
		"integrationEnvDolt": integrationEnvDolt(),
	} {
		vars := parseEnvList(env)
		for k, v := range vars {
			if k != "HOME" && toolhome.IsHomeVar(k) {
				t.Errorf("%s: host %s=%q reaches bd", name, k, v)
			}
		}
		if got := vars[toolhome.SharedServerConfigEnv]; got != "false" {
			t.Errorf("%s: %s = %q, want false", name, toolhome.SharedServerConfigEnv, got)
		}
		if _, ok := vars["BEADS_DOLT_SHARED_SERVER"]; ok {
			t.Errorf("%s: host BEADS_DOLT_SHARED_SERVER reaches bd", name)
		}
	}
}

// Split default initialization sanitizes GC_* variables before invoking bd.
// Its real binary must therefore be directly on PATH under the private HOME.
func TestIntegrationNativeBDEnvKeepsThePrivateHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "gc-home")
	env := integrationEnvFor(t, home, filepath.Join(root, "run"), true)
	native, err := integrationEnvWithNativeBD(env, rawPinnedBDBinary)
	if err != nil {
		t.Fatal(err)
	}
	vars := parseEnvList(native)
	if vars["HOME"] != integrationIsolatedHome(home) || vars[supervisorIsolatedHomeEnv] != "1" {
		t.Fatalf("native bd escaped isolated home: HOME=%q", vars["HOME"])
	}
	bd := filepath.Join(filepath.SplitList(vars["PATH"])[0], "bd")
	target, err := os.Readlink(bd)
	if err != nil || target != rawPinnedBDBinary {
		t.Fatalf("native bd target=%q err=%v", target, err)
	}
	if _, err := buildinfo.ReadFile(bd); err != nil {
		t.Fatalf("native front door is not the real Go binary: %v", err)
	}
	if vars[integrationRealBDBinaryEnv] != realBDBinary {
		t.Fatal("file-shim fallback lost its tool-home wrapper")
	}
}

func TestIntegrationNativeBDEnvRefusesHostHome(t *testing.T) {
	root := t.TempDir()
	base := integrationEnvFor(t, filepath.Join(root, "gc-home"), filepath.Join(root, "run"), true)
	for name, env := range map[string][]string{
		"host home":               replaceEnv(base, "HOME", filepath.Join(root, "operator-home")),
		"missing isolated opt-in": filterEnv(base, supervisorIsolatedHomeEnv),
		"missing gc home":         filterEnv(base, "GC_HOME"),
		"host XDG":                replaceEnv(base, "XDG_CONFIG_HOME", filepath.Join(root, "operator-config")),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := integrationEnvWithNativeBD(env, rawPinnedBDBinary); err == nil {
				t.Fatal("unwrapped bd accepted an environment with host state")
			}
		})
	}
}
