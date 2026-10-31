//go:build windows

package runtime

import (
	"os"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestShellCommandUnavailableRefusesPosixShellPrograms pins which commands
// Windows refuses to start: those whose program is a POSIX shell script or a
// POSIX shell, however it is quoted or called, and nothing else.
func TestShellCommandUnavailableRefusesPosixShellPrograms(t *testing.T) {
	refused := []string{
		"$PACK_DIR/assets/scripts/gate-sweep.sh",
		`  & "C:\dir with space\sweep.SH" --flag`,
		`& 'C:\tools\x.sh'`,
		`&'C:\tools\x.sh' arg`,
		`& $env:PACK_DIR\scripts\x.sh`,
		`. bash -c harmless`,
		`& ('sh') -c harmless`,
		`s` + "`" + `h -c harmless`,
		"# ordinary order comment\nsh -c harmless",
		`& 'C:\tools\O''Brien\sh.exe' -c harmless`,
		"./local.sh;gc x",
		"cd .; bash -c 'echo hi'",
		"$env:FOO='1'; bash -c 'echo hi'",
		"$x = bash -c harmless",
		"$x += bash -c harmless",
		"return bash -c harmless",
		"throw bash",
		"gc beads health --quiet | bash -c 'echo hi'",
		"gc beads health --quiet && bash -c 'echo hi'",
		"gc beads health --quiet || bash -c 'echo hi'",
		"bash -c 'echo hi'",
		`ba` + "`" + `sh -c 'echo hi'`,
		"sh script",
		`& "C:\Program Files\Git\bin\bash.exe" -lc true`,
		"ZSH.EXE",
		"dash|x",
		"(bash -c harmless)",
		"Write-Output (bash -c harmless)",
		"Write-Output $(sh -c harmless)",
		`test -n "$(bash -c 'echo x')"`,
		`bd list --json > "$(sh -c mktemp)"`,
		"1 | ForEach-Object { bash -c harmless }",
		"1 | % { sh -c harmless }",
		"if (bash -c harmless) { 'yes' }",
		"while (sh -c harmless) { break }",
		"@(bash -c harmless)",
		`@(& 'C:\tools\static.sh')`,
		"[void](bash -c harmless)",
		".{bash -c harmless}",
		".('bash') -c harmless",
		"param()\nend { bash -c harmless }",
		"begin { sh -c harmless }",
		"process { bash -c harmless }",
		"& \u2018bash\u2019 -c harmless",
		"& \u201cbash\u201d -c harmless",
		"function f { bash -c harmless }; f",
	}
	for _, command := range refused {
		if reason := ShellCommandUnavailable(command); !strings.Contains(reason, "POSIX shell") {
			t.Errorf("ShellCommandUnavailable(%q) = %q, want a refusal naming a POSIX shell", command, reason)
		}
	}
	allowed := []string{
		"gc beads health --quiet",
		"gc.exe order sweep-tracking --stale-after 10m --quiet --confirm",
		"pwsh -NoProfile -File sweep.ps1",
		`& "$env:PACK_DIR\scripts\sweep.ps1"`,
		"python3 worker.py x.sh",
		"shellcheck x.sh",
		`C:\tools\bash-helper.exe`,
		"report.sh.txt",
		`"bash"`,
		`"C:\Program Files\Git\bin\bash.exe" -lc true`,
		`'report.sh'`,
		`'C:\not-a-command\script.sh'`,
		"Start-Process bash",
		"Invoke-Expression 'bash -c harmless'",
		"$p='bash'; & $p -c harmless",
		"{ bash -c harmless }",
		"switch ($x) { sh { 'a' } default { 'b' } }",
		"Write-Output `\n bash",
		"@{ sh = 1 }",
		"Write-Output @'\nplain text\nbash\n'@",
		`"unterminated`,
		"",
		"   ",
	}
	for _, command := range allowed {
		if reason := ShellCommandUnavailable(command); reason != "" {
			t.Errorf("ShellCommandUnavailable(%q) = %q, want \"\"", command, reason)
		}
	}
}

// TestBundledPowerShellCommandFormsOnWindows pins the executable command
// strings bundled in packs and examples that use the Windows shell gate. The
// POSIX shell-script orders stay refused on Windows; portable PowerShell/native
// forms must remain runnable.
func TestBundledPowerShellCommandFormsOnWindows(t *testing.T) {
	commands := bundledPowerShellCommands(t)
	if len(commands) == 0 {
		t.Fatal("no bundled shell command forms found")
	}
	refused := 0
	for _, command := range commands {
		reason := ShellCommandUnavailable(command.command)
		if command.wantRefused {
			refused++
			if reason == "" {
				t.Errorf("%s %s command %q was accepted; want POSIX shell refusal", command.path, command.field, command.command)
			}
			continue
		}
		if reason != "" {
			t.Errorf("%s %s command %q was refused: %s", command.path, command.field, command.command, reason)
		}
	}
	if refused < 12 {
		t.Fatalf("only %d bundled POSIX shell commands found; want at least the 12 core script orders", refused)
	}
}

type bundledPowerShellCommand struct {
	path        string
	field       string
	command     string
	wantRefused bool
}

func bundledPowerShellCommands(t *testing.T) []bundledPowerShellCommand {
	t.Helper()
	_, file, _, ok := stdruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	var commands []bundledPowerShellCommand
	for _, rel := range []string{`internal\bootstrap\packs`, "examples"} {
		base := filepath.Join(root, rel)
		err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".toml" {
				return nil
			}
			var data map[string]any
			if _, err := toml.DecodeFile(path, &data); err != nil {
				return err
			}
			relPath, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			collectBundledPowerShellCommands(relPath, data, &commands)
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", base, err)
		}
	}
	return commands
}

