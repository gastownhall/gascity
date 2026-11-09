package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/runtimecontract"
)

// TestMain runs fakeruntime itself when re-executed by a test as a
// runtime (FAKERUNTIME_MAIN set), so the conformance kit can drive it.
func TestMain(m *testing.M) {
	if os.Getenv("FAKERUNTIME_MAIN") == "1" {
		os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// Kills a fakeruntime observe.v1 profile that does not keep the promises
// it declares: the conformance kit passes every requirement but the ones
// it does not declare (exec, provision, the fault and city-scope tokens,
// report-activity's last_activity, the listing fault the fault token
// gates, and server-lifecycle).
func TestObserveProfileIsConformant(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fakeruntime")
	body := fmt.Sprintf("#!/bin/sh\nFAKERUNTIME_MAIN=1 exec %q --state %q --profile observe.v1 \"$@\"\n", os.Args[0], dir)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := runtimecontract.Run(context.Background(), script, runtimecontract.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.V2Ready {
		t.Errorf("not v2-ready: missing %v", report.V2Missing)
	}
	skip := map[runtimecontract.Code]bool{
		runtimecontract.ReqConnectionExec: true, runtimecontract.ReqConnectionIdleBoundary: true,
		runtimecontract.ReqProvisionBoxWithoutAgent: true,
		runtimecontract.ReqObserveFaultsReadUnknown: true, runtimecontract.ReqObserveCityScoped: true,
		runtimecontract.ReqObserveInventoryHasActivity:  true,
		runtimecontract.ReqDiscoveryFailureIsNotAbsence: true,
		// No server of its own: server-lifecycle is not declared.
		runtimecontract.ReqServerStopIdempotent: true, runtimecontract.ReqServerStartAfterStop: true,
		runtimecontract.ReqServerStopEndsSession: true, runtimecontract.ReqServerStopSparesOtherCities: true,
		// The profile does not declare fs.host.
		runtimecontract.ReqFSHostSharedView: true, runtimecontract.ReqFSHostSameUser: true, runtimecontract.ReqFSHostLocal: true,
		runtimecontract.ReqFSHostRunsWhatItCarries: true, runtimecontract.ReqFSHostAgentPIDOnHost: true,
	}
	for _, res := range report.Results {
		switch {
		case res.Status == runtimecontract.StatusFail:
			t.Errorf("%s failed: %s", res.Code, res.Detail)
		case res.Status == runtimecontract.StatusSkip && !skip[res.Code]:
			t.Errorf("%s skipped: %s", res.Code, res.Detail)
		}
	}
}

// Kills a fresh_only refusal that exits 1: the profile's held-name refusal
// is the contract's exit 73.
func TestObserveProfileFreshOnlyExits73(t *testing.T) {
	dir := t.TempDir()
	start := func(cfg string) int {
		var out, errb strings.Builder
		return run([]string{"--state", dir, "--profile", "observe.v1", "start", "s"}, strings.NewReader(cfg), &out, &errb)
	}
	if code := start(`{"command":"sleep 1"}`); code != 0 {
		t.Fatalf("first start exited %d", code)
	}
	if code := start(`{"command":"sleep 1","fresh_only":true}`); code != exitFreshCollision {
		t.Fatalf("fresh_only start over a held name exited %d, want %d", code, exitFreshCollision)
	}
	if code := start(`{"command":"sleep 1"}`); code != 1 {
		t.Fatalf("a plain start over a held name exited %d, want 1", code)
	}
}
