package orders

import (
	"strings"
	"testing"
)

// boolPtr / strPtr are local helpers so tests stay self-contained.
func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

func TestApplyOverrides(t *testing.T) {
	t.Parallel()

	disabled := boolPtr(false)
	tenSec := strPtr("10s")
	thirtySec := strPtr("30s")

	tests := []struct {
		name      string
		orders    []Order
		overrides []Override
		// wantErrSubstrs: all of these substrings must appear in the
		// returned error. Empty means the call must succeed.
		wantErrSubstrs []string
		// check inspects the post-apply orders slice when no error.
		check func(t *testing.T, aa []Order)
	}{
		{
			name: "city level override matches city order",
			orders: []Order{
				{Name: "patrol", Rig: ""},
			},
			overrides: []Override{
				{Name: "patrol", Rig: "", Enabled: disabled},
			},
			check: func(t *testing.T, aa []Order) {
				t.Helper()
				if aa[0].Enabled == nil || *aa[0].Enabled {
					t.Errorf("city-level patrol not disabled: %+v", aa[0].Enabled)
				}
			},
		},
		{
			name: "rig scoped override matches only that rig",
			orders: []Order{
				{Name: "patrol", Rig: "demo"},
				{Name: "patrol", Rig: "prod"},
			},
			overrides: []Override{
				{Name: "patrol", Rig: "demo", Interval: tenSec},
			},
			check: func(t *testing.T, aa []Order) {
				t.Helper()
				if aa[0].Interval != "10s" {
					t.Errorf("demo patrol interval = %q, want 10s", aa[0].Interval)
				}
				if aa[1].Interval != "" {
					t.Errorf("prod patrol interval should be unchanged, got %q", aa[1].Interval)
				}
			},
		},
		{
			name: "rigless override does not match rig-scoped orders, error suggests rig syntax",
			orders: []Order{
				{Name: "patrol", Rig: "demo"},
				{Name: "patrol", Rig: "prod"},
				{Name: "other", Rig: ""},
			},
			overrides: []Override{
				{Name: "patrol", Rig: "", Enabled: disabled},
			},
			wantErrSubstrs: []string{
				"orders.overrides[0]",
				`"patrol"`,
				"not found",
				// regression-grade: the enriched error must mention the
				// rig-scope mismatch and the actual rig names that exist,
				// so users see exactly what to type.
				`rig = "demo"`,
				`rig = "prod"`,
			},
		},
		{
			name: "rig scoped override with no matching rig instance returns error naming the rig",
			orders: []Order{
				{Name: "patrol", Rig: "demo"},
			},
			overrides: []Override{
				{Name: "patrol", Rig: "missing", Interval: tenSec},
			},
			wantErrSubstrs: []string{
				"orders.overrides[0]",
				`"patrol"`,
				`"missing"`,
				"not found",
			},
		},
		{
			name: "wildcard rig matches every instance with that name",
			orders: []Order{
				{Name: "patrol", Rig: ""},
				{Name: "patrol", Rig: "demo"},
				{Name: "patrol", Rig: "prod"},
				{Name: "other", Rig: "demo"},
			},
			overrides: []Override{
				{Name: "patrol", Rig: RigWildcard, Enabled: disabled, Interval: thirtySec},
			},
			check: func(t *testing.T, aa []Order) {
				t.Helper()
				for i, a := range aa {
					if a.Name != "patrol" {
						if a.Enabled != nil {
							t.Errorf("aa[%d] %q: unrelated order should not be touched", i, a.Name)
						}
						continue
					}
					if a.Enabled == nil || *a.Enabled {
						t.Errorf("aa[%d] (rig=%q): expected disabled", i, a.Rig)
					}
					if a.Interval != "30s" {
						t.Errorf("aa[%d] (rig=%q): interval=%q, want 30s", i, a.Rig, a.Interval)
					}
				}
			},
		},
		{
			name: "wildcard rig with no matching name still errors",
			orders: []Order{
				{Name: "patrol", Rig: "demo"},
			},
			overrides: []Override{
				{Name: "ghost", Rig: RigWildcard, Enabled: disabled},
			},
			wantErrSubstrs: []string{
				"orders.overrides[0]",
				`"ghost"`,
				"not found",
			},
		},
		{
			name: "empty name returns error",
			orders: []Order{
				{Name: "patrol", Rig: ""},
			},
			overrides: []Override{
				{Name: "", Rig: "", Enabled: disabled},
			},
			wantErrSubstrs: []string{
				"orders.overrides[0]",
				"name is required",
			},
		},
		{
			name: "name not found anywhere returns plain not-found error",
			orders: []Order{
				{Name: "patrol", Rig: "demo"},
			},
			overrides: []Override{
				{Name: "ghost", Rig: "", Enabled: disabled},
			},
			wantErrSubstrs: []string{
				"orders.overrides[0]",
				`"ghost"`,
				"not found",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Copy orders so test cases don't bleed.
			aa := make([]Order, len(tt.orders))
			copy(aa, tt.orders)

			err := ApplyOverrides(aa, tt.overrides)
			if len(tt.wantErrSubstrs) == 0 {
				if err != nil {
					t.Fatalf("ApplyOverrides returned error: %v", err)
				}
				if tt.check != nil {
					tt.check(t, aa)
				}
				return
			}
			if err == nil {
				t.Fatalf("ApplyOverrides succeeded; want error containing %v", tt.wantErrSubstrs)
			}
			msg := err.Error()
			for _, sub := range tt.wantErrSubstrs {
				if !strings.Contains(msg, sub) {
					t.Errorf("error %q missing substring %q", msg, sub)
				}
			}
		})
	}
}

