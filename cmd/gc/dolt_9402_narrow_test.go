package main

import (
	"os"
	"path/filepath"
	"testing"
)

// #9402 narrowed regression test. It goes through managedDoltQueryProbe and
// stubs the direct probe, so it uses only names that exist on base.
// A password keyed to 127.0.0.1:3399 in the beads credentials file, with
// GC_DOLT_PASSWORD unset, must send the probe down the direct path.
func TestManagedDoltQueryProbeUsesEndpointKeyedPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(path, []byte("[127.0.0.1:3399]\npassword=pw-keyed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", path)
	t.Setenv("GC_DOLT_PASSWORD", "")

	orig := managedDoltQueryProbeDirectFn
	t.Cleanup(func() { managedDoltQueryProbeDirectFn = orig })
	called := false
	managedDoltQueryProbeDirectFn = func(_, _, _ string) error {
		called = true
		return nil
	}

	err := managedDoltQueryProbe("127.0.0.1", "3399", "root")
	if !called {
		t.Fatalf("direct probe not used for the keyed endpoint; probe fell back to CLI (err=%v)", err)
	}
}
