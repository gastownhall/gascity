//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestStorageSplitGuidedOnAStoppedManagedDoltCity is S21 for the guided split
// (S5-H): a stopped managed bd/Dolt city, the default shape, is split by
// `gc storage split --fleet-stopped` alone. The window starts the city's own
// provider and every open takes the native lane; the snapshot tags the Dolt
// database; city.toml gets [storage] and the journal its bytes from before;
// the edge payload reaches the binding; the rig is imported; the provider is
// stopped again; and the reverse restores city.toml byte for byte.
func TestStorageSplitGuidedOnAStoppedManagedDoltCity(t *testing.T) {
	city := newManagedDoltWindowCity(t)
	stubInfraControllerPing(t, 0)
	stubSessionInventory(t, nil, true, nil)
	stubOtherCities(t)
	// The fixture's session beads are bare rows, which v2's boot census
	// refuses (S5-F's check, pinned on its own); this test is the window,
	// the lanes and the split's writes.
	stubSplitPreflightLegs(t, nil)
	prevProbe := minGCVersionPathProbe
	minGCVersionPathProbe = func(*config.City) (string, string, error) { return "", "", nil }
	t.Cleanup(func() { minGCVersionPathProbe = prevProbe })
	var lanes []string
	prevOpen := openInfraMigrationSource
	openInfraMigrationSource = func(cityPath string) (beads.Store, error) {
		store, err := prevOpen(cityPath)
		if w := activeMigrationWindow(cityPath); w != nil && w.laneKnown {
			lanes = append(lanes, w.lane.Store)
		}
		return store, err
	}
	t.Cleanup(func() { openInfraMigrationSource = prevOpen })

	tomlPath := filepath.Join(city.cityPath, "city.toml")
	before, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(city.cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	request := storageOperatorRequest{CityPath: city.cityPath, Cfg: cfg, FleetStopped: true, TimeBudget: storageDefaultTimeBudget}
	var stdout, stderr bytes.Buffer
	if code := doStorageSplit(context.Background(), request, splitOptions{}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc storage split = %d:\n%s\n%s", code, stdout.String(), stderr.String())
	}
	if len(lanes) == 0 {
		t.Fatal("no work-store open went through the window")
	}
	for _, lane := range lanes {
		if lane != beads.BeadsStoreNameNativeDoltStore {
			t.Fatalf("a work-store open in the window took %s, want the native lane: %v", lane, lanes)
		}
	}
	if pid := managedDoltRuntimePID(t, city.cityPath, city.port); pid != 0 || doltPortReachable(strconv.Itoa(city.port)) {
		t.Fatalf("the provider the window started is still up (pid %d)", pid)
	}
	journal, present, err := readStorageSplitJournal(city.cityPath)
	if err != nil || !present || journal.Phase != splitPhaseComplete {
		t.Fatalf("the split journal = %+v, %t, %v", journal, present, err)
	}
	if got, _, ok := readSplitJournalCityTOML(city.cityPath); !ok || !bytes.Equal(got, before) {
		t.Fatal("the journal does not prove city.toml's bytes from before the split")
	}
	if !strings.Contains(stdout.String(), "dolt tag") && !strings.Contains(stdout.String(), "tag") {
		t.Errorf("the snapshot did not tag the Dolt database:\n%s", stdout.String())
	}
	split, err := loadCityConfigWithoutBuiltinPackRefresh(city.cityPath, io.Discard)
	if err != nil || !splitStorageMatches(split) {
		t.Fatalf("city.toml after the split: %v", err)
	}
	target := mustResolveInfraTarget(t, city.cityPath, split)
	binding, err := openInfraBindingReadOnly(target)
	if err != nil {
		t.Fatal(err)
	}
	reader, ok := binding.(beads.DepMetadataReader)
	if !ok {
		t.Fatalf("binding %T cannot be asked about payloads", binding)
	}
	got, carried, err := reader.DepMetadata(city.from, city.to)
	_ = closeBeadStoreHandle(binding)
	if err != nil || !carried || got != city.payload {
		t.Fatalf("the binding's edge carries (%q, %v, %v), want the gate %q", got, carried, err, city.payload)
	}
	for _, r := range censusRigs(city.cityPath, split, target) {
		if r.blocking() {
			t.Fatalf("rig %s after the split: %s %v", r.Rig, r.State, r.Err)
		}
	}

	// The reverse into native Dolt refuses the payload-carrying edge (S5-G:
	// native Dolt has no edge-payload writer yet), writing nothing, so the
	// split city stays as it is; the byte-for-byte restore of city.toml is
	// pinned over a file store (TestStorageSplitThenReverseRestoresTheBytes).
	splitTOML, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	request.Cfg = split
	if code := doStorageReverse(context.Background(), request, reverseOptions{Mode: reverseToWork}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "edge-payload writer") {
		t.Fatalf("--to-work over a payload edge into native Dolt = %d:\n%s", code, stderr.String())
	}
	if after, err := os.ReadFile(tomlPath); err != nil || !bytes.Equal(after, splitTOML) {
		t.Fatalf("the refused reverse changed city.toml (%v)", err)
	}
}