func collectBundledPowerShellCommands(path string, value any, commands *[]bundledPowerShellCommand) {
	switch typed := value.(type) {
	case map[string]any:
		if hooks, ok := typed["hooks"]; ok {
			collectBundledHookCommands(path, hooks, commands)
		}
		for key, child := range typed {
			switch key {
			case "exec", "check", "work_query", "scale_check", "on_boot", "on_death":
				collectBundledCommandValue(path, key, child, commands)
			case "hooks":
			default:
				collectBundledPowerShellCommands(path, child, commands)
			}
		}

	case []any:
		for _, child := range typed {
			collectBundledPowerShellCommands(path, child, commands)
		}
	}
}

func collectBundledHookCommands(path string, value any, commands *[]bundledPowerShellCommand) {
	switch typed := value.(type) {
	case []map[string]any:
		for _, hook := range typed {
			collectBundledCommandValue(path, "hooks.command", hook["command"], commands)
		}

	case []any:
		for _, entry := range typed {
			if hook, ok := entry.(map[string]any); ok {
				collectBundledCommandValue(path, "hooks.command", hook["command"], commands)
			}
		}

	case map[string]any:
		collectBundledCommandValue(path, "hooks.command", typed["command"], commands)
	}
}

func collectBundledCommandValue(path, field string, value any, commands *[]bundledPowerShellCommand) {
	switch typed := value.(type) {
	case string:
		*commands = append(*commands, bundledPowerShellCommand{
			path:        path,
			field:       field,
			command:     typed,
			wantRefused: bundledPowerShellCommandRefused(typed),
		})

	case []string:
		for _, command := range typed {
			collectBundledCommandValue(path, field, command, commands)
		}

	case []any:
		for _, command := range typed {
			collectBundledCommandValue(path, field, command, commands)
		}
	}
}

func bundledPowerShellCommandRefused(command string) bool {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return false
	}
	base := strings.ToLower(filepath.Base(strings.Trim(fields[0], `"'`)))
	return strings.HasSuffix(base, ".sh") || posixShells[strings.TrimSuffix(base, ".exe")]
}
