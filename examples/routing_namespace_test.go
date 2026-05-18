package examples_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

func TestShippedExamplesDoNotHardcodeShortRoutedToPools(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	root := filepath.Dir(filename)
	badRoutes := []string{
		"gc.routed_to=dog",
		"gc.routed_to=worker",
		"gc.routed_to=<rig>/polecat",
		"gc.routed_to=<rig>/refinery",
		"gc.routed_to={{ .RigName }}/refinery",
		"pool:dog",
	}

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(data)
		for _, bad := range badRoutes {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains short-form routed_to target %q", path, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestMigratedExampleAgentsLaunchScriptedTestAgent verifies the lifecycle and
// hyperscale example agents launch the YAML-driven scripted test-agent
// (gascity-tools-fsm-test-agent) against a migrated agent-script vendored in
// the example's own pack. The examples are `gc init --from`-copyable, so each
// script lives under the pack's assets/scripts/ and start_command references
// it via a {{.ConfigDir}}-relative path — an absolute gascity_tools path would
// not survive the copy.
//
// Replaces the bash-era TestExamplePoolScriptsUseCanonicalGCTemplateRoutes:
// canonical pool routing moved out of per-example shell scripts and into the
// test-agent's hook probe, so the example only has to wire the agent up.
func TestMigratedExampleAgentsLaunchScriptedTestAgent(t *testing.T) {
	root := examplesRoot(t)

	tests := []struct {
		name      string
		agentTOML string // relative to the examples root
		script    string // expected agent-script filename
	}{
		{
			name:      "lifecycle polecat",
			agentTOML: "lifecycle/packs/lifecycle/agents/polecat/agent.toml",
			script:    "lifecycle-polecat-claim-handoff.yaml",
		},
		{
			name:      "lifecycle refinery",
			agentTOML: "lifecycle/packs/lifecycle/agents/refinery/agent.toml",
			script:    "lifecycle-refinery-merge.yaml",
		},
		{
			name:      "hyperscale worker",
			agentTOML: "hyperscale/packs/hyperscale/agents/worker/agent.toml",
			script:    "hyperscale-worker.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agentPath := filepath.Join(root, tt.agentTOML)
			got := agentStartCommand(t, agentPath)
			want := "gascity-tools-fsm-test-agent --script {{.ConfigDir}}/assets/scripts/" + tt.script
			if got != want {
				t.Errorf("start_command = %q, want %q", got, want)
			}

			// {{.ConfigDir}} resolves to the pack directory — two levels up
			// from the agent directory. The script must be vendored there for
			// the example to be self-contained after `gc init --from`.
			packDir := filepath.Join(filepath.Dir(agentPath), "..", "..")
			scriptPath := filepath.Join(packDir, "assets", "scripts", tt.script)
			if _, err := os.Stat(scriptPath); err != nil {
				t.Errorf("agent-script not vendored in the example pack: %v", err)
			}
		})
	}

	// The migration removes the bash mocks outright — a surviving mock-*.sh
	// would be dead weight no agent.toml references.
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if !entry.IsDir() && strings.HasPrefix(name, "mock-") && strings.HasSuffix(name, ".sh") {
			t.Errorf("bash mock survived the migration: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestLifecyclePolecatHandsOffToLifecycleRefinery verifies the migrated
// lifecycle polecat script hands a claimed bead to the lifecycle refinery.
// The routing fact the bash mock expressed in a derive_refinery_target shell
// function now lives in the script's has_work turn: a bd_update that reassigns
// the bead and a mail_send that notifies the same address. {rig} is resolved
// from $GC_RIG by the test-agent at run time.
//
// Replaces the bash-era TestLifecyclePolecatDerivesRefineryTargetFromCanonicalTemplate.
func TestLifecyclePolecatHandsOffToLifecycleRefinery(t *testing.T) {
	root := examplesRoot(t)
	polecat := loadAgentScript(t, filepath.Join(root,
		"lifecycle/packs/lifecycle/assets/scripts/lifecycle-polecat-claim-handoff.yaml"))

	const refineryAddr = "{rig}/lifecycle.refinery"

	work := hookTurn(t, polecat, "has_work")

	update := dictAction(t, work, "bd_update")
	if got := stringField(t, update, "assignee"); got != refineryAddr {
		t.Errorf("polecat hands off to assignee %q, want %q", got, refineryAddr)
	}
	metadata, ok := update["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("polecat bd_update has no metadata mapping")
	}
	if got, _ := metadata["branch"].(string); got != "polecat/{bead.id}" {
		t.Errorf("polecat records metadata.branch = %q, want %q", got, "polecat/{bead.id}")
	}

	mail := dictAction(t, work, "mail_send")
	if got := stringField(t, mail, "to"); got != refineryAddr {
		t.Errorf("polecat mails %q, want the refinery %q", got, refineryAddr)
	}
}

// TestLifecycleRefineryConsumesPolecatHandoff verifies the two halves of the
// lifecycle pipeline agree on the handoff contract: the polecat reassigns the
// bead to a {rig}/lifecycle.refinery address and records the feature branch
// under metadata.branch; the refinery script merges exactly that
// metadata.branch. Drift in either the address or the metadata key strands
// the bead between the two agents.
//
// Replaces the bash-era TestLifecycleRefineryConsumesPolecatHandoffAlias.
func TestLifecycleRefineryConsumesPolecatHandoff(t *testing.T) {
	scriptDir := filepath.Join(examplesRoot(t), "lifecycle/packs/lifecycle/assets/scripts")
	polecat := loadAgentScript(t, filepath.Join(scriptDir, "lifecycle-polecat-claim-handoff.yaml"))
	refinery := loadAgentScript(t, filepath.Join(scriptDir, "lifecycle-refinery-merge.yaml"))

	// The polecat's handoff: reassign to the lifecycle refinery role, with the
	// feature branch recorded under metadata.branch.
	update := dictAction(t, hookTurn(t, polecat, "has_work"), "bd_update")
	handoff := stringField(t, update, "assignee")
	if !strings.HasSuffix(handoff, "/lifecycle.refinery") {
		t.Errorf("polecat handoff target %q does not address the lifecycle refinery", handoff)
	}
	if metadata, ok := update["metadata"].(map[string]any); !ok || metadata["branch"] == nil {
		t.Fatal("polecat bd_update does not record metadata.branch for the refinery")
	}

	// The refinery's merge turn must consume {bead.metadata.branch} — the same
	// key the polecat writes above.
	refineryWork := hookTurn(t, refinery, "has_work")
	out, err := yaml.Marshal(refineryWork)
	if err != nil {
		t.Fatalf("re-marshaling refinery turn: %v", err)
	}
	if !strings.Contains(string(out), "{bead.metadata.branch}") {
		t.Error("refinery merge turn never references {bead.metadata.branch} — handoff contract broken")
	}
}

func examplesRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Dir(filename)
}

// agentStartCommand decodes the start_command field from an example agent.toml.
func agentStartCommand(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var cfg struct {
		StartCommand string `toml:"start_command"`
	}
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return cfg.StartCommand
}

// agentScript is the subset of the gascity-tools-fsm-test-agent YAML schema
// these tests assert against. The full schema lives in
// gascity_tools/fsm/test_agent/README.md.
type agentScript struct {
	Turns []scriptTurn `yaml:"turns"`
}

// scriptTurn is one entry in a script's turn list: a `when:` predicate and the
// ordered `do:` actions that run when it matches. Each do entry is a
// single-key mapping of action name to argument.
type scriptTurn struct {
	When map[string]any   `yaml:"when"`
	Do   []map[string]any `yaml:"do"`
}

// loadAgentScript reads and parses a test-agent YAML script.
func loadAgentScript(t *testing.T, path string) agentScript {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading agent script %s: %v", path, err)
	}
	var script agentScript
	if err := yaml.Unmarshal(data, &script); err != nil {
		t.Fatalf("parsing agent script %s: %v", path, err)
	}
	return script
}

// hookTurn returns the turn whose `when:` matches the given hook state
// (has_work | empty), failing the test if no such turn exists.
func hookTurn(t *testing.T, script agentScript, hook string) scriptTurn {
	t.Helper()
	for _, turn := range script.Turns {
		if turn.When["hook"] == hook {
			return turn
		}
	}
	t.Fatalf("agent script has no turn for hook %q", hook)
	return scriptTurn{}
}

// dictAction returns the argument of the named action (e.g. bd_update,
// mail_send) as a string-keyed mapping, failing if the action is absent from
// the turn or its argument is not a mapping.
func dictAction(t *testing.T, turn scriptTurn, action string) map[string]any {
	t.Helper()
	for _, entry := range turn.Do {
		arg, ok := entry[action]
		if !ok {
			continue
		}
		m, ok := arg.(map[string]any)
		if !ok {
			t.Fatalf("action %q is %T, want a mapping", action, arg)
		}
		return m
	}
	t.Fatalf("turn has no %q action", action)
	return nil
}

// stringField returns m[key] as a string, failing if it is absent or not a string.
func stringField(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("mapping has no %q field", key)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("field %q is %T, want string", key, v)
	}
	return s
}
