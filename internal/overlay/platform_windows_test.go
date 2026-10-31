//go:build windows

package overlay

import (
	"bytes"
	"encoding/json"
	"io"
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

func bundledInstructionMarkdown(t *testing.T, provider string, rel ...string) []byte {
	t.Helper()
	parts := append([]string{bundledCopilotOverlay, "per-provider", provider}, rel...)
	data, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func bareGCInlineCommands(data []byte) []string {
	text := string(data)
	var out []string
	codeStart := -1
	for i := 0; i < len(text); {
		if text[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(text) && text[j] == '`' {
			j++
		}
		if j-i != 1 {
			i = j
			continue
		}
		if codeStart < 0 {
			codeStart = i + 1
		} else {
			code := text[codeStart:i]
			if code == "gc" || strings.HasPrefix(code, "gc ") {
				out = append(out, code)
			}
			codeStart = -1
		}
		i = j
	}
	return out
}

func TestAdaptSettingsForPlatformUsesGCExeInInstructionMarkdown(t *testing.T) {
	tests := []struct {
		name string
		rel  string
		src  []byte
		want string
	}{
		{
			name: "copilot",
			rel:  filepath.Join(".github", "copilot-instructions.md"),
			src:  bundledInstructionMarkdown(t, "copilot", ".github", "copilot-instructions.md"),
			want: "`gc.exe bd update <id> --claim`",
		},
		{
			name: "kiro",
			rel:  "AGENTS.md",
			src:  bundledInstructionMarkdown(t, "kiro", "AGENTS.md"),
			want: "`gc.exe nudge drain --inject`",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AdaptSettingsForPlatform(tt.rel, tt.src)
			if bare := bareGCInlineCommands(got); len(bare) > 0 {
				t.Fatalf("adapted instructions still contain bare gc inline commands: %q\n%s", bare, got)
			}
			if !bytes.Contains(got, []byte(tt.want)) {
				t.Fatalf("adapted instructions missing %s:\n%s", tt.want, got)
			}
		})
	}
}

func TestAdaptSettingsForPlatformInstructionMarkdownScope(t *testing.T) {
	src := []byte("Run `gc` then `gc bd close <id>`; keep `gc.exe prime`, `gc-hook`, `.gc`, and prose gc hook.\n")
	want := "Run `gc.exe` then `gc.exe bd close <id>`; keep `gc.exe prime`, `gc-hook`, `.gc`, and prose gc hook.\n"
	if got := string(AdaptSettingsForPlatform("AGENTS.md", src)); got != want {
		t.Fatalf("adapted instructions = %q, want %q", got, want)
	}
	if got := AdaptSettingsForPlatform("NOTES.md", src); !bytes.Equal(got, src) {
		t.Fatalf("non-instruction markdown changed:\n%s", got)
	}
}

func TestAdaptSettingsForPlatformBundledInstructionGolden(t *testing.T) {
	tests := []struct {
		name   string
		rel    string
		src    []byte
		golden string
	}{
		{
			name:   "copilot",
			rel:    filepath.Join(".github", "copilot-instructions.md"),
			src:    bundledInstructionMarkdown(t, "copilot", ".github", "copilot-instructions.md"),
			golden: "copilot-instructions-windows.golden.md",
		},
		{
			name:   "kiro",
			rel:    "AGENTS.md",
			src:    bundledInstructionMarkdown(t, "kiro", "AGENTS.md"),
			golden: "kiro-AGENTS-windows.golden.md",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join("testdata", tt.golden))
			if err != nil {
				t.Fatal(err)
			}
			if got := AdaptSettingsForPlatform(tt.rel, tt.src); !bytes.Equal(got, want) {
				t.Fatalf("adapted bundled instructions differ from golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

func TestAdaptSettingsForPlatformInstructionMarkdownCommonMark(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "short help",
			src:  "Run `gc -h` and `gc -help`.\n",
			want: "Run `gc.exe -h` and `gc.exe -help`.\n",
		},
		{
			name: "get-content path-like arguments",
			src:  "Read `gc README.md`, `gc .\\log.txt`, `gc C:\\logs\\x.txt`, `gc $file`, and `gc ~/x`.\n",
			want: "Read `gc README.md`, `gc .\\log.txt`, `gc C:\\logs\\x.txt`, `gc $file`, and `gc ~/x`.\n",
		},
		{
			name: "get-content quoted argument",
			src:  "Read `gc './log.txt'` and `gc \"README.md\"`.\n",
			want: "Read `gc './log.txt'` and `gc \"README.md\"`.\n",
		},
		{
			name: "get-content parameter",
			src:  "Read `gc -Path .\\x`, `gc -Raw README.md`, and `gc -t 5`.\n",
			want: "Read `gc -Path .\\x`, `gc -Raw README.md`, and `gc -t 5`.\n",
		},
		{
			name: "gas-city long flags",
			src:  "Run `gc --city X status` and `gc --city=C:\\X status`.\n",
			want: "Run `gc.exe --city X status` and `gc.exe --city=C:\\X status`.\n",
		},
		{
			name: "quoted semicolon",
			src:  "Run `Write-Output \"literal; gc status\"` and `Write-Output '(gc prime)'`.\n",
			want: "Run `Write-Output \"literal; gc status\"` and `Write-Output '(gc prime)'`.\n",
		},
		{
			name: "quoted command argument",
			src:  "Run `gc bd create --title 'example; gc prime'`.\n",
			want: "Run `gc.exe bd create --title 'example; gc prime'`.\n",
		},
		{
			name: "escaped semicolon",
			src:  "Run ``Write-Output `; gc status``.\n",
			want: "Run ``Write-Output `; gc status``.\n",
		},
		{
			name: "bare command before semicolon",
			src:  "Run `gc; gc status`.\n",
			want: "Run `gc.exe; gc.exe status`.\n",
		},
		{
			name: "double-backtick span",
			src:  "Run ``gc mail check --inject``.\n",
			want: "Run ``gc.exe mail check --inject``.\n",
		},
		{
			name: "literal single backticks in double-backtick span",
			src:  "Write `` `gc` `` literally.\n",
			want: "Write `` `gc` `` literally.\n",
		},
		{
			name: "tab separator",
			src:  "Run `gc\tprime`.\n",
			want: "Run `gc.exe\tprime`.\n",
		},
		{
			name: "multiline span",
			src:  "Run `gc\nmail check --inject`.\n",
			want: "Run `gc.exe\nmail check --inject`.\n",
		},
		{
			name: "padded span",
			src:  "Run ` gc prime `.\n",
			want: "Run ` gc.exe prime `.\n",
		},
		{
			name: "compound commands",
			src:  "Run `gc prime; gc mail check --inject | gc bd ready && gc hook || (gc bd show <id>)`.\n",
			want: "Run `gc.exe prime; gc.exe mail check --inject | gc.exe bd ready && gc.exe hook || (gc.exe bd show <id>)`.\n",
		},
		{
			name: "newline separated commands",
			src:  "Run `gc prime\ngc status`.\n",
			want: "Run `gc.exe prime\ngc.exe status`.\n",
		},
		{
			name: "leading newline span",
			src:  "Run `\ngc prime`.\n",
			want: "Run `\ngc.exe prime`.\n",
		},
		{
			name: "escaped backticks",
			src:  "Literal \\`gc prime\\` text, then `gc hook`.\n",
			want: "Literal \\`gc prime\\` text, then `gc.exe hook`.\n",
		},
		{
			name: "unmatched before blank line",
			src:  "A stray ` here.\n\nRun `gc prime`.\n",
			want: "A stray ` here.\n\nRun `gc.exe prime`.\n",
		},
		{
			name: "fenced command line",
			src:  "```powershell\ngc prime\nWrite-Output `gc hook`\n```\nRun `gc hook`.\n",
			want: "```powershell\ngc.exe prime\nWrite-Output `gc hook`\n```\nRun `gc.exe hook`.\n",
		},
		{
			name: "fenced prompts",
			src:  "```powershell\n$ gc prime\nPS> gc prime\nPS C:\\src> gc prime\n& gc status\n```\n",
			want: "```powershell\n$ gc.exe prime\nPS> gc.exe prime\nPS C:\\src> gc.exe prime\n& gc.exe status\n```\n",
		},
		{
			name: "empty shell fence",
			src:  "```\ngc prime\n```\n",
			want: "```\ngc.exe prime\n```\n",
		},
		{
			name: "fence inside blockquote",
			src:  "> ```powershell\n> gc prime\n> ```\n",
			want: "> ```powershell\n> gc.exe prime\n> ```\n",
		},
		{
			name: "blockquote fence optional marker spaces",
			src:  "> ```powershell\n>gc prime\n>```\n> Run `gc hook`.\n",
			want: "> ```powershell\n>gc.exe prime\n>```\n> Run `gc.exe hook`.\n",
		},
		{
			name: "indented opener unindented closer",
			src:  "  ```powershell\ngc prime\n```\nRun `gc hook`.\n",
			want: "  ```powershell\ngc.exe prime\n```\nRun `gc.exe hook`.\n",
		},
		{
			name: "top-level fence with list-looking payload",
			src:  "```text\n- ```\ngc prime\n```\n",
			want: "```text\n- ```\ngc.exe prime\n```\n",
		},
		{
			name: "top-level fence with quote-looking payload",
			src:  "```text\n> ```\ngc prime\n```\n",
			want: "```text\n> ```\ngc.exe prime\n```\n",
		},
		{
			name: "list-contained fence",
			src:  "- ```powershell\n  gc prime\n  ```\n",
			want: "- ```powershell\n  gc.exe prime\n  ```\n",
		},
		{
			name: "blockquote nested inside list fence",
			src:  "- > ```powershell\n  > gc prime\n  > ```\n",
			want: "- > ```powershell\n  > gc.exe prime\n  > ```\n",
		},
		{
			name: "blank line inside list-contained fence",
			src:  "- ```sh\n  gc prime\n\n  gc hook\n  ```\nRun `gc bd ready`.\n",
			want: "- ```sh\n  gc.exe prime\n\n  gc.exe hook\n  ```\nRun `gc.exe bd ready`.\n",
		},
		{
			name: "blank line ends list blockquote fence",
			src:  "- > ```text\n  > literal\n\n  > gc status\n",
			want: "- > ```text\n  > literal\n\n  > gc status\n",
		},
		{
			name: "list-contained fence ended by dedent",
			src:  "- ```powershell\n  gc prime\nRun `gc hook`.\n",
			want: "- ```powershell\n  gc.exe prime\nRun `gc.exe hook`.\n",
		},
		{
			name: "list-contained fence ended by blank line",
			src:  "- ```powershell\n  gc prime\n\nRun `gc hook`.\n",
			want: "- ```powershell\n  gc.exe prime\n\nRun `gc.exe hook`.\n",
		},
		{
			name: "toml fence",
			src:  "```toml\ngc = 1\n```\n",
			want: "```toml\ngc = 1\n```\n",
		},
		{
			name: "same-line triple-backtick span",
			src:  "```gc prime```\nThen `gc hook`.\n",
			want: "```gc.exe prime```\nThen `gc.exe hook`.\n",
		},
		{
			name: "stray backtick before fence",
			src:  "A stray ` here.\n```powershell\ngc prime\n```\nRun `gc hook`.\n",
			want: "A stray ` here.\n```powershell\ngc.exe prime\n```\nRun `gc.exe hook`.\n",
		},
		{
			name: "indented paragraph continuation",
			src:  "Run the command\n    `gc prime`.\n",
			want: "Run the command\n    `gc.exe prime`.\n",
		},
		{
			name: "nested list inline",
			src:  "- Outer item\n    - Run `gc prime`.\n",
			want: "- Outer item\n    - Run `gc.exe prime`.\n",
		},
		{
			name: "tab-indented item",
			src:  "\t- Run `gc prime`.\n",
			want: "\t- Run `gc.exe prime`.\n",
		},
		{
			name: "ordinary indented text is not a command line",
			src:  "    gc bd ready\n    Write-Output `gc hook`\n",
			want: "    gc bd ready\n    Write-Output `gc.exe hook`\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AdaptSettingsForPlatform("AGENTS.md", []byte(tt.src))
			if string(got) != tt.want {
				t.Fatalf("adapted markdown = %q, want %q", got, tt.want)
			}
			if twice := AdaptSettingsForPlatform("AGENTS.md", got); !bytes.Equal(twice, got) {
				t.Fatalf("adaptation is not idempotent: %q -> %q", got, twice)
			}
		})
	}
}

func TestAdaptSettingsForPlatformInstructionPathCaseAliases(t *testing.T) {
	src := []byte("Run `gc prime`.\n")
	want := []byte("Run `gc.exe prime`.\n")
	for _, rel := range []string{
		filepath.Join(".GitHub", "Copilot-Instructions.md"),
		"agents.md",
	} {
		if got := AdaptSettingsForPlatform(rel, src); !bytes.Equal(got, want) {
			t.Fatalf("%s adapted to %q, want %q", rel, got, want)
		}
	}
}

func writeProviderInstruction(t *testing.T, srcDir, provider, rel, content string) string {
	t.Helper()
	target := filepath.Join(srcDir, "per-provider", provider, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir provider instruction: %v", err)
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatalf("write provider instruction: %v", err)
	}
	return target
}

func TestCopyDirForProvidersAdaptsAndUpgradesInstructionMarkdown(t *testing.T) {
	t.Run("copilot", func(t *testing.T) {
		src := t.TempDir()
		dst := t.TempDir()
		legacy := "Run `gc prime`.\n"
		want := "Run `gc.exe prime`.\n"
		writeProviderInstruction(t, src, "copilot", ".github/copilot-instructions.md", legacy)

		if err := CopyDirForProviders(src, dst, []string{"copilot"}, io.Discard); err != nil {
			t.Fatalf("CopyDirForProviders fresh: %v", err)
		}
		target := filepath.Join(dst, ".github", "copilot-instructions.md")
		if got, err := os.ReadFile(target); err != nil || string(got) != want {
			t.Fatalf("fresh staged instructions = %q err=%v, want %q", got, err, want)
		}

		if err := os.WriteFile(target, []byte(legacy), 0o644); err != nil {
			t.Fatalf("seed legacy target: %v", err)
		}
		if err := CopyDirForProviders(src, dst, []string{"copilot"}, io.Discard); err != nil {
			t.Fatalf("CopyDirForProviders legacy: %v", err)
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != want {
			t.Fatalf("legacy staged instructions = %q err=%v, want %q", got, err, want)
		}

		custom := "Custom workspace instructions with `gc local`.\n"
		if err := os.WriteFile(target, []byte(custom), 0o644); err != nil {
			t.Fatalf("seed custom target: %v", err)
		}
		if err := CopyDirForProviders(src, dst, []string{"copilot"}, io.Discard); err != nil {
			t.Fatalf("CopyDirForProviders custom: %v", err)
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != want {
			t.Fatalf("custom instructions = %q err=%v, want overwritten with %q", got, err, want)
		}
	})

	t.Run("kiro", func(t *testing.T) {
		src := t.TempDir()
		dst := t.TempDir()
		legacy := "Run `gc hook`.\n"
		want := "Run `gc.exe hook`.\n"
		writeProviderInstruction(t, src, "kiro", "AGENTS.md", legacy)
		target := filepath.Join(dst, "AGENTS.md")
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("mkdir target: %v", err)
		}
		if err := os.WriteFile(target, []byte(legacy), 0o644); err != nil {
			t.Fatalf("seed legacy target: %v", err)
		}
		if err := CopyDirForProviders(src, dst, []string{"kiro"}, io.Discard); err != nil {
			t.Fatalf("CopyDirForProviders legacy: %v", err)
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != want {
			t.Fatalf("legacy Kiro instructions = %q err=%v, want %q", got, err, want)
		}

		custom := "Repository AGENTS instructions.\n"
		if err := os.WriteFile(target, []byte(custom), 0o644); err != nil {
			t.Fatalf("seed custom target: %v", err)
		}
		if err := CopyDirForProviders(src, dst, []string{"kiro"}, io.Discard); err != nil {
			t.Fatalf("CopyDirForProviders custom: %v", err)
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != custom {
			t.Fatalf("custom Kiro instructions = %q err=%v, want preserved %q", got, err, custom)
		}
	})
}

func TestCopyDirForProvidersInstructionLayeringAndRefresh(t *testing.T) {
	t.Run("later layer wins across calls", func(t *testing.T) {
		first := t.TempDir()
		second := t.TempDir()
		dst := t.TempDir()
		writeProviderInstruction(t, first, "copilot", ".github/copilot-instructions.md", "layer one: `gc prime`\n")
		writeProviderInstruction(t, second, "copilot", ".github/copilot-instructions.md", "layer two: `gc hook`\n")

		if err := CopyDirForProviders(first, dst, []string{"copilot"}, io.Discard); err != nil {
			t.Fatalf("first layer: %v", err)
		}
		if err := CopyDirForProviders(second, dst, []string{"copilot"}, io.Discard); err != nil {
			t.Fatalf("second layer: %v", err)
		}
		want := "layer two: `gc.exe hook`\n"
		if got, err := os.ReadFile(filepath.Join(dst, ".github", "copilot-instructions.md")); err != nil || string(got) != want {
			t.Fatalf("later layer result = %q err=%v, want %q", got, err, want)
		}
	})

	t.Run("provider layer wins over universal", func(t *testing.T) {
		src := t.TempDir()
		dst := t.TempDir()
		writeProviderInstruction(t, src, "copilot", ".github/copilot-instructions.md", "provider: `gc hook`\n")
		universal := filepath.Join(src, ".github", "copilot-instructions.md")
		if err := os.MkdirAll(filepath.Dir(universal), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(universal, []byte("universal: `gc prime`\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		if err := CopyDirForProviders(src, dst, []string{"copilot"}, io.Discard); err != nil {
			t.Fatalf("CopyDirForProviders: %v", err)
		}
		want := "provider: `gc.exe hook`\n"
		if got, err := os.ReadFile(filepath.Join(dst, ".github", "copilot-instructions.md")); err != nil || string(got) != want {
			t.Fatalf("provider layer result = %q err=%v, want %q", got, err, want)
		}
	})

	t.Run("source change refreshes adapted destination", func(t *testing.T) {
		src := t.TempDir()
		dst := t.TempDir()
		source := writeProviderInstruction(t, src, "copilot", ".github/copilot-instructions.md", "version one: `gc prime`\n")
		if err := CopyDirForProviders(src, dst, []string{"copilot"}, io.Discard); err != nil {
			t.Fatalf("initial copy: %v", err)
		}
		if err := os.WriteFile(source, []byte("version two: `gc mail check --inject`\n"), 0o644); err != nil {
			t.Fatalf("source update: %v", err)
		}
		if err := CopyDirForProviders(src, dst, []string{"copilot"}, io.Discard); err != nil {
			t.Fatalf("refresh copy: %v", err)
		}
		want := "version two: `gc.exe mail check --inject`\n"
		if got, err := os.ReadFile(filepath.Join(dst, ".github", "copilot-instructions.md")); err != nil || string(got) != want {
			t.Fatalf("refreshed result = %q err=%v, want %q", got, err, want)
		}
	})
}

func TestCopyDirForProvidersInstructionDestinationErrors(t *testing.T) {
	dst := t.TempDir()
	src := filepath.Join(t.TempDir(), "copilot-instructions.md")
	if err := os.WriteFile(src, []byte("Run `gc prime`.\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	target := filepath.Join(dst, ".github", "copilot-instructions.md")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target directory: %v", err)
	}
	err := copyOrMergeFile(src, target, filepath.Join(".github", "copilot-instructions.md"), false, false)
	if err == nil {
		t.Fatal("copyOrMergeFile succeeded with a directory at the destination file path")
	}
}

// Every command in the bundled Copilot hook file gets a powershell twin that
// runs the same gc subcommand, and its bash command is kept as it was.
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
