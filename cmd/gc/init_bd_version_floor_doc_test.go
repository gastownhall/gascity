package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/deps"
)

// The --beads-transport help — and docs/reference/cli.md, which is generated
// from it — states the one bd floor the code enforces: bdMinVersion covers a
// fresh provider-owned init and the legacy `--dolt-host` alias alike (the alias
// journals no initializing ownership record, so no higher floor applies to it).
// The help names no alias exception and no older floor such as 1.0.4.
func TestBeadsTransportHelpMatchesTheBdVersionFloorTheCodeEnforces(t *testing.T) {
	usage := beadsTransportFlagUsage(t)

	if !strings.Contains(usage, "bd >= "+bdMinVersion) {
		t.Errorf("help does not state the bd >= %s floor: %q", bdMinVersion, usage)
	}
	if strings.Contains(usage, "1.0.4") {
		t.Errorf("help still advertises the retired 1.0.4 floor: %q", usage)
	}
}

// The fresh provider-owned floor may equal the init floor but never sit below
// it: checkHardDependencies only ever RAISES the floor for a pending
// provider-owned scope.
func TestBdFreshProviderFloorIsNotBelowTheInitFloor(t *testing.T) {
	if deps.CompareVersions(bdFreshProviderMinVersion, bdMinVersion) < 0 {
		t.Fatalf("bdFreshProviderMinVersion %q is below bdMinVersion %q", bdFreshProviderMinVersion, bdMinVersion)
	}
}

// The alias init journals no initializing ownership record, which is the exact
// condition checkHardDependencies raises the floor on. Without a record, the
// floor stays at bdMinVersion (today the same 1.3.0).
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
