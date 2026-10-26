package rollout

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestValueOf covers the render-only generic value accessor, including the
// binding leg: every registered gate must render a non-empty value, so adding a
// gate without extending ValueOf is caught here (not silently blank in doctor).
func TestValueOf(t *testing.T) {
	t.Parallel()
	f := ForTest(WithBeadsConditionalWrites(Require), WithFormulaV2(false))
	if got := f.ValueOf(keyBeadsConditionalWrites); got != "require" {
		t.Errorf("ValueOf(beads) = %q, want require", got)
	}
	if got := f.ValueOf(keyDaemonFormulaV2); got != "false" {
		t.Errorf("ValueOf(formula_v2) = %q, want false", got)
	}
	if got := f.ValueOf("nope.nope"); got != "" {
		t.Errorf("ValueOf(unknown) = %q, want empty", got)
	}
	// binding: every registered gate renders non-empty on a resolved Flags.
	resolved, err := Resolve(&config.City{}, ResolveOptions{LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range Specs() {
		if resolved.ValueOf(s.Key) == "" {
			t.Errorf("%s: ValueOf returns empty on a resolved Flags — extend ValueOf for this gate", s.Key)
		}
	}
}

// TestNoticesReturnsDefensiveCopy proves a caller cannot mutate a Flags' retained
// notices through the slice Notices() returns.
func TestNoticesReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()
	f, err := Resolve(cityWith("require", nil),
		ResolveOptions{LookupEnv: envMap(map[string]string{envBeadsConditionalWrites: "auto"})})
	if err != nil {
		t.Fatal(err)
	}
	n1 := f.Notices()
	if len(n1) == 0 {
		t.Fatal("expected at least one notice (env overrides config)")
	}
	n1[0].Message = "MUTATED"
	if f.Notices()[0].Message == "MUTATED" {
		t.Error("Notices() must return a defensive copy; a caller's mutation leaked into the Flags")
	}
}

// TestZeroFlagsIsLegacy pins the documented degraded-safe zero value: an unwired
// Flags{} runs legacy paths (not the builtin defaults) and reports no origin.
func TestZeroFlagsIsLegacy(t *testing.T) {
	t.Parallel()
	var z Flags
	if z.BeadsConditionalWrites() != ModeUnset {
		t.Errorf("zero beads = %q, want ModeUnset", z.BeadsConditionalWrites())
	}
	if z.FormulaV2() {
		t.Errorf("zero formula_v2 = true, want false (legacy path, not the builtin default true)")
	}
	if z.OriginOf(keyBeadsConditionalWrites) != "" {
		t.Errorf("zero OriginOf = %q, want empty (unwired)", z.OriginOf(keyBeadsConditionalWrites))
	}
}

// TestBuiltinAuto pins that only a mode gate resolved to auto by its builtin
// default reads as BuiltinAuto: an explicit auto from config or env does not,
// nor an off default, a bool gate, an unknown key or a never-resolved Flags.
func TestBuiltinAuto(t *testing.T) {
	t.Parallel()
	env := func(vars map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	}
	resolve := func(cfg *config.City, vars map[string]string) Flags {
		f, err := Resolve(cfg, ResolveOptions{LookupEnv: env(vars)})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		return f
	}
	explicit := &config.City{}
	explicit.Ops.ContentMode = "auto"
	cases := []struct {
		name  string
		flags Flags
		key   string
		want  bool
	}{
		{"default ops gate", ForTest(), KeyOpsContent, true},
		{"default conditional writes", ForTest(), KeyBeadsConditionalWrites, true},
		{"resolved default", resolve(&config.City{}, nil), KeyOpsLifecycle, true},
		{"explicit config auto", resolve(explicit, nil), KeyOpsContent, false},
		{"explicit env auto", resolve(&config.City{}, map[string]string{envOpsControl: "auto"}), KeyOpsControl, false},
		{"ForTest override auto", ForTest(WithOpsContent(Auto)), KeyOpsContent, false},
		{"default off", ForTest(), KeyBeadsGuardedRelease, false},
		{"bool gate", ForTest(), keyDaemonFormulaV2, false},
		{"unknown key", ForTest(), "no.such.gate", false},
		{"zero Flags", Flags{}, KeyOpsContent, false},
	}
	for _, c := range cases {
		if got := c.flags.BuiltinAuto(c.key); got != c.want {
			t.Errorf("%s: BuiltinAuto(%s) = %t, want %t", c.name, c.key, got, c.want)
		}
	}
}
