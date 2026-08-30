package doctor

import (
	"reflect"
	"testing"
)

func namesOf(checks []Check) []string {
	out := make([]string, 0, len(checks))
	for _, c := range checks {
		out = append(out, c.Name())
	}
	return out
}

func selectorFixture() []Check {
	return []Check{
		&mockCheck{name: "city-structure"},
		&mockCheck{name: "controller"},
		&mockCheck{name: "order-firing-current"},
		&mockCheck{name: "rig:core:path"},
	}
}

func TestSelectChecksEmptyNamesSelectsEverything(t *testing.T) {
	checks := selectorFixture()
	selected, unmatched := SelectChecks(checks, nil)
	if len(unmatched) != 0 {
		t.Fatalf("unmatched = %v, want none", unmatched)
	}
	want := []string{"city-structure", "controller", "order-firing-current", "rig:core:path"}
	if got := namesOf(selected); !reflect.DeepEqual(got, want) {
		t.Errorf("selected = %v, want %v", got, want)
	}
}

func TestSelectChecksKeepsRegistrationOrderNotRequestOrder(t *testing.T) {
	// Registration order is what doctor runs in, and it is deliberate: a root
	// cause is registered ahead of the checks that fail because of it.
	selected, unmatched := SelectChecks(selectorFixture(), []string{"rig:core:path", "city-structure"})
	if len(unmatched) != 0 {
		t.Fatalf("unmatched = %v, want none", unmatched)
	}
	want := []string{"city-structure", "rig:core:path"}
	if got := namesOf(selected); !reflect.DeepEqual(got, want) {
		t.Errorf("selected = %v, want %v", got, want)
	}
}

func TestSelectChecksRepeatedNameSelectsCheckOnce(t *testing.T) {
	selected, unmatched := SelectChecks(selectorFixture(), []string{"controller", "controller"})
	if len(unmatched) != 0 {
		t.Fatalf("unmatched = %v, want none", unmatched)
	}
	if got := namesOf(selected); !reflect.DeepEqual(got, []string{"controller"}) {
		t.Errorf("selected = %v, want [controller]", got)
	}
}

func TestSelectChecksSharedNameSelectsEveryRegistration(t *testing.T) {
	// Nothing in the Check interface enforces unique names, and pack-declared
	// names come from config, so a name shared by two registrations has to
	// select both rather than silently dropping one.
	checks := []Check{
		&mockCheck{name: "custom-types"},
		&mockCheck{name: "controller"},
		&mockCheck{name: "custom-types"},
	}
	selected, unmatched := SelectChecks(checks, []string{"custom-types"})
	if len(unmatched) != 0 {
		t.Fatalf("unmatched = %v, want none", unmatched)
	}
	if len(selected) != 2 {
		t.Errorf("selected %d checks, want 2: %v", len(selected), namesOf(selected))
	}
}

func TestSelectChecksReportsUnknownNamesInRequestOrder(t *testing.T) {
	selected, unmatched := SelectChecks(selectorFixture(), []string{"nope", "controller", "also-nope"})
	want := []string{"nope", "also-nope"}
	if !reflect.DeepEqual(unmatched, want) {
		t.Errorf("unmatched = %v, want %v", unmatched, want)
	}
	// The matched half is still returned; refusing to run it is the caller's
	// decision, and cmd/gc makes it. What matters here is that the caller is
	// told, rather than handed a short list that reads as complete.
	if got := namesOf(selected); !reflect.DeepEqual(got, []string{"controller"}) {
		t.Errorf("selected = %v, want [controller]", got)
	}
}

func TestSelectChecksTrimsSurroundingWhitespace(t *testing.T) {
	selected, unmatched := SelectChecks(selectorFixture(), []string{" controller ", "\tcity-structure"})
	if len(unmatched) != 0 {
		t.Fatalf("unmatched = %v, want none", unmatched)
	}
	want := []string{"city-structure", "controller"}
	if got := namesOf(selected); !reflect.DeepEqual(got, want) {
		t.Errorf("selected = %v, want %v", got, want)
	}
}

func TestSelectChecksBlankNameIsUnmatchedNotSelectEverything(t *testing.T) {
	// `--check ""` reaches here as a one-element slice. Treating it as "no
	// selection" would run all 91 checks under a flag that asked for one.
	selected, unmatched := SelectChecks(selectorFixture(), []string{""})
	if !reflect.DeepEqual(unmatched, []string{""}) {
		t.Errorf("unmatched = %#v, want [\"\"]", unmatched)
	}
	if len(selected) != 0 {
		t.Errorf("selected = %v, want none", namesOf(selected))
	}
}

func TestSelectChecksSkipsNilEntries(t *testing.T) {
	checks := []Check{&mockCheck{name: "controller"}, nil, &mockCheck{name: "city-structure"}}

	selected, unmatched := SelectChecks(checks, nil)
	if len(unmatched) != 0 {
		t.Fatalf("unmatched = %v, want none", unmatched)
	}
	if got := namesOf(selected); !reflect.DeepEqual(got, []string{"controller", "city-structure"}) {
		t.Errorf("selected = %v, want [controller city-structure]", got)
	}

	selected, unmatched = SelectChecks(checks, []string{"controller"})
	if len(unmatched) != 0 {
		t.Fatalf("unmatched = %v, want none", unmatched)
	}
	if got := namesOf(selected); !reflect.DeepEqual(got, []string{"controller"}) {
		t.Errorf("selected = %v, want [controller]", got)
	}
}

func TestCheckNamesReturnsRegistrationOrderSkippingNil(t *testing.T) {
	checks := []Check{&mockCheck{name: "controller"}, nil, &mockCheck{name: "city-structure"}}
	want := []string{"controller", "city-structure"}
	if got := CheckNames(checks); !reflect.DeepEqual(got, want) {
		t.Errorf("CheckNames = %v, want %v", got, want)
	}
}
