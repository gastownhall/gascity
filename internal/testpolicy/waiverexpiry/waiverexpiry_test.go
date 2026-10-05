package waiverexpiry_test

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/testpolicy/resourcecensus"
	"github.com/gastownhall/gascity/internal/testpolicy/waiverclock"
	"github.com/gastownhall/gascity/internal/testutil/providerledger"
)

// datedWaivers gathers every dated test-policy waiver in the repository. A new
// dated ledger belongs here, not in a clock read of its own.
func datedWaivers() []waiverclock.Expiry {
	var all []waiverclock.Expiry
	all = append(all, providerledger.Expiries(providerledger.Catalog())...)
	all = append(all, resourcecensus.PolicyExpiries()...)
	all = append(all, beadstest.SkipExpiries()...)
	return all
}

// TestDatedWaiversAgainstToday is the repository's only comparison of a waiver
// date with the wall clock. Its Bazel target is tagged external so it is never
// served from a cache; see the package documentation.
func TestDatedWaiversAgainstToday(t *testing.T) {
	waivers := datedWaivers()
	if len(waivers) == 0 {
		t.Log("no dated waivers remain")
		return
	}
	mode, err := waiverclock.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	report := waiverclock.Check(waivers, time.Now().UTC(), mode)
	for _, warning := range report.Warnings {
		t.Logf("waiver clock: %s", warning)
	}
	for _, fatal := range report.Fatal {
		t.Errorf("waiver clock: %s", fatal)
	}
}
