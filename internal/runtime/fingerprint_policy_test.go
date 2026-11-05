package runtime

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFingerprintExtraChangePolicy pins the declared policy of every
// FingerprintExtra key family. An undeclared key must default to Restart so
// a new identity input can never under-restart silently.
func TestFingerprintExtraChangePolicy(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want ChangePolicy
	}{
		{"pool.min", ChangeIgnore},
		{"pool.max", ChangeIgnore},
		{"skills:plan", ChangeNextStart},
		{"skills:", ChangeNextStart},
		{"mcp:claude", ChangeRestart},
		{"depends_on", ChangeRestart},
		{"wake_mode", ChangeNextStart},
		{"pool", ChangeRestart},
		{"pool.check", ChangeRestart},
		{"skill:plan", ChangeRestart},
		{"brand-new-key", ChangeRestart},
	} {
		if got := FingerprintExtraChangePolicy(tc.key); got != tc.want {
			t.Errorf("FingerprintExtraChangePolicy(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// TestFingerprintExtraNonRestartKeysNeverDrift: changing, adding or removing
// an Ignore (pool sizing) or NextStart (skill content) key moves none of the
// restart-deciding hashes, while a Restart key still moves them all. (The
// diagnostic breakdown keeps hashing every key.)
func TestFingerprintExtraNonRestartKeysNeverDrift(t *testing.T) {
	base := Config{Command: "claude", FingerprintExtra: map[string]string{
		"pool.min": "0", "pool.max": "2", "skills:plan": "h1", "mcp:claude": "m1", "depends_on": "db", "wake_mode": "fresh",
	}}
	with := func(k, v string) Config {
		c := base
		c.FingerprintExtra = map[string]string{}
		for bk, bv := range base.FingerprintExtra {
			c.FingerprintExtra[bk] = bv
		}
		if v == "" {
			delete(c.FingerprintExtra, k)
		} else {
			c.FingerprintExtra[k] = v
		}
		return c
	}
	for _, tc := range []struct {
		name  string
		cfg   Config
		drift bool
	}{
		{"pool.max changed", with("pool.max", "5"), false},
		{"pool.min changed", with("pool.min", "1"), false},
		{"pool sizing removed", with("pool.max", ""), false},
		{"skill content changed", with("skills:plan", "h2"), false},
		{"skill added", with("skills:review", "h3"), false},
		{"skill removed", with("skills:plan", ""), false},
		{"mcp projection changed", with("mcp:claude", "m2"), true},
		{"depends_on changed", with("depends_on", "db,cache"), true},
		{"wake_mode changed", with("wake_mode", "resume"), false},
		{"unknown key added", with("future", "x"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, fp := range map[string]func(Config) string{
				"Config": ConfigFingerprint, "Core": CoreFingerprint, "Provision": ProvisionFingerprint,
			} {
				if got := fp(base) != fp(tc.cfg); got != tc.drift {
					t.Errorf("%sFingerprint moved = %v, want %v", name, got, tc.drift)
				}
			}
		})
	}
}

// TestPrePolicyMigration pins the §3.5 migration against the v6 binary's own
// hashes of cfg (the literals, computed by that binary). Its hashes of
// unchanged config are artifacts, rebaselined without a drain; any other v6
// hash is a real drift since start, which drains (or runs session_live) once;
// a launch-only change on a v6 row still reads its box as unchanged and
// its agent as changed. Another
// version's hash stays an artifact, as before.
func TestPrePolicyMigration(t *testing.T) {
	const (
		v6Core      = "v6:2f209c6790d01059998346b80eba9d2f6f920c710502eac1662d77219d1b6b9e"
		v6Provision = "v6:4109c8c7c284964a145fb139b16ba9b82a7a0efe10c92b046c3cd75a9cc2831f"
		v6Launch    = "v6:339053ed6be63dc41703b2ccfe9ec8a0aaeb8cecad53c4b61cdf84339d0d5194"
		v6Live      = "v6:4bf5122f344554c53bde2ebb8cd2b7e3d1600ad631c385a5d7cce23c7785459a"
	)
	cfg := Config{Command: "claude", FingerprintExtra: map[string]string{"pool.min": "0", "pool.max": "2", "skills:plan": "h1"}}
	for name, got := range map[string][2]string{
		"core": {PrePolicyCoreFingerprint(cfg), v6Core}, "provision": {PrePolicyProvisionFingerprint(cfg), v6Provision},
		"launch": {PrePolicyLaunchFingerprint(cfg), v6Launch}, "live": {PrePolicyLiveFingerprint(cfg), v6Live},
	} {
		if got[0] != got[1] {
			t.Errorf("PrePolicy %s = %q, want the v6 binary's %q", name, got[0], got[1])
		}
	}
	if CoreFingerprint(cfg) == v6Core || strings.HasPrefix(CoreFingerprint(cfg), "v6:") {
		t.Fatal("CoreFingerprint is still the v6 binary's")
	}
	commandChanged := cfg
	commandChanged.Command = "claude --other"
	poolChanged := cfg
	poolChanged.FingerprintExtra = map[string]string{"pool.min": "0", "pool.max": "5", "skills:plan": "h1"}
	liveChanged := cfg
	liveChanged.SessionLive = []string{"tmux set status on"}
	for _, tc := range []struct {
		name string
		got  bool
		want bool
	}{
		{"v6 core of unchanged config", IsCoreHashArtifact(v6Core, cfg), true},
		{"v6 core, command changed since", IsCoreHashArtifact(v6Core, commandChanged), false},
		{"v6 core, pool size changed since", IsCoreHashArtifact(v6Core, poolChanged), false},
		{"another v6 core", IsCoreHashArtifact("v6:"+strings.Repeat("c", 64), cfg), false},
		{"this version's other core", IsCoreHashArtifact(CoreFingerprint(commandChanged), cfg), false},
		{"another version's core", IsCoreHashArtifact("v5:"+strings.Repeat("a", 64), cfg), true},
		{"unversioned core", IsCoreHashArtifact(strings.Repeat("a", 64), cfg), true},
		{"no stored core", IsCoreHashArtifact("", cfg), false},
		{"v6 core is a pre-policy one", IsPrePolicyHash(v6Core), true},
		{"a v5 core is no pre-policy one", IsPrePolicyHash("v5:" + strings.Repeat("a", 64)), false},
		{"v6 live of unchanged session_live", IsLiveHashArtifact(v6Live, cfg), true},
		{"v6 live, session_live changed since", IsLiveHashArtifact(v6Live, liveChanged), false},
		{"another version's live", IsLiveHashArtifact("v5:"+strings.Repeat("b", 64), cfg), true},
		{"v6 provision, command changed since", ProvisionHashMatches(v6Provision, commandChanged), true},
		{"v6 provision, pool changed since", ProvisionHashMatches(v6Provision, poolChanged), false},
		{"this version's provision", ProvisionHashMatches(ProvisionFingerprint(poolChanged), cfg), true},
		{"v6 launch, unchanged", LaunchHashMatches(v6Launch, cfg), true},
		{"v6 launch, command changed since", LaunchHashMatches(v6Launch, commandChanged), false},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	restartOnly := Config{Command: "claude", FingerprintExtra: map[string]string{"mcp:claude": "m1", "depends_on": "db"}}
	if strings.TrimPrefix(PrePolicyCoreFingerprint(restartOnly), "v6:") != strings.TrimPrefix(CoreFingerprint(restartOnly), FingerprintVersion+":") {
		t.Error("a config with only Restart keys must hash as the v6 binary did, but for the version")
	}
}

// TestLogCoreFingerprintDriftShowsRestartExtraOnly: the stderr detail line
// for a drifted FPExtra lists only the keys that can drift, and against a
// v6 baseline also the pool and skill keys it drains once for, while the
// breakdown still hashes every key.
func TestLogCoreFingerprintDriftShowsRestartExtraOnly(t *testing.T) {
	stored := Config{Command: "claude", FingerprintExtra: map[string]string{"pool.max": "2", "depends_on": "db"}}
	current := Config{Command: "claude", FingerprintExtra: map[string]string{"pool.max": "5", "depends_on": "cache"}}
	raw, err := json.Marshal(CoreFingerprintBreakdown(stored))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	LogCoreFingerprintDrift(&out, "worker", string(raw), current)
	if !strings.Contains(out.String(), "FPExtra: map[depends_on:cache] (len=1)") {
		t.Fatalf("drift detail = %q, want FPExtra listing only depends_on", out.String())
	}
	if strings.Contains(out.String(), "v6 baseline") {
		t.Fatalf("drift detail = %q, want no v6 line against this version's breakdown", out.String())
	}
	v6Breakdown := CoreFingerprintBreakdown(stored)
	v6Breakdown.Version = "v6"
	raw, err = json.Marshal(v6Breakdown)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	LogCoreFingerprintDrift(&out, "worker", string(raw), current)
	if !strings.Contains(out.String(), "FPExtra (v6 baseline, drains once): map[pool.max:5] (len=1)") {
		t.Fatalf("drift detail = %q, want the pool key a v6 baseline drains once for", out.String())
	}
	if CoreFingerprintBreakdown(stored).Fields["FPExtra"] == CoreFingerprintBreakdown(Config{Command: "claude", FingerprintExtra: map[string]string{"pool.max": "5", "depends_on": "db"}}).Fields["FPExtra"] {
		t.Fatal("the breakdown no longer hashes Ignore keys")
	}
}
