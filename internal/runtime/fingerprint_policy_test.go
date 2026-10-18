package runtime

import (
	"encoding/json"
	"slices"
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
// restart-deciding hashes, nor the breakdown that diagnoses them, while a
// Restart key still moves them all.
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
				"ConfigFingerprint": ConfigFingerprint, "CoreFingerprint": CoreFingerprint, "ProvisionFingerprint": ProvisionFingerprint,
				"the breakdown's FPExtra": func(c Config) string { return CoreFingerprintBreakdown(c).Fields["FPExtra"] },
			} {
				if got := fp(base) != fp(tc.cfg); got != tc.drift {
					t.Errorf("%s moved = %v, want %v", name, got, tc.drift)
				}
			}
		})
	}
}

// TestCoreHashArtifactMigratesPrePolicyHashes pins the v7 migration. A
// session started by a v6 binary stored a core hash with pool sizing and skill
// hashes inside (the literal is CoreFingerprint of cfg as the v6 binary
// computed it). For unchanged config it is an artifact, rebaselined without a
// drain; once a Restart field, or the pool size itself, changed since, it
// drifts one last time, as it would have under v6. Every other version's hash
// is an artifact, as at any version bump.
func TestCoreHashArtifactMigratesPrePolicyHashes(t *testing.T) {
	const storedBeforePolicy = "v6:2f209c6790d01059998346b80eba9d2f6f920c710502eac1662d77219d1b6b9e"
	cfg := Config{Command: "claude", FingerprintExtra: map[string]string{"pool.min": "0", "pool.max": "2", "skills:plan": "h1"}}
	if got := PrePolicyCoreFingerprint(cfg); got != storedBeforePolicy {
		t.Fatalf("PrePolicyCoreFingerprint = %q, want the v6 binary's %q", got, storedBeforePolicy)
	}
	if hashDigest(CoreFingerprint(cfg)) == hashDigest(storedBeforePolicy) {
		t.Fatal("CoreFingerprint still hashes pool sizing and skills")
	}
	commandChanged := cfg
	commandChanged.Command = "claude --other"
	poolChanged := cfg
	poolChanged.FingerprintExtra = map[string]string{"pool.min": "0", "pool.max": "5", "skills:plan": "h1"}
	hex := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name   string
		stored string
		cfg    Config
		want   bool
	}{
		{"unchanged config: rebaseline", storedBeforePolicy, cfg, true},
		{"command changed since: drift", storedBeforePolicy, commandChanged, false},
		{"pool size changed since: drift one last time", storedBeforePolicy, poolChanged, false},
		{"another config's v6 hash: drift, as under v6", "v6:" + hex, cfg, false},
		{"another config's current hash: drift", CoreFingerprint(commandChanged), cfg, false},
		{"an older version's hash: rebaseline", "v5:" + hex, cfg, true},
		{"a newer version's hash: rebaseline", "v8:" + hex, cfg, true},
		{"an unversioned hash: rebaseline", hex, cfg, true},
		{"no stored hash", "", Config{}, false},
	} {
		if got := IsCoreHashArtifact(tc.stored, tc.cfg); got != tc.want {
			t.Errorf("%s: IsCoreHashArtifact = %v, want %v", tc.name, got, tc.want)
		}
	}
	// A config with only Restart keys hashes the digest it hashed under v6.
	const restartOnlyBeforePolicy = "v6:aafadbdfd4cfba9a810d585865bdf9ad6804250fa6ae9d23e83001350d8e94b1"
	restartOnly := Config{Command: "claude", FingerprintExtra: map[string]string{"mcp:claude": "m1", "depends_on": "db"}}
	if got := PrePolicyCoreFingerprint(restartOnly); got != restartOnlyBeforePolicy {
		t.Fatalf("PrePolicyCoreFingerprint = %q, want the v6 binary's %q", got, restartOnlyBeforePolicy)
	}
	if hashDigest(CoreFingerprint(restartOnly)) != hashDigest(restartOnlyBeforePolicy) {
		t.Error("a config with only Restart keys must hash the digest it hashed under v6")
	}
	if !IsCoreHashArtifact(restartOnlyBeforePolicy, restartOnly) {
		t.Error("the v6 hash of a config with only Restart keys must rebaseline")
	}
}

