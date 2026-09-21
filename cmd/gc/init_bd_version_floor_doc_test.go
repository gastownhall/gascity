package main

import (
	"bytes"
	"strings"
	"testing"
)

// The --beads-transport help — and docs/reference/cli.md, which is generated
// from it — claimed "Any fresh init, with or without a selector, requires bd >=
// 1.3.0". checkHardDependencies raises the floor to bdFreshProviderMinVersion
// only when an initializing ownership record exists, and the legacy
// `--dolt-host` alias without a selector writes none
// (persistFreshProviderOwnership returns early for it), so that init passes at
// the 1.0.4 floor. The behavior is the intended one — the design doc's "bd
// version floor" section documents it — so the text is what has to change.
func TestBeadsTransportHelpMatchesTheBdVersionFloorTheCodeEnforces(t *testing.T) {
	usage := beadsTransportFlagUsage(t)

	if strings.Contains(usage, "Any fresh init, with or without a selector, requires bd >= "+bdFreshProviderMinVersion) {
		t.Errorf("help still claims an unconditional %s floor: %q", bdFreshProviderMinVersion, usage)
	}
	for _, want := range []string{bdFreshProviderMinVersion, bdMinVersion, "--dolt-host"} {
		if !strings.Contains(usage, want) {
			t.Errorf("help does not mention %q, so it cannot state the alias exception: %q", want, usage)
		}
	}
}

// The exception only exists because the two floors differ. If they are ever
// unified the help above is wrong again, and this is the assertion that says so.
func TestBdVersionFloorsAreDistinct(t *testing.T) {
	if bdMinVersion == bdFreshProviderMinVersion {
		t.Fatalf("bdMinVersion and bdFreshProviderMinVersion are both %q; the --beads-transport help describes two floors", bdMinVersion)
	}
}

// The alias init journals no initializing ownership record, which is the exact
// condition checkHardDependencies raises the floor on. Without a record, the
// floor stays at bdMinVersion.
func TestLegacyDoltHostAliasJournalsNoInitializingOwnership(t *testing.T) {
	city := t.TempDir()
	opts := hostedDoltInitOptions{Host: "db.example", Port: "4406", Database: "hosted", ProjectID: "p1"}
	if !opts.enabled() || opts.selectorRequested() {
		t.Fatalf("fixture is not the legacy alias shape: %+v", opts)
	}
	if err := persistFreshProviderOwnership(city, opts); err != nil {
		t.Fatalf("persistFreshProviderOwnership: %v", err)
	}
	pending, err := providerScopeOwnershipHasInitializingEntry(city)
	if err != nil {
		t.Fatalf("providerScopeOwnershipHasInitializingEntry: %v", err)
	}
	if pending {
		t.Fatalf("the alias init journaled an initializing record, which would raise the floor to %s", bdFreshProviderMinVersion)
	}
}

func beadsTransportFlagUsage(t *testing.T) string {
	t.Helper()
	cmd := newInitCmd(&bytes.Buffer{}, &bytes.Buffer{})
	flag := cmd.Flags().Lookup("beads-transport")
	if flag == nil {
		t.Fatal("init has no --beads-transport flag")
	}
	return flag.Usage
}
