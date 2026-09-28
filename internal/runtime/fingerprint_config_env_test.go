package runtime

import (
	"reflect"
	"testing"
)

var stringMapType = reflect.TypeOf(map[string]string{})

// configWithOperatorAuthoredEnv sets the dedicated config-authored environment
// identity field without choosing its production name. The architecture owns
// the field's behavior and deliberately leaves its exact name to the builder.
func configWithOperatorAuthoredEnv(t *testing.T, cfg Config, env map[string]string) Config {
	t.Helper()

	typ := reflect.TypeOf(cfg)
	var candidates []int
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type != stringMapType || field.Name == "Env" || field.Name == "FingerprintExtra" {
			continue
		}
		candidates = append(candidates, i)
		names = append(names, field.Name)
	}
	if len(candidates) != 1 {
		t.Fatalf("runtime.Config has config-authored environment identity candidates %v, want exactly one dedicated map[string]string field besides Env and FingerprintExtra", names)
	}

	reflect.ValueOf(&cfg).Elem().Field(candidates[0]).Set(reflect.ValueOf(env))
	return cfg
}

// TestOperatorAuthoredEnvFingerprintIsDeterministicAndLaunchOnly defines the
// Option A' runtime contract: operator-authored environment is core identity,
// but it belongs exclusively to the cheap Launch half.
func TestOperatorAuthoredEnvFingerprintIsDeterministicAndLaunchOnly(t *testing.T) {
	base := Config{Command: "agent"}
	authored := map[string]string{
		"ALPHA": "one",
		"BETA":  "two",
	}
	one := configWithOperatorAuthoredEnv(t, base, authored)
	two := configWithOperatorAuthoredEnv(t, base, func() map[string]string {
		m := make(map[string]string, 2)
		m["BETA"] = "two"
		m["ALPHA"] = "one"
		return m
	}())
	changed := configWithOperatorAuthoredEnv(t, base, map[string]string{
		"ALPHA": "changed",
		"BETA":  "two",
	})

	if got, want := CoreFingerprint(one), CoreFingerprint(two); got != want {
		t.Errorf("CoreFingerprint depends on config-authored env insertion order: got %q want %q", got, want)
	}
	if got, want := LaunchFingerprint(one), LaunchFingerprint(two); got != want {
		t.Errorf("LaunchFingerprint depends on config-authored env insertion order: got %q want %q", got, want)
	}
	if CoreFingerprint(one) == CoreFingerprint(changed) {
		t.Error("config-authored-env-only mutation did not change CoreFingerprint")
	}
	if LaunchFingerprint(one) == LaunchFingerprint(changed) {
		t.Error("config-authored-env-only mutation did not change LaunchFingerprint")
	}
	if ProvisionFingerprint(one) != ProvisionFingerprint(changed) {
		t.Error("config-authored-env-only mutation changed ProvisionFingerprint; operator env is Launch-tier")
	}
	if !reflect.DeepEqual(authored, map[string]string{"ALPHA": "one", "BETA": "two"}) {
		t.Errorf("fingerprinting mutated the caller-owned config-authored env map: %#v", authored)
	}

	// The dedicated Launch-tier map needs its own framing. Reusing the same
	// key/value in FingerprintExtra must describe a distinct Core identity.
	extra := Config{Command: "agent", FingerprintExtra: map[string]string{"ALPHA": "one", "BETA": "two"}}
	if CoreFingerprint(one) == CoreFingerprint(extra) {
		t.Error("config-authored env collided with FingerprintExtra in CoreFingerprint")
	}
}

// TestOperatorAuthoredEnvFingerprintTreatsNilAndEmptyAsEquivalent prevents an
// empty authored-env table from causing a relaunch relative to an omitted one.
func TestOperatorAuthoredEnvFingerprintTreatsNilAndEmptyAsEquivalent(t *testing.T) {
	nilEnv := configWithOperatorAuthoredEnv(t, Config{}, nil)
	emptyEnv := configWithOperatorAuthoredEnv(t, Config{}, map[string]string{})

	for _, fingerprint := range []struct {
		name string
		fn   func(Config) string
	}{
		{name: "core", fn: CoreFingerprint},
		{name: "launch", fn: LaunchFingerprint},
		{name: "provision", fn: ProvisionFingerprint},
	} {
		if got, want := fingerprint.fn(nilEnv), fingerprint.fn(emptyEnv); got != want {
			t.Errorf("%s fingerprint differs for nil versus empty config-authored env: got %q want %q", fingerprint.name, got, want)
		}
	}
}
