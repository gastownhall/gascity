package examples_test

import (
	"os"
	"path/filepath"
	"regexp"
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
		"gc.routed_to={rig}/polecat",
		"gc.routed_to={rig}/refinery",
		"gc.routed_to={rig}/worker",
		"gc.routed_to={rig}/{role}",
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

// TestMigratedExampleAgentsLaunchAgentScript verifies the lifecycle and
// hyperscale example agents launch the built-in YAML agent-script runner
// against a migrated agent-script vendored in the example's own pack. The
// examples are `gc init --from`-copyable, so each script lives under the
// pack's assets/scripts/ and start_command references it via a
// {{.ConfigDir}}-relative path.
//
// Replaces the bash-era TestExamplePoolScriptsUseCanonicalGCTemplateRoutes:
// canonical pool routing moved out of per-example shell scripts and into the
// runner's hook probe, so the example only has to wire the agent up.
func TestMigratedExampleAgentsLaunchAgentScript(t *testing.T) {
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
			want := "gc agent-script --script {{.ConfigDir}}/assets/scripts/" + tt.script
			if got != want {
				t.Errorf("start_command = %q, want %q", got, want)
			}
			if got := loadAgentConfig(t, agentPath).Lifecycle; got != "one_shot" {
				t.Errorf("lifecycle = %q, want one_shot", got)
			}

			// {{.ConfigDir}} resolves to the pack directory — two levels up
			// from the agent directory. The script must be vendored there for
			// the example to be self-contained after `gc init --from`.
			packDir := filepath.Join(filepath.Dir(agentPath), "..", "..")
			scriptPath := filepath.Join(packDir, "assets", "scripts", tt.script)
			if _, err := os.Stat(scriptPath); err != nil {
				t.Errorf("agent-script not vendored in the example pack: %v", err)
			}
			script := loadAgentScript(t, scriptPath)
			if script.SchemaVersion != "v1" {
				t.Errorf("agent-script schema_version = %q, want v1", script.SchemaVersion)
			}
		})
	}

	// The migration removes the known bash mocks outright without asserting
	// that future unrelated examples can never ship a mock-named script.
	for _, relPath := range []string{
		"hyperscale/packs/hyperscale/assets/scripts/mock-worker.sh",
		"lifecycle/packs/lifecycle/assets/scripts/mock-polecat.sh",
		"lifecycle/packs/lifecycle/assets/scripts/mock-refinery.sh",
	} {
		if _, err := os.Stat(filepath.Join(root, relPath)); err == nil {
			t.Errorf("bash mock survived the migration: %s", relPath)
		} else if !os.IsNotExist(err) {
			t.Fatalf("checking removed bash mock %s: %v", relPath, err)
		}
	}
}

func TestLifecycleRefineryIsSerialized(t *testing.T) {
	cfg := loadAgentConfig(t, filepath.Join(examplesRoot(t),
		"lifecycle/packs/lifecycle/agents/refinery/agent.toml"))
	if cfg.MaxActiveSessions == nil {
		t.Fatal("lifecycle refinery max_active_sessions is unset")
	}
	if *cfg.MaxActiveSessions != 1 {
		t.Fatalf("lifecycle refinery max_active_sessions = %d, want 1", *cfg.MaxActiveSessions)
	}
}

func TestShippedAgentScriptsKeepShellDataInRunnerEnv(t *testing.T) {
	root := examplesRoot(t)
	shellPlaceholder := regexp.MustCompile(`\{(?:bead\.[A-Za-z0-9_.-]+|rig|alias)\}`)
	for _, relPath := range []string{
		"hyperscale/packs/hyperscale/assets/scripts/hyperscale-worker.yaml",
		"lifecycle/packs/lifecycle/assets/scripts/lifecycle-polecat-claim-handoff.yaml",
		"lifecycle/packs/lifecycle/assets/scripts/lifecycle-refinery-merge.yaml",
	} {
		t.Run(relPath, func(t *testing.T) {
			script := loadAgentScript(t, filepath.Join(root, relPath))
			for _, turn := range script.Turns {
				for _, shell := range shellActions(turn) {
					placeholder := shellPlaceholder.FindString(shell)
					if placeholder != "" {
						t.Fatalf("shell action interpolates placeholder %q directly: %q", placeholder, shell)
					}
				}
			}
		})
	}
}

