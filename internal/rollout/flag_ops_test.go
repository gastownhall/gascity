package rollout

import (
	"os"
	"regexp"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// opsGate is one ops gate's wiring, so each precedence case runs against all
// three through their own config field, env var and accessor.
type opsGate struct {
	key  string
	env  string
	set  func(*config.OpsConfig, string)
	get  func(Flags) Mode
	with func(Mode) ForTestOption
}

func opsGates() []opsGate {
	return []opsGate{
		{keyOpsContent, envOpsContent, func(o *config.OpsConfig, v string) { o.ContentMode = v }, Flags.OpsContent, WithOpsContent},
		{keyOpsControl, envOpsControl, func(o *config.OpsConfig, v string) { o.ControlMode = v }, Flags.OpsControl, WithOpsControl},
		{keyOpsLifecycle, envOpsLifecycle, func(o *config.OpsConfig, v string) { o.LifecycleMode = v }, Flags.OpsLifecycle, WithOpsLifecycle},
	}
}

func (g opsGate) city(raw string) *config.City {
	cfg := &config.City{}
	g.set(&cfg.Ops, raw)
	return cfg
}

// TestOpsGatesSpecs pins the registry shape DES-O0API §2.1 and D-GATES fix:
// infra-rollout, default off, the env break-glass, and the shared expiry.
func TestOpsGatesSpecs(t *testing.T) {
	t.Parallel()
	for _, g := range opsGates() {
		s := specByKey(g.key)
		if s.Key != g.key || s.ConfigPath != g.key {
			t.Errorf("%s: Key/ConfigPath = %q/%q, want both %q", g.key, s.Key, s.ConfigPath, g.key)
		}
		if s.Category != InfraRollout {
			t.Errorf("%s: Category = %q, want %q", g.key, s.Category, InfraRollout)
		}
		if s.Default.Mode == nil || *s.Default.Mode != Off {
			t.Errorf("%s: Default = %v, want off (D-GATES: the gates ship off)", g.key, s.Default.Mode)
		}
		if s.EnvOverride != g.env || s.EnvSemantics != EnvOverrides {
			t.Errorf("%s: env = %q/%q, want %q/%q", g.key, s.EnvOverride, s.EnvSemantics, g.env, EnvOverrides)
		}
		if s.Expires != "2027-06-30" {
			t.Errorf("%s: Expires = %q, want 2027-06-30", g.key, s.Expires)
		}
		if s.VersionAnchor != "internal/opjournal/legacyop" {
			t.Errorf("%s: VersionAnchor = %q, want internal/opjournal/legacyop", g.key, s.VersionAnchor)
		}
		if !beadIDShape.MatchString(s.Owner.Bead) {
			t.Errorf("%s: Owner.Bead = %q, want a tracker bead id (ga-...), not a plan label", g.key, s.Owner.Bead)
		}
	}
	if envOpsContent != "GC_OPS_CONTENT" || envOpsControl != "GC_OPS_CONTROL" || envOpsLifecycle != "GC_OPS_LIFECYCLE" {
		t.Errorf("env names = %q, %q, %q", envOpsContent, envOpsControl, envOpsLifecycle)
	}
}

// beadIDShape is the shape of a tracker bead id.
var beadIDShape = regexp.MustCompile(`^ga-[a-z0-9]+$`)

// TestOpsGatesVersionAnchorPresent ties the ops gates' VersionAnchor to the
// package it names: legacyop is deleted with the last ops gate (OJ-22), so
// once it is gone these gates are dead and must be removed too.
func TestOpsGatesVersionAnchorPresent(t *testing.T) {
	t.Parallel()
	for _, g := range opsGates() {
		anchor := specByKey(g.key).VersionAnchor
		if _, err := os.Stat("../../" + anchor + "/doc.go"); err != nil {
			t.Errorf("%s: VersionAnchor %s is gone (%v); the legacy executors it gates were deleted, so remove the %s gate", g.key, anchor, err, g.key)
		}
	}
}

// TestOpsGatesForTest pins the ForTest default and each typed override.
func TestOpsGatesForTest(t *testing.T) {
	t.Parallel()
	for _, g := range opsGates() {
		if got := g.get(ForTest()); got != Off {
			t.Errorf("%s: ForTest default = %q, want off", g.key, got)
		}
		f := ForTest(g.with(Require))
		if got := g.get(f); got != Require {
			t.Errorf("%s: With(Require) = %q, want require", g.key, got)
		}
		if f.OriginOf(g.key) != OriginConfig || f.ValueOf(g.key) != string(Require) {
			t.Errorf("%s: OriginOf/ValueOf = %q/%q, want config/require", g.key, f.OriginOf(g.key), f.ValueOf(g.key))
		}
		for _, other := range opsGates() {
			if other.key != g.key && other.get(f) != Off {
				t.Errorf("With for %s leaked into %s = %q", g.key, other.key, other.get(f))
			}
		}
	}
}

// TestResolveOpsGatesPrecedence walks builtin < config < env for each ops
// gate through its own config field and env var.
func TestResolveOpsGatesPrecedence(t *testing.T) {
	t.Parallel()
	for _, g := range opsGates() {
		g := g
		t.Run(g.key, func(t *testing.T) {
			t.Parallel()
			env := func(m map[string]string) ResolveOptions { return ResolveOptions{LookupEnv: envMap(m)} }

			f, err := Resolve(g.city(""), env(nil))
			if err != nil {
				t.Fatal(err)
			}
			if g.get(f) != Off || f.OriginOf(g.key) != OriginBuiltin {
				t.Errorf("unset: %q/%q, want off/builtin", g.get(f), f.OriginOf(g.key))
			}

			f, err = Resolve(g.city("auto"), env(nil))
			if err != nil {
				t.Fatal(err)
			}
			if g.get(f) != Auto || f.OriginOf(g.key) != OriginConfig || f.ValueOf(g.key) != "auto" {
				t.Errorf("config: %q/%q, want auto/config", g.get(f), f.OriginOf(g.key))
			}

			f, err = Resolve(g.city(""), env(map[string]string{g.env: "require"}))
			if err != nil {
				t.Fatal(err)
			}
			if g.get(f) != Require || f.OriginOf(g.key) != OriginEnv {
				t.Errorf("env, config unset: %q/%q, want require/env", g.get(f), f.OriginOf(g.key))
			}
			assertOneNotice(t, f, NoticeEnvOverrideActive)

			f, err = Resolve(g.city("auto"), env(map[string]string{g.env: "off"}))
			if err != nil {
				t.Fatal(err)
			}
			if g.get(f) != Off || f.OriginOf(g.key) != OriginEnv {
				t.Errorf("env over config: %q/%q, want off/env", g.get(f), f.OriginOf(g.key))
			}
			assertOneNotice(t, f, NoticeEnvOverridesConfig)

			f, err = Resolve(g.city("auto"), env(map[string]string{g.env: "shadow"}))
			if err != nil {
				t.Fatalf("a malformed env value must never fail Resolve: %v", err)
			}
			if g.get(f) != Auto || f.OriginOf(g.key) != OriginConfig {
				t.Errorf("malformed env: %q/%q, want auto/config kept", g.get(f), f.OriginOf(g.key))
			}
			assertOneNotice(t, f, NoticeInvalidEnvIgnored)

			if _, err := Resolve(g.city("requre"), env(nil)); err == nil {
				t.Errorf("an out-of-enum config value must fail Resolve (a typo never means off)")
			}
		})
	}
}

// TestOpsGatesAreIndependent proves each ops gate reads its own field and
// writes its own slot through the shared resolveModeGate.
func TestOpsGatesAreIndependent(t *testing.T) {
	t.Parallel()
	cfg := &config.City{Ops: config.OpsConfig{ContentMode: "auto", ControlMode: "require", LifecycleMode: "off"}}
	f, err := Resolve(cfg, ResolveOptions{LookupEnv: envMap(map[string]string{envOpsLifecycle: "auto"})})
	if err != nil {
		t.Fatal(err)
	}
	if f.OpsContent() != Auto || f.OpsControl() != Require || f.OpsLifecycle() != Auto {
		t.Errorf("content/control/lifecycle = %q/%q/%q, want auto/require/auto",
			f.OpsContent(), f.OpsControl(), f.OpsLifecycle())
	}
	if f.BeadsConditionalWrites() != Auto || f.BeadsGuardedRelease() != Off {
		t.Errorf("ops gates leaked into the beads gates: %q/%q", f.BeadsConditionalWrites(), f.BeadsGuardedRelease())
	}
}
