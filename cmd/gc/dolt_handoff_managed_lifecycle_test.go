package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// handedOffTestCity builds the shape a committed ownership handoff leaves
// behind: a direct city whose Dolt server is bd's, whose beads metadata still
// says dolt_mode "server" because the transport did not change, whose config
// names bd's replacement endpoint, and whose stale gc-managed runtime state
// still claims the server gc used to run. The provider script records every
// operation it is asked for.
type handedOffCity struct {
	path    string
	script  string
	logPath string
}

func newHandedOffTestCity(t *testing.T, journaled bool) handedOffCity {
	t.Helper()
	city := handedOffCity{path: handoffGuardTestCity(t)}
	city.logPath = filepath.Join(city.path, "provider-ops")
	// The provider has to be recognizable as the bd store contract, which is
	// decided by the script's basename, or the city reads as a foreign exec
	// provider and none of the managed-Dolt questions apply to it at all.
	city.script = filepath.Join(city.path, "scripts", "gc-beads-bd.sh")
	if err := os.MkdirAll(filepath.Dir(city.script), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"$BEADS_DIR\" >> \"" + city.logPath + "\"\n"
	if err := os.WriteFile(city.script, []byte(body), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	cityTOML := "[workspace]\nname = \"handed-off\"\n[beads]\nprovider = \"exec:" + city.script + "\"\n"
	if err := os.WriteFile(filepath.Join(city.path, "city.toml"), []byte(cityTOML), 0o644); err != nil { //nolint:gosec // fixture config
		t.Fatal(err)
	}
	writeScopeBeadsMetadata(t, city.path, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`)
	if err := os.MkdirAll(filepath.Join(city.path, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := "issue_prefix: hq\ndolt.auto-start: true\ndolt.mode: server\ndolt.host: 127.0.0.1\ndolt.port: 3307\n" +
		"gc.endpoint_origin: city_canonical\ngc.endpoint_status: verified\n"
	if err := os.WriteFile(filepath.Join(city.path, ".beads", "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if journaled {
		writeCommittedHandoffJournal(t, city.path)
	}
	return city
}

// writeStaleManagedDoltState plants the runtime state the legacy lifecycle
// published before the handoff. `gc dolt-state handoff-stop` deliberately does
// not rewrite it — the captured workspace artifacts are the rollback's to
// restore — so it survives the transfer claiming a process that is gone.
func writeStaleManagedDoltState(t *testing.T, cityPath string) string {
	t.Helper()
	path := managedDoltStatePath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"running":true,"pid":424242,"port":3307,"data_dir":"` + filepath.Join(cityPath, ".beads", "dolt") + `"}`
	if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil { //nolint:gosec // fixture state file
		t.Fatal(err)
	}
	return path
}

func providerOps(t *testing.T, city handedOffCity) string {
	t.Helper()
	data, err := os.ReadFile(city.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(data)
}

// allocate-port is the one managed-Dolt verb that carried no admission check,
// and it is the verb the dolt pack calls first: everything downstream of it
// treats the city as gc's to run.
func TestAllocatePortRefusesAHandedOffCity(t *testing.T) {
	city := handoffGuardTestCity(t)
	journal := writeCommittedHandoffJournal(t, city)

	var stdout, stderr bytes.Buffer
	cmd := newDoltStateCmd(&stdout, &stderr)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"allocate-port", "--city", city})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("allocate-port chose a managed port for a scope bd owns: %q", stdout.String())
	}
	// Cobra prints usage on a failed RunE, so "nothing on stdout" is not the
	// assertion. What must not be there is a port: the caller reads this
	// command's stdout as one.
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if _, err := strconv.Atoi(line); err == nil {
			t.Errorf("a refused allocate-port still printed a port: %q", line)
		}
	}
	for _, want := range []string{"ownership handoff", journal} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("allocate-port refusal does not mention %q:\n%s", want, stderr.String())
		}
	}
}