// TestLifecyclePolecatHandsOffToLifecycleRefinery verifies the migrated
// lifecycle polecat script hands a claimed bead to the lifecycle refinery.
// The routing fact the bash mock expressed in a derive_refinery_target shell
// function now lives in the script's has_work turn: a shell action records
// branch metadata, then a bd_update reassigns the bead and a mail_send notifies
// the same address. {rig} is resolved from $GC_RIG by gc agent-script at run time.
//
// Replaces the bash-era TestLifecyclePolecatDerivesRefineryTargetFromCanonicalTemplate.
func TestLifecyclePolecatHandsOffToLifecycleRefinery(t *testing.T) {
	root := examplesRoot(t)
	polecat := loadAgentScript(t, filepath.Join(root,
		"lifecycle/packs/lifecycle/assets/scripts/lifecycle-polecat-claim-handoff.yaml"))

	const refineryAddr = "{rig}/lifecycle.refinery"

	work := hookTurn(t, polecat)

	update := dictAction(t, work, "bd_update")
	if got := stringField(t, update, "assignee"); got != refineryAddr {
		t.Errorf("polecat hands off to assignee %q, want %q", got, refineryAddr)
	}
	shells := strings.Join(shellActions(work), "\n")
	for _, want := range []string{
		"--set-metadata branch=\"$BRANCH\"",
		"--set-metadata branch_head=\"$HEAD_SHA\"",
	} {
		if !strings.Contains(shells, want) {
			t.Fatalf("polecat shell does not durably record %q before handoff: %q", want, shells)
		}
	}
	if metadata, ok := update["metadata"].(map[string]any); ok && metadata["branch"] != nil {
		t.Fatalf("polecat bd_update splits branch metadata ownership with shell action: %#v", metadata)
	}

	mail := dictAction(t, work, "mail_send")
	if got := stringField(t, mail, "to"); got != refineryAddr {
		t.Errorf("polecat mails %q, want the refinery %q", got, refineryAddr)
	}
}

// TestLifecycleRefineryConsumesPolecatHandoff verifies the two halves of the
// lifecycle pipeline agree on the handoff contract: the polecat reassigns the
// bead to a {rig}/lifecycle.refinery address and records branch metadata in
// its shell handoff; the refinery script merges exactly that metadata.branch.
// Drift in either the address or the metadata key strands the bead between the
// two agents.
//
// Replaces the bash-era TestLifecycleRefineryConsumesPolecatHandoffAlias.
func TestLifecycleRefineryConsumesPolecatHandoff(t *testing.T) {
	scriptDir := filepath.Join(examplesRoot(t), "lifecycle/packs/lifecycle/assets/scripts")
	polecat := loadAgentScript(t, filepath.Join(scriptDir, "lifecycle-polecat-claim-handoff.yaml"))
	refinery := loadAgentScript(t, filepath.Join(scriptDir, "lifecycle-refinery-merge.yaml"))

	// The polecat's handoff: record the branch, then reassign to the lifecycle
	// refinery role.
	polecatWork := hookTurn(t, polecat)
	update := dictAction(t, polecatWork, "bd_update")
	handoff := stringField(t, update, "assignee")
	if !strings.HasSuffix(handoff, "/lifecycle.refinery") {
		t.Errorf("polecat handoff target %q does not address the lifecycle refinery", handoff)
	}
	polecatShells := strings.Join(shellActions(polecatWork), "\n")
	for _, want := range []string{
		"--set-metadata branch=\"$BRANCH\"",
		"--set-metadata branch_head=\"$HEAD_SHA\"",
	} {
		if !strings.Contains(polecatShells, want) {
			t.Fatalf("polecat shell does not record %q for the refinery: %q", want, polecatShells)
		}
	}

	// The refinery's merge turn must consume branch metadata through the
	// runner-provided environment variable — the same key the polecat writes
	// above, without splicing an untrusted value into shell source.
	refineryWork := hookTurn(t, refinery)
	usesBranchEnv := false
	usesBranchHeadEnv := false
	for _, shell := range shellActions(refineryWork) {
		if strings.Contains(shell, "GC_SCRIPT_BEAD_METADATA_BRANCH") {
			usesBranchEnv = true
		}
		if strings.Contains(shell, "GC_SCRIPT_BEAD_METADATA_BRANCH_HEAD") {
			usesBranchHeadEnv = true
		}
	}
	if !usesBranchEnv {
		t.Error("refinery merge turn never references GC_SCRIPT_BEAD_METADATA_BRANCH — handoff contract broken")
	}
	if !usesBranchHeadEnv {
		t.Error("refinery merge turn never references GC_SCRIPT_BEAD_METADATA_BRANCH_HEAD — retry recovery contract broken")
	}
}

