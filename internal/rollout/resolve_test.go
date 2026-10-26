package rollout

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func cityWith(conditionalWrites string, formulaV2 *bool) *config.City {
	return &config.City{
		Beads:  config.BeadsConfig{ConditionalWrites: conditionalWrites},
		Daemon: config.DaemonConfig{FormulaV2: formulaV2},
	}
}

// TestResolvePrecedence walks builtin < config < env for the Mode gate with an
// injected LookupEnv (never t.Setenv), and the config/builtin path for the bool
// gate.
func TestResolvePrecedence(t *testing.T) {
	t.Parallel()
	env := func(m map[string]string) ResolveOptions { return ResolveOptions{LookupEnv: envMap(m)} }
	// Source the env key from the single-source const so this test breaks if the
	// registry's env override name drifts.
	K := envBeadsConditionalWrites

	t.Run("builtin when unset everywhere", func(t *testing.T) {
		t.Parallel()
		f, err := Resolve(cityWith("", nil), env(nil))
		if err != nil {
			t.Fatal(err)
		}
		if f.BeadsConditionalWrites() != Auto || f.OriginOf(keyBeadsConditionalWrites) != OriginBuiltin {
			t.Errorf("beads = %q/%q, want auto/builtin", f.BeadsConditionalWrites(), f.OriginOf(keyBeadsConditionalWrites))
		}
		if !f.FormulaV2() || f.OriginOf(keyDaemonFormulaV2) != OriginBuiltin {
			t.Errorf("formula_v2 = %v/%q, want true/builtin", f.FormulaV2(), f.OriginOf(keyDaemonFormulaV2))
		}
	})

	t.Run("config wins over builtin", func(t *testing.T) {
		t.Parallel()
		f, err := Resolve(cityWith("require", ptr(false)), env(nil))
		if err != nil {
			t.Fatal(err)
		}
		if f.BeadsConditionalWrites() != Require || f.OriginOf(keyBeadsConditionalWrites) != OriginConfig {
			t.Errorf("beads = %q/%q, want require/config", f.BeadsConditionalWrites(), f.OriginOf(keyBeadsConditionalWrites))
		}
		if f.FormulaV2() || f.OriginOf(keyDaemonFormulaV2) != OriginConfig {
			t.Errorf("formula_v2 = %v/%q, want false/config", f.FormulaV2(), f.OriginOf(keyDaemonFormulaV2))
		}
	})

	t.Run("valid env active when config unset", func(t *testing.T) {
		t.Parallel()
		f, err := Resolve(cityWith("", nil), env(map[string]string{K: "auto"}))
		if err != nil {
			t.Fatal(err)
		}
		if f.BeadsConditionalWrites() != Auto || f.OriginOf(keyBeadsConditionalWrites) != OriginEnv {
			t.Errorf("beads = %q/%q, want auto/env", f.BeadsConditionalWrites(), f.OriginOf(keyBeadsConditionalWrites))
		}
		assertOneNotice(t, f, NoticeEnvOverrideActive)
	})

	t.Run("valid env overrides config, loudly", func(t *testing.T) {
		t.Parallel()
		f, err := Resolve(cityWith("require", nil), env(map[string]string{K: " AUTO "}))
		if err != nil {
			t.Fatal(err)
		}
		if f.BeadsConditionalWrites() != Auto || f.OriginOf(keyBeadsConditionalWrites) != OriginEnv {
			t.Errorf("beads = %q/%q, want auto/env (case+space tolerant)", f.BeadsConditionalWrites(), f.OriginOf(keyBeadsConditionalWrites))
		}
		assertOneNotice(t, f, NoticeEnvOverridesConfig)
	})

	t.Run("valid env agreeing with explicit config keeps config origin, no notice", func(t *testing.T) {
		t.Parallel()
		f, err := Resolve(cityWith("auto", nil), env(map[string]string{K: "auto"}))
		if err != nil {
			t.Fatal(err)
		}
		if f.BeadsConditionalWrites() != Auto || f.OriginOf(keyBeadsConditionalWrites) != OriginConfig {
			t.Errorf("beads = %q/%q, want auto/config (env agrees; config authoritative)", f.BeadsConditionalWrites(), f.OriginOf(keyBeadsConditionalWrites))
		}
		for _, n := range f.Notices() {
			if n.FlagKey == keyBeadsConditionalWrites {
				t.Errorf("env agreeing with config must emit no (misleading) notice, got %+v", n)
			}
		}
	})

	t.Run("malformed env warns and uses config (never errors)", func(t *testing.T) {
		t.Parallel()
		f, err := Resolve(cityWith("require", nil), env(map[string]string{K: "yes-please"}))
		if err != nil {
			t.Fatalf("malformed env must NOT error: %v", err)
		}
		if f.BeadsConditionalWrites() != Require || f.OriginOf(keyBeadsConditionalWrites) != OriginConfig {
			t.Errorf("beads = %q/%q, want require/config (config kept)", f.BeadsConditionalWrites(), f.OriginOf(keyBeadsConditionalWrites))
		}
		assertOneNotice(t, f, NoticeInvalidEnvIgnored)
	})

	t.Run("out-of-enum CONFIG value errors (typo never means off)", func(t *testing.T) {
		t.Parallel()
		if _, err := Resolve(cityWith("requre", nil), env(nil)); err == nil {
			t.Errorf("expected an error for an out-of-enum config value")
		}
	})

	t.Run("nil config errors", func(t *testing.T) {
		t.Parallel()
		if _, err := Resolve(nil, env(nil)); err == nil {
			t.Errorf("expected an error for nil config")
		}
	})
}