// managedDoltLifecycleOwned is the predicate every publication path asks, and
// it answered from gc's own ownership journal alone. A handed-off city has no
// journal entry — bd's handoff journal is the record — so the reconcile tick's
// publisher read the scope as gc's.
func TestManagedDoltLifecycleOwnedFollowsTheCommittedHandoff(t *testing.T) {
	legacy := newHandedOffTestCity(t, false)
	owned, err := managedDoltLifecycleOwned(legacy.path)
	if err != nil {
		t.Fatalf("managedDoltLifecycleOwned on a legacy city: %v", err)
	}
	if owned {
		// The fixture publishes a canonical endpoint, so this arm is already
		// not gc's. Keep the assertion honest by saying which shape it is.
		t.Log("fixture city is not gc-managed even without the journal")
	}

	handed := newHandedOffTestCity(t, true)
	owned, err = managedDoltLifecycleOwned(handed.path)
	if err != nil {
		t.Fatalf("managedDoltLifecycleOwned on a handed-off city: %v", err)
	}
	if owned {
		t.Fatal("a handed-off city still reads as gc's managed Dolt lifecycle")
	}
	if err := publishManagedDoltRuntimeStateIfOwned(handed.path); err != nil {
		t.Fatalf("publish on a handed-off city: %v", err)
	}
	if _, err := os.Stat(managedDoltStatePath(handed.path)); !os.IsNotExist(err) {
		t.Fatalf("gc published managed Dolt runtime state for a scope bd owns: %v", err)
	}
}

// The reconcile tick's publisher must not reach its health fan-out for a scope
// bd owns; that fan-out is what republished the runtime state every tick.
func TestManagedDoltPublishTickSkipsAHandedOffCity(t *testing.T) {
	city := newHandedOffTestCity(t, true)
	healthCalled := false
	var stderr bytes.Buffer
	ensureManagedDoltPublishedForRuntime(city.path, &stderr, "test", func(string) error {
		healthCalled = true
		return nil
	}, managedDoltLifecycleOwned, currentResolvableManagedDoltPort)
	if healthCalled {
		t.Fatal("the reconcile tick ran a managed Dolt health preflight for a scope bd owns")
	}
	if strings.Contains(stderr.String(), "preflight") {
		t.Errorf("the tick reported a preflight failure instead of skipping:\n%s", stderr.String())
	}
}

