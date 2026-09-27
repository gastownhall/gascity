// Linux-only by its filename for the same reason as
// beads_provider_script_scope_lifecycle_linux_test.go: it executes the POSIX
// provider script.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEnvRecordingBd installs a bd that logs the two shared-server switches it
// was handed, so a test can prove what the provider script exported.
func writeEnvRecordingBd(t *testing.T, dir string) (bin, logPath string) {
	t.Helper()
	logPath = filepath.Join(dir, "bd-env.log")
	bin = filepath.Join(dir, "bd")
	body := "#!/bin/sh\nprintf 'BEADS_DOLT_SHARED_SERVER=%s BD_DOLT_SHARED_SERVER=%s args=%s\\n' \"${BEADS_DOLT_SHARED_SERVER-unset}\" \"${BD_DOLT_SHARED_SERVER-unset}\" \"$*\" >> '" + logPath + "'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	return bin, logPath
}

// F9: the provider script runs bd for provider-owned scopes (init, start,
// health, stop). For a proxied scope every one of those must pin bd's
// shared-server mode off, including when the parent process carries
// BEADS_DOLT_SHARED_SERVER=1 and including a READY scope, whose adapter env has
// no GC_BEADS_TRANSPORT (the persisted binding decides the transport).
func TestProviderScriptPinsSharedServerOffForProxiedScopes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready bool
	}{
		{name: "initializing (transport selector present)"},
		{name: "ready (binding decides)", ready: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			writeScopeBeadsMetadata(t, city, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`)
			writeProxiedSidecar(t, city, filepath.Join(city, ".beads", "dolt"))
			bin, logPath := writeEnvRecordingBd(t, city)
			env := providerOwnedScriptEnv(city, city, bin)
			if tc.ready {
				env = removeEnvKey(removeEnvKey(removeEnvKey(env, "GC_BEADS_TRANSPORT"), "GC_BEADS_TARGET"), "BEADS_DOLT_PROXIED_SERVER")
			}
			env = append(env, "BEADS_DOLT_SHARED_SERVER=1")

			out, err := runProviderOwnedScriptOp(t, env, "health")
			if err != nil {
				t.Fatalf("health: %v\n%s", err, out)
			}
			calls := bdCalls(t, logPath)
			if len(calls) == 0 {
				t.Fatalf("health ran no bd\n%s", out)
			}
			for _, call := range calls {
				if !strings.HasPrefix(call, "BEADS_DOLT_SHARED_SERVER=0 BD_DOLT_SHARED_SERVER=false ") {
					t.Errorf("bd ran without the shared-server pin: %s", call)
				}
			}
		})
	}
}

// A direct provider-owned scope is not what the pin is about: bd keeps the
// operator's own resolution for it.
func TestProviderScriptLeavesSharedServerAloneForDirectScopes(t *testing.T) {
	city := t.TempDir()
	writeScopeBeadsMetadata(t, city, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`)
	bin, logPath := writeEnvRecordingBd(t, city)
	env := providerOwnedScriptEnv(city, city, bin)
	env = removeEnvKey(removeEnvKey(removeEnvKey(env, "GC_BEADS_TRANSPORT"), "GC_BEADS_TARGET"), "BEADS_DOLT_PROXIED_SERVER")
	env = removeEnvKey(removeEnvKey(env, "BEADS_DOLT_SHARED_SERVER"), "BD_DOLT_SHARED_SERVER")

	out, _ := runProviderOwnedScriptOp(t, env, "health")
	calls := bdCalls(t, logPath)
	if len(calls) == 0 {
		t.Fatalf("health ran no bd for the direct scope\n%s", out)
	}
	for _, call := range calls {
		if !strings.HasPrefix(call, "BEADS_DOLT_SHARED_SERVER=unset BD_DOLT_SHARED_SERVER=unset ") {
			t.Errorf("direct scope bd was handed a shared-server pin: %s\n%s", call, out)
		}
	}
}