// TestApplyOverrides_RiglessHintExcludesUnrelatedOrders ensures that the
// rig-suggestion hint listing only reports rigs that have an order with the
// override's name, not arbitrary rigs in the slice.
func TestApplyOverrides_RiglessHintExcludesUnrelatedOrders(t *testing.T) {
	t.Parallel()

	aa := []Order{
		{Name: "patrol", Rig: "demo"},
		{Name: "elsewhere", Rig: "unrelated-rig"},
	}
	err := ApplyOverrides(aa, []Override{{Name: "patrol", Rig: ""}})
	if err == nil {
		t.Fatal("expected error for rigless override against rig-scoped patrol")
	}
	msg := err.Error()
	if !strings.Contains(msg, `rig = "demo"`) {
		t.Errorf("error should suggest rig = %q; got %q", "demo", msg)
	}
	if strings.Contains(msg, "unrelated-rig") {
		t.Errorf("error should NOT mention unrelated-rig; got %q", msg)
	}
}

// TestApplyOverrides_PreservesNotFoundSubstring is a regression guard for
// cmd/gc/order_dispatch_test.go's TestBuildOrderDispatcherOverrideNotFoundNonFatal,
// which asserts strings.Contains(stderr, "not found"). If we change the
// error wording in the future, this test fails first and forces an update
// to the dispatcher test in the same change.
func TestApplyOverrides_PreservesNotFoundSubstring(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		orders []Order
		ov     Override
	}{
		{"missing name", []Order{{Name: "patrol"}}, Override{Name: "ghost"}},
		{"missing rig", []Order{{Name: "patrol", Rig: "demo"}}, Override{Name: "patrol", Rig: "missing"}},
		{"rigless against rig-scoped", []Order{{Name: "patrol", Rig: "demo"}}, Override{Name: "patrol"}},
		{"wildcard against missing name", []Order{{Name: "patrol", Rig: "demo"}}, Override{Name: "ghost", Rig: RigWildcard}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ApplyOverrides(tc.orders, []Override{tc.ov})
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "not found") {
				t.Errorf("error %q must contain literal substring %q", err.Error(), "not found")
			}
		})
	}
}
