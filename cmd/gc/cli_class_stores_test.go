package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestCLIMailStoreServesTheBinding is the unit under the end-to-end mail and
// handoff rows: the route itself, asserted without a command around it.
func TestCLIMailStoreServesTheBinding(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)

	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening the city work store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(work) })

	if got := cliMailStore(work, cfg, cityPath).Store; got == work {
		t.Error("the messaging route returned the work store on a city that has migrated messaging onto its binding")
	}
}

// TestCLIMailStoreIsIdentityWhenNothingRelocates is the compatibility control. A
// city that authors no [storage] must get back the EXACT store value it passed
// in — one-shot callers assert optional capabilities on whatever comes back, so
// a wrapper here would change behavior on every city that has not migrated.
//
// The assertion is on the EMBEDDED store, not on the beads.MailStore around it.
// The wrapper is always a new value, so a row that compared the wrapper would
// pass on nothing at all.
func TestCLIMailStoreIsIdentityWhenNothingRelocates(t *testing.T) {
	cityPath := oneShotCLICity(t, "")
	refuseInfraMigrationSource(t)
	captureCLIStorageStderr(t)

	work := beads.NewMemStore()
	if got := cliMailStore(work, nil, cityPath).Store; got != beads.Store(work) {
		t.Errorf("the one-shot messaging route returned %p, want the work store %p", got, work)
	}
}
