package beadmeta

import "testing"

// TestDispatchHoldLabelsMatchCanonicalHoldValues pins beadmeta as the single
// named home for the two canonical hold values documented in
// engdocs/contributors/hold-label-conventions.md (hold:mayor, hold:external)
// so internal/config and cmd/gc can share one definition instead of each
// re-spelling the label strings (ga-x9kptu / ga-5736js).
func TestDispatchHoldLabelsMatchCanonicalHoldValues(t *testing.T) {
	if HoldMayorLabel != "hold:mayor" {
		t.Fatalf("HoldMayorLabel = %q, want %q", HoldMayorLabel, "hold:mayor")
	}
	if HoldExternalLabel != "hold:external" {
		t.Fatalf("HoldExternalLabel = %q, want %q", HoldExternalLabel, "hold:external")
	}
	want := []string{HoldMayorLabel, HoldExternalLabel}
	if len(DispatchHoldLabels) != len(want) {
		t.Fatalf("DispatchHoldLabels = %#v, want %#v", DispatchHoldLabels, want)
	}
	for i, v := range want {
		if DispatchHoldLabels[i] != v {
			t.Fatalf("DispatchHoldLabels[%d] = %q, want %q", i, DispatchHoldLabels[i], v)
		}
	}
}

func TestHasDispatchHold(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"nil", nil, false},
		{"unrelated", []string{"mysql-cutover", "needs-mayor", "mpr-human-hold"}, false},
		{"other hold value", []string{"hold:polecat"}, false},
		{"hold external", []string{"x", HoldExternalLabel}, true},
		{"hold mayor", []string{HoldMayorLabel}, true},
		{"case and space folded", []string{"  Hold:External "}, true},
	}
	for _, tc := range cases {
		if got := HasDispatchHold(tc.labels); got != tc.want {
			t.Errorf("%s: HasDispatchHold(%q) = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}
}

func TestHasHumanLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"nil", nil, false},
		{"unrelated", []string{"needs-mayor", "mpr-human-hold", "human-hold"}, false},
		{"dispatch hold is not human", []string{HoldMayorLabel, HoldExternalLabel}, false},
		{"human", []string{"x", HumanLabel}, true},
		{"space trimmed", []string{"  human "}, true},
		{"case kept, as bd's --exclude-label keeps it", []string{"Human"}, false},
	}
	for _, tc := range cases {
		if got := HasHumanLabel(tc.labels); got != tc.want {
			t.Errorf("%s: HasHumanLabel(%q) = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}
}

// TestParkedLabelsAreHoldsThenHuman pins the shared "what to DO" list: the
// dispatch holds in order, then bd's human label, without aliasing or mutating
// DispatchHoldLabels.
func TestParkedLabelsAreHoldsThenHuman(t *testing.T) {
	want := []string{HoldMayorLabel, HoldExternalLabel, HumanLabel}
	if len(ParkedLabels) != len(want) {
		t.Fatalf("ParkedLabels = %#v, want %#v", ParkedLabels, want)
	}
	for i, v := range want {
		if ParkedLabels[i] != v {
			t.Fatalf("ParkedLabels[%d] = %q, want %q", i, ParkedLabels[i], v)
		}
	}
	if len(DispatchHoldLabels) != 2 {
		t.Fatalf("DispatchHoldLabels = %#v, must stay the two canonical holds", DispatchHoldLabels)
	}
}

func TestHasParkedLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"nil", nil, false},
		{"unrelated", []string{"mysql-cutover", "needs-mayor"}, false},
		{"near misses", []string{"needs-human", "human-hold", "mpr-human-hold", "Human"}, false},
		{"human", []string{"x", HumanLabel}, true},
		{"human space trimmed", []string{" human "}, true},
		{"hold mayor", []string{HoldMayorLabel}, true},
		{"hold case and space folded", []string{"  Hold:External "}, true},
	}
	for _, tc := range cases {
		if got := HasParkedLabel(tc.labels); got != tc.want {
			t.Errorf("%s: HasParkedLabel(%q) = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}
}
