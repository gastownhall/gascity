package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// This is the de-conflation safety net (PR0). The WorkerSpec/Runtime/Place/
// Transport refactor re-composes how a session's runtime.Config is built; if
// that changes the fingerprint of an existing session, every live session
// restarts on upgrade. These tests FREEZE the fingerprint functions: the golden
// hashes pin the hashing surface, and the version/allow-list pins catch the two
// other ways the fingerprint can silently move. Any drift fails CI loudly; an
// intentional change is a deliberate golden regen (UPDATE_GOLDEN=1) + review.

// goldenFixtures exercises every field hashCoreFields/hashLiveFields touches.
func goldenFixtures() map[string]Config {
	tru := true
	return map[string]Config{
		"empty": {},
		"comprehensive": {
			Command:   "agent --serve --model opus",
			Lifecycle: LifecycleOneShot,
			Env: map[string]string{
				// every allow-listed key (behavioral identity) ...
				"GC_CITY": "c1", "GC_CITY_PATH": "/city",
				"GC_RIG": "r1", "GC_RIG_ROOT": "/rig", "BEADS_DIR": "/beads",
				"GC_TEMPLATE": "claude", "GC_SKILLS_DIR": "/skills", "GC_BLESSED_BIN_DIR": "/bin",
				"GC_PUBLICATION_PROVIDER":           "cf",
				"GC_PUBLICATION_PUBLIC_BASE_DOMAIN": "ex.com",
				"GC_PUBLICATION_PUBLIC_BASE_URL":    "https://ex.com",
				"GC_PUBLICATION_TENANT_BASE_DOMAIN": "t.ex.com",
				"GC_PUBLICATION_TENANT_BASE_URL":    "https://t.ex.com",
				"GC_PUBLICATION_TENANT_SLUG":        "acme",
				// ... plus excluded keys that must NOT affect the hash.
				"GC_SESSION_ID": "ignored", "GC_AGENT": "ignored", "NOT_GC": "ignored",
			},
			MCPServers: []MCPServerConfig{{
				Name: "mail", Transport: MCPTransport("stdio"), Command: "mcp-mail",
				Args: []string{"--port", "0"}, Env: map[string]string{"K": "V"},
				URL: "https://mcp", Headers: map[string]string{"H": "1"},
			}},
			FingerprintExtra:     map[string]string{"pool": "p1"},
			PreStart:             []string{"mkdir -p x", "echo pre"},
			SessionSetup:         []string{"setup-a", "setup-b"},
			SessionSetupScript:   "/setup.sh",
			SessionLive:          []string{"tmux-theme", "keybinds"},
			OverlayDir:           "/overlay",
			ProviderName:         "claude",
			ProviderOverlayName:  "claude-code",
			InstallAgentHooks:    []string{"hookA"},
			AcceptStartupDialogs: &tru,
			MouseOn:              true,
			CopyFiles: []CopyEntry{
				{Src: "/src/a", RelDst: "a"},
				{RelDst: "hooks/h", Probed: true, ContentHash: "abc123"},
				{RelDst: "hooks/empty", Probed: true, ContentHash: ""}, // HASH_UNAVAILABLE sentinel
			},
		},
		// Boundary: nil vs empty Env must hash identically (asserted below).
		"env-nil":   {Env: nil},
		"env-empty": {Env: map[string]string{}},
	}
}

type goldenHashes struct {
	Config    string `json:"config"`
	Core      string `json:"core"`
	Live      string `json:"live"`
	Provision string `json:"provision"`
	Launch    string `json:"launch"`
}

const goldenPath = "testdata/fingerprint_golden.json"

func TestFingerprintGolden(t *testing.T) {
	got := map[string]goldenHashes{}
	for name, cfg := range goldenFixtures() {
		got[name] = goldenHashes{
			Config:    ConfigFingerprint(cfg),
			Core:      CoreFingerprint(cfg),
			Live:      LiveFingerprint(cfg),
			Provision: ProvisionFingerprint(cfg),
			Launch:    LaunchFingerprint(cfg),
		}
	}

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(goldenPath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d fixtures)", goldenPath, len(got))
		return
	}

	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (create with UPDATE_GOLDEN=1): %v", err)
	}
	var want map[string]goldenHashes
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	for name, g := range got {
		if g != want[name] {
			t.Errorf("FINGERPRINT DRIFT for %q:\n got  %+v\n want %+v\nA live session with this config would restart on upgrade. If intentional, regen with UPDATE_GOLDEN=1 and review.", name, g, want[name])
		}
	}
	// nil vs empty Env is the same agent identity → identical hash.
	if got["env-nil"] != got["env-empty"] {
		t.Errorf("nil Env and empty Env must hash identically:\n nil   %+v\n empty %+v", got["env-nil"], got["env-empty"])
	}
}

func TestFingerprintVersionPin(t *testing.T) {
	// The version namespaces stored hashes; an UNINTENTIONAL bump during the
	// de-conflation rebaselines every session (mass restart). An intentional
	// bump is a deliberate edit to this assertion + a golden regen.
	if FingerprintVersion != "v5" {
		t.Errorf("FingerprintVersion = %q, want v5", FingerprintVersion)
	}
}

func TestEnvFingerprintAllowPin(t *testing.T) {
	// The allow-list defines which env values are behavioral identity. Adding a
	// key changes fingerprints (e.g. Upstream injection during the refactor must
	// NOT leak ANTHROPIC_* in here); removing one drops drift detection.
	want := []string{
		"BEADS_DIR", "GC_BLESSED_BIN_DIR", "GC_CITY", "GC_CITY_PATH",
		"GC_PUBLICATION_PROVIDER", "GC_PUBLICATION_PUBLIC_BASE_DOMAIN",
		"GC_PUBLICATION_PUBLIC_BASE_URL", "GC_PUBLICATION_TENANT_BASE_DOMAIN",
		"GC_PUBLICATION_TENANT_BASE_URL", "GC_PUBLICATION_TENANT_SLUG",
		"GC_RIG", "GC_RIG_ROOT", "GC_SKILLS_DIR", "GC_TEMPLATE",
	}
	var got []string
	for k := range envFingerprintAllow {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("envFingerprintAllow has %d keys, want %d:\n got  %v\n want %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("envFingerprintAllow drift at %d: got %q want %q", i, got[i], want[i])
		}
	}
}
