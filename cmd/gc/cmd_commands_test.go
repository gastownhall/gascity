package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

func TestAddDiscoveredCommandsToRoot_BuildsBindingScopedNestedTree(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{Use: "start"})

	entries := []config.DiscoveredCommand{
		{
			BindingName: "gs",
			Command:     []string{"status"},
			Description: "Show status",
		},
		{
			BindingName: "gs",
			Command:     []string{"repo", "sync"},
			Description: "Sync repo",
		},
	}

	addDiscoveredCommandsToRoot(root, entries, "/city", "testcity", os.Stdout, os.Stderr, true)

	gs := findSubcommand(root, "gs")
	if gs == nil {
		t.Fatal("missing binding namespace command")
	}
	if findSubcommand(gs, "status") == nil {
		t.Fatal("missing status leaf under binding namespace")
	}
	repo := findSubcommand(gs, "repo")
	if repo == nil {
		t.Fatal("missing nested repo namespace")
	}
	sync := findSubcommand(repo, "sync")
	if sync == nil {
		t.Fatal("missing nested sync leaf")
	}
	if !sync.DisableFlagParsing {
		t.Fatal("sync leaf DisableFlagParsing = false, want true")
	}
	for name, command := range map[string]*cobra.Command{
		"binding namespace": gs,
		"intermediate":      repo,
		"leaf":              sync,
	} {
		if got := command.Annotations["gc.productmetrics.class"]; got != "pack-command" {
			t.Errorf("%s product-metrics class = %q, want %q", name, got, "pack-command")
		}
	}
}

