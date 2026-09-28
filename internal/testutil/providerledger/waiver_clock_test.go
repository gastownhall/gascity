package providerledger

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testpolicy/waiverclock"
)

func lapsedEntry(expires time.Time) Entry {
	return Entry{
		ID:           "runtime.builtin.example",
		Roles:        []Role{RoleProductionProvider},
		Port:         PortRuntimeProvider,
		Constructors: []SymbolRef{repoSymbol("internal/runtime/example", "NewSeamBacked")},
		Catalog:      runtimeCatalogRef("exact:example"),
		Claims: []ContractClaim{{
			Constructor: repoSymbol("internal/runtime/example", "NewSeamBacked"),
			Contract:    ContractRuntimeProvider,
			Disposition: DispositionWaived,
			Waiver: &Waiver{
				Owner:   "ga-example",
				Expires: expires,
				Reason:  "the production composition has no full shared runtime contract",
			},
		}},
	}
}

// TestValidateLapseFollowsTheClockMode pins the whole point of ga-b51te: a
// lapsed waiver must not fail a bystander's push on the day it lapses. It warns,
// naming its owner, and only goes fatal once the grace window is spent -- or
// immediately, in the strict lanes the owner runs.
func TestValidateLapseFollowsTheClockMode(t *testing.T) {
	expires := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		now         time.Time
		mode        waiverclock.Mode
		wantErr     bool
		wantWarning bool
	}{
		{"unexpired is silent", time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC), waiverclock.ModeGrace, false, false},
		{"valid through its own day", expires, waiverclock.ModeGrace, false, true},
		{"lapsed inside grace warns", time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC), waiverclock.ModeGrace, false, true},
		{"lapsed past grace is fatal", time.Date(2026, time.September, 22, 0, 0, 0, 0, time.UTC), waiverclock.ModeGrace, true, false},
		{"strict is fatal on the first lapsed day", time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC), waiverclock.ModeStrict, true, false},
		{"strict spares an unexpired waiver", expires, waiverclock.ModeStrict, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings, err := Validate([]Entry{lapsedEntry(expires)}, tc.now, tc.mode)
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() = nil error, want a lapse failure (warnings %v)", warnings)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want the lapse to be non-fatal", err)
			}
			if got := len(warnings) > 0; got != tc.wantWarning {
				t.Fatalf("Validate() warnings = %v, want any=%t", warnings, tc.wantWarning)
			}
		})
	}
}

// TestValidateKeepsStructuralProblemsFatalInEveryMode is the other half of the
// split: grace applies to the clock and to nothing else. A missing owner is a
// defect somebody committed, so it fails whoever committed it, always.
func TestValidateKeepsStructuralProblemsFatalInEveryMode(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	for _, mode := range []waiverclock.Mode{waiverclock.ModeGrace, waiverclock.ModeStrict} {
		t.Run(mode.String(), func(t *testing.T) {
			entry := lapsedEntry(now.Add(30 * 24 * time.Hour))
			entry.Claims[0].Waiver.Owner = ""
			if _, err := Validate([]Entry{entry}, now, mode); err == nil {
				t.Fatal("Validate() = nil, want a missing-owner failure in every mode")
			}
		})
	}
}

// TestValidateKeepsTheHorizonFatalInEveryMode guards the check that stops
// someone parking a waiver in 2027. It reads the clock but is self-healing --
// time passing can only make it pass -- so it can never red a bystander and
// must not be softened along with the lapse check.
func TestValidateKeepsTheHorizonFatalInEveryMode(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	for _, mode := range []waiverclock.Mode{waiverclock.ModeGrace, waiverclock.ModeStrict} {
		t.Run(mode.String(), func(t *testing.T) {
			entry := lapsedEntry(now.Add(365 * 24 * time.Hour))
			_, err := Validate([]Entry{entry}, now, mode)
			if err == nil || !strings.Contains(err.Error(), "horizon") {
				t.Fatalf("Validate() error = %v, want a horizon failure in every mode", err)
			}
		})
	}
}

// TestValidateLapseMessageRoutesToTheOwner pins the artifact the 2026-08-26
// incident produced. The message a blocked engineer got was "waiver owned by
// ga-uz5t3a expired 2026-09-07" -- an owner id that resolves to nothing in this
// repo and no next step. Every lapse finding now carries its route.
func TestValidateLapseMessageRoutesToTheOwner(t *testing.T) {
	expires := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	_, err := Validate([]Entry{lapsedEntry(expires)}, time.Date(2026, time.September, 22, 0, 0, 0, 0, time.UTC), waiverclock.ModeGrace)
	if err == nil {
		t.Fatal("Validate() = nil, want a lapse failure")
	}
	for _, want := range []string{
		"ga-example",
		"expired 2026-09-07",
		"fleet-fatal after 2026-09-21",
		"bd show ga-example",
		"GC_WAIVER_CLOCK=strict",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("lapse message is missing %q:\n%v", want, err)
		}
	}
}

// TestValidateReportsOneFindingPerLapsedWaiver keeps the failure output
// proportional to the defect; the census side of this change fixes a duplicate
// that doubled its cliff from 37 findings to 74.
func TestValidateReportsOneFindingPerLapsedWaiver(t *testing.T) {
	expires := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	_, err := Validate([]Entry{lapsedEntry(expires)}, time.Date(2026, time.September, 22, 0, 0, 0, 0, time.UTC), waiverclock.ModeStrict)
	if err == nil {
		t.Fatal("Validate() = nil, want a lapse failure")
	}
	if got := strings.Count(err.Error(), "expired 2026-09-07"); got != 1 {
		t.Fatalf("lapse reported %d times, want 1:\n%v", got, err)
	}
}
