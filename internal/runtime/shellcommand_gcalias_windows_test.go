//go:build windows

package runtime

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGasCityMarker opens what this test binary prints when it runs as gc.exe;
// see fakeGasCityOnPath.
const fakeGasCityMarker = "fake-gas-city:"

// Run as gc.exe, this test binary stands in for Gas City's program: it prints
// fakeGasCityMarker and its arguments, and exits before any test runs.
func init() {
	if len(os.Args) > 0 && strings.EqualFold(filepath.Base(os.Args[0]), "gc.exe") {
		fmt.Print(fakeGasCityMarker + strings.Join(os.Args[1:], " "))
		os.Exit(0)
	}
}

// fakeGasCityOnPath puts this test binary, as gc.exe, first on PATH for the
// rest of the test. The link names the running test binary's own image, which
// the temp dir's RemoveAll cannot unlink (access denied) and os.Remove can, so
// a cleanup registered after the temp dir's, and therefore run before it,
// removes the link first.
func fakeGasCityOnPath(t *testing.T) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "gc.exe")
	if err := os.Link(self, fake); err != nil {
		copyExecutable(t, self, fake)
	}
	t.Cleanup(func() { _ = os.Remove(fake) })
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func copyExecutable(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close() //nolint:errcheck
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close() //nolint:errcheck
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestShellCommandGcRunsGasCity pins gcAliasPrologue under every interpreter
// present: `gc` in a command runs the gc program on PATH, as it does under sh,
// where PowerShell alone runs its alias for Get-Content. Get-Content itself
// still works. A script that must lead cannot carry the prologue, and keeps
// the alias.
func TestShellCommandGcRunsGasCity(t *testing.T) {
	fakeGasCityOnPath(t)
	file := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(file, []byte("file-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, command, stdout string
	}{
		{name: "gc", command: "gc hook --claim --json", stdout: fakeGasCityMarker + "hook --claim --json"},
		{name: "Get-Content", command: "[Console]::Out.Write((Get-Content -LiteralPath " + psQuote(file) + " -Raw))", stdout: "file-content"},
		{name: "script that must lead", command: "param()\n[Console]::Out.Write((Get-Command gc).CommandType)", stdout: "Alias"},
	}
	eachPowerShell(t, func(t *testing.T, shell string) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				stdout, stderr, err := runShellWith(t, shell, tc.command)
				if err != nil || stdout != tc.stdout {
					t.Fatalf("stdout = %q, err = %v, stderr = %q; want %q", stdout, err, stderr, tc.stdout)
				}
			})
		}
	})
}

// TestShellCommandPromptGcRunsGasCity pins gcAliasPrologue in the script a
// prompt's statement hands its child, for every prompt and child present.
func TestShellCommandPromptGcRunsGasCity(t *testing.T) {
	fakeGasCityOnPath(t)
	const want = fakeGasCityMarker + "hook --claim --json"
	for _, prompt := range powerShells(t) {
		for _, child := range powerShells(t) {
			t.Run(filepath.Base(prompt)+"/"+filepath.Base(child), func(t *testing.T) {
				launch, err := shellCommandPromptWith(child, "gc hook --claim --json")
				if err != nil {
					t.Fatalf("shellCommandPromptWith: %v", err)
				}
				if code, stdout, stderr := runPrompt(t, prompt, launch.Statement); code != 0 || stdout != want {
					t.Fatalf("prompt exited %d with stdout %q, want 0 and %q; stderr=%q", code, stdout, want, stderr)
				}
			})
		}
	}
}

// TestGcAliasPrologueIsQuietWhereTheAliasIsGone pins the prologue's guard
// under every interpreter present. The command repeats the prologue, so the
// second one runs where the alias is already gone: it must neither write an
// error nor leave $? false.
func TestGcAliasPrologueIsQuietWhereTheAliasIsGone(t *testing.T) {
	eachPowerShell(t, func(t *testing.T, shell string) {
		stdout, stderr, err := runShellWith(t, shell, gcAliasPrologue+`[Console]::Out.Write("$? $($Error.Count)")`)
		if err != nil || stdout != "True 0" || stderr != "" {
			t.Fatalf("stdout = %q, err = %v, stderr = %q; want \"True 0\", no error and nothing on stderr", stdout, err, stderr)
		}
	})
}

// TestPromptScriptRemovesTheGcAliasFirst pins the script a prompt's statement
// hands its child: gcAliasPrologue, the command and shellExitEpilogue, with no
// prologue for a script whose first statement must come first.
func TestPromptScriptRemovesTheGcAliasFirst(t *testing.T) {
	if got, want := promptScript("exit 0"), gcAliasPrologue+"exit 0"+shellExitEpilogue; got != want {
		t.Errorf("promptScript(%q) = %q, want %q", "exit 0", got, want)
	}
	const leads = "param($x = 'p')\n$x"
	if got, want := promptScript(leads), leads+shellExitEpilogue; got != want {
		t.Errorf("promptScript(%q) = %q, want %q", leads, got, want)
	}
	const attributedParam = "[OutputType([string])]\nparam($x = 'p')\n$x"
	if got, want := promptScript(attributedParam), attributedParam+shellExitEpilogue; got != want {
		t.Errorf("promptScript(%q) = %q, want %q", attributedParam, got, want)
	}
	const attributedBacktickParam = "[OutputType('`')]\nparam($x = 'p')\n$x"
	if got, want := promptScript(attributedBacktickParam), attributedBacktickParam+shellExitEpilogue; got != want {
		t.Errorf("promptScript(%q) = %q, want %q", attributedBacktickParam, got, want)
	}
}
