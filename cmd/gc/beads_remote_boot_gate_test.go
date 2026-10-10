package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// The G10 boot capability gate at the composition root
// (beads_remote_boot_gate.go). The per-scope verdict itself is
// beads.CheckRemoteScopeBootGate, exercised against the bd serve stand-in in
// internal/beads; here the seam answers so the tests state the wiring.

// stubRemoteBootGate makes refuse (by scope root) fail the gate and records
// every scope the gate was asked about.
func stubRemoteBootGate(t *testing.T, refuse map[string]bool) *[]string {
	t.Helper()
	return stubRemoteBootGateVerdicts(t, refuse, nil)
}

// stubRemoteBootGateVerdicts is stubRemoteBootGate with a second set of
// scopes whose server is down: a transport failure, which warns and starts.
func stubRemoteBootGateVerdicts(t *testing.T, refuse, down map[string]bool) *[]string {
	t.Helper()
	var asked []string
	previous := checkRemoteScopeBootGate
	checkRemoteScopeBootGate = func(_ context.Context, _ string, scopeRoot string) (contract.PreflightCheckResult, bool, error) {
		asked = append(asked, scopeRoot)
		switch {
		case refuse[scopeRoot]:
			summary := "server does not advertise required capabilities: issues.casMetadata"
			return contract.NewPreflightCheckResult(contract.PreflightCheckWireCompat, contract.PreflightCheckFail, summary, contract.PreflightDetails{}), true,
				&beads.RemoteCapabilityGateError{ScopeRoot: scopeRoot, Backend: "http", Check: "wire_compat", Summary: summary, MissingRequired: []string{"issues.casMetadata"}}
		case down[scopeRoot]:
			summary := "server handshake failed (unreachable): dial tcp 127.0.0.1:9: connect: connection refused"
			return contract.NewPreflightCheckResult(contract.PreflightCheckWireCompat, contract.PreflightCheckWarn, summary, contract.PreflightDetails{}), true,
				&beads.RemoteScopeUnreachableError{ScopeRoot: scopeRoot, Backend: "http", Summary: summary}
		}
		return contract.NewPreflightCheckResult(contract.PreflightCheckWireCompat, contract.PreflightCheckPass, "all required capabilities advertised", contract.PreflightDetails{}), true, nil
	}
	t.Cleanup(func() { checkRemoteScopeBootGate = previous })
	return &asked
}

func TestRemoteBeadsBootGateRefusesAFailingScopeAndSkipsUnderOff(t *testing.T) {
	t.Setenv("GC_BEADS_FORCE_FALLBACK", "")
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "remote")
	cfg := &config.City{Rigs: []config.Rig{{Name: "remote", Path: "rigs/remote"}, {Name: "pathless"}}}

	asked := stubRemoteBootGate(t, map[string]bool{rigPath: true})
	err := remoteBeadsBootGate(context.Background(), cityPath, cfg, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "issues.casMetadata") || !strings.Contains(err.Error(), "wire_compat") {
		t.Fatalf("gate = %v, want the rig's wire_compat refusal naming the token", err)
	}
	if got := strings.Join(*asked, ","); got != cityPath+","+rigPath {
		t.Fatalf("gate asked about %q, want the city and the rig made absolute", got)
	}

	off := &config.City{Beads: config.BeadsConfig{NativeTransport: "off"}, Rigs: cfg.Rigs}
	*asked = nil
	if err := remoteBeadsBootGate(context.Background(), cityPath, off, io.Discard); err != nil || len(*asked) != 0 {
		t.Fatalf("gate under native_transport=off = %v after asking %v, want no gate and no dial", err, *asked)
	}
	t.Setenv("GC_BEADS_FORCE_FALLBACK", "1")
	if err := remoteBeadsBootGate(context.Background(), cityPath, cfg, io.Discard); err != nil || len(*asked) != 0 {
		t.Fatalf("gate under GC_BEADS_FORCE_FALLBACK = %v after asking %v, want none", err, *asked)
	}
}

