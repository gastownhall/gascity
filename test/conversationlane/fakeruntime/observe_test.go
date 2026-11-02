package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
// report-activity's last_activity).
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
		runtimecontract.ReqConnectionExec: true, runtimecontract.ReqProvisionBoxWithoutAgent: true,
		runtimecontract.ReqObserveFaultsReadUnknown: true, runtimecontract.ReqObserveCityScoped: true,
		runtimecontract.ReqObserveInventoryHasActivity: true,
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