func TestLifecycleAgentScriptsKeepLoadBearingGitSetup(t *testing.T) {
	root := examplesRoot(t)
	tests := []struct {
		name    string
		script  string
		wantAll []string
	}{
		{
			name:   "polecat",
			script: "lifecycle/packs/lifecycle/assets/scripts/lifecycle-polecat-claim-handoff.yaml",
			wantAll: []string{
				"commit.gpgsign false",
				"GITHUB_TOKEN",
				"github.com",
				"$HOME/.netrc",
				"chmod 600",
				"user.email",
				"user.name",
			},
		},
		{
			name:   "refinery",
			script: "lifecycle/packs/lifecycle/assets/scripts/lifecycle-refinery-merge.yaml",
			wantAll: []string{
				"commit.gpgsign false",
				"GITHUB_TOKEN",
				"github.com",
				"$HOME/.netrc",
				"chmod 600",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := loadAgentScript(t, filepath.Join(root, tt.script))
			setupShell := strings.Join(stringActionsFromList(script.Setup, "shell"), "\n")
			for _, want := range tt.wantAll {
				if !strings.Contains(setupShell, want) {
					t.Errorf("setup shell actions = %q, want %q", setupShell, want)
				}
			}
		})
	}
}

func TestLifecycleRefineryMergeIsBaseSynchronizedBeforeClose(t *testing.T) {
	script := loadAgentScript(t, filepath.Join(examplesRoot(t),
		"lifecycle/packs/lifecycle/assets/scripts/lifecycle-refinery-merge.yaml"))
	work := hookTurn(t, script)

	mergeShellIndex := -1
	pushShellIndex := -1
	cleanupShellIndex := -1
	bdUpdateIndex := -1
	for i, entry := range work.Do {
		if _, ok := entry["bd_update"]; ok {
			bdUpdateIndex = i
			continue
		}
		shell, ok := entry["shell"].(string)
		if !ok {
			continue
		}
		if strings.Contains(shell, "git merge --no-edit") {
			mergeShellIndex = i
			for _, want := range []string{
				"git fetch origin main",
				"git checkout -B main origin/main",
				"git update-ref \"refs/heads/$BRANCH\" FETCH_HEAD",
				"git check-ref-format --branch",
			} {
				if !strings.Contains(shell, want) {
					t.Errorf("merge shell missing %q: %q", want, shell)
				}
			}
		}
		if strings.Contains(shell, "git push origin --delete") {
			cleanupShellIndex = i
			for _, want := range []string{
				"git branch -d \"$BRANCH\" 2>/dev/null || git branch -D \"$BRANCH\" || true",
				"git push origin --delete \"$BRANCH\" || true",
			} {
				if !strings.Contains(shell, want) {
					t.Errorf("cleanup shell missing best-effort guard %q: %q", want, shell)
				}
			}
		}
		if strings.Contains(shell, "git push origin HEAD:main") {
			pushShellIndex = i
		}
	}
	if mergeShellIndex == -1 {
		t.Fatal("refinery has_work turn has no merge shell action")
	}
	if pushShellIndex == -1 {
		t.Fatal("refinery has_work turn has no push shell action")
	}
	if cleanupShellIndex == -1 {
		t.Fatal("refinery has_work turn has no branch cleanup shell action")
	}
	if bdUpdateIndex == -1 {
		t.Fatal("refinery has_work turn has no bd_update action")
	}
	if bdUpdateIndex < pushShellIndex {
		t.Fatalf("bd_update index %d runs before push shell index %d; bead must close only after main is synchronized", bdUpdateIndex, pushShellIndex)
	}
	if cleanupShellIndex < bdUpdateIndex {
		t.Fatalf("cleanup shell action index %d runs before bd_update index %d; cleanup must happen after durable close", cleanupShellIndex, bdUpdateIndex)
	}
}

