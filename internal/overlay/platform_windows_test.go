//go:build windows

package overlay

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// argvRecordEnv makes the test binary act as gc: it writes its arguments, one
// per line, to the file the variable names, then exits.
const argvRecordEnv = "GC_OVERLAY_TEST_ARGV_RECORD"

func TestMain(m *testing.M) {
	if out := os.Getenv(argvRecordEnv); out != "" {
		_ = os.WriteFile(out, []byte(strings.Join(os.Args[1:], "\n")), 0o644)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// bundledCopilotOverlay is the core pack's per-provider overlay directory.
var bundledCopilotOverlay = filepath.Join("..", "bootstrap", "packs", "core", "overlay")

type copilotHookEntry struct {
	Bash       string `json:"bash"`
	PowerShell string `json:"powershell"`
}

func copilotHookEntries(t *testing.T, data []byte) []copilotHookEntry {
	t.Helper()
	var doc struct {
		Hooks map[string][]copilotHookEntry `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing copilot hooks: %v\n%s", err, data)
	}
	var out []copilotHookEntry
	for _, entries := range doc.Hooks {
		out = append(out, entries...)
	}
	if len(out) == 0 {
		t.Fatalf("no copilot hook entries in:\n%s", data)
	}
	return out
}

func bundledCopilotHooks(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(bundledCopilotOverlay, "per-provider", "copilot", ".github", "hooks", "gascity.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAdaptSettingsForPlatformGivesEveryBundledCopilotHookAPowerShellTwin(t *testing.T) {
	src := bundledCopilotHooks(t)
	before := copilotHookEntries(t, src)
	after := copilotHookEntries(t, AdaptSettingsForPlatform(copilotHooksPath, src))
	if len(after) != len(before) {
		t.Fatalf("entry count changed: %d -> %d", len(before), len(after))
	}
	bash := map[string]bool{}
	for _, e := range before {
		bash[e.Bash] = true
	}
	for _, e := range after {
		if !bash[e.Bash] {
			t.Errorf("bash command changed or added: %q", e.Bash)
		}
		args, ok := strings.CutPrefix(e.Bash, posixGCHookPrefix)
		if !ok {
			t.Fatalf("bundled hook no longer starts with the gc prefix this adapter translates: %q", e.Bash)
		}
		if want := powerShellGCHookPrefix + args; e.PowerShell != want {
			t.Errorf("powershell twin for %q = %q, want %q", e.Bash, e.PowerShell, want)
		}
	}
}

// Only Copilot's hook file is adapted, an existing powershell field is kept,
// and a command outside the translatable shape is left without a twin.
func TestAdaptSettingsForPlatformLeavesEverythingElseAlone(t *testing.T) {
	src := bundledCopilotHooks(t)
	if got := AdaptSettingsForPlatform(filepath.Join(".codex", "hooks.json"), src); !bytes.Equal(got, src) {
		t.Errorf("a file other than Copilot's hook file was changed")
	}
	custom := []byte(`{"hooks":{"sessionStart":[{"bash":"echo hi","type":"command"},` +
		`{"bash":"export PATH=\"$PATH:$HOME/go/bin:$HOME/.local/bin\" \u0026\u0026 \"${GC_BIN:-gc}\" prime --hook","powershell":"mine","type":"command"},` +
		`{"bash":"export PATH=\"$PATH:$HOME/go/bin:$HOME/.local/bin\" \u0026\u0026 \"${GC_BIN:-gc}\" mail check $(whoami)","type":"command"}]},"version":1}`)
	for _, e := range copilotHookEntries(t, AdaptSettingsForPlatform(copilotHooksPath, custom)) {
		switch {
		case e.Bash == "echo hi" && e.PowerShell != "":
			t.Errorf("a non-gc command got a powershell twin: %q", e.PowerShell)
		case strings.HasSuffix(e.Bash, "prime --hook") && e.PowerShell != "mine":
			t.Errorf("an existing powershell field was replaced: %q", e.PowerShell)
		case strings.Contains(e.Bash, "$(whoami)") && e.PowerShell != "":
			t.Errorf("a command whose arguments PowerShell reads differently got a twin: %q", e.PowerShell)
		}
	}
	if got := AdaptSettingsForPlatform(copilotHooksPath, []byte("not json")); string(got) != "not json" {
		t.Errorf("an unparsable document was changed: %q", got)
	}
}

func TestPortableHookArgs(t *testing.T) {
	for args, want := range map[string]bool{
		"prime --hook":                         true,
		`handoff --auto "context cycle"`:       true,
		"hook run --timeout 15s -- mail check": true,
		"":                                     false,
		"mail check $(whoami)":                 false,
		"a,b":                                  false,
		`--auto="x y"`:                         false,
		`"unterminated`:                        false,
		`""`:                                   false,
		"a;b":                                  false,
		"a `b":                                 false,
		"a 'b'":                                false,
	} {
		if got := portableHookArgs(args); got != want {
			t.Errorf("portableHookArgs(%q) = %v, want %v", args, got, want)
		}
	}
}

// posixArgv splits the arguments of a translatable hook the way bash does.
// portableHookArgs admits only bare words and wholly double-quoted ones, so
// this is exact for every input the adapter translates.
func posixArgv(args string) []string {
	var out []string
	for i := 0; i < len(args); {
		switch {
		case args[i] == ' ':
			i++
		case args[i] == '"':
			end := strings.IndexByte(args[i+1:], '"')
			out = append(out, args[i+1:i+1+end])
			i += end + 2
		default:
			end := strings.IndexByte(args[i:], ' ')
			if end < 0 {
				end = len(args) - i
			}
			out = append(out, args[i:i+end])
			i += end
		}
	}
	return out
}

func powerShells(t *testing.T) []string {
	t.Helper()
	shells := []string{filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")}
	if pwsh, err := exec.LookPath("pwsh.exe"); err == nil {
		shells = append(shells, pwsh)
	}
	return shells
}

// runHook runs one hook command the way Copilot CLI runs a powershell hook and
// returns the argument list gc received.
func runHook(t *testing.T, shell, command string, env []string) []string {
	t.Helper()
	record := filepath.Join(t.TempDir(), "argv.txt")
	cmd := exec.Command(shell, "-NoProfile", "-NonInteractive", "-Command", command)
	cmd.Env = append(env, argvRecordEnv+"="+record)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s -Command %q: %v\n%s", filepath.Base(shell), command, err, out)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("%s -Command %q started no gc (%v); output:\n%s", filepath.Base(shell), command, err, out)
	}
	if len(data) == 0 {
		return nil
	}
	return strings.Split(string(data), "\n")
}

func withoutEnv(env []string, names ...string) []string {
	var out []string
	for _, kv := range env {
		keep := true
		for _, name := range names {
			if strings.HasPrefix(strings.ToUpper(kv), strings.ToUpper(name)+"=") {
				keep = false
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	return out
}

// Each powershell twin, run by Windows PowerShell 5.1 and by pwsh, starts the
// gc that GC_BIN names with exactly the argument list bash would pass it.
func TestCopilotPowerShellHooksRunGCWithTheBashArgv(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(withoutEnv(os.Environ(), "GC_BIN"), "GC_BIN="+self)
	for _, shell := range powerShells(t) {
		for _, e := range copilotHookEntries(t, AdaptSettingsForPlatform(copilotHooksPath, bundledCopilotHooks(t))) {
			want := posixArgv(strings.TrimPrefix(e.Bash, posixGCHookPrefix))
			if got := runHook(t, shell, e.PowerShell, env); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
				t.Errorf("%s ran %q with argv %q, want %q", filepath.Base(shell), e.PowerShell, got, want)
			}
		}
	}
}

// Without GC_BIN the twin runs gc.exe from PATH: never PowerShell's gc alias,
// which is Get-Content.
func TestCopilotPowerShellHookFallsBackToGCExeOnPath(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gc.exe"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	env := withoutEnv(os.Environ(), "GC_BIN", "PATH")
	env = append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	command := powerShellGCHookPrefix + "prime --hook"
	for _, shell := range powerShells(t) {
		if got := runHook(t, shell, command, env); strings.Join(got, " ") != "prime --hook" {
			t.Errorf("%s ran gc.exe from PATH with argv %q, want [prime --hook]", filepath.Base(shell), got)
		}
	}
}

// Staging the bundled Copilot overlay into a work directory writes the twins,
// both when it creates the hook file and when it merges into an existing file
// that has none; and restaging leaves the file byte-identical.
func TestCopilotOverlayStagingWritesPowerShellTwins(t *testing.T) {
	rel := filepath.Join(".github", "hooks", "gascity.json")
	assertTwins := func(t *testing.T, path string) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range copilotHookEntries(t, data) {
			if !strings.HasPrefix(e.PowerShell, powerShellGCHookPrefix) {
				t.Errorf("staged hook %q has no powershell twin (got %q)", e.Bash, e.PowerShell)
			}
		}
		return data
	}

	t.Run("create", func(t *testing.T) {
		dst := t.TempDir()
		if err := CopyDirForProviders(bundledCopilotOverlay, dst, []string{"copilot"}, os.Stderr); err != nil {
			t.Fatal(err)
		}
		first := assertTwins(t, filepath.Join(dst, rel))
		if err := CopyDirForProviders(bundledCopilotOverlay, dst, []string{"copilot"}, os.Stderr); err != nil {
			t.Fatal(err)
		}
		if second := assertTwins(t, filepath.Join(dst, rel)); !bytes.Equal(first, second) {
			t.Errorf("restaging changed the hook file:\nfirst:\n%s\nsecond:\n%s", first, second)
		}
	})

	t.Run("merge into a file without twins", func(t *testing.T) {
		dst := t.TempDir()
		path := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bundledCopilotHooks(t), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := CopyDirForProviders(bundledCopilotOverlay, dst, []string{"copilot"}, os.Stderr); err != nil {
			t.Fatal(err)
		}
		assertTwins(t, path)
	})
}
