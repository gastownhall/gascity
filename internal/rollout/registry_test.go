package rollout

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// hasErr reports whether any error contains substr — so a masking sibling rule
// cannot satisfy an assertion meant for a specific rule.
func hasErr(errs []error, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			return true
		}
	}
	return false
}

// TestCanonicalRegistryValid proves the shipped registry passes every structural
// rule (Category, one-Default-arm, reflection-verified ConfigPath, env hygiene,
// Owner, per-category lifecycle anchors, SelectsBetween, Justification).
func TestCanonicalRegistryValid(t *testing.T) {
	t.Parallel()
	for _, e := range ValidateSpecs(Specs()) {
		t.Errorf("canonical registry violation: %v", e)
	}
}

// TestSpecIsPureData proves Spec (transitively) has no func-kind field, so a
// capability predicate can never be stored on the registry and registry.go stays
// CODEOWNERS-reviewable data.
func TestSpecIsPureData(t *testing.T) {
	t.Parallel()
	assertNoFuncFields(t, reflect.TypeOf(Spec{}), "Spec")
}

func assertNoFuncFields(t *testing.T, ty reflect.Type, path string) {
	t.Helper()
	switch ty.Kind() {
	case reflect.Func:
		t.Errorf("%s is a func-kind field; Spec must be pure data", path)
	case reflect.Struct:
		for i := 0; i < ty.NumField(); i++ {
			f := ty.Field(i)
			assertNoFuncFields(t, f.Type, path+"."+f.Name)
		}
	case reflect.Pointer, reflect.Slice, reflect.Array:
		assertNoFuncFields(t, ty.Elem(), path+"[]")
	case reflect.Map:
		assertNoFuncFields(t, ty.Elem(), path+"[v]")
	}
}

// TestValidateSpecsHasTeeth proves the validator reports (never panics on)
// concrete violations and returns clean for a well-formed synthetic set.
func TestValidateSpecsHasTeeth(t *testing.T) {
	t.Parallel()

	good := Spec{
		Key: "beads.conditional_writes", Category: InfraRollout,
		ConfigPath: "beads.conditional_writes", EnvOverride: "GC_X", EnvSemantics: EnvOverrides,
		Default: Default{Mode: ptr(Off)}, Owner: Owner{Bead: "b", GitHub: "@t"},
		Expires: "2027-01-15", VersionAnchor: "ANCHOR",
		SelectsBetween: [2]string{"a", "b"}, Justification: "why",
	}
	if errs := ValidateSpecs([]Spec{good}); len(errs) != 0 {
		t.Fatalf("well-formed spec rejected: %v", errs)
	}

	// Each row asserts a SPECIFIC error substring, so a sibling rule that also
	// fires cannot vacuously satisfy the row (the masking bug the red team found).
	rows := []struct {
		name string
		mut  func(s *Spec)
		want string
	}{
		{"empty key", func(s *Spec) { s.Key = "" }, "empty Key"},
		{"bad category", func(s *Spec) { s.Category = "agent-capability" }, "invalid Category"},
		{"both default arms", func(s *Spec) { s.Default = Default{Mode: ptr(Off), Bool: ptr(true)} }, "both Mode and Bool"},
		{"no default arm", func(s *Spec) { s.Default = Default{} }, "neither Mode nor Bool"},
		{"empty configpath", func(s *Spec) { s.ConfigPath = "" }, "empty ConfigPath"},
		{"unresolvable configpath", func(s *Spec) { s.ConfigPath = "beads.nope_nope" }, "does not resolve"},
		{"non-leaf configpath", func(s *Spec) { s.ConfigPath = "beads" }, "string config field"},
		{"mode arm on bool field", func(s *Spec) { s.ConfigPath = "daemon.formula_v2" }, "string config field"},
		{"bool arm on string field", func(s *Spec) { s.Default = Default{Bool: ptr(true)} }, "bool/*bool config field"},
		{"non-GC env", func(s *Spec) { s.EnvOverride = "X" }, "GC_-prefixed"},
		{"invalid envsemantics", func(s *Spec) { s.EnvSemantics = "bogus" }, "EnvSemantics"},
		{"missing owner bead", func(s *Spec) { s.Owner.Bead = "" }, "Owner requires"},
		{"missing owner github", func(s *Spec) { s.Owner.GitHub = "" }, "Owner requires"},
		{"rollout missing expires", func(s *Spec) { s.Expires = "" }, "requires Expires"},
		{"rollout malformed expires", func(s *Spec) { s.Expires = "2027-1-5" }, "not YYYY-MM-DD"},
		{"rollout missing anchor", func(s *Spec) { s.VersionAnchor = "" }, "requires a VersionAnchor"},
		{"empty selectsbetween arm", func(s *Spec) { s.SelectsBetween = [2]string{"a", ""} }, "two non-empty"},
		{"identical selectsbetween", func(s *Spec) { s.SelectsBetween = [2]string{"x", "x"} }, "must differ"},
		{"empty justification", func(s *Spec) { s.Justification = "" }, "empty Justification"},
	}
	for _, tc := range rows {
		s := good
		tc.mut(&s)
		if errs := ValidateSpecs([]Spec{s}); !hasErr(errs, tc.want) {
			t.Errorf("%s: want an error containing %q, got %v", tc.name, tc.want, errs)
		}
	}

	// killswitch anchor rules, each in isolation (no sibling masking).
	ksExpires := good
	ksExpires.Category, ksExpires.VersionAnchor = InfraKillswitch, ""
	if !hasErr(ValidateSpecs([]Spec{ksExpires}), "killswitch must not set Expires") {
		t.Errorf("killswitch with Expires not rejected: %v", ValidateSpecs([]Spec{ksExpires}))
	}
	ksAnchor := good
	ksAnchor.Category, ksAnchor.Expires = InfraKillswitch, ""
	if !hasErr(ValidateSpecs([]Spec{ksAnchor}), "killswitch must not set VersionAnchor") {
		t.Errorf("killswitch with VersionAnchor not rejected: %v", ValidateSpecs([]Spec{ksAnchor}))
	}
	// a clean killswitch (no lifecycle anchors) validates.
	ksClean := good
	ksClean.Category, ksClean.Expires, ksClean.VersionAnchor = InfraKillswitch, "", ""
	if errs := ValidateSpecs([]Spec{ksClean}); len(errs) != 0 {
		t.Errorf("clean killswitch rejected: %v", errs)
	}
}

