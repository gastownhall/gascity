package runtime

import (
	"reflect"
	"testing"
)

// partitionHalfCases is the behavioral witness for the un-weld split: mutating
// ONLY the named field must change CoreFingerprint AND change exactly the named
// half of {Provision, Launch}. Hoisted to package scope so the completeness
// guard (TestFingerprintPartitionAccountsForEveryConfigField) can cross-check it
// against coreFieldHalf.
var partitionHalfCases = []struct {
	field  string
	half   string // "provision" | "launch"
	mutate func(c *Config)
}{
	// LAUNCH (agent) half.
	{"Command", "launch", func(c *Config) { c.Command += " --changed" }},
	{"Lifecycle", "launch", func(c *Config) { c.Lifecycle = Lifecycle("persistent") }},
	{"Upstream", "launch", func(c *Config) { c.Upstream = "bedrock" }},
	{"MCPServers", "launch", func(c *Config) {
		c.MCPServers = []MCPServerConfig{{Name: "mail", Transport: MCPTransport("stdio"), Command: "different-mcp"}}
	}},
	{"AcceptStartupDialogs", "launch", func(c *Config) { b := false; c.AcceptStartupDialogs = &b }},
	{"MouseOn", "launch", func(c *Config) { c.MouseOn = !c.MouseOn }},
	// SessionSetup/SessionSetupScript are LAUNCH-half (B2): the carriers replay
	// them on relaunch, so a change relaunches rather than reprovisions.
	{"SessionSetup", "launch", func(c *Config) { c.SessionSetup = []string{"echo different-setup"} }},
	{"SessionSetupScript", "launch", func(c *Config) { c.SessionSetupScript = "/different-setup.sh" }},

	// PROVISION (box) half.
	{"Env", "provision", func(c *Config) { c.Env = envWith(c.Env, "GC_CITY", "different-city") }},
	{"FingerprintExtra", "provision", func(c *Config) { c.FingerprintExtra = map[string]string{"pool": "different"} }},
	{"PreStart", "provision", func(c *Config) { c.PreStart = []string{"echo different-prestart"} }},
	{"OverlayDir", "provision", func(c *Config) { c.OverlayDir = "/different-overlay" }},
	// ProviderOverlayName is the behavioral witness for the overlay-providers core
	// hash, which also folds in ProviderName + InstallAgentHooks (see coreFieldHalf).
	{"ProviderOverlayName", "provision", func(c *Config) { c.ProviderOverlayName = "different-overlay-provider" }},
	{"CopyFiles", "provision", func(c *Config) { c.CopyFiles = []CopyEntry{{Src: "/different", RelDst: "z"}} }},
}

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

	for _, tc := range partitionHalfCases {
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

// coreFieldHalf maps EVERY Config field that feeds CoreFingerprint to its
// partition half. excludedFromCore lists every Config field deliberately NOT in
// CoreFingerprint, with the reason. Together they must account for every exported
// Config field (enforced by TestFingerprintPartitionAccountsForEveryConfigField).
//
// Not every core field has its own behavioral case in partitionHalfCases: the
// overlay-providers core hash folds ProviderName + ProviderOverlayName +
// InstallAgentHooks through OverlayProviderNames into the PROVISION half
// (ProviderName is used only when ProviderOverlayName is empty), so all three are
// classified provision here and ProviderOverlayName is the behavioral witness.
var coreFieldHalf = map[string]string{
	// LAUNCH (agent) half.
	"Command":              "launch",
	"Lifecycle":            "launch",
	"Upstream":             "launch",
	"MCPServers":           "launch",
	"AcceptStartupDialogs": "launch",
	"MouseOn":              "launch",
	"SessionSetup":         "launch",
	"SessionSetupScript":   "launch",
	// PROVISION (box) half.
	"Env":                 "provision",
	"FingerprintExtra":    "provision",
	"PreStart":            "provision",
	"OverlayDir":          "provision",
	"CopyFiles":           "provision",
	"ProviderName":        "provision",
	"ProviderOverlayName": "provision",
	"InstallAgentHooks":   "provision",
}

var excludedFromCore = map[string]string{
	"WorkDir":                "run location, not config identity",
	"StartupEnvelope":        "T3 startup metadata, explicitly excluded from Core",
	"ReadyPromptPrefix":      "startup readiness hint",
	"ReadyDelayMs":           "startup readiness hint",
	"ProcessNames":           "liveness-check hint",
	"EmitsPermissionWarning": "startup dialog hint",
	"Nudge":                  "post-ready typed text",
	"SessionLive":            "the LIVE axis — re-applied without restart (LiveFingerprint, not Core)",
	"PackOverlayDirs":        "additive pack file staging, not hashed",
	"PromptSuffix":           "volatile beacon text, deliberately excluded",
	"PromptFlag":             "command-reconstruction hint, not hashed",
}

// TestFingerprintPartitionAccountsForEveryConfigField is the FP-1/GAP-6
// structural guard. The behavioral partition test only covers a hand-listed set
// of fields, so a NEW optional core field (the hashOptionalString pattern used
// for Upstream) added to hashCoreFields but forgotten in the partition would move
// CoreFingerprint while moving NEITHER half — a silently dropped relaunch/restart
// trigger — and both the golden and the partition tests would stay green. This
// reflection check fails the moment a Config field is neither classified into a
// half nor explicitly excluded, forcing the author to make the call.
func TestFingerprintPartitionAccountsForEveryConfigField(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	valid := make(map[string]bool, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		valid[typ.Field(i).Name] = true
	}

	// (1) Every Config field is classified exactly once.
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		_, classified := coreFieldHalf[name]
		_, excluded := excludedFromCore[name]
		switch {
		case classified && excluded:
			t.Errorf("Config.%s is in BOTH coreFieldHalf and excludedFromCore — pick one", name)
		case !classified && !excluded:
			t.Errorf("Config.%s is unclassified. Add it to coreFieldHalf (its Provision/Launch half + a behavioral case in partitionHalfCases) or excludedFromCore (with a reason). A new core field left unpartitioned moves CoreFingerprint while moving NEITHER half — a silently dropped restart trigger (FP-1/GAP-6).", name)
		}
	}

	// (2) The maps don't reference removed fields.
	for name := range coreFieldHalf {
		if !valid[name] {
			t.Errorf("coreFieldHalf references %q, which is not a Config field", name)
		}
	}
	for name := range excludedFromCore {
		if !valid[name] {
			t.Errorf("excludedFromCore references %q, which is not a Config field", name)
		}
	}

	// (3) Every behavioral case agrees with coreFieldHalf (no drift between the
	// proven half and the declared half).
	for _, tc := range partitionHalfCases {
		half, ok := coreFieldHalf[tc.field]
		if !ok {
			t.Errorf("partitionHalfCases tests %q but coreFieldHalf omits it", tc.field)
			continue
		}
		if half != tc.half {
			t.Errorf("partitionHalfCases puts %q in the %s half but coreFieldHalf says %s", tc.field, tc.half, half)
		}
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
