package resourcecensus

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testpolicy/waiverclock"
)

// lapsedPolicyAndLedger dates the same debt row in both the bootstrap policy and
// the ledger. Dating only one of them would also trip comparePolicyFields, and
// this file is about the clock, not about drift between the two.
func lapsedPolicyAndLedger(expires string) (policy, ledger Ledger) {
	policy = validLedger(Census{})
	policy.Debt[0].Expires = expires
	ledger = cloneLedger(policy)
	return policy, ledger
}

// TestValidateLapseFollowsTheClockMode is the census half of ga-b51te. The
// census runs untagged, so it lands in unit-core, which .githooks/pre-push
// runs -- a date passing here reds every Go-touching push in the fleet. It now
// warns first and names its owner, and only the owner's strict lanes fail on
// the first lapsed day.
func TestValidateLapseFollowsTheClockMode(t *testing.T) {
	t.Parallel()

	// fixedNow is 2026-07-13.
	for _, tc := range []struct {
		name        string
		expires     string
		mode        waiverclock.Mode
		wantErr     bool
		wantWarning bool
	}{
		{"distant expiry is silent", "2026-10-01", waiverclock.ModeGrace, false, false},
		{"valid through its own day", "2026-07-13", waiverclock.ModeGrace, false, true},
		{"lapsed inside grace warns", "2026-07-12", waiverclock.ModeGrace, false, true},
		{"lapsed past grace is fatal", "2026-06-01", waiverclock.ModeGrace, true, false},
		{"strict is fatal on the first lapsed day", "2026-07-12", waiverclock.ModeStrict, true, false},
		{"strict spares an unexpired row", "2026-07-13", waiverclock.ModeStrict, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			policy, ledger := lapsedPolicyAndLedger(tc.expires)
			warnings, err := validateAgainstPolicy(policy, ledger, Census{}, fixedNow(), tc.mode)
			if tc.wantErr && err == nil {
				t.Fatalf("validateAgainstPolicy() = nil error, want a lapse failure (warnings %v)", warnings)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateAgainstPolicy() error = %v, want the lapse to be non-fatal", err)
			}
			if got := len(warnings) > 0; got != tc.wantWarning {
				t.Fatalf("validateAgainstPolicy() warnings = %v, want any=%t", warnings, tc.wantWarning)
			}
		})
	}
}

// TestValidateReportsOneFindingPerLapsedRow pins the fix for a duplicate that
// doubled the census cliff. Every row was checked once as a bootstrap policy row
// and again as a ledger row, and comparePolicyFields already forces the two
// expires strings equal -- so a shared date lapsing reported 74 findings for 37
// rows. The second copy was pure noise on top of an already fleet-wide failure.
func TestValidateReportsOneFindingPerLapsedRow(t *testing.T) {
	t.Parallel()

	policy, ledger := lapsedPolicyAndLedger("2026-06-01")
	_, err := validateAgainstPolicy(policy, ledger, Census{}, fixedNow(), waiverclock.ModeStrict)
	if err == nil {
		t.Fatal("validateAgainstPolicy() = nil, want a lapse failure")
	}
	if got := strings.Count(err.Error(), "expired 2026-06-01"); got != 1 {
		t.Fatalf("lapse reported %d times, want 1:\n%v", got, err)
	}
}

// TestValidateReportsOneFindingPerLapsedMediumRow covers the same duplicate on
// the medium path, which counted a row three times: once as a policy row, once
// as a ledger row, and once more from validateMediumOwners.
func TestValidateReportsOneFindingPerLapsedMediumRow(t *testing.T) {
	t.Parallel()

	policy := validLedger(Census{})
	policy.Medium = []MediumOwner{{
		PackageDir:      "internal/example",
		PackageName:     "example",
		Owner:           "TestExample",
		Resources:       []Resource{ResourceSubprocess},
		OwnerBead:       "ga-example",
		Invariant:       "the owning runnable cleans up its own subprocesses",
		ResourceOwner:   "ga-example owns this runnable",
		MigrationTarget: "D1/D2",
		Expires:         "2026-06-01",
	}}
	ledger := cloneLedger(policy)
	census := Census{Runnables: []RunnableOwner{{
		PackageDir:  "internal/example",
		PackageName: "example",
		Owner:       "TestExample",
	}}}

	_, err := validateAgainstPolicy(policy, ledger, census, fixedNow(), waiverclock.ModeStrict)
	if err == nil {
		t.Fatal("validateAgainstPolicy() = nil, want a lapse failure")
	}
	if got := strings.Count(err.Error(), "expired 2026-06-01"); got != 1 {
		t.Fatalf("lapse reported %d times, want 1:\n%v", got, err)
	}
}

