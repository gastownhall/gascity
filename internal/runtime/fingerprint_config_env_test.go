package runtime

import (
	"reflect"
	"testing"
)

// configWithOperatorAuthoredEnv sets Config.OperatorEnv, the dedicated
// config-authored environment identity field. It names the field directly, so a
// rename fails to compile and another map[string]string field added to Config
// cannot be mistaken for it (a new field must be classified by
// TestFingerprintPartitionAccountsForEveryConfigField).
func configWithOperatorAuthoredEnv(t *testing.T, cfg Config, env map[string]string) Config {
	t.Helper()
	cfg.OperatorEnv = env
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

// TestDoltPortProjectionDoesNotDriftAgentFingerprint guards the upgrade path
// of gastownhall/gascity#1656: gc now withholds the projected Dolt port from
// a gc-managed mirrored scope's session env, so the same agent config exists
// in the wild with the port projected (base) and withheld (empty). Neither
// shape may change the agent's identity — nothing drains on the upgrade, and
// nothing drains when a managed Dolt restart moves the port.
func TestDoltPortProjectionDoesNotDriftAgentFingerprint(t *testing.T) {
	base := Config{Command: "agent", Env: map[string]string{"BEADS_DIR": "/city/.beads"}}
	projected := map[string]string{"BEADS_DIR": "/city/.beads"}
	for key, value := range map[string]string{
		"GC_DOLT_PORT":           "45217",
		"BEADS_DOLT_SERVER_PORT": "45217",
		"BEADS_DOLT_PORT":        "45217",
	} {
		projected[key] = value
	}
	withheld := map[string]string{"BEADS_DIR": "/city/.beads"}
	for _, key := range []string{"GC_DOLT_PORT", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"} {
		withheld[key] = ""
	}

	one := base
	one.Env = projected
	two := base
	two.Env = withheld

	if got, want := CoreFingerprint(one), CoreFingerprint(two); got != want {
		t.Errorf("CoreFingerprint depends on the projected Dolt port: got %q want %q", got, want)
	}
	if got, want := ConfigFingerprint(one), ConfigFingerprint(two); got != want {
		t.Errorf("ConfigFingerprint depends on the projected Dolt port: got %q want %q", got, want)
	}
	if got, want := LaunchFingerprint(one), LaunchFingerprint(two); got != want {
		t.Errorf("LaunchFingerprint depends on the projected Dolt port: got %q want %q", got, want)
	}
}