func assertOneNotice(t *testing.T, f Flags, kind NoticeKind) {
	t.Helper()
	n := 0
	for _, notice := range f.Notices() {
		if notice.Kind == kind {
			n++
		}
	}
	if n != 1 {
		t.Errorf("want exactly one %q notice, got %d (all: %+v)", kind, n, f.Notices())
	}
}

func TestParseMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want Mode
		ok   bool
	}{
		{"off", Off, true},
		{"AUTO", Auto, true},
		{"  Require ", Require, true},
		{"", ModeUnset, false},
		{"true", ModeUnset, false},
		{"on", ModeUnset, false},
	} {
		got, err := ParseMode(tc.in)
		if (err == nil) != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("ParseMode(%q) = %q,%v; want %q,ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

// TestConditionalWritesDefaultsToAuto pins mc-dpo2b's default: an unset
// beads.conditional_writes resolves auto, an explicit "off" stays off, and
// GC_BEADS_CONDITIONAL_WRITES still wins over both.
func TestConditionalWritesDefaultsToAuto(t *testing.T) {
	t.Parallel()
	K := envBeadsConditionalWrites
	for _, tc := range []struct {
		name, config string
		env          map[string]string
		want         Mode
		origin       Origin
	}{
		{name: "unset", want: Auto, origin: OriginBuiltin},
		{name: "explicit off", config: "off", want: Off, origin: OriginConfig},
		{name: "explicit require", config: "require", want: Require, origin: OriginConfig},
		{name: "env off over the default", env: map[string]string{K: "off"}, want: Off, origin: OriginEnv},
		{name: "env off over config auto", config: "auto", env: map[string]string{K: "off"}, want: Off, origin: OriginEnv},
		{name: "env auto over config off", config: "off", env: map[string]string{K: "auto"}, want: Auto, origin: OriginEnv},
	} {
		f, err := Resolve(cityWith(tc.config, nil), ResolveOptions{LookupEnv: envMap(tc.env)})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := f.BeadsConditionalWrites(); got != tc.want || f.OriginOf(keyBeadsConditionalWrites) != tc.origin {
			t.Errorf("%s: beads = %q/%q, want %q/%q", tc.name, got, f.OriginOf(keyBeadsConditionalWrites), tc.want, tc.origin)
		}
	}
	// The other mode gates keep their own defaults (guarded_release off,
	// ops.content auto).
	f, err := Resolve(cityWith("", nil), ResolveOptions{LookupEnv: envMap(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if f.BeadsGuardedRelease() != Off || f.OpsContent() != Auto {
		t.Errorf("guarded_release/ops.content = %q/%q, want off/auto", f.BeadsGuardedRelease(), f.OpsContent())
	}
}
