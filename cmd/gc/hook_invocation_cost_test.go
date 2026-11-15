package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// hookClaimNoWorkCity sets up the shape every managed claim runs in: a session
// bead with an instance token (so the fence reads it), the session's identity
// in the environment (so the drain-pending probe reads it too), and a work query
// that finds nothing.
func hookClaimNoWorkCity(t *testing.T) {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	sessionID := newFenceSessionBead(t, cityDir, session.StateActive, "live-token")
	installFenceWorkQueryProbe(t)
	setFenceClaimEnv(t, cityDir, sessionID, "live-token")
	// Run from inside the city, as an operator shell or a city-rooted session
	// does: city resolution from here also names the rig context the hook never
	// reads, and that cost a full config load of its own.
	t.Chdir(cityDir)
}

// runHookClaimNoWork runs `gc hook --claim --json` and asserts the no-work drain
// every test here measures.
func runHookClaimNoWork(t *testing.T) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr); code != 1 {
		t.Fatalf("gc hook --claim = %d, want 1 (no-work drain without --drain-ack); stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON result: %v\n%s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("result = %+v, want action=drain reason=no_work", result)
	}
}

// TestHookClaimLoadsCityConfigOnce pins `gc hook --claim` to one city-config
// load per invocation, the way TestReadyLoadsCityConfigOnce pins `gc ready`.
//
// The front door loads the config, and the city resolution before it used to
// load it again to name a rig context the hook never reads (rigFromCwdDir).
// Each load composes city.toml and every pack include, ~0.15-0.2 s on
// maintainer-city.
//
// Not parallel: loadCityConfigCalls is process-wide.
func TestHookClaimLoadsCityConfigOnce(t *testing.T) {
	hookClaimNoWorkCity(t)
	before := loadCityConfigCalls.Load()
	runHookClaimNoWork(t)
	if grew := loadCityConfigCalls.Load() - before; grew != 1 {
		t.Fatalf("gc hook --claim loaded the city config %d times, want exactly 1: the claim's steps must reuse the config the front door loaded", grew)
	}
}

// TestHookClaimComposesTheCityConfigOncePerInvocation pins the invocation's
// config memo: whatever helper of a `gc hook` invocation loads the city config,
// the composition happens once, and the scope closes with the command.
//
// Not parallel: composedCityConfigLoads is process-wide.
func TestHookClaimComposesTheCityConfigOncePerInvocation(t *testing.T) {
	hookClaimNoWorkCity(t)
	before := composedCityConfigLoads.Load()
	runHookClaimNoWork(t)
	if grew := composedCityConfigLoads.Load() - before; grew != 1 {
		t.Fatalf("gc hook --claim composed the city config %d times, want exactly 1", grew)
	}
	if currentInvocationConfigMemo() != nil {
		t.Fatal("gc hook left its invocation config memo open after returning")
	}
}

// TestHookClaimDrainAckComposesTheCityConfigOnce pins the memo where it pays:
// the no-work drain-ack, the most frequent exit of an idle seat. The ack decides
// the city's reconciler mode, builds the session provider and releases held
// claims, and each of those loaded the city config on its own after the front
// door had.
//
// Not parallel: composedCityConfigLoads is process-wide.
func TestHookClaimDrainAckComposesTheCityConfigOnce(t *testing.T) {
	hookClaimNoWorkCity(t)
	t.Setenv("GC_SESSION", "fake")
	before := composedCityConfigLoads.Load()
	var stdout, stderr bytes.Buffer
	_ = cmdHookWithOptions(nil, hookCommandOptions{Claim: true, DrainAck: true, JSON: true}, &stdout, &stderr)
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON result: %v\n%s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("result = %+v, want action=drain reason=no_work; stderr: %s", result, stderr.String())
	}
	if grew := composedCityConfigLoads.Load() - before; grew != 1 {
		t.Fatalf("gc hook --claim --drain-ack composed the city config %d times, want exactly 1; stderr: %s", grew, stderr.String())
	}
}