// TestStartStandaloneRefusesARemoteScopeFailingTheBootGate: gc start stops at
// the gate, by name, before any bead store starts.
func TestStartStandaloneRefusesARemoteScopeFailingTheBootGate(t *testing.T) {
	cityPath, opsLog := newForegroundStartLockCity(t, "0")
	stubRemoteBootGate(t, map[string]bool{cityPath: true})
	oldDryRun := dryRunMode
	dryRunMode = false
	t.Cleanup(func() { dryRunMode = oldDryRun })

	var stdout, stderr bytes.Buffer
	if code := doStartStandalone([]string{cityPath}, true, &stdout, &stderr); code != 1 {
		t.Fatalf("doStartStandalone exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"boot capability gate (wire_compat)", "issues.casMetadata", `native_transport = "off"`} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr does not name %q:\n%s", want, stderr.String())
		}
	}
	assertRefusedCityUntouched(t, cityPath, opsLog)
}

// TestReconcileCitiesRefusesARemoteScopeFailingTheBootGate: the supervisor
// records the refusal and publishes nothing.
func TestReconcileCitiesRefusesARemoteScopeFailingTheBootGate(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cityPath, opsLog := newForegroundStartLockCity(t, "0")
	stubRemoteBootGate(t, map[string]bool{cityPath: true})

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "gated-city"); err != nil {
		t.Fatal(err)
	}
	supRec := events.NewFake()
	registry := newCityRegistry()
	registry.SetSupervisorRecorder(supRec)
	if err := registry.StorePendingRequestID(cityPath, "req-gated"); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	reconcileCities(context.Background(), reg, registry, supervisor.PublicationConfig{}, &stdout, &stderr)

	if len(supRec.Events) != 1 {
		t.Fatalf("recorded %d supervisor events, want 1; stderr=%s", len(supRec.Events), stderr.String())
	}
	var payload api.RequestFailedPayload
	if err := json.Unmarshal(supRec.Events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ErrorCode != "remote_capability_gate" {
		t.Fatalf("error code = %q, want remote_capability_gate", payload.ErrorCode)
	}
	registry.ReadCallback(func(cities map[string]*managedCity, _ map[string]cityInitProgress, initFailures map[string]*initFailRecord, _ map[string]*panicRecord) {
		if cities[cityPath] != nil {
			t.Error("a gated city was published")
		}
		if rec := initFailures[cityPath]; rec == nil || !strings.Contains(rec.lastError, "issues.casMetadata") {
			t.Errorf("init failure = %+v, want the gate refusal recorded", rec)
		}
	})
	assertRefusedCityUntouched(t, cityPath, opsLog)
}

// TestRemoteWireCompatDoctorCheckNamesTheGate: gc doctor registers the check
// only for a city with a remote scope, under the name wire_compat, and fails
// it with the scope's refusal.
func TestRemoteWireCompatDoctorCheckNamesTheGate(t *testing.T) {
	t.Setenv("GC_BEADS_FORCE_FALLBACK", "")
	local := t.TempDir()
	if c := newRemoteWireCompatDoctorCheck(local, &config.City{}); c != nil {
		t.Fatalf("a city without a remote scope registered %q", c.Name())
	}

	cityPath := t.TempDir()
	attachRemoteScope(t, cityPath)
	check := newRemoteWireCompatDoctorCheck(cityPath, &config.City{})
	if check == nil || check.Name() != "wire_compat" {
		t.Fatalf("check = %v, want one named wire_compat", check)
	}
	stubRemoteBootGate(t, map[string]bool{cityPath: true})
	result := check.Run(&doctor.CheckContext{CityPath: cityPath})
	if result.Status != doctor.StatusError || result.Name != "wire_compat" {
		t.Fatalf("result = %+v, want an error under wire_compat", result)
	}
	if details := strings.Join(result.Details, "\n"); !strings.Contains(details, "issues.casMetadata") || !strings.Contains(details, cityPath) {
		t.Fatalf("details %q do not name the scope and the missing token", details)
	}

	stubRemoteBootGate(t, nil)
	if result := check.Run(&doctor.CheckContext{CityPath: cityPath}); result.Status != doctor.StatusOK {
		t.Fatalf("passing gate result = %+v, want OK", result)
	}
}

