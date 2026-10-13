package resourcecensus

import (
	"strings"
	"testing"
	"time"
)

// lapsedPolicyAndLedger dates the same debt row in both the bootstrap policy and
// the ledger. Dating only one of them would also trip comparePolicyFields, and
// this file is about the dates, not about drift between the two.
func lapsedPolicyAndLedger(expires string) (policy, ledger Ledger) {
	policy = validLedger(Census{})
	policy.Debt[0].Expires = expires
	ledger = cloneLedger(policy)
	return policy, ledger
}

// TestValidateIsDateFree pins the split that keeps this package cacheable: the
// census and ledger checks judge structure and counts only, so their verdict
// cannot change when the calendar does. Whether a row's date is acceptable
// today is internal/testpolicy/waiverexpiry's question, never this package's.
func TestValidateIsDateFree(t *testing.T) {
	t.Parallel()

	for _, expires := range []string{"2000-01-01", "2100-01-01"} {
		policy, ledger := lapsedPolicyAndLedger(expires)
		if err := validateAgainstPolicy(policy, ledger, Census{}); err != nil {
			t.Fatalf("validateAgainstPolicy(row dated %s) = %v, want no date verdict from the structural check", expires, err)
		}
	}
}

// TestValidateKeepsStructuralProblemsFatal: a missing owner or a malformed date
// needs a code change to appear, so it fails whoever made that change.
func TestValidateKeepsStructuralProblemsFatal(t *testing.T) {
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
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			policy := validLedger(Census{})
			ledger := cloneLedger(policy)
			tc.mutate(&policy)
			tc.mutate(&ledger)
			err := validateAgainstPolicy(policy, ledger, Census{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateAgainstPolicy() error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

// TestCollectExpiriesHandsEachRowToTheClockOnce pins the fix for a duplicate
// that doubled the census cliff: every row was once checked as a policy row and
// again as a ledger row (and medium rows a third time), so a shared date lapsing
// reported 74 findings for 37 rows. Collecting from one ledger, once per row,
// keeps the finding count proportional to the defect.
func TestCollectExpiriesHandsEachRowToTheClockOnce(t *testing.T) {
	t.Parallel()

	ledger := validLedger(Census{})
	ledger.Medium = []MediumOwner{{
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
	want := len(ledger.AuditBaseline) + len(ledger.Debt) + len(ledger.SmallDebt) + len(ledger.Medium)
	got := collectExpiries(ledger)
	if len(got) != want {
		t.Fatalf("collectExpiries() = %d expiries, want one per dated row (%d)", len(got), want)
	}
	var medium bool
	for _, expiry := range got {
		if strings.HasPrefix(expiry.Label, "medium owner package_dir=internal/example") {
			medium = true
			if expiry.Owner != "ga-example" || !expiry.Expires.Equal(time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)) {
				t.Fatalf("medium expiry = %+v, want owner ga-example dated 2026-06-01", expiry)
			}
		}
	}
	if !medium {
		t.Fatalf("collectExpiries() = %+v, want the medium row included", got)
	}
}

// TestCollectExpiriesSkipsMalformedRows keeps one authoring mistake from
// becoming two findings: validateOwnershipFields already reports it.
func TestCollectExpiriesSkipsMalformedRows(t *testing.T) {
	t.Parallel()

	ledger := validLedger(Census{})
	ledger.Debt[0].Expires = "07/12/2026"
	ledger.Debt[0].OwnerBead = ""
	for _, expiry := range collectExpiries(ledger) {
		if strings.HasPrefix(expiry.Label, "debt baseline") && expiry.Owner == "" {
			t.Fatalf("collectExpiries() handed on a malformed row: %+v", expiry)
		}
	}
}

// TestPolicyExpiriesCoverTheCheckedLedger ties the date check to the ledger the
// repository actually ships. The structural test already forces every
// test/test-resources.toml row to equal the bootstrap policy, expires included,
// so the policy's dates are the ledger's dates.
func TestPolicyExpiriesCoverTheCheckedLedger(t *testing.T) {
	t.Parallel()

	got := PolicyExpiries()
	if len(got) == 0 {
		t.Fatal("PolicyExpiries() is empty; the date check would pass vacuously")
	}
	if want := len(collectExpiries(bootstrapPolicy)); len(got) != want {
		t.Fatalf("PolicyExpiries() = %d, want %d", len(got), want)
	}
}
