package main

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/spf13/cobra"
)

func TestPromptGCInvocationsReadsOnlyCode(t *testing.T) {
	prompt := strings.Join([]string{
		"Run gc agent list in prose; this is not code.",
		"Claim with `gc hook --claim --json` before work.",
		"Never `gcx foo`, and `gc.run-operator` is a target, not a command.",
		"```bash",
		"$ gc mail inbox",
		"bd ready | xargs echo && gc session list --json # trailing comment gc nope",
		"ID=$(gc bd create \"title\" --json)",
		"echo 'gc quoted words'",
		"```",
		"~~~",
		"gc rig status",
		"~~~",
	}, "\n")
	got := promptGCInvocations(prompt)
	want := [][]string{
		{"hook", "--claim", "--json"},
		{"mail", "inbox"},
		{"session", "list", "--json"},
		{"bd", "create", "\x00quoted", "--json"},
		{"rig", "status"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("promptGCInvocations =\n%q\nwant\n%q", got, want)
	}
}

func TestPromptGCInvocationsSkipsProhibitedCommands(t *testing.T) {
	prompt := strings.Join([]string{
		// The shipped mayor prompt's sentence, wrapped across two lines.
		"Do not invent `gc mail list`, `gc city status`, etc. from them. For bead work",
		"use `gc bd ready`, and never run",
		"`gc agent claim` by hand.",
		"Don't use `gc old one`; use `gc mail inbox`.",
		"You must not call `gc old two`. Avoid `gc old three`!",
		"Use `gc session list` instead of `gc old four`.",
		"Prefer `gc rig list` rather than `gc old five`?",
		"",
		"Paragraphs reset: `gc status`.",
		"```bash",
		"# Do not run the line below.",
		"gc agent claim",
		"```",
	}, "\n")
	got := promptGCInvocations(prompt)
	want := [][]string{
		{"bd", "ready"},
		{"mail", "inbox"},
		{"session", "list"},
		{"rig", "list"},
		{"status"},
		{"agent", "claim"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("promptGCInvocations =\n%q\nwant\n%q", got, want)
	}
}

// TestPromptGCInvocationsProhibitionScope pins how far a prohibition reaches:
// to the end of its sentence (including one closed by emphasis, ".**"), and
// never past a list item, heading, or table row. "don't" and "never" after a
// subject ("you don't know") describe rather than forbid.
func TestPromptGCInvocationsProhibitionScope(t *testing.T) {
	prompt := strings.Join([]string{
		"**Never look with `gc old one` or",
		"`gc old two`.** Those `gc bd list` and `gc bd ready` hide wisps.",
		"If you don't know where state lives, the answer is a `gc status`",
		"command, and you never have to guess with `gc agent list`.",
		"This is the one command — do not",
		"substitute `gc old three`:",
		"Never run these by hand",
		"- `gc mail inbox`",
		"Never edit below this heading",
		"## Commands",
		"Run `gc rig list` first",
		"| `gc session list` | lists sessions |",
	}, "\n")
	got := promptGCInvocations(prompt)
	want := [][]string{
		{"bd", "list"},
		{"bd", "ready"},
		{"status"},
		{"agent", "list"},
		{"mail", "inbox"},
		{"rig", "list"},
		{"session", "list"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("promptGCInvocations =\n%q\nwant\n%q", got, want)
	}
}

// TestPromptGCStockPromptsResolve guards the prompts gc ships: the default
// init prompts and the bundled core pack must produce no prompt-gc-commands
// finding against this gc's own command tree.
func TestPromptGCStockPromptsResolve(t *testing.T) {
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	resolver := promptGCResolver{roots: []*cobra.Command{root}}
	scanned := 0
	for name, fsys := range map[string]fs.FS{"cmd/gc": defaultPrompts, "packs/core": core.PackFS} {
		err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
				return err
			}
			data, err := fs.ReadFile(fsys, path)
			if err != nil {
				return err
			}
			scanned++
			for _, inv := range promptGCInvocations(string(data)) {
				for _, f := range resolver.resolve(inv) {
					t.Errorf("%s/%s: %s | %s | %s", name, path, f.Kind, f.Command, f.Reason)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", name, err)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no stock prompt files")
	}
}

// promptGCTestTree is a small command tree with the shapes the resolver
// distinguishes: a persistent root flag, a group that takes no arguments, a
// group whose usage declares an argument, a deprecated command, a bool flag,
// a value flag, and a pass-through command.
func promptGCTestTree() *cobra.Command {
	root := &cobra.Command{Use: "gc"}
	root.PersistentFlags().String("city", "", "")
	agent := &cobra.Command{Use: "agent", Args: cobra.ArbitraryArgs, RunE: func(*cobra.Command, []string) error { return nil }}
	list := &cobra.Command{Use: "list", RunE: func(*cobra.Command, []string) error { return nil }}
	list.Flags().Bool("json", false, "")
	agent.AddCommand(list)
	hook := &cobra.Command{Use: "hook [agent]", Args: cobra.MaximumNArgs(1), RunE: func(*cobra.Command, []string) error { return nil }}
	hook.Flags().Bool("claim", false, "")
	hook.Flags().String("format", "", "")
	hook.AddCommand(&cobra.Command{Use: "run", DisableFlagParsing: true, RunE: func(*cobra.Command, []string) error { return nil }})
	old := &cobra.Command{Use: "old", Deprecated: "use gc agent list", RunE: func(*cobra.Command, []string) error { return nil }}
	bd := &cobra.Command{Use: "bd", DisableFlagParsing: true, RunE: func(*cobra.Command, []string) error { return nil }}
	root.AddCommand(agent, hook, old, bd)
	return root
}

func promptGCTestPackRoot() *cobra.Command {
	packRoot := &cobra.Command{Use: "gc"}
	addDiscoveredCommandsToRoot(packRoot, []config.DiscoveredCommand{
		{Name: "claim", Command: []string{"claim"}, BindingName: "gc", RunScript: "claim.sh"},
	}, "/city", "demo", io.Discard, io.Discard, false)
	return packRoot
}

func TestPromptGCResolver(t *testing.T) {
	resolver := promptGCResolver{roots: []*cobra.Command{promptGCTestTree(), promptGCTestPackRoot()}}
	tests := []struct {
		name string
		args string
		want []promptGCFinding
	}{
		{name: "known leaf with flag", args: "agent list --json"},
		{name: "persistent root flag with value before command", args: "--city /tmp/c agent list"},
		{name: "persistent flag inherited by leaf", args: "agent list --city=/tmp/c"},
		{name: "group declaring an argument takes positionals", args: "hook worker --claim"},
		{name: "value flag consumes its value", args: "hook --format json"},
		{name: "placeholder stops the walk", args: "<binding> claim"},
		{name: "pass-through command is not parsed", args: "bd create --anything goes"},
		{name: "pack namespace resolves", args: "gc claim"},
		{name: "help is always available", args: "agent --help"},
		{
			name: "unknown top-level command",
			args: "frobnicate now",
			want: []promptGCFinding{{Kind: promptGCUnknownCommand, Command: "gc frobnicate"}},
		},
		{
			name: "unknown subcommand of an argument-less group",
			args: "agent claim --json",
			want: []promptGCFinding{{Kind: promptGCUnknownSubcommand, Command: "gc agent claim"}},
		},
		{
			name: "unknown pack command",
			args: "gc bogus",
			want: []promptGCFinding{{Kind: promptGCUnknownSubcommand, Command: "gc gc bogus"}},
		},
		{
			name: "unknown flag",
			args: "hook --claim --lease 5m",
			want: []promptGCFinding{{Kind: promptGCUnknownFlag, Command: "gc hook --lease"}},
		},
		{
			name: "deprecated command",
			args: "old",
			want: []promptGCFinding{{Kind: promptGCDeprecatedCommand, Command: "gc old"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolver.resolve(strings.Fields(tt.args))
			if len(got) != len(tt.want) {
				t.Fatalf("resolve(%q) = %+v, want %d finding(s) %+v", tt.args, got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i].Kind != tt.want[i].Kind || got[i].Command != tt.want[i].Command {
					t.Errorf("finding %d = {%s %q}, want {%s %q}", i, got[i].Kind, got[i].Command, tt.want[i].Kind, tt.want[i].Command)
				}
				if got[i].Reason == "" {
					t.Errorf("finding %d has no reason", i)
				}
			}
		})
	}
}

// TestPromptGCResolverAgainstRealTree pins the regression the check exists
// for against this gc's own command tree: a prompt line that resolves stays
// clean, and one naming a subcommand gc does not have is reported.
func TestPromptGCResolverAgainstRealTree(t *testing.T) {
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	resolver := promptGCResolver{roots: []*cobra.Command{root}}
	if got := resolver.resolve([]string{"hook", "--claim", "--json"}); len(got) != 0 {
		t.Fatalf("gc hook --claim --json: findings %+v, want none", got)
	}
	got := resolver.resolve([]string{"agent", "claim"})
	if len(got) != 1 || got[0].Kind != promptGCUnknownSubcommand {
		t.Fatalf("gc agent claim: findings %+v, want one %s", got, promptGCUnknownSubcommand)
	}
}

func TestPromptGCCommandsCheckRun(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	stale := writePromptFile(t, cityPath, "prompts/stale.md", "Claim:\n\n```\ngc agent claim --json\ngc hook --claim\n```\n")
	clean := writePromptFile(t, cityPath, "prompts/clean.md", "Run `gc hook --claim` and `gc gc claim`.\n")
	alsoStale := writePromptFile(t, cityPath, "prompts/also.md", "Then `gc agent claim`.\n")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents: []config.Agent{
			promptFixtureAgent("worker", stale, "", "arg"),
			promptFixtureAgent("reviewer", clean, "", "arg"),
			promptFixtureAgent("planner", alsoStale, "", "arg"),
			{Name: "no-prompt", StartCommand: "true"},
		},
		PackCommands: []config.DiscoveredCommand{
			{Name: "claim", Command: []string{"claim"}, BindingName: "gc", RunScript: "claim.sh"},
		},
	}

	res := newPromptGCCommandsDoctorCheck(cityPath, cfg, promptGCTestTree()).Run(&doctor.CheckContext{CityPath: cityPath})
	if res.Status != doctor.StatusWarning || res.Severity != doctor.SeverityAdvisory {
		t.Fatalf("status/severity = %v/%v, want warning/advisory; message=%q details=%v", res.Status, res.Severity, res.Message, res.Details)
	}
	payload, ok := res.Payload.(promptGCCommandsPayload)
	if !ok {
		t.Fatalf("payload type = %T, want promptGCCommandsPayload", res.Payload)
	}
	if payload.AgentsScanned != 3 {
		t.Errorf("agents scanned = %d, want 3", payload.AgentsScanned)
	}
	want := []promptGCCommandFindings{{
		Kind:    promptGCUnknownSubcommand,
		Command: "gc agent claim",
		Agents:  []string{"planner", "worker"},
	}}
	if len(payload.Findings) != 1 || payload.Findings[0].Kind != want[0].Kind || payload.Findings[0].Command != want[0].Command ||
		!reflect.DeepEqual(payload.Findings[0].Agents, want[0].Agents) {
		t.Fatalf("findings = %+v, want %+v", payload.Findings, want)
	}
	if len(res.Details) != 1 || !strings.Contains(res.Details[0], "gc agent claim") || !strings.Contains(res.Details[0], "planner, worker") {
		t.Errorf("details = %v, want one line naming the command and both agents", res.Details)
	}
}

func TestPromptGCCommandsCheckRunClean(t *testing.T) {
	clearPromptDeliveryBudgetEnv(t)
	cityPath := t.TempDir()
	tmpl := writePromptFile(t, cityPath, "prompts/ok.md", "Use `gc agent list --json`.\n")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{promptFixtureAgent("worker", tmpl, "", "arg")},
	}
	res := newPromptGCCommandsDoctorCheck(cityPath, cfg, promptGCTestTree()).Run(&doctor.CheckContext{CityPath: cityPath})
	if res.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; message=%q details=%v", res.Status, res.Message, res.Details)
	}
	if !strings.Contains(res.Message, "1 gc invocation(s) in 1 agent prompt(s)") {
		t.Errorf("message = %q, want the invocation and prompt counts", res.Message)
	}
}

func TestPromptGCCommandsCheckWithoutTreeOrConfig(t *testing.T) {
	for name, check := range map[string]*promptGCCommandsDoctorCheck{
		"nil config": newPromptGCCommandsDoctorCheck("", nil, promptGCTestTree()),
		"nil tree":   newPromptGCCommandsDoctorCheck("", &config.City{}, nil),
	} {
		if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
			t.Errorf("%s: status = %v, want OK", name, res.Status)
		}
	}
	if newPromptGCCommandsDoctorCheck("", nil, nil).CanFix() {
		t.Error("CanFix() = true, want false")
	}
}

func TestPromptGCCommandsCheckRegisteredOnlyWithCommandRoot(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}
	base := buildDoctorChecksOpts{ControllerRunning: true, SkipCityDoltCheck: true, SkipManagedDoltCheck: true}

	if doctorCheckIndex(doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, base)), promptGCCommandsCheckName) >= 0 {
		t.Errorf("%s registered without a command tree", promptGCCommandsCheckName)
	}
	withRoot := base
	withRoot.CommandRoot = promptGCTestTree()
	if doctorCheckIndex(doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, withRoot)), promptGCCommandsCheckName) < 0 {
		t.Errorf("%s not registered with a command tree", promptGCCommandsCheckName)
	}
}