func TestLifecycleRefineryMergeUsesFreshFetchedHandoffBranch(t *testing.T) {
	script := loadAgentScript(t, filepath.Join(examplesRoot(t),
		"lifecycle/packs/lifecycle/assets/scripts/lifecycle-refinery-merge.yaml"))
	work := hookTurn(t, script)
	mergeShell := shellActionContaining(t, work, "git merge --no-edit")
	allShells := strings.Join(shellActions(work), "\n")

	for _, want := range []string{
		"git fetch origin \"$BRANCH\"",
		"git update-ref \"refs/heads/$BRANCH\" FETCH_HEAD",
		"git merge --no-edit \"$BRANCH\"",
		"git push origin HEAD:main",
	} {
		if !strings.Contains(mergeShell, want) && !strings.Contains(allShells, want) {
			t.Fatalf("refinery script missing %q", want)
		}
	}
	if strings.Contains(mergeShell, "git branch \"$BRANCH\" FETCH_HEAD") {
		t.Fatalf("refinery merge still creates stale-prone local branch fallback: %q", mergeShell)
	}
}

func TestLifecycleRefineryNoOriginPathRequiresMain(t *testing.T) {
	script := loadAgentScript(t, filepath.Join(examplesRoot(t),
		"lifecycle/packs/lifecycle/assets/scripts/lifecycle-refinery-merge.yaml"))
	mergeShell := shellActionContaining(t, hookTurn(t, script), "main branch is required")
	if !strings.Contains(mergeShell, "git checkout main ||") {
		t.Fatalf("refinery no-origin path does not fail loudly when main is unavailable: %q", mergeShell)
	}
}

func TestLifecycleRefineryMergeFailureNotificationIsBestEffort(t *testing.T) {
	script := loadAgentScript(t, filepath.Join(examplesRoot(t),
		"lifecycle/packs/lifecycle/assets/scripts/lifecycle-refinery-merge.yaml"))
	mergeShell := shellActionContaining(t, hookTurn(t, script), "MERGE FAILED")
	if !strings.Contains(mergeShell, "gc mail send --to \"$GC_SCRIPT_RIG/lifecycle.polecat\"") ||
		!strings.Contains(mergeShell, "|| true") ||
		!strings.Contains(mergeShell, "exit 1") {
		t.Fatalf("merge failure path should send best-effort mail before explicit exit: %q", mergeShell)
	}
}

func TestLifecycleRefineryCleanupIsIdempotent(t *testing.T) {
	script := loadAgentScript(t, filepath.Join(examplesRoot(t),
		"lifecycle/packs/lifecycle/assets/scripts/lifecycle-refinery-merge.yaml"))
	cleanupShell := shellActionContaining(t, hookTurn(t, script), "git push origin --delete")
	for _, want := range []string{
		"git rev-parse --verify --quiet \"refs/heads/$BRANCH\"",
		"git ls-remote --exit-code --heads origin \"$BRANCH\"",
	} {
		if !strings.Contains(cleanupShell, want) {
			t.Fatalf("cleanup shell missing %q: %q", want, cleanupShell)
		}
	}
}