// TestRemoteBeadsBootGateStartsPastAnOutage: an unreachable server is a WARN
// naming the scope, never a refusal, whether it is one rig's server or every
// scope's; a structural failure on another scope still refuses, alone.
func TestRemoteBeadsBootGateStartsPastAnOutage(t *testing.T) {
	t.Setenv("GC_BEADS_FORCE_FALLBACK", "")
	cityPath := t.TempDir()
	downRig := filepath.Join(cityPath, "rigs", "down")
	upRig := filepath.Join(cityPath, "rigs", "up")
	cfg := &config.City{Rigs: []config.Rig{{Name: "down", Path: "rigs/down"}, {Name: "up", Path: "rigs/up"}}}

	stubRemoteBootGateVerdicts(t, nil, map[string]bool{downRig: true})
	var stderr bytes.Buffer
	if err := remoteBeadsBootGate(context.Background(), cityPath, cfg, &stderr); err != nil {
		t.Fatalf("one rig's server down refused the city: %v", err)
	}
	warning := stderr.String()
	if !strings.Contains(warning, downRig) || !strings.Contains(warning, `rig "down"`) || strings.Contains(warning, upRig) {
		t.Fatalf("warning %q, want one naming only the down rig", warning)
	}
	if strings.Contains(warning, "native_transport") {
		t.Fatalf("an outage's warning suggests native_transport: %q", warning)
	}

	stubRemoteBootGateVerdicts(t, nil, map[string]bool{cityPath: true, downRig: true, upRig: true})
	stderr.Reset()
	if err := remoteBeadsBootGate(context.Background(), cityPath, cfg, &stderr); err != nil {
		t.Fatalf("every server down refused the city: %v", err)
	}
	if got := strings.Count(stderr.String(), "warning: remote beads "); got != 3 {
		t.Fatalf("warned %d times, want once per scope:\n%s", got, stderr.String())
	}

	stubRemoteBootGateVerdicts(t, map[string]bool{upRig: true}, map[string]bool{downRig: true})
	stderr.Reset()
	err := remoteBeadsBootGate(context.Background(), cityPath, cfg, &stderr)
	var refusal *beads.RemoteCapabilityGateError
	if !errors.As(err, &refusal) || refusal.ScopeRoot != upRig || strings.Contains(err.Error(), downRig) {
		t.Fatalf("gate = %v, want the structural refusal of the up rig alone", err)
	}
}

// TestBootGateRefusalsAreStructuralInitFailures: the supervisor classifies
// every gate refusal as structural (no retry fixes a missing capability, a
// mismatched project or a rejected credential), and never sees an outage as
// an init failure at all.
func TestBootGateRefusalsAreStructuralInitFailures(t *testing.T) {
	refusal := &beads.RemoteCapabilityGateError{ScopeRoot: "/city", Backend: "http", Check: "wire_compat", Summary: "server owns project \"a\", scope expects \"b\""}
	if !isStructuralInitFailureMessage(refusal.Error()) {
		t.Fatalf("gate refusal %q is not classified structural", refusal)
	}
	down := &beads.RemoteScopeUnreachableError{ScopeRoot: "/city", Backend: "http", Summary: "connection refused"}
	if isStructuralInitFailureMessage(down.Error()) {
		t.Fatalf("an outage %q is classified structural", down)
	}
}

// TestRemoteWireCompatDoctorCheckWarnsForAnOutage: gc doctor reports an
// unreachable server as a warning naming the scope, not as a start refusal.
func TestRemoteWireCompatDoctorCheckWarnsForAnOutage(t *testing.T) {
	t.Setenv("GC_BEADS_FORCE_FALLBACK", "")
	cityPath := t.TempDir()
	attachRemoteScope(t, cityPath)
	check := newRemoteWireCompatDoctorCheck(cityPath, &config.City{})
	stubRemoteBootGateVerdicts(t, nil, map[string]bool{cityPath: true})
	result := check.Run(&doctor.CheckContext{CityPath: cityPath})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("result = %+v, want a warning", result)
	}
	if details := strings.Join(result.Details, "\n"); !strings.Contains(details, cityPath) || strings.Contains(details, "native_transport") {
		t.Fatalf("details %q, want the scope named and no native_transport remedy", details)
	}
}
