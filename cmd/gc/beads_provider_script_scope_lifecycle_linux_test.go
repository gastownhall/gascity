// This file is Linux-only by its filename, for the same reason as
// beads_provider_missing_scope_stop_linux_test.go: it executes the POSIX
// provider script, and 07-design scopes the proxied lifecycle to Linux.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// providerScriptPath is the real script every test in this file drives. The
// decisions under test are shell decisions — a `cd`, a sidecar read, a case arm
// — so only the shell can prove them.
func providerScriptPath(t *testing.T) string {
	t.Helper()
	script := filepath.Join("..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("provider script not available: %v", err)
	}
	return script
}

// runProviderOwnedScriptOp runs one provider-owned op against the real script.
//
// It is the single subprocess call site for the provider-script tests on purpose:
// the source resource census ratchet is shrink-only, so a new shape is covered by
// another call to this helper rather than another exec.Command.
func runProviderOwnedScriptOp(t *testing.T, env []string, op string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", providerScriptPath(t), op) //nolint:gosec // repository script under test
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// providerOwnedScriptEnv is the environment gc's provider adapter passes for a
// proxied, provider-owned scope.
func providerOwnedScriptEnv(city, scope, bdBin string) []string {
	return append(os.Environ(),
		"GC_CITY_PATH="+city,
		"GC_BEADS_PROVIDER_OWNED=1",
		"GC_BEADS_TRANSPORT=proxied",
		"GC_BEADS_TARGET=local",
		"BEADS_DOLT_PROXIED_SERVER=1",
		"GC_DOLT=",
		"BEADS_DIR="+filepath.Join(scope, ".beads"),
		"BD_BIN="+bdBin,
	)
}

// writeProxiedSidecar plants the bd client-info sidecar that names the physical
// Dolt root a proxied scope's lifecycle commands act on.
func writeProxiedSidecar(t *testing.T, scope, rootPath string) {
	t.Helper()
	beads := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beads, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"root_path": "` + rootPath + `", "idle_timeout": -1}`
	if err := os.WriteFile(filepath.Join(beads, "proxied_server_client_info.json"), []byte(body), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
}

// writeRecordingBd installs a bd that logs its argv and succeeds, so a test can
// assert on the lifecycle commands the script chose to issue.
func writeRecordingBd(t *testing.T, dir string) (bin, logPath string) {
	t.Helper()
	logPath = filepath.Join(dir, "bd-calls.log")
	bin = filepath.Join(dir, "bd")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logPath + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	return bin, logPath
}

func bdCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

// TestProviderScriptRecoverLeavesTheSharedCityProxyAlone pins the invariant that
// a single flaky rig must not cycle the city's Dolt and every other rig's.
//
// `gc beads city migrate-proxied` points a rig's Dolt data dir at the city's
// .beads/dolt, and bd resolves the root for `bd dolt stop` from the sidecar
// rather than from BEADS_DIR. So a rig-scoped recover used to shut down the one
// proxy and Dolt child serving hq and every other rig, under live agents, to
// recover one rig — and the ping that follows cold-starts it while the rig-local
// cause is still there, so the next health pass does it again.
func TestProviderScriptRecoverLeavesTheSharedCityProxyAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		// rootPath is the rig sidecar's root_path, relative to the rig or absolute.
		rigRootPath func(city, rig string) string
		wantStop    bool
	}{
		{
			// Three segments, not two: bd resolves a relative root_path
			// against BEADS_DIR (`<rig>/.beads`), so the city's root is three
			// levels up from a rig at <city>/rigs/api. The two-segment spelling
			// this used to pin resolves to <city>/rigs/.beads/dolt for bd —
			// a root the rig would own — and only matched because the script
			// joined it to the scope dir instead.
			name:        "shared city root via a relative data dir",
			rigRootPath: func(_, _ string) string { return filepath.Join("..", "..", "..", ".beads", "dolt") },
			wantStop:    false,
		},
		{
			name:        "shared city root spelled absolutely",
			rigRootPath: func(city, _ string) string { return filepath.Join(city, ".beads", "dolt") },
			wantStop:    false,
		},
		{
			name:        "the rig's own root is the rig's to cycle",
			rigRootPath: func(_, rig string) string { return filepath.Join(rig, ".beads", "dolt") },
			wantStop:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			rig := filepath.Join(city, "rigs", "api")
			for _, dir := range []string{
				filepath.Join(city, ".beads", "dolt"),
				filepath.Join(rig, ".beads", "dolt"),
			} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			writeProxiedSidecar(t, city, filepath.Join(city, ".beads", "dolt"))
			writeProxiedSidecar(t, rig, tc.rigRootPath(city, rig))

			bin, logPath := writeRecordingBd(t, city)
			out, err := runProviderOwnedScriptOp(t, providerOwnedScriptEnv(city, rig, bin), "recover")
			if err != nil {
				t.Fatalf("recover on the rig: %v\n%s", err, out)
			}

			calls := bdCalls(t, logPath)
			var stopped bool
			for _, call := range calls {
				if strings.HasPrefix(call, "dolt stop") {
					stopped = true
				}
			}
			if stopped != tc.wantStop {
				t.Fatalf("rig recover issued `bd dolt stop` = %v, want %v; bd calls %q\n%s", stopped, tc.wantStop, calls, out)
			}
			// Either way recover must still re-establish the rig's own store.
			var pinged bool
			for _, call := range calls {
				if call == "ping" {
					pinged = true
				}
			}
			if !pinged {
				t.Fatalf("rig recover never pinged the scope; bd calls %q\n%s", calls, out)
			}
		})
	}
}

// TestProviderScriptRecoverStillCyclesTheCityScopesOwnProxy is the control for
// the guard above: the city scope owns the shared root, so its own recover must
// keep retiring it. A guard that silenced the city's recover too would leave
// nothing able to cycle a wedged shared proxy.
func TestProviderScriptRecoverStillCyclesTheCityScopesOwnProxy(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProxiedSidecar(t, city, filepath.Join(city, ".beads", "dolt"))

	bin, logPath := writeRecordingBd(t, city)
	out, err := runProviderOwnedScriptOp(t, providerOwnedScriptEnv(city, city, bin), "recover")
	if err != nil {
		t.Fatalf("recover on the city: %v\n%s", err, out)
	}
	var stopped bool
	for _, call := range bdCalls(t, logPath) {
		if strings.HasPrefix(call, "dolt stop") {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("city recover did not retire its own proxy; bd calls %q\n%s", bdCalls(t, logPath), out)
	}
}
