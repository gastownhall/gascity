package runtime

import "testing"

// TestFingerprintPartitionCoversCoreDisjointly is the safety net for the un-weld
// split: it proves ProvisionFingerprint and LaunchFingerprint partition the
// CoreFingerprint field set completely and disjointly, AND that each field lands
// in the deliberately-chosen half (see fingerprint_partition.go / the un-weld
// design §6). For every core field, mutating ONLY that field must (a) change
// CoreFingerprint — so the field is genuinely in Core — and (b) change exactly
// one of {Provision, Launch} — the expected one. If a field stops moving the
// fingerprint, or lands in both/neither half, or flips half, this fails loudly.
func TestFingerprintPartitionCoversCoreDisjointly(t *testing.T) {
	base := goldenFixtures()["comprehensive"]
	falsePtr := false

	cases := []struct {
		field  string
		half   string // "provision" | "launch"
		mutate func(c *Config)
	}{
		// LAUNCH (agent) half.
		{"Command", "launch", func(c *Config) { c.Command += " --changed" }},
		{"Lifecycle", "launch", func(c *Config) { c.Lifecycle = Lifecycle("persistent") }},
		{"MCPServers", "launch", func(c *Config) {
			c.MCPServers = []MCPServerConfig{{Name: "mail", Transport: MCPTransport("stdio"), Command: "different-mcp"}}
		}},
		{"AcceptStartupDialogs", "launch", func(c *Config) { c.AcceptStartupDialogs = &falsePtr }},
		{"MouseOn", "launch", func(c *Config) { c.MouseOn = !c.MouseOn }},
		// SessionSetup/SessionSetupScript are LAUNCH-half (B2): the carriers replay
		// them on relaunch, so a change relaunches rather than reprovisions.
		{"SessionSetup", "launch", func(c *Config) { c.SessionSetup = []string{"echo different-setup"} }},
		{"SessionSetupScript", "launch", func(c *Config) { c.SessionSetupScript = "/different-setup.sh" }},

		// PROVISION (box) half.
		{"Env", "provision", func(c *Config) { c.Env = envWith(base.Env, "GC_CITY", "different-city") }},
		{"FingerprintExtra", "provision", func(c *Config) { c.FingerprintExtra = map[string]string{"pool": "different"} }},
		{"PreStart", "provision", func(c *Config) { c.PreStart = []string{"echo different-prestart"} }},
		{"OverlayDir", "provision", func(c *Config) { c.OverlayDir = "/different-overlay" }},
		{"OverlayProviders", "provision", func(c *Config) { c.ProviderOverlayName = "different-overlay-provider" }},
		{"CopyFiles", "provision", func(c *Config) { c.CopyFiles = []CopyEntry{{Src: "/different", RelDst: "z"}} }},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			mutated := base // struct copy; mutators REPLACE slice/map fields (never mutate base's shared backing)
			tc.mutate(&mutated)

			if CoreFingerprint(base) == CoreFingerprint(mutated) {
				t.Fatalf("%s: mutation did not change CoreFingerprint — field is not in Core (or the mutation is a no-op)", tc.field)
			}

			provChanged := ProvisionFingerprint(base) != ProvisionFingerprint(mutated)
			launchChanged := LaunchFingerprint(base) != LaunchFingerprint(mutated)

			if provChanged == launchChanged {
				t.Fatalf("%s: must change exactly one half (disjoint+complete), got provChanged=%v launchChanged=%v", tc.field, provChanged, launchChanged)
			}
			switch tc.half {
			case "provision":
				if !provChanged {
					t.Fatalf("%s: expected the PROVISION half to change, but LAUNCH changed", tc.field)
				}
			case "launch":
				if !launchChanged {
					t.Fatalf("%s: expected the LAUNCH half to change, but PROVISION changed", tc.field)
				}
			}
		})
	}
}

// TestFingerprintPartitionStableAndVersioned pins the shape: both halves are
// version-prefixed and deterministic for the same config.
func TestFingerprintPartitionStableAndVersioned(t *testing.T) {
	cfg := goldenFixtures()["comprehensive"]
	for _, fp := range []struct {
		name string
		fn   func(Config) string
	}{
		{"ProvisionFingerprint", ProvisionFingerprint},
		{"LaunchFingerprint", LaunchFingerprint},
	} {
		a, b := fp.fn(cfg), fp.fn(cfg)
		if a != b {
			t.Errorf("%s not deterministic: %q != %q", fp.name, a, b)
		}
		if want := FingerprintVersion + ":"; len(a) <= len(want) || a[:len(want)] != want {
			t.Errorf("%s = %q, want %q prefix", fp.name, a, want)
		}
	}
	// The two halves are distinct hashes (not accidentally the same function).
	if ProvisionFingerprint(cfg) == LaunchFingerprint(cfg) {
		t.Error("ProvisionFingerprint and LaunchFingerprint produced identical hashes for the comprehensive fixture")
	}
}

func envWith(base map[string]string, key, val string) map[string]string {
	m := make(map[string]string, len(base)+1)
	for k, v := range base {
		m[k] = v
	}
	m[key] = val
	return m
}
