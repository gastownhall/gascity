package runtime

import (
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
		{"wake_mode", ChangeRestart},
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
		{"wake_mode changed", with("wake_mode", "resume-ish"), true},
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

// TestCoreHashArtifactMigratesPrePolicyHashes pins the §3.5 migration. A
// session started before the policy stored a core hash with pool sizing and
// skill hashes inside (the literal is CoreFingerprint of cfg as the previous
// binary computed it). For unchanged config it is an artifact, rebaselined
// without a drain; once a Restart field, or the pool size itself, changed
// since, the old behavior applies one last time. A config with no Ignore or
// NextStart key hashes byte-identically, so its sessions are not touched.
func TestCoreHashArtifactMigratesPrePolicyHashes(t *testing.T) {
	const storedBeforePolicy = "v6:2f209c6790d01059998346b80eba9d2f6f920c710502eac1662d77219d1b6b9e"
	cfg := Config{Command: "claude", FingerprintExtra: map[string]string{"pool.min": "0", "pool.max": "2", "skills:plan": "h1"}}
	if got := PrePolicyCoreFingerprint(cfg); got != storedBeforePolicy {
		t.Fatalf("PrePolicyCoreFingerprint = %q, want the previous binary's %q", got, storedBeforePolicy)
	}
	if CoreFingerprint(cfg) == storedBeforePolicy {
		t.Fatal("CoreFingerprint still hashes pool sizing and skills")
	}
	commandChanged := cfg
	commandChanged.Command = "claude --other"
	poolChanged := cfg
	poolChanged.FingerprintExtra = map[string]string{"pool.min": "0", "pool.max": "5", "skills:plan": "h1"}
	for _, tc := range []struct {
		name   string
		stored string
		cfg    Config
		want   bool
	}{
		{"unchanged config: rebaseline", storedBeforePolicy, cfg, true},
		{"command changed since: drift", storedBeforePolicy, commandChanged, false},
		{"pool size changed since: drift one last time", storedBeforePolicy, poolChanged, false},
		{"another config's current hash: drift", CoreFingerprint(commandChanged), cfg, false},
		{"another version's hash: rebaseline", "v5:" + strings.Repeat("a", 64), cfg, true},
		{"no stored hash", "", Config{}, false},
	} {
		if got := IsCoreHashArtifact(tc.stored, tc.cfg); got != tc.want {
			t.Errorf("%s: IsCoreHashArtifact = %v, want %v", tc.name, got, tc.want)
		}
	}
	restartOnly := Config{Command: "claude", FingerprintExtra: map[string]string{"mcp:claude": "m1", "depends_on": "db"}}
	if PrePolicyCoreFingerprint(restartOnly) != CoreFingerprint(restartOnly) {
		t.Error("a config with only Restart keys must hash as it did before the policy")
	}
}
