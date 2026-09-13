package rollout

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// cityGates builds a City that sets both Mode gates, so independence tests can
// prove one gate's value never bleeds into another's slot through the shared
// resolveModeGate (read/set are injected per gate).
func cityGates(conditionalWrites, guardedRelease string) *config.City {
	return &config.City{
		Beads: config.BeadsConfig{
			ConditionalWrites: conditionalWrites,
			GuardedRelease:    guardedRelease,
		},
	}
}

// TestBeadsGuardedReleaseAccessorAndForTest pins the ForTest default and the
// typed override for the guarded-release gate.
func TestBeadsGuardedReleaseAccessorAndForTest(t *testing.T) {
	t.Parallel()
	if got := ForTest().BeadsGuardedRelease(); got != Off {
		t.Errorf("ForTest default guarded-release = %q, want off", got)
	}
	if got := ForTest(WithBeadsGuardedRelease(Require)).BeadsGuardedRelease(); got != Require {
		t.Errorf("WithBeadsGuardedRelease(Require) = %q, want require", got)
	}
	// The override sets a config origin so doctor/status render it as an
	// explicit choice, not a builtin default.
	if got := ForTest(WithBeadsGuardedRelease(Auto)).OriginOf(keyBeadsGuardedRelease); got != OriginConfig {
		t.Errorf("WithBeadsGuardedRelease origin = %q, want config", got)
	}
}

// TestResolveGuardedReleasePrecedence walks builtin < config < env for the
// guarded-release gate through its own config field and env var, proving the
// gate-specific wiring (readBeadsGuardedRelease, the Flags setter, and
// envBeadsGuardedRelease) threads correctly through the shared resolveModeGate.
func TestResolveGuardedReleasePrecedence(t *testing.T) {
	t.Parallel()
	env := func(m map[string]string) ResolveOptions { return ResolveOptions{LookupEnv: envMap(m)} }
	K := envBeadsGuardedRelease

	t.Run("builtin off when unset everywhere", func(t *testing.T) {
		t.Parallel()
		f, err := Resolve(cityGates("", ""), env(nil))
		if err != nil {
			t.Fatal(err)
		}
		if f.BeadsGuardedRelease() != Off || f.OriginOf(keyBeadsGuardedRelease) != OriginBuiltin {
			t.Errorf("guarded-release = %q/%q, want off/builtin", f.BeadsGuardedRelease(), f.OriginOf(keyBeadsGuardedRelease))
		}
	})

	t.Run("config wins over builtin", func(t *testing.T) {
		t.Parallel()
		f, err := Resolve(cityGates("", "require"), env(nil))
		if err != nil {
			t.Fatal(err)
		}
		if f.BeadsGuardedRelease() != Require || f.OriginOf(keyBeadsGuardedRelease) != OriginConfig {
			t.Errorf("guarded-release = %q/%q, want require/config", f.BeadsGuardedRelease(), f.OriginOf(keyBeadsGuardedRelease))
		}
	})

	t.Run("valid env active when config unset", func(t *testing.T) {
		t.Parallel()
		f, err := Resolve(cityGates("", ""), env(map[string]string{K: "auto"}))
		if err != nil {
			t.Fatal(err)
		}
		if f.BeadsGuardedRelease() != Auto || f.OriginOf(keyBeadsGuardedRelease) != OriginEnv {
			t.Errorf("guarded-release = %q/%q, want auto/env", f.BeadsGuardedRelease(), f.OriginOf(keyBeadsGuardedRelease))
		}
		assertOneNotice(t, f, NoticeEnvOverrideActive)
	})

	t.Run("env overrides config loudly", func(t *testing.T) {
		t.Parallel()
		f, err := Resolve(cityGates("", "off"), env(map[string]string{K: "require"}))
		if err != nil {
			t.Fatal(err)
		}
		if f.BeadsGuardedRelease() != Require || f.OriginOf(keyBeadsGuardedRelease) != OriginEnv {
			t.Errorf("guarded-release = %q/%q, want require/env", f.BeadsGuardedRelease(), f.OriginOf(keyBeadsGuardedRelease))
		}
		assertOneNotice(t, f, NoticeEnvOverridesConfig)
	})

	t.Run("out-of-enum config value errors (typo never means off)", func(t *testing.T) {
		t.Parallel()
		if _, err := Resolve(cityGates("", "requre"), env(nil)); err == nil {
			t.Errorf("expected an error for an out-of-enum guarded_release config value")
		}
	})
}

// TestModeGatesAreIndependent is the load-bearing guard for the shared
// resolveModeGate: each Mode gate reads its own config field and writes its own
// Flags slot. A cross-wired read/set (the classic copy-paste bug when adding the
// second gate) would make one gate's value appear in the other's accessor; this
// sets the two gates to distinct values and asserts neither leaks.
func TestModeGatesAreIndependent(t *testing.T) {
	t.Parallel()
	f, err := Resolve(cityGates("auto", "require"), ResolveOptions{LookupEnv: envMap(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if f.BeadsConditionalWrites() != Auto {
		t.Errorf("conditional_writes = %q, want auto (guarded_release must not bleed in)", f.BeadsConditionalWrites())
	}
	if f.BeadsGuardedRelease() != Require {
		t.Errorf("guarded_release = %q, want require (conditional_writes must not bleed in)", f.BeadsGuardedRelease())
	}
}
