package rollout

import (
	"bufio"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestResolveConsultsExactlyRegisteredEnvVars pins the env var NAMES Resolve
// reads to the registry's Spec.EnvOverride set: nothing undeclared is consulted,
// and every declared name is consulted. This kills the "rename Spec.EnvOverride,
// break-glass silently no-ops" drift — the registry becomes the source of truth
// the resolver actually obeys.
func TestResolveConsultsExactlyRegisteredEnvVars(t *testing.T) {
	t.Parallel()
	var consulted []string
	rec := func(k string) (string, bool) { consulted = append(consulted, k); return "", false }
	if _, err := Resolve(&config.City{}, ResolveOptions{LookupEnv: rec}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := map[string]bool{}
	for _, s := range Specs() {
		if s.EnvOverride != "" {
			want[s.EnvOverride] = true
		}
	}
	got := map[string]bool{}
	for _, k := range consulted {
		got[k] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve consulted env vars %v, want exactly the registered Spec.EnvOverride set %v", sortedKeys(got), sortedKeys(want))
	}
}

// TestConfigPathAddressesTheFieldResolveReads sets the config field named by each
// Spec.ConfigPath (via reflection) to a valid non-default value and asserts the
// gate resolves as config-origin. If ConfigPath is repointed away from the field
// Resolve actually reads, the gate stays builtin and this fails.
func TestConfigPathAddressesTheFieldResolveReads(t *testing.T) {
	t.Parallel()
	for _, s := range Specs() {
		s := s
		t.Run(s.Key, func(t *testing.T) {
			t.Parallel()
			cfg := &config.City{}
			setConfigFieldNonDefault(t, cfg, s.ConfigPath, s.Default)
			f, err := Resolve(cfg, ResolveOptions{LookupEnv: func(string) (string, bool) { return "", false }})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if f.OriginOf(s.Key) != OriginConfig {
				t.Errorf("%s: set the field at ConfigPath %q to a non-default value but the gate origin is %q, not config — "+
					"ConfigPath does not address the field Resolve reads", s.Key, s.ConfigPath, f.OriginOf(s.Key))
			}
		})
	}
}

// TestEnvOverridesAreLeakVectors moved to internal/testenv (it owns
// LeakVectorVars, and the stray-import lint forbids non-testenv test files from
// importing internal/testenv). See internal/testenv/rollout_leak_vector_test.go.

// TestBeadsVersionAnchorPending documents the CAS gate's "pending" anchor state:
// VersionAnchor names a deps.env key that is currently ABSENT (untagged
// beads#4682), which is legal — distinct from an empty VersionAnchor (a
// validation failure). When the key lands, this test flips and prompts wiring the
// graduation tooth.
func TestBeadsVersionAnchorPending(t *testing.T) {
	t.Parallel()
	s := beadsConditionalWritesSpec()
	if s.VersionAnchor != "BD_CONDITIONAL_WRITES_MIN_VERSION" {
		t.Fatalf("beads VersionAnchor = %q, want BD_CONDITIONAL_WRITES_MIN_VERSION", s.VersionAnchor)
	}
	present, err := depsEnvHasKey("../../deps.env", s.VersionAnchor)
	if err != nil {
		t.Skipf("deps.env not readable from the package dir: %v", err)
	}
	if present {
		t.Errorf("%s is now present in deps.env — the CAS gate has graduated past pending; wire the graduation/removal tooth", s.VersionAnchor)
	}
}

// TestGuardedReleaseVersionAnchorPending documents the guarded-release gate's
// "pending" anchor state, mirroring TestBeadsVersionAnchorPending: the
// VersionAnchor names a deps.env key that is currently ABSENT (the guarded-verb
// bd pin is untagged), which is legal. When the key lands, this test flips and
// prompts wiring the consumer swap (A-G2b) and the graduation tooth.
func TestGuardedReleaseVersionAnchorPending(t *testing.T) {
	t.Parallel()
	s := beadsGuardedReleaseSpec()
	if s.VersionAnchor != "BD_GUARDED_RELEASE_MIN_VERSION" {
		t.Fatalf("guarded-release VersionAnchor = %q, want BD_GUARDED_RELEASE_MIN_VERSION", s.VersionAnchor)
	}
	present, err := depsEnvHasKey("../../deps.env", s.VersionAnchor)
	if err != nil {
		t.Skipf("deps.env not readable from the package dir: %v", err)
	}
	if present {
		t.Errorf("%s is now present in deps.env — the guarded-release gate has graduated past pending; wire the consumer swap and graduation tooth", s.VersionAnchor)
	}
}

// --- reflection helpers (test-only) ---

func setConfigFieldNonDefault(t *testing.T, cfg *config.City, path string, def Default) {
	t.Helper()
	v := reflect.ValueOf(cfg).Elem()
	segs := strings.Split(path, ".")
	for i, seg := range segs {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		f, ok := valueFieldByTOMLName(v, seg)
		if !ok {
			t.Fatalf("ConfigPath %q: no field with toml tag %q", path, seg)
		}
		if i == len(segs)-1 {
			setNonDefault(t, f, def)
			return
		}
		v = f
	}
}

func valueFieldByTOMLName(v reflect.Value, name string) (reflect.Value, bool) {
	tt := v.Type()
	for i := 0; i < tt.NumField(); i++ {
		tag := tt.Field(i).Tag.Get("toml")
		if before, _, _ := strings.Cut(tag, ","); before == name {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// setNonDefault sets f to a valid value that differs from the gate's built-in
// default: a distinct valid mode for a string (Mode) gate, or !default for a
// bool gate.
func setNonDefault(t *testing.T, f reflect.Value, def Default) {
	t.Helper()
	switch {
	case def.Mode != nil:
		for _, m := range []Mode{Require, Auto, Off} {
			if m != *def.Mode {
				f.SetString(string(m))
				return
			}
		}
	case def.Bool != nil:
		want := !*def.Bool
		if f.Kind() == reflect.Pointer {
			np := reflect.New(f.Type().Elem())
			np.Elem().SetBool(want)
			f.Set(np)
		} else {
			f.SetBool(want)
		}
	default:
		t.Fatalf("Default sets no arm")
	}
}

func depsEnvHasKey(path, key string) (bool, error) {
	data, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = data.Close() }()
	sc := bufio.NewScanner(data)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, _, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
			return true, nil
		}
	}
	return false, sc.Err()
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