// gc stopped the legacy server during the handoff but left its published
// runtime state behind, still saying running:true. Every later reader — the
// dolt pack, `gc status`, the acceptance assertion that gc raised no second
// server — reads that file as "gc runs a Dolt here".
func TestProviderOwnedLifecycleRetiresTheStaleManagedPublication(t *testing.T) {
	city := newHandedOffTestCity(t, true)
	state := writeStaleManagedDoltState(t, city.path)
	port := filepath.Join(city.path, ".beads", "dolt-server.port")
	if err := os.WriteFile(port, []byte("3307\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ensureBeadsProvider(city.path); err != nil {
		t.Fatalf("provider-owned start: %v", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("gc start left its managed Dolt runtime state published for a scope bd owns: %v", err)
	}
	// bd owns .beads now. Retiring gc's own publication must not reach into
	// the workspace artifacts the handoff captured and a rollback restores.
	if _, err := os.Stat(port); err != nil {
		t.Fatalf("retiring the publication removed bd's port file: %v", err)
	}
	if ops := providerOps(t, city); !strings.Contains(ops, "start ") {
		t.Errorf("the provider-owned start op never ran: %q", ops)
	}

	writeStaleManagedDoltState(t, city.path)
	if err := shutdownBeadsProvider(city.path); err != nil {
		t.Fatalf("provider-owned stop: %v", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("gc stop left its managed Dolt runtime state published for a scope bd owns: %v", err)
	}
	if ops := providerOps(t, city); !strings.Contains(ops, "stop ") {
		t.Errorf("the provider-owned stop op never ran: %q", ops)
	}
}

// gc suppresses bd's built-in Dolt auto-start so a bd command cannot raise a
// rogue server beside the one gc runs. For a scope bd owns there is no server
// of gc's to protect, and the suppression is what left `gc start` with no way
// to bring back the server `gc stop` just retired.
func TestBdRuntimeEnvLetsBdRestartAHandedOffScope(t *testing.T) {
	handed := newHandedOffTestCity(t, true)
	env, err := bdRuntimeEnvWithErrorNoRecovery(handed.path)
	if err != nil {
		t.Fatalf("bdRuntimeEnvWithErrorNoRecovery on a handed-off city: %v", err)
	}
	if got, ok := env["BEADS_DOLT_AUTO_START"]; ok && got == "0" {
		t.Error("gc forbade bd from restarting the Dolt server it owns")
	}
	processEnv, err := cityRuntimeProcessEnvWithError(handed.path)
	if err != nil {
		t.Fatalf("cityRuntimeProcessEnvWithError on a handed-off city: %v", err)
	}
	if got, ok := envEntriesMap(processEnv)["BEADS_DOLT_AUTO_START"]; ok && got == "0" {
		t.Error("the provider lifecycle env forbade bd from restarting the Dolt server it owns")
	}
}

func TestBdRuntimeEnvStillSuppressesAutoStartForAManagedCity(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	city := handoffGuardTestCity(t)
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"managed\"\n"), 0o644); err != nil { //nolint:gosec // fixture config
		t.Fatal(err)
	}
	writeScopeBeadsMetadata(t, city, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`)
	config := "issue_prefix: hq\ndolt.auto-start: false\ndolt.mode: server\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\n"
	if err := os.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := bdRuntimeEnvWithErrorNoRecovery(city)
	if err != nil {
		t.Logf("bdRuntimeEnvWithErrorNoRecovery on a managed city reported %v", err)
	}
	if got := env["BEADS_DOLT_AUTO_START"]; got != "0" {
		t.Errorf("BEADS_DOLT_AUTO_START = %q on a gc-managed city, want \"0\"", got)
	}
}

// The dolt pack's orders fire on every city. On a scope bd owns they have to
// be typed no-ops, and the predicate that decides that was keyed on the
// proxied binding alone — which a handed-off direct city does not have.
func TestDoltCleanupNoOpsOnABdOwnedDirectScope(t *testing.T) {
	city := newHandedOffTestCity(t, true)
	owned, err := cityDoltLifecycleOwnedByBd(city.path)
	if err != nil {
		t.Fatalf("cityDoltLifecycleOwnedByBd: %v", err)
	}
	if !owned {
		t.Fatal("a handed-off city does not read as bd-owned to the dolt pack")
	}
	legacy := newHandedOffTestCity(t, false)
	owned, err = cityDoltLifecycleOwnedByBd(legacy.path)
	if err != nil {
		t.Fatalf("cityDoltLifecycleOwnedByBd on a legacy city: %v", err)
	}
	if owned {
		t.Fatal("a city with no bd binding and no handoff reads as bd-owned")
	}
}

func TestBdOwnedCleanupSkipNamesTheTransportItActuallyHas(t *testing.T) {
	proxied := newBdOwnedCleanupSkipReport(true)
	if proxied.Skipped == nil || proxied.Skipped.Reason != cleanupSkipReasonProxiedScope {
		t.Fatalf("proxied skip report = %+v", proxied.Skipped)
	}
	direct := newBdOwnedCleanupSkipReport(false)
	if direct.Skipped == nil || direct.Skipped.Reason != cleanupSkipReasonBdOwnedScope {
		t.Fatalf("direct skip report = %+v", direct.Skipped)
	}
	if strings.Contains(direct.Skipped.Message, "proxied") {
		t.Errorf("a bd-owned direct scope was described as proxied: %q", direct.Skipped.Message)
	}
}
