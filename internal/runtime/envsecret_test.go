package runtime

import (
	"maps"
	"strings"
	"testing"
)

func TestArgvSecretEnvValue(t *testing.T) {
	tests := []struct {
		key, value string
		secret     bool
		why        string
	}{
		{"ANTHROPIC_AUTH_TOKEN", "sk-test", true, "the credential this whole path exists for"},
		{"OPENAI_API_KEY", "sk-test", true, ""},
		{"ZCODE_API_KEY", "zk-test", true, ""},
		{"GC_INSTANCE_TOKEN", "deadbeef", true, "fences drain/stop; a capability, not an identifier"},
		{"GC_CONTROLLER_TOKEN", "deadbeef", true, "controller scope; drives the convergence loop"},
		{"GC_DOLT_PASSWORD", "hunter2", true, ""},
		{"SOME_BRAND_NEW_SECRET", "x", true, "an unknown name defaults to secret — that is the point of an allow list"},
		{"LANG", "en_US.UTF-8", false, ""},
		{"GC_RIG", "kernel", false, ""},
		{"GT_ROLE", "rig/crew/name", false, ""},
		{"ANTHROPIC_AUTH_TOKEN", "", false, "an empty value means 'withhold this var' and reveals nothing"},
	}
	for _, tc := range tests {
		if got := ArgvSecretEnvValue(tc.key, tc.value); got != tc.secret {
			t.Errorf("ArgvSecretEnvValue(%q, len %d) = %v, want %v %s",
				tc.key, len(tc.value), got, tc.secret, tc.why)
		}
	}
}

// TestArgvAllowListHoldsNoCredentialShapedNames is a standing guard on the
// allow list itself: the review that adds a name is the only thing between a
// credential and a world-readable command line, so make an obviously
// credential-shaped name fail the build rather than the audit.
func TestArgvAllowListHoldsNoCredentialShapedNames(t *testing.T) {
	banned := []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "PRIVATE", "API_KEY", "APIKEY", "AUTH"}
	for key := range envArgvSafe {
		upper := strings.ToUpper(key)
		for _, frag := range banned {
			if strings.Contains(upper, frag) {
				t.Errorf("%q is on the argv allow list but its name contains %q", key, frag)
			}
		}
	}
}

func TestEnvHasArgvSecrets(t *testing.T) {
	if EnvHasArgvSecrets(nil) {
		t.Error("nil env holds no secrets")
	}
	if EnvHasArgvSecrets(map[string]string{"LANG": "C", "GC_RIG": "r", "LC_ALL": ""}) {
		t.Error("an all-inert env must not force the private-file path")
	}
	if !EnvHasArgvSecrets(map[string]string{"LANG": "C", "GC_INSTANCE_TOKEN": "deadbeef"}) {
		t.Error("one secret is enough to force the private-file path")
	}
}

func TestSplitEnvByArgvSafety(t *testing.T) {
	env := map[string]string{
		"LANG":                 "C",
		"GC_RIG":               "kernel",
		"LC_ALL":               "",
		"ANTHROPIC_AUTH_TOKEN": "sk-test",
		"GC_INSTANCE_TOKEN":    "deadbeef",
	}
	safe, secret := SplitEnvByArgvSafety(env)
	wantSafe := map[string]string{"LANG": "C", "GC_RIG": "kernel", "LC_ALL": ""}
	wantSecret := map[string]string{"ANTHROPIC_AUTH_TOKEN": "sk-test", "GC_INSTANCE_TOKEN": "deadbeef"}
	if !maps.Equal(safe, wantSafe) {
		t.Errorf("safe keys = %v, want %v", keysOf(safe), keysOf(wantSafe))
	}
	if !maps.Equal(secret, wantSecret) {
		t.Errorf("secret keys = %v, want %v", keysOf(secret), keysOf(wantSecret))
	}

	// Every input entry must land in exactly one half — a dropped entry is a
	// silently missing variable in the agent's environment.
	if len(safe)+len(secret) != len(env) {
		t.Errorf("partition lost entries: %d + %d != %d", len(safe), len(secret), len(env))
	}

	empty, emptySecret := SplitEnvByArgvSafety(nil)
	if empty == nil || emptySecret == nil {
		t.Error("both halves must be non-nil so callers can range without a nil check")
	}
}

func TestMetaSeedableEnvKey(t *testing.T) {
	// The meta store is a private sidecar, not argv, so it seeds one key argv
	// refuses: the incarnation fence's own token. Everything else tracks
	// envArgvSafe, so there is no second classification to drift.
	if !MetaSeedableEnvKey("GC_INSTANCE_TOKEN") {
		t.Error("the fence token must seed: every consumer reads it back via GetMeta and treats absence as permission")
	}
	if !MetaSeedableEnvKey("GC_SESSION_ID") {
		t.Error("an argv-safe identity key must remain seedable")
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "BEADS_HOLDER_TOKEN", "SOME_FUTURE_KEY"} {
		if MetaSeedableEnvKey(key) {
			t.Errorf("%s has no GetMeta consumer and must not reach disk", key)
		}
	}
}

func TestSplitEnvForMetaSeed(t *testing.T) {
	env := map[string]string{
		"GC_SESSION_ID":        "sess-1",
		"GC_INSTANCE_TOKEN":    "deadbeef",
		"GC_RUNTIME_EPOCH":     "3",
		"LC_ALL":               "",
		"ANTHROPIC_AUTH_TOKEN": "sk-test-NOT-REAL",
	}
	seed, withheld := SplitEnvForMetaSeed(env)

	wantSeed := map[string]string{
		"GC_SESSION_ID":     "sess-1",
		"GC_INSTANCE_TOKEN": "deadbeef",
		"GC_RUNTIME_EPOCH":  "3",
		"LC_ALL":            "",
	}
	wantWithheld := map[string]string{"ANTHROPIC_AUTH_TOKEN": "sk-test-NOT-REAL"}
	if !maps.Equal(seed, wantSeed) {
		t.Errorf("seed keys = %v, want %v", keysOf(seed), keysOf(wantSeed))
	}
	if !maps.Equal(withheld, wantWithheld) {
		t.Errorf("withheld keys = %v, want %v", keysOf(withheld), keysOf(wantWithheld))
	}

	// The control on over-withholding: a filter that withheld everything would
	// satisfy "no credential on disk" and silently break incarnation fencing,
	// which fails open and so reports nothing when it breaks.
	if seed["GC_INSTANCE_TOKEN"] != "deadbeef" {
		t.Error("the fence token must survive the split with its exact value")
	}
	if len(seed)+len(withheld) != len(env) {
		t.Errorf("partition lost entries: %d + %d != %d", len(seed), len(withheld), len(env))
	}

	seedNil, withheldNil := SplitEnvForMetaSeed(nil)
	if seedNil == nil || withheldNil == nil {
		t.Error("both halves must be non-nil so callers can range without a nil check")
	}
}

func TestMetaCapabilityKeysAreNotArgvSafe(t *testing.T) {
	// The two classifications answer different questions, and the capability
	// set exists precisely because a key can be too secret for argv while still
	// being required on the private sidecar. A name drifting into envArgvSafe
	// would widen argv routing without anyone editing that list.
	for key := range metaCapabilityEnv {
		if ArgvSafeEnvKey(key) {
			t.Errorf("%s is in both metaCapabilityEnv and envArgvSafe; the capability set is only for keys argv must refuse", key)
		}
	}
	if len(metaCapabilityEnv) == 0 {
		t.Error("control: an empty capability set would make this test vacuous")
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
