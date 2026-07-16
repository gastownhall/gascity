package beads

import (
	"strings"
	"testing"
	"time"
)

func TestAdmissionPolicyResolveDefaultsToPriorityFIFO(t *testing.T) {
	if got := AdmissionPolicy("").Resolve(); got != PolicyPriorityFIFO {
		t.Fatalf("unset policy resolved to %q, want %q", got, PolicyPriorityFIFO)
	}
	if got := PolicyFIFO.Resolve(); got != PolicyFIFO {
		t.Fatalf("explicit fifo resolved to %q, want %q", got, PolicyFIFO)
	}
}

func TestAdmissionPolicyValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  AdmissionPolicy
		wantErr bool
	}{
		{name: "unset is valid and means default", policy: "", wantErr: false},
		{name: "priority_fifo", policy: PolicyPriorityFIFO, wantErr: false},
		{name: "fifo", policy: PolicyFIFO, wantErr: false},
		{name: "unknown value fails fast", policy: "lifo", wantErr: true},
		{name: "case variant is not silently accepted", policy: "FIFO", wantErr: true},
		{name: "near-miss spelling fails fast", policy: "priority-fifo", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate(%q) = nil, want error", tc.policy)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate(%q) = %v, want nil", tc.policy, err)
			}
		})
	}
}

// The error must name the offending value and the permitted set so an operator
// can fix a typo in pack.toml/city.toml without reading Go source.
func TestAdmissionPolicyValidateErrorIsActionable(t *testing.T) {
	err := AdmissionPolicy("lifo").Validate()
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"lifo", string(PolicyPriorityFIFO), string(PolicyFIFO)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestFIFOLessIgnoresPriorityAndTieBreaksDeterministically(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	p0, p1 := 0, 1

	if !FIFOLess(
		Bead{ID: "old-p1", Priority: &p1, CreatedAt: base},
		Bead{ID: "new-p0", Priority: &p0, CreatedAt: base.Add(time.Hour)},
	) {
		t.Fatal("fifo must admit older work first even when newer work is P0")
	}
	if !FIFOLess(
		Bead{ID: "a", Priority: &p1, CreatedAt: base},
		Bead{ID: "b", Priority: &p0, CreatedAt: base},
	) {
		t.Fatal("ID must provide the deterministic final tie-break, independent of priority")
	}
	if FIFOLess(
		Bead{ID: "b", Priority: nil, CreatedAt: base},
		Bead{ID: "a", Priority: nil, CreatedAt: base},
	) {
		t.Fatal("ID tie-break must be a strict weak ordering")
	}
}

// The whole point of the bead: one selected policy drives every admission path.
// LessFunc is the single source of truth that each path resolves through.
func TestLessFuncSelectsComparatorByPolicy(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	p0, p1 := 0, 1
	oldP1 := Bead{ID: "old-p1", Priority: &p1, CreatedAt: base}
	newP0 := Bead{ID: "new-p0", Priority: &p0, CreatedAt: base.Add(time.Hour)}

	if !LessFunc(PolicyPriorityFIFO)(newP0, oldP1) {
		t.Fatal("priority_fifo must prefer the P0 bead")
	}
	if !LessFunc(PolicyFIFO)(oldP1, newP0) {
		t.Fatal("fifo must prefer the older bead")
	}
	if !LessFunc("")(newP0, oldP1) {
		t.Fatal("unset must behave exactly as priority_fifo (PR #4322 behavior preserved)")
	}
}

// #4322 latches the active priority band so a lost P0 race cannot fall through
// to P1. Under fifo there are no bands: leaving the latch armed would break the
// candidate scan on a band change and skip older eligible work, which is the
// opposite of FIFO. The latch must therefore be policy-gated, not unconditional.
func TestHasPriorityBandsGatesTheBandLatch(t *testing.T) {
	if !PolicyPriorityFIFO.HasPriorityBands() {
		t.Fatal("priority_fifo must latch bands to prevent P0->P1 inversion")
	}
	if PolicyFIFO.HasPriorityBands() {
		t.Fatal("fifo has no bands; latching would skip older eligible work")
	}
	if !AdmissionPolicy("").HasPriorityBands() {
		t.Fatal("unset must latch bands exactly as priority_fifo does")
	}
}

// The shell/jq path and the Go path must agree; a divergence here is the bug
// class this bead exists to prevent.
func TestJQSortKeyMatchesComparator(t *testing.T) {
	if got := PolicyPriorityFIFO.JQSortKey(); got != `(.priority // 2), (.created_at // ""), (.id // "")` {
		t.Fatalf("priority_fifo jq key = %q", got)
	}
	if got := PolicyFIFO.JQSortKey(); got != `(.created_at // ""), (.id // "")` {
		t.Fatalf("fifo jq key = %q", got)
	}
}