// TestDuplicateKeysAndEnvRejected proves cross-spec uniqueness.
func TestDuplicateKeysAndEnvRejected(t *testing.T) {
	t.Parallel()
	base := Spec{
		Key: "k1", Category: InfraKillswitch, ConfigPath: "beads.conditional_writes",
		EnvOverride: "GC_DUP", EnvSemantics: EnvOverrides, Default: Default{Mode: ptr(Off)},
		Owner: Owner{Bead: "b", GitHub: "@t"}, SelectsBetween: [2]string{"a", "b"}, Justification: "x",
	}
	other := base
	other.Key = "k2"
	if errs := ValidateSpecs([]Spec{base, other}); len(errs) == 0 {
		t.Errorf("duplicate EnvOverride across specs should be rejected")
	}
	dupKey := base
	dupKey.EnvOverride, dupKey.EnvSemantics = "", ""
	dupKey2 := dupKey
	if errs := ValidateSpecs([]Spec{dupKey, dupKey2}); len(errs) == 0 {
		t.Errorf("duplicate Key across specs should be rejected")
	}
}

// TestDefaultsDoNotDrift pins the three homes of each gate's default together:
// the Spec.Default, the defaultFlags() value Resolve/ForTest start from, and the
// config accessor. A drift here is the classic feature-flag silent-default bug.
func TestDefaultsDoNotDrift(t *testing.T) {
	t.Parallel()
	byKey := map[string]Spec{}
	for _, s := range Specs() {
		byKey[s.Key] = s
	}
	def := defaultFlags()

	// beads.conditional_writes: Mode gate, default Off.
	beads := byKey[keyBeadsConditionalWrites]
	if beads.Default.Mode == nil || *beads.Default.Mode != Off {
		t.Fatalf("beads Spec.Default = %v, want Off", beads.Default.Mode)
	}
	if def.BeadsConditionalWrites() != Off {
		t.Errorf("defaultFlags beads = %q, want off", def.BeadsConditionalWrites())
	}
	if got := (config.BeadsConfig{}).NormalizedConditionalWrites(); got != string(Off) {
		t.Errorf("config accessor default = %q, want %q", got, Off)
	}

	// daemon.formula_v2: bool gate, default true.
	fv2 := byKey[keyDaemonFormulaV2]
	if fv2.Default.Bool == nil || *fv2.Default.Bool != true {
		t.Fatalf("formula_v2 Spec.Default = %v, want true", fv2.Default.Bool)
	}
	if !def.FormulaV2() {
		t.Errorf("defaultFlags formula_v2 = false, want true")
	}
	if !(config.DaemonConfig{}).FormulaV2Enabled() {
		t.Errorf("config accessor formula_v2 default = false, want true")
	}
}
