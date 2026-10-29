//go:build integration

package integration

import (
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/test/toolhome"
)

// Every real bd this suite runs resolves to realBDBinary, directly or through
// the file-store shim (GC_INTEGRATION_REAL_BD). It must be the tool-home
// wrapper, or bd reads the operator's ~/.beads through the real HOME gc needs.
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