// TestPrePolicyProvisionAndLiveHashes pins the v6 forms of the other stored
// hashes (the literals are as the v6 binary computed them). A v6 provision hash
// of the config still proves a launch-only drift, so the session relaunches in
// its box; a v6 live hash is an artifact only for the session_live it was
// computed from, so a session_live changed since still runs.
func TestPrePolicyProvisionAndLiveHashes(t *testing.T) {
	const provisionBeforePolicy = "v6:4109c8c7c284964a145fb139b16ba9b82a7a0efe10c92b046c3cd75a9cc2831f"
	cfg := Config{Command: "claude", FingerprintExtra: map[string]string{"pool.min": "0", "pool.max": "2", "skills:plan": "h1"}}
	if got := PrePolicyProvisionFingerprint(cfg); got != provisionBeforePolicy {
		t.Fatalf("PrePolicyProvisionFingerprint = %q, want the v6 binary's %q", got, provisionBeforePolicy)
	}
	if got, want := hashDigest(LaunchFingerprint(cfg)), hashDigest("v6:339053ed6be63dc41703b2ccfe9ec8a0aaeb8cecad53c4b61cdf84339d0d5194"); got != want {
		t.Errorf("LaunchFingerprint digest = %s, want the v6 binary's %s: the launch half hashes no FingerprintExtra", got, want)
	}
	poolChanged := cfg
	poolChanged.FingerprintExtra = map[string]string{"pool.min": "0", "pool.max": "5", "skills:plan": "h1"}
	hex := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name   string
		stored string
		want   bool
	}{
		{"this binary's hash", ProvisionFingerprint(cfg), true},
		{"the v6 hash of the config", provisionBeforePolicy, true},
		{"the v6 hash of another pool size", PrePolicyProvisionFingerprint(poolChanged), false},
		{"another version's hash", "v5:" + hex, false},
		{"no stored hash", "", false},
	} {
		if got := ProvisionHashMatches(tc.stored, cfg); got != tc.want {
			t.Errorf("%s: ProvisionHashMatches = %v, want %v", tc.name, got, tc.want)
		}
	}

	const liveBeforePolicy = "v6:f8ac99cd970367c7abedd88bb4ad8d5c9b1f100ec459df8c35154c265d13616d"
	live := Config{Command: "claude", SessionLive: []string{"tmux set status on"}}
	for _, tc := range []struct {
		name   string
		stored string
		want   bool
	}{
		{"the v6 hash of the session_live: rebaseline", liveBeforePolicy, true},
		{"the v6 hash of no session_live: run it", "v6:4bf5122f344554c53bde2ebb8cd2b7e3d1600ad631c385a5d7cce23c7785459a", false},
		{"this binary's hash", LiveFingerprint(live), false},
		{"this binary's hash of no session_live", LiveFingerprint(Config{}), false},
		{"an older version's hash: rebaseline", "v5:" + hex, true},
		{"a newer version's hash: rebaseline", "v8:" + hex, true},
		{"an unversioned hash: rebaseline", hex, true},
		{"no stored hash", "", false},
	} {
		if got := IsLiveHashArtifact(tc.stored, live); got != tc.want {
			t.Errorf("%s: IsLiveHashArtifact = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCoreFingerprintDriftFieldsFollowThePolicy: the breakdown names FPExtra
// only for a change the core hash sees. A breakdown another version stored
// (v6 hashed every key; a legacy map carries no version) is diffed against the
// config hashed the way it was, so the upgrade alone names no field.
func TestCoreFingerprintDriftFieldsFollowThePolicy(t *testing.T) {
	// CoreFingerprintBreakdown(started) as the v6 binary stored it.
	const startedBeforePolicy = `{"version":"v6","fields":{"AcceptStartupDialogs":"e3b0c44298fc1c14","Command":"c857d09db23e6822","CopyFiles":"e3b0c44298fc1c14","Env":"e3b0c44298fc1c14","FPExtra":"0da4cd209bedc4a9","Lifecycle":"e3b0c44298fc1c14","MCPServers":"e3b0c44298fc1c14","MouseOn":"b1865da277ec01c2","OverlayDir":"e3b0c44298fc1c14","OverlayProviders":"e3b0c44298fc1c14","PreStart":"e3b0c44298fc1c14","SessionSetup":"e3b0c44298fc1c14","SessionSetupScript":"e3b0c44298fc1c14"}}`
	started := Config{Command: "claude", FingerprintExtra: map[string]string{"pool.max": "2", "skills:plan": "h1", "mcp:claude": "m1"}}
	with := func(command string, extra map[string]string) Config {
		c := Config{Command: command, FingerprintExtra: map[string]string{}}
		for k, v := range started.FingerprintExtra {
			c.FingerprintExtra[k] = v
		}
		for k, v := range extra {
			c.FingerprintExtra[k] = v
		}
		return c
	}
	var beforePolicy BreakdownV1
	if err := json.Unmarshal([]byte(startedBeforePolicy), &beforePolicy); err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(beforePolicy.Fields)
	if err != nil {
		t.Fatal(err)
	}
	current, err := json.Marshal(CoreFingerprintBreakdown(started))
	if err != nil {
		t.Fatal(err)
	}
	commandAndNonRestart := with("claude --other", map[string]string{"pool.max": "5", "skills:plan": "h2"})
	for _, tc := range []struct {
		name   string
		stored string
		cfg    Config
		want   []string
	}{
		{"pool and skill edits beside a command change", string(current), commandAndNonRestart, []string{"Command"}},
		{"a Restart key's change", string(current), with("claude", map[string]string{"mcp:claude": "m2"}), []string{"FPExtra"}},
		{"v6: unchanged config", startedBeforePolicy, started, nil},
		{"v6: a command change", startedBeforePolicy, with("claude --other", nil), []string{"Command"}},
		{"v6: a pool edit, which the v6 hash saw", startedBeforePolicy, with("claude", map[string]string{"pool.max": "5"}), []string{"FPExtra"}},
		{"legacy map: unchanged config", string(legacy), started, nil},
		{"legacy map: a command change", string(legacy), with("claude --other", nil), []string{"Command"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CoreFingerprintDriftFieldsFromJSON(tc.stored, tc.cfg); !slices.Equal(got, tc.want) {
				t.Errorf("CoreFingerprintDriftFieldsFromJSON = %v, want %v", got, tc.want)
			}
			var stored BreakdownV1
			if err := json.Unmarshal([]byte(tc.stored), &stored); err != nil || stored.Version == "" {
				return // a legacy map: no typed form
			}
			if got := CoreFingerprintDriftFields(stored, tc.cfg); !slices.Equal(got, tc.want) {
				t.Errorf("CoreFingerprintDriftFields = %v, want %v", got, tc.want)
			}
		})
	}
	for _, stored := range []string{string(current), startedBeforePolicy} {
		var out strings.Builder
		LogCoreFingerprintDrift(&out, "worker", stored, with("claude --other", nil))
		if got := out.String(); !strings.Contains(got, "drifted fields: Command\n") || strings.Contains(got, "FPExtra") {
			t.Errorf("LogCoreFingerprintDrift against %s:\n%s\nwant only Command drifted", stored, got)
		}
	}
	var out strings.Builder
	LogCoreFingerprintDrift(&out, "worker", string(current), commandAndNonRestart)
	if got := out.String(); !strings.Contains(got, "drifted fields: Command\n") || strings.Contains(got, "FPExtra") {
		t.Errorf("LogCoreFingerprintDrift:\n%s\nwant only Command drifted", got)
	}
	// An FPExtra drift prints the keys the stored version hashed: this
	// binary's Restart keys, or, for a v6 breakdown, every key.
	for _, tc := range []struct {
		stored string
		cfg    Config
		want   string
	}{
		{string(current), with("claude", map[string]string{"mcp:claude": "m2"}), "    FPExtra: map[mcp:claude:m2] (len=1)\n"},
		{startedBeforePolicy, with("claude", map[string]string{"pool.max": "5"}), "    FPExtra: map[mcp:claude:m1 pool.max:5 skills:plan:h1] (len=3)\n"},
	} {
		var out strings.Builder
		LogCoreFingerprintDrift(&out, "worker", tc.stored, tc.cfg)
		if got := out.String(); !strings.Contains(got, tc.want) {
			t.Errorf("LogCoreFingerprintDrift against %s:\n%s\nwant the line %q", tc.stored, got, tc.want)
		}
	}
}

// hashDigest is a fingerprint hash without its version prefix.
func hashDigest(stored string) string {
	_, digest, _ := strings.Cut(stored, ":")
	return digest
}