// TestValidateKeepsStructuralProblemsFatalInEveryMode is the other half of the
// split: grace bounds what a bystander pays for a passing date and nothing else.
// A missing owner or a malformed date needs a code change to appear, so it fails
// whoever made that change, in every mode.
func TestValidateKeepsStructuralProblemsFatalInEveryMode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mutate func(*Ledger)
		want   string
	}{
		{"missing owner bead", func(l *Ledger) { l.Debt[0].OwnerBead = "" }, "owner_bead is required"},
		{"missing migration target", func(l *Ledger) { l.Debt[0].MigrationTarget = "" }, "migration_target is required"},
		{"malformed expiry", func(l *Ledger) { l.Debt[0].Expires = "07/12/2026" }, "must use YYYY-MM-DD"},
		{"empty expiry", func(l *Ledger) { l.Debt[0].Expires = "" }, "must use YYYY-MM-DD"},
	} {
		for _, mode := range []waiverclock.Mode{waiverclock.ModeGrace, waiverclock.ModeStrict} {
			t.Run(tc.name+"/"+mode.String(), func(t *testing.T) {
				t.Parallel()
				policy := validLedger(Census{})
				ledger := cloneLedger(policy)
				tc.mutate(&policy)
				tc.mutate(&ledger)
				_, err := validateAgainstPolicy(policy, ledger, Census{}, fixedNow(), mode)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("validateAgainstPolicy() error = %v, want containing %q in every mode", err, tc.want)
				}
			})
		}
	}
}

// TestValidateLapseMessageRoutesToTheOwner pins what a blocked engineer reads.
// The 2026-08-26 wording was "expired 2026-09-07" and nothing else, which is why
// the fleet cleared this three times without the owner ever hearing about it.
func TestValidateLapseMessageRoutesToTheOwner(t *testing.T) {
	t.Parallel()

	policy, ledger := lapsedPolicyAndLedger("2026-06-01")
	_, err := validateAgainstPolicy(policy, ledger, Census{}, fixedNow(), waiverclock.ModeGrace)
	if err == nil {
		t.Fatal("validateAgainstPolicy() = nil, want a lapse failure")
	}
	for _, want := range []string{
		"debt baseline scope=untagged resource=subprocess",
		"expired 2026-06-01",
		"fleet-fatal after 2026-06-15",
		"bd show P0.4",
		"GC_WAIVER_CLOCK=strict",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("lapse message is missing %q:\n%v", want, err)
		}
	}
}

// TestValidateWarnsBeforeARowLapses is the point of the warn-ahead window: the
// owner should hear about a date while there is still time to act on it, rather
// than discovering it from a red pre-push on the morning it passes.
func TestValidateWarnsBeforeARowLapses(t *testing.T) {
	t.Parallel()

	// fixedNow is 2026-07-13; the warn window opens 14 days ahead.
	policy, ledger := lapsedPolicyAndLedger("2026-07-20")
	warnings, err := validateAgainstPolicy(policy, ledger, Census{}, fixedNow(), waiverclock.ModeStrict)
	if err != nil {
		t.Fatalf("validateAgainstPolicy() error = %v, want an unexpired row to pass even in strict mode", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "expires 2026-07-20") {
		t.Fatalf("warnings = %v, want one approaching-expiry warning", warnings)
	}
}

// TestValidateClockIsIndependentOfTheOtherChecks keeps the clock from masking a
// real regression. validateManifestAgainstPolicy returns early on any structural
// problem, so a lapse must not be the thing that short-circuits a growth check
// somebody actually needs to see.
func TestValidateClockIsIndependentOfTheOtherChecks(t *testing.T) {
	t.Parallel()

	policy, ledger := lapsedPolicyAndLedger("2026-06-01")
	ledger.Debt[0].BaselineCalls = policy.Debt[0].BaselineCalls + 1
	_, err := validateAgainstPolicy(policy, ledger, Census{}, fixedNow(), waiverclock.ModeGrace)
	if err == nil {
		t.Fatal("validateAgainstPolicy() = nil, want the drifted baseline reported")
	}
	if !strings.Contains(err.Error(), "baseline_calls") {
		t.Fatalf("a tolerated lapse hid the baseline drift:\n%v", err)
	}
}

func TestValidateUsesWholeDayGranularity(t *testing.T) {
	t.Parallel()

	// A row is valid through the whole UTC day it names, which is what the
	// rendered TESTING.md table claims.
	policy, ledger := lapsedPolicyAndLedger("2026-07-13")
	if _, err := validateAgainstPolicy(policy, ledger, Census{}, time.Date(2026, time.July, 13, 23, 59, 59, 0, time.UTC), waiverclock.ModeStrict); err != nil {
		t.Fatalf("validateAgainstPolicy() error = %v, want the row valid through its own day", err)
	}
	if _, err := validateAgainstPolicy(policy, ledger, Census{}, time.Date(2026, time.July, 14, 0, 0, 0, 0, time.UTC), waiverclock.ModeStrict); err == nil {
		t.Fatal("validateAgainstPolicy() = nil, want the row lapsed the next day")
	}
}