func TestLifecycleRefineryCleanupRejectsDefaultBranches(t *testing.T) {
	script := loadAgentScript(t, filepath.Join(examplesRoot(t),
		"lifecycle/packs/lifecycle/assets/scripts/lifecycle-refinery-merge.yaml"))
	cleanupShell := shellActionContaining(t, hookTurn(t, script), "git push origin --delete")
	for _, want := range []string{"main", "master"} {
		if !strings.Contains(cleanupShell, want) {
			t.Fatalf("cleanup shell missing default branch guard %q: %q", want, cleanupShell)
		}
	}
}

func TestLifecyclePolecatValidatesBeadIDBeforePathsAndRefs(t *testing.T) {
	script := loadAgentScript(t, filepath.Join(examplesRoot(t),
		"lifecycle/packs/lifecycle/assets/scripts/lifecycle-polecat-claim-handoff.yaml"))
	workShells := strings.Join(shellActions(hookTurn(t, script)), "\n")
	for _, want := range []string{
		"BEAD_ID=\"${GC_SCRIPT_BEAD_ID:-}\"",
		"*[!A-Za-z0-9-]*)",
		"WT=\"/tmp/gc-scripted-wt/$BEAD_ID\"",
		"BRANCH=\"polecat/$BEAD_ID\"",
	} {
		if !strings.Contains(workShells, want) {
			t.Fatalf("polecat shell missing %q: %q", want, workShells)
		}
	}
}

func TestLifecycleRefineryDoesNotReferenceMissingRejectScript(t *testing.T) {
	path := filepath.Join(examplesRoot(t),
		"lifecycle/packs/lifecycle/assets/scripts/lifecycle-refinery-merge.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading refinery script: %v", err)
	}
	if strings.Contains(string(data), "refinery-reject.yaml") {
		t.Fatal("refinery script references missing refinery-reject.yaml")
	}
}

func examplesRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Dir(filename)
}

type agentConfig struct {
	StartCommand      string `toml:"start_command"`
	Lifecycle         string `toml:"lifecycle"`
	MaxActiveSessions *int   `toml:"max_active_sessions"`
}

// agentStartCommand decodes the start_command field from an example agent.toml.
func agentStartCommand(t *testing.T, path string) string {
	t.Helper()
	return loadAgentConfig(t, path).StartCommand
}

func loadAgentConfig(t *testing.T, path string) agentConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var cfg agentConfig
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return cfg
}

// agentScript is the subset of the gc agent-script YAML schema these tests
// assert against.
type agentScript struct {
	SchemaVersion string           `yaml:"schema_version"`
	Setup         []map[string]any `yaml:"setup"`
	Turns         []scriptTurn     `yaml:"turns"`
}

// scriptTurn is one entry in a script's turn list: a `when:` predicate and the
// ordered `do:` actions that run when it matches. Each do entry is a
// single-key mapping of action name to argument.
type scriptTurn struct {
	When map[string]any   `yaml:"when"`
	Do   []map[string]any `yaml:"do"`
}

// loadAgentScript reads and parses a YAML agent script.
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

// hookTurn returns the turn whose `when:` matches has_work, failing the test if
// no such turn exists.
func hookTurn(t *testing.T, script agentScript) scriptTurn {
	t.Helper()
	for _, turn := range script.Turns {
		if turn.When["hook"] == "has_work" {
			return turn
		}
	}
	t.Fatal("agent script has no has_work hook turn")
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

func shellActions(turn scriptTurn) []string {
	return stringActionsFromList(turn.Do, "shell")
}

func shellActionContaining(t *testing.T, turn scriptTurn, needle string) string {
	t.Helper()
	for _, shell := range shellActions(turn) {
		if strings.Contains(shell, needle) {
			return shell
		}
	}
	t.Fatalf("turn has no shell action containing %q", needle)
	return ""
}

func stringActionsFromList(actions []map[string]any, action string) []string {
	var out []string
	for _, entry := range actions {
		arg, ok := entry[action]
		if !ok {
			continue
		}
		if s, ok := arg.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