func TestRunDiscoveredCommand_UsesPackContext(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "pack")
	sourceDir := filepath.Join(packDir, "commands", "status")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(sourceDir, "run.sh")
	script := `#!/bin/sh
echo "packdir=$GC_PACK_DIR"
echo "packname=$GC_PACK_NAME"
echo "cityname=$GC_CITY_NAME"
echo "args=$*"
echo "gcmetrics=$GC_DISABLE_USAGE_METRICS"
echo "bdmetrics=$BD_DISABLE_METRICS"
echo "otel=$OTEL_SERVICE_NAME"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	entry := config.DiscoveredCommand{
		BindingName: "gs",
		PackName:    "mypack",
		Command:     []string{"status"},
		RunScript:   scriptPath,
		PackDir:     packDir,
		SourceDir:   sourceDir,
	}
	t.Setenv("GC_DISABLE_USAGE_METRICS", "ambient-value-must-lose")
	t.Setenv("BD_DISABLE_METRICS", "keep-beads-setting")
	t.Setenv("OTEL_SERVICE_NAME", "keep-otel-setting")

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", []string{"hello", "world"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "packdir="+packDir) {
		t.Fatalf("stdout missing pack dir, got:\n%s", out)
	}
	if !strings.Contains(out, "packname=mypack") {
		t.Fatalf("stdout missing pack name, got:\n%s", out)
	}
	if !strings.Contains(out, "cityname=testcity") {
		t.Fatalf("stdout missing city name, got:\n%s", out)
	}
	if !strings.Contains(out, "args=hello world") {
		t.Fatalf("stdout missing args, got:\n%s", out)
	}
	for _, want := range []string{
		"gcmetrics=1",
		"bdmetrics=keep-beads-setting",
		"otel=keep-otel-setting",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q, got:\n%s", want, out)
		}
	}
}

const packCommandProcessHelperArg = "pack-command-process-helper"

type packCommandProcessInvocation struct {
	scenario string
	afterRun string
	args     []string
}

func packCommandScenarioRootOptions(t *testing.T, scenario string, args []string) rootCommandOptions {
	t.Helper()
	options := rootCommandOptionsForArgs(args)
	options.discoverPackCommands = true
	switch scenario {
	case "eager":
		options.eagerPackCommandDiscovery = true
	case "lazy":
		options.eagerPackCommandDiscovery = false
	default:
		t.Fatalf("unknown pack-command scenario %q", scenario)
	}
	return options
}

func runPackCommandScenario(t *testing.T, scenario string, args []string, stdout, stderr io.Writer) int {
	t.Helper()
	return runWithRootCommandOptions(args, stdout, stderr, packCommandScenarioRootOptions(t, scenario, args))
}

func TestPackCommandExitHelper(t *testing.T) {
	invocation, ok := parsePackCommandProcessInvocation(os.Args)
	if !ok {
		return
	}

	code := func() int {
		defer func() {
			if err := os.WriteFile(invocation.afterRun, []byte("reached\n"), 0o600); err != nil {
				_, _ = os.Stderr.WriteString("write post-run marker: " + err.Error() + "\n")
			}
		}()
		return runPackCommandScenario(t, invocation.scenario, invocation.args, os.Stdout, os.Stderr)
	}()
	os.Exit(code)
}

func parsePackCommandProcessInvocation(args []string) (packCommandProcessInvocation, bool) {
	for index, arg := range args {
		if arg != "--" {
			continue
		}
		tail := args[index+1:]
		if len(tail) < 4 || tail[0] != packCommandProcessHelperArg {
			return packCommandProcessInvocation{}, false
		}
		return packCommandProcessInvocation{
			scenario: tail[1],
			afterRun: tail[2],
			args:     append([]string(nil), tail[3:]...),
		}, true
	}
	return packCommandProcessInvocation{}, false
}

func packCommandProcessEnv(extra ...string) []string {
	input := append(sanitizedBaseEnv(), extra...)
	out := make([]string, 0, len(input)+1)
	for _, entry := range input {
		key, _, _ := strings.Cut(entry, "=")
		if len(key) >= len("OTEL_") && strings.EqualFold(key[:len("OTEL_")], "OTEL_") {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "OTEL_SDK_DISABLED=true")
}

type packCommandProcessResult struct {
	exitCode int
	stdout   string
	stderr   string
}

func runPackCommandProcess(t *testing.T, cityPath, scenario string, args ...string) packCommandProcessResult {
	t.Helper()
	afterRun := filepath.Join(t.TempDir(), "after-run")
	commandArgs := []string{
		"-test.run=^TestPackCommandExitHelper$",
		"--",
		packCommandProcessHelperArg,
		scenario,
		afterRun,
	}
	commandArgs = append(commandArgs, args...)
	cmd := exec.Command(os.Args[0], commandArgs...)
	cmd.Dir = cityPath
	cmd.Env = packCommandProcessEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("pack-command helper error = %v", err)
		}
		exitCode = exitErr.ExitCode()
	}
	if got, err := os.ReadFile(afterRun); err != nil || string(got) != "reached\n" {
		t.Fatalf("post-run marker = %q, err=%v; run did not return through deferred lifecycle", got, err)
	}
	return packCommandProcessResult{exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String()}
}

func setupPackExitCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	for _, commandDir := range []string{
		filepath.Join(cityPath, "commands", "hello"),
		filepath.Join(cityPath, "commands", "repo", "sync"),
	} {
		if err := os.MkdirAll(commandDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"testcity\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nname = \"backstage\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(cityPath, "commands", "hello", "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nprintf 'pack-before-exit\\n'\nexit 42\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	nestedDir := filepath.Join(cityPath, "commands", "repo", "sync")
	if err := os.WriteFile(filepath.Join(nestedDir, "run.sh"), []byte("#!/bin/sh\nprintf 'nested-pack-command\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, "command.toml"), []byte("description = \"Synchronize repository state\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

func addE1HelpOnlyCommand(t *testing.T, city string, command ...string) {
	t.Helper()
	dir := filepath.Join(append([]string{city, "commands"}, command...)...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeE1ArgEchoCommand(t *testing.T, city, label string, command ...string) {
	t.Helper()
	dir := filepath.Join(append([]string{city, "commands"}, command...)...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '" + label + " args:'\nfor arg in \"$@\"; do printf '<%s>' \"$arg\"; done\nprintf '\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func setupE1PreLeafHelpFixture(t *testing.T) (cityA, cityB, targetRig string) {
	t.Helper()
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_RIG", "")

	cityA = setupPackExitCity(t)
	cityB = setupPackExitCity(t)
	for _, command := range [][]string{{"city-b-only"}, {"repo", "city-b-only"}} {
		addE1HelpOnlyCommand(t, cityB, command...)
	}
	writeE1ArgEchoCommand(t, cityA, "ambient-hello", "hello")
	writeE1ArgEchoCommand(t, cityA, "ambient-sync", "repo", "sync")
	writeE1ArgEchoCommand(t, cityB, "selected-hello", "hello")
	writeE1ArgEchoCommand(t, cityB, "selected-sync", "repo", "sync")
	for path, text := range map[string]string{
		filepath.Join(cityA, "commands", "repo", "sync", "help.md"): "ambient-sync-help",
		filepath.Join(cityB, "commands", "repo", "sync", "help.md"): "selected-sync-help",
	} {
		if err := os.WriteFile(path, []byte(text+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	targetRig = "target-rig"
	targetRigDir := filepath.Join(t.TempDir(), targetRig)
	if err := os.MkdirAll(targetRigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	registerRigBindingForResolution(t, os.Getenv("GC_HOME"), cityB, "city-b", targetRig, targetRigDir)
	if err := os.WriteFile(filepath.Join(cityB, "pack.toml"), []byte("[pack]\nname = \"backstage\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityA, cityB, targetRig
}

func TestE1Final3MalformedBooleanHelpBeforeLeafNeverExecutesAmbient(t *testing.T) {
	cityA, cityB, targetRig := setupE1PreLeafHelpFixture(t)
	tests := []struct {
		name string
		args []string
	}{
		{name: "namespace city separate", args: []string{"backstage", "--help=maybe", "--city", cityB, "hello"}},
		{name: "namespace city equals", args: []string{"backstage", "--help=maybe", "--city=" + cityB, "hello"}},
		{name: "intermediate rig council missing", args: []string{"backstage", "repo", "-h=maybe", "--rig=missing-rig", "sync"}},
		{name: "intermediate rig selected separate", args: []string{"backstage", "repo", "-h=maybe", "--rig", targetRig, "sync"}},
		{name: "namespace no scope", args: []string{"backstage", "--help=maybe", "hello"}},
		{name: "intermediate no scope", args: []string{"backstage", "repo", "-h=maybe", "sync"}},
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	for _, lazy := range []bool{false, true} {
		scenario := "eager"
		cwd := cityA
		if lazy {
			scenario = "lazy"
			cwd = t.TempDir()
		}
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}

		for _, test := range tests {
			t.Run(scenario+"/"+test.name, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				code := runPackCommandScenario(t, scenario, test.args, &stdout, &stderr)
				for _, sentinel := range []string{"ambient-hello args:", "ambient-sync args:", "selected-hello args:", "selected-sync args:"} {
					if strings.Contains(stdout.String(), sentinel) {
						t.Fatalf("malformed group help executed pack sentinel %q: code=%d stdout=%q stderr=%q", sentinel, code, stdout.String(), stderr.String())
					}
				}
				if code == 0 || !strings.Contains(stderr.String(), "invalid argument") || !strings.Contains(stderr.String(), "help") {
					t.Fatalf("malformed group help lost Cobra error: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
			})
		}
	}
}

func TestE1PreLeafBooleanHelpSemantics(t *testing.T) {
	cityA, cityB, targetRig := setupE1PreLeafHelpFixture(t)
	tests := []struct {
		name         string
		args         []string
		wantStdout   string
		wantHelpText string
		parityKey    string
		baseline     bool
	}{
		{name: "city namespace bare help", args: []string{"backstage", "--help", "--city", cityB, "hello"}, wantHelpText: "city-b-only", parityKey: "namespace", baseline: true},
		{name: "city namespace long true", args: []string{"backstage", "--help=true", "--city", cityB, "hello"}, wantHelpText: "city-b-only", parityKey: "namespace"},
		{name: "rig intermediate bare help", args: []string{"backstage", "repo", "-h", "--rig=" + targetRig, "sync"}, wantHelpText: "selected-sync-help", parityKey: "intermediate", baseline: true},
		{name: "rig intermediate short one", args: []string{"backstage", "repo", "-h=1", "--rig=" + targetRig, "sync"}, wantHelpText: "selected-sync-help", parityKey: "intermediate"},
		{name: "city namespace long false", args: []string{"backstage", "--help=false", "--city", cityB, "hello", "payload"}, wantStdout: "selected-hello args:<payload>\n"},
		{name: "rig intermediate short zero", args: []string{"backstage", "repo", "-h=0", "--rig=" + targetRig, "sync", "payload"}, wantStdout: "selected-sync args:<payload>\n"},
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	for _, lazy := range []bool{false, true} {
		scenario := "eager"
		cwd := cityA
		if lazy {
			scenario = "lazy"
			cwd = t.TempDir()
		}
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
		helpBaselines := map[string]string{}

		for _, test := range tests {
			t.Run(scenario+"/"+test.name, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				code := runPackCommandScenario(t, scenario, test.args, &stdout, &stderr)
				if code != 0 || stderr.Len() != 0 {
					t.Fatalf("pre-leaf help outcome failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				if test.wantHelpText != "" {
					if !strings.Contains(stdout.String(), test.wantHelpText) {
						t.Fatalf("pre-leaf help used wrong group tree: stdout=%q", stdout.String())
					}
					for _, sentinel := range []string{"ambient-hello args:", "ambient-sync args:", "selected-hello args:", "selected-sync args:"} {
						if strings.Contains(stdout.String(), sentinel) {
							t.Fatalf("pre-leaf help executed pack sentinel %q: stdout=%q", sentinel, stdout.String())
						}
					}
					if test.baseline {
						helpBaselines[test.parityKey] = stdout.String()
					} else if got, want := stdout.String(), helpBaselines[test.parityKey]; got != want {
						t.Fatalf("valued help differs from bare help\ngot:\n%s\nwant:\n%s", got, want)
					}
				} else if stdout.String() != test.wantStdout {
					t.Fatalf("false pre-leaf help leaked flags or selected wrong child: stdout=%q want=%q", stdout.String(), test.wantStdout)
				}
			})
		}
	}
}

func TestE1PreLeafBooleanHelpNoScopeEager(t *testing.T) {
	cityA, _, _ := setupE1PreLeafHelpFixture(t)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	for _, args := range [][]string{
		{"backstage", "--help", "hello"},
		{"backstage", "--help=true", "hello"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 || strings.Contains(stdout.String(), "ambient-hello") || stderr.Len() != 0 {
			t.Fatalf("no-scope true help executed ambient child: args=%q code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"backstage", "--help=0", "hello", "payload"}, &stdout, &stderr); code != 0 || stdout.String() != "ambient-hello args:<payload>\n" || stderr.Len() != 0 {
		t.Fatalf("no-scope false help did not execute clean ambient child: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestE1ScopeAfterGroupHelpUsesSelectedTree(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_RIG", "")

	cityA := setupPackExitCity(t)
	cityB := setupPackExitCity(t)
	for _, command := range [][]string{{"city-b-only"}, {"repo", "city-b-only"}} {
		addE1HelpOnlyCommand(t, cityB, command...)
	}
	targetRig := "target-rig"
	targetRigDir := filepath.Join(t.TempDir(), targetRig)
	if err := os.MkdirAll(targetRigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	registerRigBindingForResolution(t, os.Getenv("GC_HOME"), cityB, "city-b", targetRig, targetRigDir)
	if err := os.WriteFile(filepath.Join(cityB, "pack.toml"), []byte("[pack]\nname = \"backstage\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		args []string
	}{
		{name: "city namespace long help separate", args: []string{"backstage", "--help", "--city", cityB}},
		{name: "city intermediate short help equals", args: []string{"backstage", "repo", "-h", "--city=" + cityB}},
		{name: "rig namespace short help equals", args: []string{"backstage", "-h", "--rig=" + targetRig}},
		{name: "rig intermediate long help separate", args: []string{"backstage", "repo", "--help", "--rig", targetRig}},
		{name: "repeated city last value after help", args: []string{"backstage", "--city", cityA, "--help", "--city=" + cityB}},
		{name: "repeated rig last value after help", args: []string{"backstage", "repo", "--rig", "missing-rig", "-h", "--rig=" + targetRig}},
		{name: "city namespace lone dash before later scope", args: []string{"backstage", "-", "--city", cityB, "--help"}},
		{name: "rig intermediate lone dash before later scope", args: []string{"backstage", "repo", "-", "--rig=" + targetRig, "--help"}},
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	for _, lazy := range []bool{false, true} {
		scenario := "eager"
		cwd := cityA
		if lazy {
			scenario = "lazy"
			cwd = t.TempDir()
		}
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}

		for _, test := range tests {
			t.Run(scenario+"/"+test.name, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				code := runPackCommandScenario(t, scenario, test.args, &stdout, &stderr)
				if code != 0 || !strings.Contains(stdout.String(), "city-b-only") || stderr.Len() != 0 {
					t.Fatalf("scope after group help used wrong tree: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
			})
		}
	}
}

func TestE1LoneDashIsTransparentToEagerLazyDiscovery(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	cityA, cityB, targetRig := setupE1PreLeafHelpFixture(t)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	tests := []struct {
		name         string
		args         []string
		wantExact    string
		wantContains string
	}{
		{
			name:      "before binding with city",
			args:      []string{"-", "--city", cityB, "backstage", "hello", "payload"},
			wantExact: "selected-hello args:<-><--city><" + cityB + "><payload>\n",
		},
		{
			name:      "between binding and leaf with city",
			args:      []string{"backstage", "-", "--city", cityB, "hello", "payload"},
			wantExact: "selected-hello args:<-><--city><" + cityB + "><payload>\n",
		},
		{
			name:      "between intermediate and leaf with rig",
			args:      []string{"backstage", "repo", "-", "--rig", targetRig, "sync", "payload"},
			wantExact: "selected-sync args:<-><--rig><" + targetRig + "><payload>\n",
		},
		{
			name:         "before binding group help",
			args:         []string{"-", "--city", cityB, "backstage", "repo", "--help"},
			wantContains: "city-b-only",
		},
		{
			name:         "between namespace words group help",
			args:         []string{"backstage", "-", "repo", "--rig", targetRig, "--help"},
			wantContains: "city-b-only",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results := make(map[string]packCommandProcessResult, 2)
			for _, scenario := range []string{"eager", "lazy"} {
				var stdout, stderr bytes.Buffer
				results[scenario] = packCommandProcessResult{
					exitCode: runPackCommandScenario(t, scenario, test.args, &stdout, &stderr),
					stdout:   stdout.String(),
					stderr:   stderr.String(),
				}
			}

			eager, lazy := results["eager"], results["lazy"]
			if eager != lazy {
				t.Fatalf("lone dash changed eager/lazy dispatch\neager=%+v\nlazy=%+v", eager, lazy)
			}
			if eager.exitCode != 0 || eager.stderr != "" {
				t.Fatalf("lone dash dispatch = %+v, want success with empty stderr", eager)
			}
			if test.wantExact != "" && eager.stdout != test.wantExact {
				t.Fatalf("lone dash stdout = %q, want %q", eager.stdout, test.wantExact)
			}
			if test.wantContains != "" && !strings.Contains(eager.stdout, test.wantContains) {
				t.Fatalf("lone dash help stdout = %q, want %q", eager.stdout, test.wantContains)
			}
		})
	}
}

func TestE1GlobalJSONControlDoesNotBlockScopedPackResolution(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	t.Setenv("GC_JSON_CONTRACT_STRICT", "1")
	cityA, cityB, targetRig := setupE1PreLeafHelpFixture(t)
	for path, marker := range map[string]string{
		filepath.Join(cityA, "commands", "hello", "schemas", "result.schema.json"):        "ambient-hello-schema",
		filepath.Join(cityA, "commands", "repo", "sync", "schemas", "result.schema.json"): "ambient-sync-schema",
		filepath.Join(cityB, "commands", "hello", "schemas", "result.schema.json"):        "selected-hello-schema",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"type":"string","const":"`+marker+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	tests := []struct {
		name          string
		args          []string
		wantCode      int
		wantStdout    string
		wantSubstring string
	}{
		{
			name:       "city success after binding",
			args:       []string{"backstage", "--json", "--city", cityB, "hello", "payload"},
			wantCode:   0,
			wantStdout: "selected-hello args:<--city><" + cityB + "><--json><payload>\n",
		},
		{
			name:       "rig success before binding",
			args:       []string{"--json=1", "--rig", targetRig, "backstage", "hello", "payload"},
			wantCode:   0,
			wantStdout: "selected-hello args:<--rig><" + targetRig + "><--json=1><payload>\n",
		},
		{
			name:          "selected missing schema fails despite ambient schema",
			args:          []string{"backstage", "repo", "--json=true", "--rig", targetRig, "sync", "payload"},
			wantCode:      1,
			wantSubstring: `"code":"json_unsupported"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results := make(map[string]packCommandProcessResult, 2)
			for _, scenario := range []string{"eager", "lazy"} {
				var stdout, stderr bytes.Buffer
				results[scenario] = packCommandProcessResult{
					exitCode: runPackCommandScenario(t, scenario, test.args, &stdout, &stderr),
					stdout:   stdout.String(),
					stderr:   stderr.String(),
				}
			}

			eager, lazy := results["eager"], results["lazy"]
			if eager != lazy {
				t.Fatalf("JSON control changed eager/lazy dispatch\neager=%+v\nlazy=%+v", eager, lazy)
			}
			if eager.exitCode != test.wantCode || eager.stderr != "" {
				t.Fatalf("JSON control dispatch = %+v, want exit=%d with empty stderr", eager, test.wantCode)
			}
			if test.wantStdout != "" && eager.stdout != test.wantStdout {
				t.Fatalf("JSON control stdout = %q, want %q", eager.stdout, test.wantStdout)
			}
			if test.wantSubstring != "" && !strings.Contains(eager.stdout, test.wantSubstring) {
				t.Fatalf("JSON control stdout = %q, want substring %q", eager.stdout, test.wantSubstring)
			}
			for _, forbidden := range []string{"ambient-hello args:", "ambient-sync args:", "selected-sync args:"} {
				if strings.Contains(eager.stdout+eager.stderr, forbidden) {
					t.Fatalf("JSON control used wrong pack path %q: %+v", forbidden, eager)
				}
			}
		})
	}
}

func TestE1BooleanHelpValueAfterGroupUsesSelectedTree(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_RIG", "")

	cityA := setupPackExitCity(t)
	cityB := setupPackExitCity(t)
	for _, command := range [][]string{{"city-b-only"}, {"repo", "city-b-only"}} {
		addE1HelpOnlyCommand(t, cityB, command...)
	}
	targetRig := "target-rig"
	targetRigDir := filepath.Join(t.TempDir(), targetRig)
	if err := os.MkdirAll(targetRigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	registerRigBindingForResolution(t, os.Getenv("GC_HOME"), cityB, "city-b", targetRig, targetRigDir)
	if err := os.WriteFile(filepath.Join(cityB, "pack.toml"), []byte("[pack]\nname = \"backstage\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	valid := []struct {
		name string
		args []string
	}{
		{name: "city namespace long true separate", args: []string{"backstage", "--help=true", "--city", cityB}},
		{name: "city intermediate short true equals", args: []string{"backstage", "repo", "-h=true", "--city=" + cityB}},
		{name: "rig namespace long false equals", args: []string{"backstage", "--help=false", "--rig=" + targetRig}},
		{name: "rig intermediate short false separate", args: []string{"backstage", "repo", "-h=false", "--rig", targetRig}},
		{name: "city namespace long one equals", args: []string{"backstage", "--help=1", "--city=" + cityB}},
		{name: "city intermediate short zero separate", args: []string{"backstage", "repo", "-h=0", "--city", cityB}},
		{name: "rig namespace long one separate", args: []string{"backstage", "--help=1", "--rig", targetRig}},
		{name: "rig intermediate short zero equals", args: []string{"backstage", "repo", "-h=0", "--rig=" + targetRig}},
	}
	invalid := []struct {
		name string
		args []string
	}{
		{name: "city invalid long", args: []string{"backstage", "--help=maybe", "--city", cityB}},
		{name: "rig invalid short", args: []string{"backstage", "repo", "-h=maybe", "--rig=" + targetRig}},
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	for _, lazy := range []bool{false, true} {
		scenario := "eager"
		cwd := cityA
		if lazy {
			scenario = "lazy"
			cwd = t.TempDir()
		}
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}

		for _, test := range valid {
			t.Run(scenario+"/valid/"+test.name, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				code := runPackCommandScenario(t, scenario, test.args, &stdout, &stderr)
				if code != 0 || !strings.Contains(stdout.String(), "city-b-only") || stderr.Len() != 0 {
					t.Fatalf("boolean help value used wrong scope: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
			})
		}
		for _, test := range invalid {
			t.Run(scenario+"/invalid/"+test.name, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				code := runPackCommandScenario(t, scenario, test.args, &stdout, &stderr)
				if code == 0 || strings.Contains(stdout.String(), "city-b-only") || strings.Contains(stdout.String(), "pack-before-exit") {
					t.Fatalf("invalid boolean help value did not preserve Cobra error behavior: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
			})
		}
	}
}

func TestE1PackCommandTreeRequestBooleanHelpGrammar(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	annotation := map[string]string{productMetricsClassAnnotation: packCommandClassificationValue}
	namespace := &cobra.Command{Use: "backstage", Annotations: annotation}
	intermediate := &cobra.Command{Use: "repo", Annotations: annotation}
	leaf := &cobra.Command{Use: "hello", Annotations: annotation, DisableFlagParsing: true}
	namespace.AddCommand(intermediate, leaf)
	root.AddCommand(namespace)

	tests := []struct {
		name string
		args []string
		want packCommandTreePreparation
	}{
		{
			name: "long true scans city",
			args: []string{"backstage", "--help=true", "--city", "/city"},
			want: packCommandTreePreparation{binding: "backstage", city: "/city", citySet: true, scopeCount: 1},
		},
		{
			name: "short false scans rig",
			args: []string{"backstage", "repo", "-h=false", "--rig=rig-a"},
			want: packCommandTreePreparation{binding: "backstage", rig: "rig-a", rigSet: true, scopeCount: 1},
		},
		{
			name: "long one scans city",
			args: []string{"backstage", "--help=1", "--city=/city"},
			want: packCommandTreePreparation{binding: "backstage", city: "/city", citySet: true, scopeCount: 1},
		},
		{
			name: "short zero scans rig",
			args: []string{"backstage", "repo", "-h=0", "--rig", "rig-a"},
			want: packCommandTreePreparation{binding: "backstage", rig: "rig-a", rigSet: true, scopeCount: 1},
		},
		{
			name: "invalid long still scans city for fail closed guard",
			args: []string{"backstage", "--help=maybe", "--city", "/city"},
			want: packCommandTreePreparation{binding: "backstage", city: "/city", citySet: true, scopeCount: 1},
		},
		{
			name: "invalid short still scans rig for fail closed guard",
			args: []string{"backstage", "repo", "-h=maybe", "--rig=rig-a"},
			want: packCommandTreePreparation{binding: "backstage", rig: "rig-a", rigSet: true, scopeCount: 1},
		},
		{
			name: "selected leaf owns valued help and city",
			args: []string{"backstage", "hello", "--help=true", "--city", "/city"},
			want: packCommandTreePreparation{binding: "backstage", preLeafCommandIndex: 1},
		},
		{
			name: "terminator owns valued help and rig",
			args: []string{"backstage", "--", "--help=true", "--rig=rig-a"},
			want: packCommandTreePreparation{binding: "backstage"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := packCommandTreeRequest(root, test.args)
			if !ok || got != test.want {
				t.Fatalf("packCommandTreeRequest(%q) = (%+v, %v), want (%+v, true)", test.args, got, ok, test.want)
			}
		})
	}
}

func TestE1ExplicitCityOverridesEagerPackBinding(t *testing.T) {
	cityA := setupPackExitCity(t)
	cityB := setupPackExitCity(t)
	cityBScript := filepath.Join(cityB, "commands", "hello", "run.sh")
	if err := os.WriteFile(cityBScript, []byte("#!/bin/sh\nprintf 'city-b\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityB, "backstage", "hello"}, &stdout, &stderr)
	if code != 0 || stdout.String() != "city-b\n" || stderr.Len() != 0 {
		t.Fatalf("explicit city selected wrong pack: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestE1EmptyExplicitCityFailsClosed(t *testing.T) {
	cityA := setupPackExitCity(t)
	cityB := setupPackExitCity(t)
	if err := os.WriteFile(filepath.Join(cityB, "commands", "hello", "run.sh"), []byte("#!/bin/sh\nprintf 'city-b\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "leading empty equals", args: []string{"--city=", "backstage", "hello"}},
		{name: "leading empty separate", args: []string{"--city", "", "backstage", "hello"}},
		{name: "inherited empty equals", args: []string{"backstage", "--city=", "hello"}},
		{name: "inherited empty separate", args: []string{"backstage", "--city", "", "hello"}},
		{name: "repeated last empty", args: []string{"--city", cityB, "--city=", "backstage", "hello"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(test.args, &stdout, &stderr)
			if code == 0 || strings.Contains(stdout.String(), "pack-before-exit") {
				t.Fatalf("empty explicit scope did not fail closed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city=", "--city", cityB, "backstage", "hello"}, &stdout, &stderr)
	if code != 0 || stdout.String() != "city-b\n" || stderr.Len() != 0 {
		t.Fatalf("last non-empty city did not recover earlier empty value: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestE1EmptyExplicitRigFailsClosed(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_RIG", "")

	cityA := setupPackExitCity(t)
	cityB := setupPackExitCity(t)
	if err := os.WriteFile(filepath.Join(cityB, "commands", "hello", "run.sh"), []byte("#!/bin/sh\nprintf 'rig-city-b\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigName := "target-rig"
	rigDir := filepath.Join(t.TempDir(), rigName)
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	registerRigBindingForResolution(t, os.Getenv("GC_HOME"), cityB, "rig-city-b", rigName, rigDir)
	if err := os.WriteFile(filepath.Join(cityB, "pack.toml"), []byte("[pack]\nname = \"backstage\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "leading empty equals", args: []string{"--rig=", "backstage", "hello"}},
		{name: "leading empty separate", args: []string{"--rig", "", "backstage", "hello"}},
		{name: "inherited empty equals", args: []string{"backstage", "--rig=", "hello"}},
		{name: "inherited empty separate", args: []string{"backstage", "--rig", "", "hello"}},
		{name: "repeated last empty", args: []string{"--rig", rigName, "--rig=", "backstage", "hello"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(test.args, &stdout, &stderr)
			if code == 0 || strings.Contains(stdout.String(), "pack-before-exit") {
				t.Fatalf("empty explicit rig did not fail closed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--rig=", "--rig", rigName, "backstage", "hello"}, &stdout, &stderr)
	if code != 0 || stdout.String() != "rig-city-b\n" || stderr.Len() != 0 {
		t.Fatalf("last non-empty rig did not recover earlier empty value: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestE1LazyInheritedCityMaterializesGroupHelpAndSchema(t *testing.T) {
	city := setupPackExitCity(t)
	onlyDir := filepath.Join(city, "commands", "city-b-only")
	if err := os.MkdirAll(onlyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(onlyDir, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	schemaDir := filepath.Join(city, "commands", "hello", "schemas")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(schemaDir, "result.schema.json"), []byte(`{"type":"string","const":"lazy-city"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	t.Run("group help", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"backstage", "--city", city, "--help"}
		code := runPackCommandScenario(t, "lazy", args, &stdout, &stderr)
		if code != 0 || !strings.Contains(stdout.String(), "city-b-only") || stderr.Len() != 0 {
			t.Fatalf("lazy scoped group help did not use selected pack tree: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})
	t.Run("schema", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"backstage", "--city", city, "--json-schema", "result", "hello"}
		code := runPackCommandScenario(t, "lazy", args, &stdout, &stderr)
		if code != 0 || !strings.Contains(stdout.String(), `"const":"lazy-city"`) || stderr.Len() != 0 {
			t.Fatalf("lazy scoped schema did not use selected pack tree: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})
}

func TestE1LazyInheritedScopeCoversGroupsAndAllSchemaRoles(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_RIG", "")

	city := setupPackExitCity(t)
	for _, commandPath := range [][]string{{"city-b-only"}, {"repo", "city-b-only"}} {
		commandDir := filepath.Join(append([]string{city, "commands"}, commandPath...)...)
		if err := os.MkdirAll(commandDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(commandDir, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, marker := range map[string]string{
		filepath.Join(city, "commands", "hello", "schemas", "result.schema.json"):         "lazy-result-hello",
		filepath.Join(city, "commands", "hello", "schemas", "failure.schema.json"):        "lazy-failure-hello",
		filepath.Join(city, "commands", "repo", "sync", "schemas", "result.schema.json"):  "lazy-result-sync",
		filepath.Join(city, "commands", "repo", "sync", "schemas", "failure.schema.json"): "lazy-failure-sync",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"type":"string","const":"`+marker+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rigName := "target-rig"
	rigDir := filepath.Join(t.TempDir(), rigName)
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	registerRigBindingForResolution(t, os.Getenv("GC_HOME"), city, "lazy-city", rigName, rigDir)
	if err := os.WriteFile(filepath.Join(city, "pack.toml"), []byte("[pack]\nname = \"backstage\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	helpTests := []struct {
		name string
		args []string
		want string
	}{
		{name: "city namespace separate", args: []string{"backstage", "--city", city, "--help"}, want: "city-b-only"},
		{name: "city intermediate equals", args: []string{"backstage", "repo", "--city=" + city, "--help"}, want: "city-b-only"},
		{name: "rig namespace equals", args: []string{"backstage", "--rig=" + rigName, "--help"}, want: "city-b-only"},
		{name: "rig intermediate separate", args: []string{"backstage", "repo", "--rig", rigName, "--help"}, want: "city-b-only"},
	}
	for _, test := range helpTests {
		t.Run("help "+test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runPackCommandScenario(t, "lazy", test.args, &stdout, &stderr)
			if code != 0 || !strings.Contains(stdout.String(), test.want) || stderr.Len() != 0 {
				t.Fatalf("lazy scoped group help used wrong tree: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}

	schemaTests := []struct {
		name string
		args []string
		want string
	}{
		{name: "city result separate", args: []string{"backstage", "--city", city, "--json-schema", "result", "hello"}, want: `"const":"lazy-result-hello"`},
		{name: "city failure equals", args: []string{"backstage", "repo", "--city=" + city, "--json-schema=failure", "sync"}, want: `"const":"lazy-failure-sync"`},
		{name: "city manifest separate", args: []string{"backstage", "repo", "--city", city, "--json-schema", "manifest", "sync"}, want: `"const":"lazy-result-sync"`},
		{name: "rig result equals", args: []string{"backstage", "--rig=" + rigName, "--json-schema=result", "hello"}, want: `"const":"lazy-result-hello"`},
		{name: "rig failure separate", args: []string{"backstage", "repo", "--rig", rigName, "--json-schema", "failure", "sync"}, want: `"const":"lazy-failure-sync"`},
		{name: "rig manifest equals", args: []string{"backstage", "--rig=" + rigName, "--json-schema=manifest", "hello"}, want: `"const":"lazy-result-hello"`},
	}
	for _, test := range schemaTests {
		t.Run("schema "+test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runPackCommandScenario(t, "lazy", test.args, &stdout, &stderr)
			if code != 0 || !strings.Contains(stdout.String(), test.want) || stderr.Len() != 0 {
				t.Fatalf("lazy scoped schema used wrong tree: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestE1InheritedCityAfterBindingOverridesEagerPackBinding(t *testing.T) {
	cityA := setupPackExitCity(t)
	cityB := setupPackExitCity(t)
	cityBScript := filepath.Join(cityB, "commands", "hello", "run.sh")
	if err := os.WriteFile(cityBScript, []byte("#!/bin/sh\nprintf 'city-b-after-binding\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	var stdout, stderr bytes.Buffer
	code := run([]string{"backstage", "--city", cityB, "hello"}, &stdout, &stderr)
	if code != 0 || stdout.String() != "city-b-after-binding\n" || stderr.Len() != 0 {
		t.Fatalf("inherited city selected wrong pack: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestE1InheritedCityAfterIntermediateOverridesEagerPackBinding(t *testing.T) {
	cityA := setupPackExitCity(t)
	cityB := setupPackExitCity(t)
	cityBScript := filepath.Join(cityB, "commands", "repo", "sync", "run.sh")
	if err := os.WriteFile(cityBScript, []byte("#!/bin/sh\nprintf 'nested-city-b-after-binding\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	var stdout, stderr bytes.Buffer
	code := run([]string{"backstage", "repo", "--city", cityB, "sync"}, &stdout, &stderr)
	if code != 0 || stdout.String() != "nested-city-b-after-binding\n" || stderr.Len() != 0 {
		t.Fatalf("inherited city after intermediate selected wrong pack: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestE1InheritedCityResolutionFailureDropsEagerPackBinding(t *testing.T) {
	city := setupPackExitCity(t)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(city); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	var stdout, stderr bytes.Buffer
	missingCity := filepath.Join(t.TempDir(), "missing-city")
	code := run([]string{"backstage", "--city", missingCity, "hello"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "pack-before-exit") {
		t.Fatalf("inherited city resolution failure executed ambient pack: stdout=%q", stdout.String())
	}
}

func TestE1InheritedCityUnavailableScopeDropsEagerPackBinding(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{
			name: "selected city has no binding",
			prepare: func(t *testing.T, city string) {
				if err := os.WriteFile(filepath.Join(city, "pack.toml"), []byte("[pack]\nname = \"other-binding\"\nschema = 2\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "selected city config is invalid",
			prepare: func(t *testing.T, city string) {
				if err := os.WriteFile(filepath.Join(city, "pack.toml"), []byte("[pack\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cityA := setupPackExitCity(t)
			cityB := setupPackExitCity(t)
			test.prepare(t, cityB)

			oldWD, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(cityA); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(oldWD) })

			var stdout, stderr bytes.Buffer
			code := run([]string{"backstage", "--city", cityB, "hello"}, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String(), "pack-before-exit") {
				t.Fatalf("unavailable selected scope executed ambient pack: stdout=%q", stdout.String())
			}
		})
	}
}

func TestE1InheritedRigOverridesEagerPackBinding(t *testing.T) {
	tests := []struct {
		name       string
		args       func(string) []string
		scriptPath func(string) string
		want       string
	}{
		{
			name: "after binding separate value",
			args: func(rig string) []string {
				return []string{"backstage", "--rig", rig, "hello"}
			},
			scriptPath: func(city string) string {
				return filepath.Join(city, "commands", "hello", "run.sh")
			},
			want: "rig-city-b-after-binding\n",
		},
		{
			name: "after intermediate equals value",
			args: func(rig string) []string {
				return []string{"backstage", "repo", "--rig=" + rig, "sync"}
			},
			scriptPath: func(city string) string {
				return filepath.Join(city, "commands", "repo", "sync", "run.sh")
			},
			want: "nested-rig-city-b-after-binding\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GC_HOME", t.TempDir())
			t.Setenv("GC_CITY", "")
			t.Setenv("GC_CITY_PATH", "")
			t.Setenv("GC_CITY_ROOT", "")
			t.Setenv("GC_DIR", "")
			t.Setenv("GC_RIG", "")

			cityA := setupPackExitCity(t)
			cityB := setupPackExitCity(t)
			if err := os.WriteFile(test.scriptPath(cityB), []byte("#!/bin/sh\nprintf '"+strings.TrimSuffix(test.want, "\n")+"\\n'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			rigName := "target-rig"
			rigDir := filepath.Join(t.TempDir(), rigName)
			if err := os.MkdirAll(rigDir, 0o755); err != nil {
				t.Fatal(err)
			}
			registerRigBindingForResolution(t, os.Getenv("GC_HOME"), cityB, "rig-city-b", rigName, rigDir)
			if err := os.WriteFile(filepath.Join(cityB, "pack.toml"), []byte("[pack]\nname = \"backstage\"\nschema = 2\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			oldWD, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(cityA); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(oldWD) })

			var stdout, stderr bytes.Buffer
			code := run(test.args(rigName), &stdout, &stderr)
			if code != 0 || stdout.String() != test.want || stderr.Len() != 0 {
				t.Fatalf("inherited rig selected wrong pack: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestE1InheritedRigResolutionFailureDropsEagerPackBinding(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_RIG", "")

	city := setupPackExitCity(t)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(city); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	var stdout, stderr bytes.Buffer
	code := run([]string{"backstage", "--rig", "missing-rig", "hello"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "pack-before-exit") {
		t.Fatalf("inherited rig resolution failure executed ambient pack: stdout=%q", stdout.String())
	}
}

func TestE1LazyMissingTreeMatchesEagerFlagOwnership(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_RIG", "")

	cityA := setupPackExitCity(t)
	cityB := setupPackExitCity(t)
	writeE1ArgEchoCommand(t, cityA, "city-a-hello", "hello")
	writeE1ArgEchoCommand(t, cityA, "city-a-sync", "repo", "sync")
	writeE1ArgEchoCommand(t, cityB, "city-b-hello", "hello")
	writeE1ArgEchoCommand(t, cityB, "city-b-sync", "repo", "sync")
	targetRig := "target-rig"
	targetRigDir := filepath.Join(t.TempDir(), targetRig)
	if err := os.MkdirAll(targetRigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	registerRigBindingForResolution(t, os.Getenv("GC_HOME"), cityB, "city-b", targetRig, targetRigDir)
	if err := os.WriteFile(filepath.Join(cityB, "pack.toml"), []byte("[pack]\nname = \"backstage\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "namespace city remains root owned",
			args: []string{"backstage", "--city", cityB, "hello", "payload"},
			want: "city-b-hello args:<--city><" + cityB + "><payload>\n",
		},
		{
			name: "intermediate rig remains root owned",
			args: []string{"backstage", "repo", "--rig=" + targetRig, "sync", "payload"},
			want: "city-b-sync args:<--rig=" + targetRig + "><payload>\n",
		},
		{
			name: "leaf city separate is child owned",
			args: []string{"backstage", "hello", "--city", cityB, "payload"},
			want: "city-a-hello args:<--city><" + cityB + "><payload>\n",
		},
		{
			name: "leaf city equals is child owned",
			args: []string{"backstage", "hello", "--city=" + cityB, "payload"},
			want: "city-a-hello args:<--city=" + cityB + "><payload>\n",
		},
		{
			name: "leaf empty scope is child owned",
			args: []string{"backstage", "hello", "--city=", "--rig", "", "payload"},
			want: "city-a-hello args:<--city=><--rig><><payload>\n",
		},
		{
			name: "leaf repeated scopes are child owned",
			args: []string{"backstage", "hello", "--city", cityB, "--city=" + cityA, "payload"},
			want: "city-a-hello args:<--city><" + cityB + "><--city=" + cityA + "><payload>\n",
		},
		{
			name: "leaf malformed help and rig are child owned",
			args: []string{"backstage", "hello", "-h=maybe", "--rig=child-rig"},
			want: "city-a-hello args:<-h=maybe><--rig=child-rig>\n",
		},
		{
			name: "leaf valued help and city are child owned",
			args: []string{"backstage", "hello", "--help=true", "--city", cityB},
			want: "city-a-hello args:<--help=true><--city><" + cityB + ">\n",
		},
		{
			name: "post terminator controls are child owned",
			args: []string{"backstage", "hello", "--", "--city", cityB, "--rig=" + targetRig},
			want: "city-a-hello args:<--><--city><" + cityB + "><--rig=" + targetRig + ">\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results := make(map[string]packCommandProcessResult, 2)
			for _, scenario := range []string{"eager", "lazy"} {
				var stdout, stderr bytes.Buffer
				results[scenario] = packCommandProcessResult{
					exitCode: runPackCommandScenario(t, scenario, test.args, &stdout, &stderr),
					stdout:   stdout.String(),
					stderr:   stderr.String(),
				}
			}

			eager, lazy := results["eager"], results["lazy"]
			if lazy != eager {
				t.Fatalf("lazy dispatch differs from eager ownership\neager: %+v\nlazy:  %+v", eager, lazy)
			}
			if eager.exitCode != 0 || eager.stdout != test.want || eager.stderr != "" {
				t.Fatalf("dispatch outcome = %+v, want exit=0 stdout=%q stderr empty", eager, test.want)
			}
		})
	}
}

func TestE1EagerLazyControlDifferentialMatrix(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	cityA, cityB, targetRig := setupE1PreLeafHelpFixture(t)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	tests := []struct {
		name       string
		args       []string
		noPackExec bool
	}{
		{name: "root help before binding", args: []string{"--help", "backstage", "hello"}, noPackExec: true},
		{name: "root false help before binding", args: []string{"--help=false", "backstage", "hello", "payload"}},
		{name: "uppercase true before leaf", args: []string{"backstage", "--help=TRUE", "--city", cityB, "hello"}, noPackExec: true},
		{name: "uppercase false before leaf", args: []string{"backstage", "--help=FALSE", "--city", cityB, "hello", "payload"}},
		{name: "short T before leaf", args: []string{"backstage", "-h=T", "--rig", targetRig, "hello"}, noPackExec: true},
		{name: "short F before leaf", args: []string{"backstage", "-h=F", "--rig", targetRig, "hello", "payload"}},
		{name: "last help true", args: []string{"backstage", "--help=false", "--help", "--city", cityB, "hello"}, noPackExec: true},
		{name: "last help false", args: []string{"backstage", "--help", "--help=false", "--city", cityB, "hello", "payload"}},
		{name: "invalid help remains first error", args: []string{"backstage", "--help=bad", "--help=false", "--city", cityB, "hello"}, noPackExec: true},
		{name: "unknown flag before scope", args: []string{"backstage", "--unknown", "--city", cityB, "hello"}, noPackExec: true},
		{name: "unknown group child before scope", args: []string{"backstage", "repo", "missing", "--city", cityB}, noPackExec: true},
		{name: "schema before scope", args: []string{"backstage", "--json-schema", "result", "--city", cityB, "hello"}, noPackExec: true},
		{name: "scope before schema", args: []string{"backstage", "--city", cityB, "--json-schema", "result", "hello"}, noPackExec: true},
		{name: "terminator before child", args: []string{"backstage", "--", "--city", cityB, "hello"}, noPackExec: true},
		{name: "leaf bare long help", args: []string{"backstage", "hello", "--help"}, noPackExec: true},
		{name: "leaf valued long help is child owned", args: []string{"backstage", "hello", "--help=true"}},
		{name: "leaf bare short help", args: []string{"backstage", "hello", "-h"}, noPackExec: true},
		{name: "leaf valued short help is child owned", args: []string{"backstage", "hello", "-h=true"}},
		{name: "preleaf city with later child-owned city", args: []string{"backstage", "--city", cityB, "hello", "--city", cityA, "payload"}},
		{name: "preleaf rig with later child-owned rig", args: []string{"backstage", "--rig", targetRig, "hello", "--rig", "child-rig", "payload"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results := make(map[string]packCommandProcessResult, 2)
			for _, scenario := range []string{"eager", "lazy"} {
				var stdout, stderr bytes.Buffer
				results[scenario] = packCommandProcessResult{
					exitCode: runPackCommandScenario(t, scenario, test.args, &stdout, &stderr),
					stdout:   stdout.String(),
					stderr:   stderr.String(),
				}
			}

			eager, lazy := results["eager"], results["lazy"]
			if eager != lazy {
				t.Fatalf("eager/lazy drift for %q:\neager=%+v\nlazy=%+v", test.args, eager, lazy)
			}
			if test.noPackExec {
				combined := eager.stdout + eager.stderr
				for _, sentinel := range []string{"ambient-hello args:", "ambient-sync args:", "selected-hello args:", "selected-sync args:", "pack-before-exit"} {
					if strings.Contains(combined, sentinel) {
						t.Fatalf("control invocation executed pack sentinel %q: %+v", sentinel, eager)
					}
				}
			}
		})
	}
}

func TestE1LazyExplicitScopeBeforeLeafWinsOutsideAmbientCity(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	cityA, cityB, targetRig := setupE1PreLeafHelpFixture(t)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "city separate root scope",
			args: []string{"backstage", "--city", cityB, "hello", "--city", cityA, "payload"},
			want: "selected-hello args:<--city><" + cityA + "><payload>\n",
		},
		{
			name: "city equals root scope",
			args: []string{"backstage", "--city=" + cityB, "hello", "--city=" + cityA, "payload"},
			want: "selected-hello args:<--city=" + cityA + "><payload>\n",
		},
		{
			name: "repeated pre-leaf city remains last wins",
			args: []string{"backstage", "--city", cityA, "--city=" + cityB, "hello", "--city", cityA, "payload"},
			want: "selected-hello args:<--city><" + cityA + "><payload>\n",
		},
		{
			name: "false help preserves later child city",
			args: []string{"backstage", "--help=false", "--city", cityB, "hello", "--city", cityA, "payload"},
			want: "selected-hello args:<--city><" + cityA + "><payload>\n",
		},
		{
			name: "rig separate root scope",
			args: []string{"backstage", "--rig", targetRig, "hello", "--rig", "child-rig", "payload"},
			want: "selected-hello args:<--rig><child-rig><payload>\n",
		},
		{
			name: "rig equals root scope",
			args: []string{"backstage", "--rig=" + targetRig, "hello", "--rig=child-rig", "payload"},
			want: "selected-hello args:<--rig=child-rig><payload>\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runPackCommandScenario(t, "lazy", test.args, &stdout, &stderr)
			if code != 0 || stdout.String() != test.want || stderr.Len() != 0 {
				t.Fatalf("explicit pre-leaf scope lost outside ambient city: code=%d stdout=%q stderr=%q want=%q", code, stdout.String(), stderr.String(), test.want)
			}
		})
	}
}

func TestE1ScopeTopologyCycleFailsClosed(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	cityA := setupPackExitCity(t)
	cityB := setupPackExitCity(t)
	writeE1ArgEchoCommand(t, cityA, "ambient-pivot", "pivot")
	writeE1ArgEchoCommand(t, cityB, "selected-nested", "pivot", "hello")

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	args := []string{"backstage", "--city", cityB, "pivot", "--city", cityA, "hello", "payload"}
	results := make(map[string]packCommandProcessResult, 2)
	for _, scenario := range []string{"eager", "lazy"} {
		var stdout, stderr bytes.Buffer
		results[scenario] = packCommandProcessResult{
			exitCode: runPackCommandScenario(t, scenario, args, &stdout, &stderr),
			stdout:   stdout.String(),
			stderr:   stderr.String(),
		}
	}

	eager, lazy := results["eager"], results["lazy"]
	if eager != lazy {
		t.Fatalf("scope-topology cycle changed eager/lazy outcome\neager=%+v\nlazy=%+v", eager, lazy)
	}
	if eager.exitCode != 1 || !strings.Contains(eager.stderr, `gc: unknown command "backstage"`) {
		t.Fatalf("scope-topology cycle outcome = %+v, want root unknown failure", eager)
	}
	for _, sentinel := range []string{"ambient-pivot", "selected-nested", "pack-before-exit"} {
		if strings.Contains(eager.stdout+eager.stderr, sentinel) {
			t.Fatalf("scope-topology cycle executed pack sentinel %q: %+v", sentinel, eager)
		}
	}
}

func TestE1InheritedCitySelectsScopedGroupHelp(t *testing.T) {
	tests := []struct {
		name        string
		commandPath []string
		args        func(string) []string
	}{
		{
			name:        "namespace separate value",
			commandPath: []string{"city-b-only"},
			args: func(city string) []string {
				return []string{"backstage", "--city", city, "--help"}
			},
		},
		{
			name:        "intermediate equals value",
			commandPath: []string{"repo", "city-b-only"},
			args: func(city string) []string {
				return []string{"backstage", "repo", "--city=" + city, "--help"}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cityA := setupPackExitCity(t)
			cityB := setupPackExitCity(t)
			commandDir := filepath.Join(append([]string{cityB, "commands"}, test.commandPath...)...)
			if err := os.MkdirAll(commandDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(commandDir, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(commandDir, "command.toml"), []byte("description = \"City B only command\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			oldWD, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(cityA); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(oldWD) })

			var stdout, stderr bytes.Buffer
			code := run(test.args(cityB), &stdout, &stderr)
			if code != 0 || !strings.Contains(stdout.String(), "city-b-only") || stderr.Len() != 0 {
				t.Fatalf("scoped group help used ambient tree: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestE1InheritedCitySelectsScopedJSONSchema(t *testing.T) {
	tests := []struct {
		name       string
		schemaPath func(string) string
		args       func(string) []string
	}{
		{
			name: "after namespace separate value",
			schemaPath: func(city string) string {
				return filepath.Join(city, "commands", "hello", "schemas", "result.schema.json")
			},
			args: func(city string) []string {
				return []string{"backstage", "--city", city, "--json-schema", "result", "hello"}
			},
		},
		{
			name: "after intermediate equals value",
			schemaPath: func(city string) string {
				return filepath.Join(city, "commands", "repo", "sync", "schemas", "result.schema.json")
			},
			args: func(city string) []string {
				return []string{"backstage", "repo", "--city=" + city, "--json-schema", "result", "sync"}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cityA := setupPackExitCity(t)
			cityB := setupPackExitCity(t)
			for city, value := range map[string]string{cityA: "city-a", cityB: "city-b"} {
				schemaPath := test.schemaPath(city)
				if err := os.MkdirAll(filepath.Dir(schemaPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(schemaPath, []byte(`{"type":"string","const":"`+value+`"}`), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			oldWD, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(cityA); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(oldWD) })

			var stdout, stderr bytes.Buffer
			code := run(test.args(cityB), &stdout, &stderr)
			if code != 0 || !strings.Contains(stdout.String(), `"const":"city-b"`) || stderr.Len() != 0 {
				t.Fatalf("scoped schema used ambient tree: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestE1ScopeLookingArgsAfterLeafPassThrough(t *testing.T) {
	cityA := setupPackExitCity(t)
	cityB := setupPackExitCity(t)
	script := "#!/bin/sh\nprintf 'args:'\nfor arg in \"$@\"; do printf '<%s>' \"$arg\"; done\nprintf '\\n'\n"
	if err := os.WriteFile(filepath.Join(cityA, "commands", "hello", "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "city separate value",
			args: []string{"backstage", "hello", "--city", cityB},
			want: "args:<--city><" + cityB + ">\n",
		},
		{
			name: "rig equals value",
			args: []string{"backstage", "hello", "--rig=child-rig"},
			want: "args:<--rig=child-rig>\n",
		},
		{
			name: "empty city equals value",
			args: []string{"backstage", "hello", "--city="},
			want: "args:<--city=>\n",
		},
		{
			name: "empty rig separate value",
			args: []string{"backstage", "hello", "--rig", ""},
			want: "args:<--rig><>\n",
		},
		{
			name: "after terminator",
			args: []string{"backstage", "hello", "--", "--city", cityB},
			want: "args:<--><--city><" + cityB + ">\n",
		},
		{
			name: "valued true help and city after selected leaf",
			args: []string{"backstage", "hello", "--help=true", "--city", cityB},
			want: "args:<--help=true><--city><" + cityB + ">\n",
		},
		{
			name: "valued true help and city after terminator",
			args: []string{"backstage", "hello", "--", "--help=true", "--city", cityB},
			want: "args:<--><--help=true><--city><" + cityB + ">\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(test.args, &stdout, &stderr)
			if code != 0 || stdout.String() != test.want || stderr.Len() != 0 {
				t.Fatalf("scope-looking child args were consumed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestE1JSONSchemaSeparateRoleStillFindsPackCommand(t *testing.T) {
	city := setupPackExitCity(t)
	schemaDir := filepath.Join(city, "commands", "hello", "schemas")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(schemaDir, "result.schema.json"), []byte(`{"type":"object"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Chdir(outside); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	var stdout, stderr bytes.Buffer
	args := []string{"--city", city, "--json-schema", "result", "backstage", "hello"}
	code := run(args, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), `"type":"object"`) || stderr.Len() != 0 {
		t.Fatalf("separate json-schema role failed pack lookup: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestE1ExplicitCityResolutionFailureDropsEagerPackBinding(t *testing.T) {
	city := setupPackExitCity(t)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(city); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	var stdout, stderr bytes.Buffer
	missingCity := filepath.Join(t.TempDir(), "missing-city")
	code := run([]string{"--city", missingCity, "backstage", "hello"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "pack-before-exit") {
		t.Fatalf("explicit city resolution failure executed ambient pack: stdout=%q", stdout.String())
	}
}

func TestE1ExplicitCityWithoutBindingDropsEagerPackBinding(t *testing.T) {
	cityA := setupPackExitCity(t)
	cityB := setupPackExitCity(t)
	if err := os.WriteFile(filepath.Join(cityB, "pack.toml"), []byte("[pack]\nname = \"other-binding\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityB, "backstage", "hello"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "pack-before-exit") {
		t.Fatalf("explicit city without binding executed ambient pack: stdout=%q", stdout.String())
	}
}

func TestE1ExplicitRigOverridesEagerPackBinding(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_RIG", "")

	cityA := setupPackExitCity(t)
	cityB := setupPackExitCity(t)
	cityBScript := filepath.Join(cityB, "commands", "hello", "run.sh")
	if err := os.WriteFile(cityBScript, []byte("#!/bin/sh\nprintf 'rig-city-b\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "target-rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	registerRigBindingForResolution(t, os.Getenv("GC_HOME"), cityB, "rig-city-b", "target-rig", rigDir)
	if err := os.WriteFile(filepath.Join(cityB, "pack.toml"), []byte("[pack]\nname = \"backstage\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	var stdout, stderr bytes.Buffer
	code := run([]string{"--rig", "target-rig", "backstage", "hello"}, &stdout, &stderr)
	if code != 0 || stdout.String() != "rig-city-b\n" || stderr.Len() != 0 {
		t.Fatalf("explicit rig selected wrong pack: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestE1ExplicitRigResolutionFailureDropsEagerPackBinding(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_RIG", "")

	city := setupPackExitCity(t)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(city); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	var stdout, stderr bytes.Buffer
	code := run([]string{"--rig", "missing-rig", "backstage", "hello"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "pack-before-exit") {
		t.Fatalf("explicit rig resolution failure executed ambient pack: stdout=%q", stdout.String())
	}
}

func TestE1TerminatorLeavesPackSelectionToScopedFallback(t *testing.T) {
	cityA := setupPackExitCity(t)
	cityB := setupPackExitCity(t)
	cityBScript := filepath.Join(cityB, "commands", "hello", "run.sh")
	if err := os.WriteFile(cityBScript, []byte("#!/bin/sh\nprintf 'terminated-city-b\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityB, "--", "backstage", "hello"}, &stdout, &stderr)
	if code != 0 || stdout.String() != "terminated-city-b\n" || stderr.Len() != 0 {
		t.Fatalf("terminator fallback selected wrong pack: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestE1ExplicitScopePreservesBuiltInSameBinding(t *testing.T) {
	city := setupPackExitCity(t)
	root := &cobra.Command{Use: "gc"}
	builtin := &cobra.Command{Use: "backstage"}
	root.AddCommand(builtin)

	materializePackCommandTreeForArgs(root, []string{"--city", city, "backstage", "hello"}, io.Discard, io.Discard)
	if got := findSubcommand(root, "backstage"); got != builtin {
		t.Fatalf("explicit scope replaced built-in command: got=%p want=%p", got, builtin)
	}

	aliasRoot := &cobra.Command{Use: "gc"}
	aliasBuiltin := &cobra.Command{Use: "builtin", Aliases: []string{"backstage"}}
	aliasRoot.AddCommand(aliasBuiltin)
	materializePackCommandTreeForArgs(aliasRoot, []string{"backstage", "--city", city, "hello"}, io.Discard, io.Discard)
	if got := findSubcommand(aliasRoot, "builtin"); got != aliasBuiltin || len(aliasRoot.Commands()) != 1 {
		t.Fatalf("explicit scope replaced built-in alias: got=%p commands=%d want=%p", got, len(aliasRoot.Commands()), aliasBuiltin)
	}
}

func TestPackCommandTreeRequestLeadingFlagsAndTerminator(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	tests := []struct {
		name string
		args []string
		want packCommandTreePreparation
		ok   bool
	}{
		{
			name: "city and rig separate values",
			args: []string{"--city", "/city", "--rig", "rig-a", "backstage", "hello"},
			want: packCommandTreePreparation{binding: "backstage", city: "/city", rig: "rig-a", citySet: true, rigSet: true, scopeCount: 2},
			ok:   true,
		},
		{
			name: "city and rig equals values",
			args: []string{"--rig=rig-a", "--city=/city", "backstage", "hello"},
			want: packCommandTreePreparation{binding: "backstage", city: "/city", rig: "rig-a", citySet: true, rigSet: true, scopeCount: 2},
			ok:   true,
		},
		{
			name: "schema manifest separate role",
			args: []string{"--json-schema", "manifest", "backstage", "hello"},
			want: packCommandTreePreparation{binding: "backstage"},
			ok:   true,
		},
		{
			name: "schema result separate role",
			args: []string{"--json-schema", "result", "backstage", "hello"},
			want: packCommandTreePreparation{binding: "backstage"},
			ok:   true,
		},
		{
			name: "schema failure separate role",
			args: []string{"--json-schema", "failure", "backstage", "hello"},
			want: packCommandTreePreparation{binding: "backstage"},
			ok:   true,
		},
		{
			name: "schema equals role",
			args: []string{"--json-schema=result", "backstage", "hello"},
			want: packCommandTreePreparation{binding: "backstage"},
			ok:   true,
		},
		{
			name: "unknown separate schema value is command token",
			args: []string{"--json-schema", "backstage", "hello"},
			want: packCommandTreePreparation{binding: "backstage"},
			ok:   true,
		},
		{
			name: "terminator before binding",
			args: []string{"--city", "/city", "--", "backstage", "hello"},
			ok:   false,
		},
		{
			name: "terminator after separate schema role",
			args: []string{"--json-schema", "result", "--", "backstage", "hello"},
			ok:   false,
		},
		{
			name: "scope-looking token after terminator",
			args: []string{"backstage", "--", "--city", "/other-city", "hello"},
			want: packCommandTreePreparation{binding: "backstage"},
			ok:   true,
		},
		{
			name: "help and scope-looking token after terminator",
			args: []string{"backstage", "--", "--help", "--rig=/other-rig"},
			want: packCommandTreePreparation{binding: "backstage"},
			ok:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := packCommandTreeRequest(root, test.args)
			if ok != test.ok || got != test.want {
				t.Fatalf("packCommandTreeRequest(%q) = (%+v, %v), want (%+v, %v)", test.args, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestPackCommandTreeFixedPointFailsClosedOnScopeTopologyCycle(t *testing.T) {
	args := []string{"backstage", "--city", "city-b", "pivot", "--city", "city-a", "hello"}
	candidates := map[string]packCommandTreeCandidate{
		"city-a": testPackCommandTreeCandidate("pivot"),
		"city-b": testPackCommandTreeCandidate("hello"),
	}
	resolve := func(request packCommandTreePreparation) (packCommandTreeCandidate, bool) {
		candidate, ok := candidates[request.city]
		return candidate, ok
	}

	_, _, status := resolvePackCommandTreeFixedPoint(args, packCommandTreePreparation{
		binding: "backstage",
		city:    "city-a",
		citySet: true,
	}, resolve)
	if status != packCommandTreeResolutionAmbiguous {
		t.Fatalf("cycle resolution status = %v, want ambiguous/fail-closed", status)
	}
}

func TestPackCommandTreeFixedPointConvergesAcrossAllFiniteArgvScopeStates(t *testing.T) {
	const scopeStates = 12
	args := []string{"backstage"}
	for index := 1; index <= scopeStates; index++ {
		args = append(args, "--city", fmt.Sprintf("city-%d", index), fmt.Sprintf("step-%d", index))
	}
	resolve := func(request packCommandTreePreparation) (packCommandTreeCandidate, bool) {
		index := 0
		if request.city != "seed" {
			if _, err := fmt.Sscanf(request.city, "city-%d", &index); err != nil {
				return packCommandTreeCandidate{}, false
			}
		}
		command := make([]string, index+1)
		for commandIndex := range command {
			command[commandIndex] = fmt.Sprintf("step-%d", commandIndex+1)
		}
		return testPackCommandTreeCandidate(command...), true
	}

	_, request, status := resolvePackCommandTreeFixedPoint(args, packCommandTreePreparation{
		binding: "backstage",
		city:    "seed",
		citySet: true,
	}, resolve)
	if status != packCommandTreeResolutionStable || request.city != "city-12" || request.scopeCount != scopeStates {
		t.Fatalf("finite-chain resolution = (%+v, %v), want stable city-12 after %d scope states", request, status, scopeStates)
	}
}

func TestPackCommandTreeStableCandidatesRequireCompleteSnapshotAgreement(t *testing.T) {
	args := []string{"backstage", "--city", "target", "hello", "--city", "child", "payload"}
	first := testPackCommandTreeCandidate("hello")
	first.cityPath = "/same-city-path"
	first.cityName = "first-city-name"
	first.entries[0].RunScript = "/first/run.sh"

	second := testPackCommandTreeCandidate("hello")
	second.cityPath = first.cityPath
	second.cityName = "second-city-name"
	second.entries[0].RunScript = "/second/run.sh"
	second.entries = append(second.entries, config.DiscoveredCommand{
		BindingName: "backstage",
		Command:     []string{"repo", "sync"},
		RunScript:   "/second/repo-sync.sh",
	})

	targetResolutions := 0
	resolve := func(request packCommandTreePreparation) (packCommandTreeCandidate, bool) {
		switch request.city {
		case "child":
			return first, true
		case "target":
			targetResolutions++
			if targetResolutions == 1 {
				return first, true
			}
			return second, true
		default:
			return packCommandTreeCandidate{}, false
		}
	}

	_, _, status := resolvePackCommandTreeFromScopeSeeds(args, packCommandTreePreparation{
		binding:    "backstage",
		city:       "child",
		citySet:    true,
		scopeCount: 2,
	}, resolve)
	if targetResolutions < 2 {
		t.Fatalf("target candidate resolutions = %d, want at least 2 stable snapshots", targetResolutions)
	}
	if status != packCommandTreeResolutionAmbiguous {
		t.Fatalf("distinct stable candidate snapshots status = %v, want ambiguous/fail-closed", status)
	}
}

func TestPackCommandTreeCandidateSnapshotAgreementIsExact(t *testing.T) {
	withNilCommand := testPackCommandTreeCandidate("hello")
	withNilCommand.entries = append(withNilCommand.entries, config.DiscoveredCommand{
		BindingName: "backstage",
		Command:     nil,
	})
	withEmptyCommand := withNilCommand
	withEmptyCommand.entries = append([]config.DiscoveredCommand(nil), withNilCommand.entries...)
	withEmptyCommand.entries[1].Command = []string{}

	if !packCommandTreeCandidatesEqual(withNilCommand, withNilCommand) {
		t.Fatal("candidate snapshot does not agree with itself")
	}
	if packCommandTreeCandidatesEqual(withNilCommand, withEmptyCommand) {
		t.Fatal("candidate snapshots with nil and empty command slices agree; want exact whole-snapshot comparison")
	}
}

func TestPackCommandTreeCandidateSnapshotFieldCountRatchet(t *testing.T) {
	// The production comparator operates on the complete value. These counts
	// are an independent structural ratchet: adding a candidate or nested entry
	// field must fail this test and trigger an explicit snapshot-policy review.
	// Counts avoid duplicating the field-by-field comparison that this guard is
	// specifically intended to prevent from drifting in tandem.
	for _, test := range []struct {
		name string
		typ  reflect.Type
		want int
	}{
		{name: "candidate", typ: reflect.TypeOf(packCommandTreeCandidate{}), want: 3},
		{name: "discovered command", typ: reflect.TypeOf(config.DiscoveredCommand{}), want: 9},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.typ.NumField(); got != test.want {
				t.Fatalf("%s field count = %d, want %d; review whole-snapshot agreement before updating this ratchet", test.name, got, test.want)
			}
		})
	}
}

func testPackCommandTreeCandidate(command ...string) packCommandTreeCandidate {
	return packCommandTreeCandidate{
		entries: []config.DiscoveredCommand{{
			BindingName: "backstage",
			Command:     command,
		}},
		cityPath: "/unused",
		cityName: "unused",
	}
}

func TestTryPackCommandFallbackReturnsTypedNonzeroOutcome(t *testing.T) {
	cityPath := setupPackExitCity(t)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	var stdout, stderr bytes.Buffer
	got := tryPackCommandFallback([]string{"backstage", "hello"}, &stdout, &stderr)
	want := packCommandOutcome{handled: true, classification: packCommandClassification, exitCode: 42}
	if got != want {
		t.Fatalf("fallback outcome = %+v, want %+v", got, want)
	}
	if got, want := stdout.String(), "pack-before-exit\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestPackCommandExitReturnsThroughRun(t *testing.T) {
	cityPath := setupPackExitCity(t)

	for _, scenario := range []string{"eager", "lazy"} {
		t.Run(scenario, func(t *testing.T) {
			result := runPackCommandProcess(t, cityPath, scenario, "backstage", "hello")
			if result.exitCode != 42 {
				t.Fatalf("helper exit code = %d, want 42; stdout=%q stderr=%q", result.exitCode, result.stdout, result.stderr)
			}
			if got, want := result.stdout, "pack-before-exit\n"; got != want {
				t.Fatalf("helper stdout = %q, want %q", got, want)
			}
			if got := result.stderr; got != "" {
				t.Fatalf("helper stderr = %q, want empty", got)
			}
		})
	}
}

func TestPackCommandCobraHelpAndUnknownParity(t *testing.T) {
	cityPath := setupPackExitCity(t)
	tests := []struct {
		name           string
		args           []string
		wantExit       int
		wantStdoutText []string
		wantStderrText []string
	}{
		{
			name:           "binding help flag",
			args:           []string{"backstage", "--help"},
			wantStdoutText: []string{"Commands from the backstage import", "Available Commands:", "hello", "repo"},
		},
		{
			name:           "intermediate help flag",
			args:           []string{"backstage", "repo", "--help"},
			wantStdoutText: []string{"Usage:", "gc backstage repo", "sync"},
		},
		{
			name:           "persistent city flag before binding help",
			args:           []string{"--city", cityPath, "backstage", "repo", "--help"},
			wantStdoutText: []string{"Usage:", "gc backstage repo", "sync"},
		},
		{
			name:           "bare intermediate help",
			args:           []string{"backstage", "repo"},
			wantStdoutText: []string{"Usage:", "gc backstage repo", "sync"},
		},
		{
			name:           "known namespace miss",
			args:           []string{"backstage", "missing"},
			wantExit:       1,
			wantStderrText: []string{`unknown command "missing"`, "Usage:", "gc backstage", "hello", "repo"},
		},
		{
			name:           "known intermediate miss",
			args:           []string{"backstage", "repo", "missing"},
			wantExit:       1,
			wantStderrText: []string{`unknown command "missing"`, "Usage:", "gc backstage repo", "sync"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			eager := runPackCommandProcess(t, cityPath, "eager", test.args...)
			lazy := runPackCommandProcess(t, cityPath, "lazy", test.args...)
			if eager.exitCode != test.wantExit || lazy.exitCode != test.wantExit {
				t.Fatalf("exit codes = eager:%d lazy:%d, want %d; eager stderr=%q lazy stderr=%q", eager.exitCode, lazy.exitCode, test.wantExit, eager.stderr, lazy.stderr)
			}
			if eager.stdout != lazy.stdout {
				t.Fatalf("stdout differs between eager and lazy dispatch\neager:\n%s\nlazy:\n%s", eager.stdout, lazy.stdout)
			}
			if eager.stderr != lazy.stderr {
				t.Fatalf("stderr differs between eager and lazy dispatch\neager:\n%s\nlazy:\n%s", eager.stderr, lazy.stderr)
			}
			for _, want := range test.wantStdoutText {
				if !strings.Contains(eager.stdout, want) {
					t.Fatalf("stdout missing %q:\n%s", want, eager.stdout)
				}
			}
			for _, want := range test.wantStderrText {
				if !strings.Contains(eager.stderr, want) {
					t.Fatalf("stderr missing %q:\n%s", want, eager.stderr)
				}
			}
		})
	}
}

func TestPackCommandGroupMissRejectsUnknownSubcommands(t *testing.T) {
	cityPath := setupPackExitCity(t)
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "namespace",
			args: []string{"backstage", "missing"},
			want: []string{`unknown command "missing"`, "Usage:", "gc backstage", "hello", "repo"},
		},
		{
			name: "intermediate",
			args: []string{"backstage", "repo", "missing"},
			want: []string{`unknown command "missing"`, "Usage:", "gc backstage repo", "sync"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, scenario := range []string{"eager", "lazy"} {
				result := runPackCommandProcess(t, cityPath, scenario, test.args...)
				if result.exitCode != 1 || result.stdout != "" {
					t.Fatalf("%s group miss = %+v, want unknown-command failure on stderr", scenario, result)
				}
				for _, want := range test.want {
					if !strings.Contains(result.stderr, want) {
						t.Fatalf("%s group miss stderr missing %q:\n%s", scenario, want, result.stderr)
					}
				}
			}
		})
	}
}

func TestPackCommandProcessHelperIgnoresAmbientControlEnvironment(t *testing.T) {
	cityPath := setupPackExitCity(t)
	marker := filepath.Join(t.TempDir(), "ambient-marker")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPackCommandExitHelper$")
	cmd.Dir = cityPath
	cmd.Env = packCommandProcessEnv(
		"GC_TEST_PACK_EXIT_HELPER=1",
		"GC_TEST_PACK_EXIT_SCENARIO=eager",
		"GC_TEST_PACK_EXIT_AFTER_RUN="+marker,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ambient helper controls changed child behavior: %v; output=%q", err, output)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ambient helper controls created marker: %v", err)
	}
}

func TestPackCommandProcessEnvDisablesAmbientOTel(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "broken-resource-attributes")
	t.Setenv("OTEL_SDK_DISABLED", "false")

	count := 0
	for _, entry := range packCommandProcessEnv("OTEL_LOG_LEVEL=debug") {
		key, value, _ := strings.Cut(entry, "=")
		if len(key) < len("OTEL_") || !strings.EqualFold(key[:len("OTEL_")], "OTEL_") {
			continue
		}
		count++
		if key != "OTEL_SDK_DISABLED" || value != "true" {
			t.Fatalf("process environment retained ambient OTel entry %q", entry)
		}
	}
	if count != 1 {
		t.Fatalf("OTel process environment entries = %d, want only OTEL_SDK_DISABLED=true", count)
	}
}

func TestPackCommandOutcomeContainsOnlyLifecycleClassification(t *testing.T) {
	typ := reflect.TypeOf(packCommandOutcome{})
	want := []string{"handled", "classification", "exitCode"}
	if typ.NumField() != len(want) {
		t.Fatalf("packCommandOutcome fields = %d, want %d", typ.NumField(), len(want))
	}
	for i, name := range want {
		if got := typ.Field(i).Name; got != name {
			t.Fatalf("packCommandOutcome field %d = %q, want %q", i, got, name)
		}
	}
}

func TestResolveDiscoveredCommandFallbackPreclassifiesBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "pack", "commands", "fail")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "executed")
	scriptPath := filepath.Join(sourceDir, "run.sh")
	script := "#!/bin/sh\nprintf 'ran-pack\\n'\nprintf executed >\"$PACK_ACTION_MARKER\"\nexit 42\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PACK_ACTION_MARKER", marker)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "testcity"},
		PackCommands: []config.DiscoveredCommand{{
			BindingName: "private-binding",
			PackName:    "private-pack",
			Command:     []string{"private-command"},
			RunScript:   scriptPath,
			SourceDir:   sourceDir,
		}},
	}

	var stdout, stderr bytes.Buffer
	action := resolveDiscoveredCommandFallback([]string{"private-binding", "private-command"}, cfg, dir, &stdout, &stderr)
	wantResolved := packCommandOutcome{handled: true, classification: packCommandClassification, exitCode: 0}
	if action.outcome != wantResolved {
		t.Fatalf("resolved outcome = %+v, want %+v", action.outcome, wantResolved)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolution executed pack child: marker stat err = %v", err)
	}

	got := action.execute()
	wantExecuted := packCommandOutcome{handled: true, classification: packCommandClassification, exitCode: 42}
	if got != wantExecuted {
		t.Fatalf("executed outcome = %+v, want %+v", got, wantExecuted)
	}
	if gotCode := commandExitCode(got.err()); gotCode != 42 {
		t.Fatalf("outcome error exit code = %d, want 42", gotCode)
	}
	if got, want := stdout.String(), "ran-pack\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestResolveDiscoveredLeafActionClassifiesHelpWithoutExecutingChild(t *testing.T) {
	cmd := &cobra.Command{Use: "private-command", Long: "Private pack help."}
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	invoked := false

	action := resolveDiscoveredLeafAction(cmd, []string{"--help"}, func() int {
		invoked = true
		return 42
	})
	want := packCommandOutcome{handled: true, classification: packCommandClassification, exitCode: 0}
	if action.outcome != want {
		t.Fatalf("resolved help outcome = %+v, want %+v", action.outcome, want)
	}
	if got := action.execute(); got != want {
		t.Fatalf("executed help outcome = %+v, want %+v", got, want)
	}
	if invoked {
		t.Fatal("help action executed pack child")
	}
	if !strings.Contains(stdout.String(), "Private pack help.") {
		t.Fatalf("help stdout = %q, want long help", stdout.String())
	}
}

func TestResolveDiscoveredCommandFallbackReturnsTypedUnknown(t *testing.T) {
	action := resolveDiscoveredCommandFallback([]string{"private-binding", "missing"}, &config.City{}, t.TempDir(), io.Discard, io.Discard)
	want := packCommandOutcome{handled: false, classification: unknownCommandClassification, exitCode: 1}
	if action.outcome != want {
		t.Fatalf("resolved unknown outcome = %+v, want %+v", action.outcome, want)
	}
	if got := action.execute(); got != want {
		t.Fatalf("executed unknown outcome = %+v, want %+v", got, want)
	}
	if got := commandExitCode(want.err()); got != 1 {
		t.Fatalf("unknown outcome error exit code = %d, want 1", got)
	}
}

func TestResolveDiscoveredCommandFallbackSelectsNestedUnknown(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "testcity"},
		PackCommands: []config.DiscoveredCommand{{
			BindingName: "private-binding",
			PackName:    "private-pack",
			Command:     []string{"repo", "sync"},
		}},
	}
	var stdout, stderr bytes.Buffer
	action := resolveDiscoveredCommandFallback([]string{"private-binding", "repo", "missing"}, cfg, t.TempDir(), &stdout, &stderr)
	want := packCommandOutcome{handled: false, classification: unknownCommandClassification, exitCode: 1}
	if !action.selected {
		t.Fatal("known pack namespace miss was not selected by the pack dispatcher")
	}
	if action.outcome != want {
		t.Fatalf("resolved nested unknown outcome = %+v, want %+v", action.outcome, want)
	}
	if got := action.execute(); got != want {
		t.Fatalf("executed nested unknown outcome = %+v, want %+v", got, want)
	}
	if got := stdout.String(); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}
	for _, text := range []string{`gc: unknown command "missing"`, "Usage:", "gc private-binding repo"} {
		if !strings.Contains(stderr.String(), text) {
			t.Fatalf("stderr missing %q:\n%s", text, stderr.String())
		}
	}
}

func TestRunDiscoveredCommand_ProjectsCanonicalExternalDoltEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: city_canonical",
		"gc.endpoint_status: verified",
		"dolt.host: 127.0.0.1",
		"dolt.port: 4406",
		"dolt.user: city-user",
		"",
	}, "\n"))

	packDir := filepath.Join(dir, "pack")
	sourceDir := filepath.Join(packDir, "commands", "compact")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	script := `#!/bin/sh
echo "managed=$GC_DOLT_MANAGED_LOCAL"
echo "host=$GC_DOLT_HOST"
echo "port=$GC_DOLT_PORT"
echo "user=$GC_DOLT_USER"
echo "beadsport=$BEADS_DOLT_SERVER_PORT"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Stale ambient values from a parent session must lose to the city's
	// canonical endpoint, exactly as they do on the order-dispatch path.
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("GC_DOLT_MANAGED_LOCAL", "1")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")

	entry := config.DiscoveredCommand{
		BindingName: "dolt",
		PackName:    "dolt",
		Command:     []string{"compact"},
		RunScript:   scriptPath,
		PackDir:     packDir,
		SourceDir:   sourceDir,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", nil, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"managed=0",
		"host=127.0.0.1",
		"port=4406",
		"user=city-user",
		"beadsport=4406",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("stdout missing %q, got:\n%s", want, out)
		}
	}
}

func TestRunDiscoveredCommand_KeepsAmbientDoltEnvWithoutScopeConfig(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "pack")
	sourceDir := filepath.Join(packDir, "commands", "compact")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	script := `#!/bin/sh
echo "port=$GC_DOLT_PORT"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Without an authoritative scope config the operator-seeded ambient
	// value must pass through untouched.
	t.Setenv("GC_DOLT_PORT", "7777")

	entry := config.DiscoveredCommand{
		BindingName: "dolt",
		PackName:    "dolt",
		Command:     []string{"compact"},
		RunScript:   scriptPath,
		PackDir:     packDir,
		SourceDir:   sourceDir,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", nil, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "port=7777\n") {
		t.Fatalf("ambient GC_DOLT_PORT must pass through without scope config, got:\n%s", stdout.String())
	}
}

func TestRunDiscoveredCommand_AmbientBeadsDoltPasswordLosesToCredentialsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: city_canonical",
		"gc.endpoint_status: verified",
		"dolt.host: 127.0.0.1",
		"dolt.port: 4406",
		"dolt.user: city-user",
		"",
	}, "\n"))
	credentialsPath := filepath.Join(dir, "credentials")
	writeFile(t, credentialsPath, strings.Join([]string{
		"[127.0.0.1:4406]",
		"password = cred-file-pass",
		"",
	}, "\n"))

	packDir := filepath.Join(dir, "pack")
	sourceDir := filepath.Join(packDir, "commands", "compact")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	script := `#!/bin/sh
echo "password=$GC_DOLT_PASSWORD"
echo "beadspass=$BEADS_DOLT_PASSWORD"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// A stale BEADS_DOLT_PASSWORD mirrored into the parent session for a
	// different scope must not be treated as already-resolved auth for
	// the city's canonical endpoint; the endpoint's credentials-file
	// password must win. GC_DOLT_PASSWORD stays neutral so the operator
	// override (read via os.Getenv) does not shadow the lookup.
	t.Setenv("BEADS_DOLT_PASSWORD", "stale-cross-scope-pass")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	entry := config.DiscoveredCommand{
		BindingName: "dolt",
		PackName:    "dolt",
		Command:     []string{"compact"},
		RunScript:   scriptPath,
		PackDir:     packDir,
		SourceDir:   sourceDir,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", nil, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"password=cred-file-pass",
		"beadspass=cred-file-pass",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("stdout missing %q, got:\n%s", want, out)
		}
	}
}

func TestRunDiscoveredCommand_RemovesAmbientDoltEnvDeletedByProjection(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".beads", "config.yaml"), strings.Join([]string{
		"issue_prefix: ct",
		"gc.endpoint_origin: managed_city",
		"",
	}, "\n"))

	packDir := filepath.Join(dir, "pack")
	sourceDir := filepath.Join(packDir, "commands", "compact")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	// ${VAR+set} distinguishes unset from set-but-empty: the projection
	// must delete these keys, not blank them.
	script := `#!/bin/sh
echo "managed=$GC_DOLT_MANAGED_LOCAL"
echo "gchost=${GC_DOLT_HOST+set}"
echo "gcport=${GC_DOLT_PORT+set}"
echo "mirrorhost=${BEADS_DOLT_SERVER_HOST+set}"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// A managed-local canonical city deletes the host key (and its
	// BEADS mirror) instead of projecting a value; stale ambient
	// entries must be stripped from the child environment, not passed
	// through.
	t.Setenv("GC_DOLT_HOST", "stale.example")
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale.example")
	t.Setenv("BEADS_CREDENTIALS_FILE", filepath.Join(dir, "no-credentials"))

	entry := config.DiscoveredCommand{
		BindingName: "dolt",
		PackName:    "dolt",
		Command:     []string{"compact"},
		RunScript:   scriptPath,
		PackDir:     packDir,
		SourceDir:   sourceDir,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", nil, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "managed=1\n") {
		t.Fatalf("projection did not run (want managed=1), got:\n%s", out)
	}
	for _, want := range []string{
		"gchost=",
		"gcport=",
		"mirrorhost=",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("stale ambient key not removed (want %q line), got:\n%s", want, out)
		}
	}
}

func TestRunDiscoveredCommand_PrefersEntryPackDir(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "actual-pack")
	sourceDir := filepath.Join(dir, "somewhere", "else", "commands", "status")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(sourceDir, "run.sh")
	script := `#!/bin/sh
echo "packdir=$GC_PACK_DIR"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	entry := config.DiscoveredCommand{
		BindingName: "gs",
		PackName:    "mypack",
		Command:     []string{"status"},
		RunScript:   scriptPath,
		PackDir:     packDir,
		SourceDir:   sourceDir,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", nil, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "packdir="+packDir) {
		t.Fatalf("stdout missing composed pack dir, got:\n%s", stdout.String())
	}
}

func TestPackRootFromEntryDir_UsesLastTopLevelSegment(t *testing.T) {
	sourceDir := filepath.Join("/workspace", "commands", "mypk", "commands", "status")
	got := packRootFromEntryDir(sourceDir, "commands")
	want := filepath.Join("/workspace", "commands", "mypk")
	if got != want {
		t.Fatalf("packRootFromEntryDir(%q) = %q, want %q", sourceDir, got, want)
	}
}

func TestPackRootFromEntryDir_FallsBackToParent(t *testing.T) {
	sourceDir := filepath.Join("/workspace", "misc", "status")
	got := packRootFromEntryDir(sourceDir, "commands")
	want := filepath.Dir(sourceDir)
	if got != want {
		t.Fatalf("packRootFromEntryDir(%q) = %q, want %q", sourceDir, got, want)
	}
}

func TestRunDiscoveredCommand_ExitCodePropagates(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "commands", "fail")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 42\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	entry := config.DiscoveredCommand{
		PackName:  "mypack",
		Command:   []string{"fail"},
		RunScript: scriptPath,
		SourceDir: sourceDir,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", nil, strings.NewReader(""), &stdout, &stderr)
	if code != 42 {
		t.Fatalf("exit code = %d, want 42", code)
	}
}

func TestRunDiscoveredCommand_MissingScriptFails(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "commands", "missing")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}

	entry := config.DiscoveredCommand{
		BindingName: "gs",
		PackName:    "mypack",
		Command:     []string{"missing"},
		RunScript:   filepath.Join(sourceDir, "run.sh"),
		SourceDir:   sourceDir,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", nil, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no such file") {
		t.Fatalf("stderr missing missing-file message, got:\n%s", stderr.String())
	}
}

func TestRunDiscoveredCommand_NonExecutableFails(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "commands", "nonexec")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entry := config.DiscoveredCommand{
		BindingName: "gs",
		PackName:    "mypack",
		Command:     []string{"nonexec"},
		RunScript:   scriptPath,
		SourceDir:   sourceDir,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", nil, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Fatalf("stderr missing permission error, got:\n%s", stderr.String())
	}
}

func TestRunDiscoveredCommand_PassthroughArgs(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "commands", "echo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	script := `#!/bin/sh
for arg in "$@"; do
	echo "arg:$arg"
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	entry := config.DiscoveredCommand{
		PackName:  "mypack",
		Command:   []string{"echo"},
		RunScript: scriptPath,
		SourceDir: sourceDir,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscoveredCommand(entry, dir, "testcity", []string{"--verbose", "-n", "3", "hello world"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"arg:--verbose", "arg:-n", "arg:3", "arg:hello world"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q, got:\n%s", want, out)
		}
	}
}

func TestAddDiscoveredCommandsToRoot_HelpFlagShowsBuiltInHelp(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "pack")
	sourceDir := filepath.Join(packDir, "commands", "status")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho should-not-run\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	helpPath := filepath.Join(sourceDir, "help.md")
	if err := os.WriteFile(helpPath, []byte("Long discovered help.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := &cobra.Command{Use: "gc"}
	addDiscoveredCommandsToRoot(root, []config.DiscoveredCommand{{
		BindingName: "ops",
		PackName:    "ops",
		Command:     []string{"status"},
		Description: "Show status",
		RunScript:   scriptPath,
		HelpFile:    helpPath,
		SourceDir:   sourceDir,
		PackDir:     packDir,
	}}, dir, "testcity", &bytes.Buffer{}, &bytes.Buffer{}, true)

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"ops", "status", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr=%s", err, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Long discovered help.") {
		t.Fatalf("stdout missing built-in help text, got:\n%s", out)
	}
	if strings.Contains(out, "should-not-run") {
		t.Fatalf("help should not execute the discovered command, got:\n%s", out)
	}
}

func TestAddDiscoveredCommandsToRoot_CollisionProtection(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{Use: "start"})

	entries := []config.DiscoveredCommand{
		{
			BindingName: "start",
			Command:     []string{"status"},
			Description: "Show status",
		},
	}

	var stdout, stderr bytes.Buffer
	addDiscoveredCommandsToRoot(root, entries, "/city", "testcity", &stdout, &stderr, true)

	if !strings.Contains(stderr.String(), "shadows core command") {
		t.Fatalf("expected collision warning, got stderr: %q", stderr.String())
	}
	startCount := 0
	for _, c := range root.Commands() {
		if c.Name() == "start" {
			startCount++
		}
	}
	if startCount != 1 {
		t.Fatalf("got %d start commands, want 1", startCount)
	}
}

func TestTryDiscoveredCommandFallback_PrefersLongestMatch(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "pack", "commands", "repo")
	syncDir := filepath.Join(dir, "pack", "commands", "repo-sync")
	for _, p := range []string{repoDir, syncDir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "run.sh"), []byte("#!/bin/sh\necho repo:$*\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(syncDir, "run.sh"), []byte("#!/bin/sh\necho sync:$*\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "testcity"},
		PackCommands: []config.DiscoveredCommand{
			{
				BindingName: "gs",
				PackName:    "mypack",
				Command:     []string{"repo"},
				RunScript:   filepath.Join(repoDir, "run.sh"),
				SourceDir:   repoDir,
			},
			{
				BindingName: "gs",
				PackName:    "mypack",
				Command:     []string{"repo", "sync"},
				RunScript:   filepath.Join(syncDir, "run.sh"),
				SourceDir:   syncDir,
			},
		},
	}

	var stdout, stderr bytes.Buffer
	outcome := tryDiscoveredCommandFallback([]string{"gs", "repo", "sync", "now"}, cfg, dir, &stdout, &stderr)
	if !outcome.handled || outcome.classification != packCommandClassification || outcome.exitCode != 0 {
		t.Fatalf("tryDiscoveredCommandFallback outcome = %+v, want handled pack-command success", outcome)
	}
	if !strings.Contains(stdout.String(), "sync:now") {
		t.Fatalf("stdout missing longest-match execution, got:\n%s", stdout.String())
	}
}

func TestTryDiscoveredCommandFallback_HelpFlagShowsHelpWithoutRunning(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "pack", "commands", "status")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho should-not-run\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	helpPath := filepath.Join(sourceDir, "help.md")
	if err := os.WriteFile(helpPath, []byte("Status help from pack.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "testcity"},
		PackCommands: []config.DiscoveredCommand{{
			BindingName: "gs",
			PackName:    "mypack",
			Command:     []string{"status"},
			Description: "Show status",
			RunScript:   scriptPath,
			HelpFile:    helpPath,
			SourceDir:   sourceDir,
		}},
	}

	var stdout, stderr bytes.Buffer
	outcome := tryDiscoveredCommandFallback([]string{"gs", "status", "--help"}, cfg, dir, &stdout, &stderr)
	if !outcome.handled || outcome.classification != packCommandClassification || outcome.exitCode != 0 {
		t.Fatalf("tryDiscoveredCommandFallback outcome = %+v, want handled pack-command help", outcome)
	}
	out := stdout.String()
	if !strings.Contains(out, "Status help from pack.") {
		t.Fatalf("stdout missing discovered help, got:\n%s", out)
	}
	if strings.Contains(out, "should-not-run") {
		t.Fatalf("help should not execute the discovered command, got:\n%s", out)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestTryDiscoveredCommandFallback_HelpAfterTerminatorPassesThrough(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "pack", "commands", "status")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(sourceDir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nprintf 'args=%s %s\\n' \"$1\" \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	helpPath := filepath.Join(sourceDir, "help.md")
	if err := os.WriteFile(helpPath, []byte("Status help from pack.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "testcity"},
		PackCommands: []config.DiscoveredCommand{{
			BindingName: "gs",
			PackName:    "mypack",
			Command:     []string{"status"},
			Description: "Show status",
			RunScript:   scriptPath,
			HelpFile:    helpPath,
			SourceDir:   sourceDir,
		}},
	}

	var stdout, stderr bytes.Buffer
	outcome := tryDiscoveredCommandFallback([]string{"gs", "status", "--", "--help"}, cfg, dir, &stdout, &stderr)
	if !outcome.handled || outcome.classification != packCommandClassification || outcome.exitCode != 0 {
		t.Fatalf("tryDiscoveredCommandFallback outcome = %+v, want handled pack-command success", outcome)
	}
	out := stdout.String()
	if !strings.Contains(out, "args=-- --help") {
		t.Fatalf("stdout missing script passthrough args, got:\n%s", out)
	}
	if strings.Contains(out, "Status help from pack.") {
		t.Fatalf("terminator should pass --help through to the script, got:\n%s", out)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestTryDiscoveredCommandFallback_NamespaceHelpListsChildren(t *testing.T) {
	dir := t.TempDir()
	repoSyncDir := filepath.Join(dir, "pack", "commands", "repo", "sync")
	repoCleanDir := filepath.Join(dir, "pack", "commands", "repo", "clean")
	for _, p := range []string{repoSyncDir, repoCleanDir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "run.sh"), []byte("#!/bin/sh\necho should-not-run\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "testcity"},
		PackCommands: []config.DiscoveredCommand{
			{
				BindingName: "gs",
				PackName:    "mypack",
				Command:     []string{"repo", "sync"},
				Description: "Sync repo",
				RunScript:   filepath.Join(repoSyncDir, "run.sh"),
				SourceDir:   repoSyncDir,
			},
			{
				BindingName: "gs",
				PackName:    "mypack",
				Command:     []string{"repo", "clean"},
				Description: "Clean repo",
				RunScript:   filepath.Join(repoCleanDir, "run.sh"),
				SourceDir:   repoCleanDir,
			},
		},
	}

	var stdout, stderr bytes.Buffer
	outcome := tryDiscoveredCommandFallback([]string{"gs", "repo", "--help"}, cfg, dir, &stdout, &stderr)
	if !outcome.handled || outcome.classification != packCommandClassification || outcome.exitCode != 0 {
		t.Fatalf("tryDiscoveredCommandFallback outcome = %+v, want handled pack-command help", outcome)
	}
	out := stdout.String()
	for _, want := range []string{"Available commands for gs repo:", "clean", "Clean repo", "sync", "Sync repo"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "should-not-run") {
		t.Fatalf("namespace help should not execute a discovered command, got:\n%s", out)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestPrintDiscoveredCommandHelpFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry config.DiscoveredCommand
		want  string
	}{
		{
			name:  "description",
			entry: config.DiscoveredCommand{Command: []string{"status"}, Description: "Show status"},
			want:  "Show status\n",
		},
		{
			name:  "generic",
			entry: config.DiscoveredCommand{Command: []string{"repo", "sync"}},
			want:  "Pack command: repo sync\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			printDiscoveredCommandHelp(&stdout, tc.entry)
			if got := stdout.String(); got != tc.want {
				t.Fatalf("stdout = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPrintDiscoveredCommandListFiltersPrefixAndSkipsExactNamespace(t *testing.T) {
	entries := []config.DiscoveredCommand{
		{Command: []string{"repo"}, Description: "Repo namespace"},
		{Command: []string{"repo", "sync"}, Description: "Sync repo"},
		{Command: []string{"repo", "clean"}, Description: "Clean repo"},
		{Command: []string{"status"}, Description: "Show status"},
	}

	var stdout bytes.Buffer
	printDiscoveredCommandList(&stdout, "gs", []string{"repo"}, entries)

	out := stdout.String()
	for _, want := range []string{
		"Available commands for gs repo:",
		"sync",
		"Sync repo",
		"clean",
		"Clean repo",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q, got:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"Repo namespace", "status", "Show status"} {
		if strings.Contains(out, notWant) {
			t.Fatalf("stdout unexpectedly contained %q, got:\n%s", notWant, out)
		}
	}
}

func TestDiscoveredCommandPrefixHelpers(t *testing.T) {
	entries := []config.DiscoveredCommand{{Command: []string{"repo", "sync"}}}
	if !discoveredCommandPrefixExists(entries, []string{"repo"}) {
		t.Fatal("expected repo prefix to exist")
	}
	if discoveredCommandPrefixExists(entries, []string{"missing"}) {
		t.Fatal("missing prefix unexpectedly exists")
	}
	if commandHasPrefix([]string{"repo"}, []string{"repo", "sync"}) {
		t.Fatal("short command unexpectedly matched longer prefix")
	}
}

func TestAddDiscoveredCommandsToRoot_DedupsDuplicateLeaf(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	entries := []config.DiscoveredCommand{
		{BindingName: "gs", Command: []string{"status"}, Description: "first"},
		{BindingName: "gs", Command: []string{"status"}, Description: "second"},
	}

	addDiscoveredCommandsToRoot(root, entries, "/city", "testcity", os.Stdout, os.Stderr, true)
	gs := findSubcommand(root, "gs")
	if gs == nil {
		t.Fatal("missing binding namespace")
	}
	count := 0
	for _, c := range gs.Commands() {
		if c.Name() == "status" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("got %d status commands, want 1", count)
	}
}

func TestAddDiscoveredCommandsToRoot_CanSuppressCollisionWarnings(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{Use: "import"})

	entries := []config.DiscoveredCommand{
		{
			BindingName: "import",
			Command:     []string{"list"},
			Description: "Show imports",
		},
	}

	var stdout, stderr bytes.Buffer
	addDiscoveredCommandsToRoot(root, entries, "/city", "testcity", &stdout, &stderr, false)

	if stderr.Len() != 0 {
		t.Fatalf("expected suppressed collision warning, got stderr: %q", stderr.String())
	}
	importCount := 0
	for _, c := range root.Commands() {
		if c.Name() == "import" {
			importCount++
		}
	}
	if importCount != 1 {
		t.Fatalf("got %d import commands, want 1", importCount)
	}
}

// An imported command group must reject unknown subcommands with a non-zero
// exit ("unknown command"), matching native command groups, rather than
// printing help and exiting 0. Regression for #3966.
func TestDiscoveredNamespace_UnknownSubcommandErrors(t *testing.T) {
	newRoot := func() *cobra.Command {
		root := &cobra.Command{Use: "gc", SilenceUsage: true, SilenceErrors: true}
		entries := []config.DiscoveredCommand{
			{BindingName: "gs", Command: []string{"status"}, Description: "Show status"},
			{BindingName: "gs", Command: []string{"repo", "sync"}, Description: "Sync repo"},
		}
		addDiscoveredCommandsToRoot(root, entries, "/city", "testcity", os.Stdout, os.Stderr, true)
		root.SetOut(new(bytes.Buffer))
		root.SetErr(new(bytes.Buffer))
		return root
	}

	t.Run("unknown subcommand under namespace fails", func(t *testing.T) {
		root := newRoot()
		root.SetArgs([]string{"gs", "bogus"})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected error for unknown subcommand, got nil (would exit 0)")
		}
		if !strings.Contains(err.Error(), "unknown command") {
			t.Fatalf("error = %q, want it to mention \"unknown command\"", err.Error())
		}
	})

	t.Run("unknown subcommand under nested namespace fails", func(t *testing.T) {
		root := newRoot()
		root.SetArgs([]string{"gs", "repo", "bogus"})
		if err := root.Execute(); err == nil {
			t.Fatal("expected error for unknown nested subcommand, got nil (would exit 0)")
		}
	})

	t.Run("bare namespace still succeeds (prints help)", func(t *testing.T) {
		root := newRoot()
		root.SetArgs([]string{"gs"})
		if err := root.Execute(); err != nil {
			t.Fatalf("bare namespace should succeed with help, got error: %v", err)
		}
	})

	t.Run("bare nested namespace still succeeds (prints help)", func(t *testing.T) {
		root := newRoot()
		root.SetArgs([]string{"gs", "repo"})
		if err := root.Execute(); err != nil {
			t.Fatalf("bare nested namespace should succeed with help, got error: %v", err)
		}
	})
}
