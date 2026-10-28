//go:build windows

package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// psQuote renders s as a PowerShell single-quoted literal.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// powerShells lists each distinct interpreter a command could run under on
// this machine: whichever of pwsh and Windows PowerShell PATH resolves, and the
// in-box Windows PowerShell. pwsh runs on .NET and Windows PowerShell on .NET
// Framework, and they differ in how they read their command line and write
// their streams, so a result measured under one says nothing about the other.
func powerShells(t *testing.T) []string {
	t.Helper()
	var shells []string
	add := func(path string) {
		for _, known := range shells {
			if strings.EqualFold(filepath.Clean(known), filepath.Clean(path)) {
				return
			}
		}
		shells = append(shells, path)
	}
	for _, name := range powerShellNames {
		if path, err := exec.LookPath(name); err == nil {
			add(path)
		}
	}
	inBox := inBoxPowerShell()
	if info, err := os.Stat(inBox); err == nil && !info.IsDir() {
		add(inBox)
	}
	if len(shells) == 0 {
		t.Fatal("no PowerShell interpreter found")
	}
	return shells
}

// eachPowerShell runs fn as a subtest under every interpreter powerShells
// finds.
func eachPowerShell(t *testing.T, fn func(t *testing.T, shell string)) {
	t.Helper()
	for _, shell := range powerShells(t) {
		t.Run(filepath.Base(shell), func(t *testing.T) {
			fn(t, shell)
		})
	}
}

// runShell runs command through the Windows shell path and returns its
// captured streams. dir and env are applied exactly as a production caller
// applies them; an empty dir leaves the process working directory alone.
func runShell(t *testing.T, dir string, env []string, command string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd, err := ShellCommandContext(ctx, command)
	if err != nil {
		t.Fatalf("ShellCommandContext(%q): %v", command, err)
	}
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	return stdout.String(), stderr.String(), runErr
}

// runShellWith runs command with the argv production builds, under shell
// rather than the interpreter findPowerShell prefers.
func runShellWith(t *testing.T, shell, command string) (string, string, error) {
	t.Helper()
	return runShellWithIn(t, shell, "", command)
}

// runShellWithIn is runShellWith in working directory dir; an empty dir leaves
// the process working directory alone.
func runShellWithIn(t *testing.T, shell, dir, command string) (string, string, error) {
	t.Helper()
	args, err := powerShellArgs(shell, command)
	if err != nil {
		t.Fatalf("powerShellArgs(%q): %v", command, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	return stdout.String(), stderr.String(), runErr
}

// TestShellCommandArgvCarriesTheScriptVerbatim pins the invocation shape: the
// script is the last argument, verbatim, directly after -Command, and the
// profile and interactive switches are always present. -EncodedCommand must
// not return: it makes the interpreter write its error records as CLIXML (see
// TestShellCommandErrorRecordsReachStderrAsText).
func TestShellCommandArgvCarriesTheScriptVerbatim(t *testing.T) {
	const command = `Write-Output "a b" & $x ` + "`t"
	cmd, err := ShellCommandContext(context.Background(), command)
	if err != nil {
		t.Fatalf("ShellCommandContext: %v", err)
	}
	args := cmd.Args[1:]
	for _, want := range []string{"-NoProfile", "-NonInteractive"} {
		if !slices.Contains(args, want) {
			t.Errorf("argv %q is missing %s", args, want)
		}
	}
	if slices.Contains(args, "-EncodedCommand") {
		t.Errorf("argv %q passes the script encoded", args)
	}
	if len(args) < 2 || args[len(args)-2] != "-Command" {
		t.Fatalf("argv %q does not end in -Command and the script; PowerShell takes everything after -Command as the command, so it must come last", args)
	}
	script := args[len(args)-1]
	prologue, ok := strings.CutSuffix(script, command+shellExitEpilogue)
	if !ok {
		t.Fatalf("script does not end in the command verbatim and the exit epilogue:\n got %q\nwant suffix %q", script, command+shellExitEpilogue)
	}
	if want := scriptPrologue(cmd.Path, command); prologue != want {
		t.Errorf("script opens with %q, want %q for %s", prologue, want, cmd.Path)
	}
}

// TestScriptPrologueByInterpreterAndScript pins what each script opens with:
// gcAliasPrologue, after plainTextPrologue for pwsh, and nothing for a script
// whose first statement must come first.
func TestScriptPrologueByInterpreterAndScript(t *testing.T) {
	const pwsh = `C:\Program Files\PowerShell\7\pwsh.exe`
	const windowsPowerShell = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	const pwshPrologue = plainTextPrologue + gcAliasPrologue
	cases := []struct {
		shell, command string
		want           string
	}{
		{shell: pwsh, command: "Write-Error 'x'", want: pwshPrologue},
		{shell: `C:\tools\PWSH.EXE`, command: "[Console]::Out.Write('x')", want: pwshPrologue},
		{shell: windowsPowerShell, command: "Write-Error 'x'", want: gcAliasPrologue},
		{shell: pwsh, command: "using namespace System.Text\n[StringBuilder]::new()", want: ""},
		{shell: pwsh, command: "# a comment\r\n<# and a\nblock #>\n  USING module Foo", want: ""},
		{shell: pwsh, command: "param($x = 'p')\n$x", want: ""},
		{shell: pwsh, command: "[CmdletBinding()]\nparam()\n'x'", want: ""},
		{shell: pwsh, command: "[OutputType([string])]\nparam()\n'x'", want: ""},
		{shell: pwsh, command: "[OutputType('`')]\nparam()\n'x'", want: ""},
		{shell: pwsh, command: "[OutputType([string])]\n[CmdletBinding()]\nparam()\n'x'", want: ""},
		{shell: pwsh, command: "[OutputType([string])]\n# comment\n[CmdletBinding()]\nparam()\n'x'", want: ""},
		{shell: windowsPowerShell, command: "param($x = 'p')\n$x", want: ""},
		{shell: windowsPowerShell, command: "[OutputType([string])]\nparam()\n'x'", want: ""},
		{shell: windowsPowerShell, command: "[OutputType('`')]\nparam()\n'x'", want: ""},
		{shell: pwsh, command: "# using namespace is only a comment here\n'x'", want: pwshPrologue},
		{shell: pwsh, command: "$parameters = 1", want: pwshPrologue},
		{shell: pwsh, command: "[Console]::Out.Write('x')", want: pwshPrologue},
	}
	for _, tc := range cases {
		if got := scriptPrologue(tc.shell, tc.command); got != tc.want {
			t.Errorf("scriptPrologue(%s, %q) = %q, want %q", filepath.Base(tc.shell), tc.command, got, tc.want)
		}
	}
}

// TestAttributedParamScriptMustLead pins that every prologue stays behind
// script attributes that apply to a leading param block.
func TestAttributedParamScriptMustLead(t *testing.T) {
	scripts := []string{
		"[OutputType([string])]\nparam([string]$Text = 'ATTR-PARAM-OK')\n[Console]::Out.Write($Text)",
		"[OutputType('`')]\nparam([string]$Text = 'ATTR-PARAM-OK')\n[Console]::Out.Write($Text)",
	}
	eachPowerShell(t, func(t *testing.T, shell string) {
		for _, script := range scripts {
			t.Run(filepath.Base(shell)+"/"+script[:12], func(t *testing.T) {
				stdout, stderr, err := runShellWith(t, shell, script)
				if err != nil || stdout != "ATTR-PARAM-OK" || stderr != "" {
					t.Fatalf("stdout = %q, err = %v, stderr = %q; want attributed param script to run", stdout, err, stderr)
				}
			})
		}
	})
}

// TestShellCommandStreamsCarryNoEscapeSequences pins that captured output is
// plain text under every interpreter present, and where each PowerShell
// stream lands. pwsh colored its error, warning, verbose and debug records even
// into a pipe. Warning, verbose and debug records go to stdout under -Command,
// as they did not under -EncodedCommand; only errors reach stderr.
//
// The markers are computed, because Windows PowerShell repeats the whole
// script in an error record, and a literal there would be found in stderr.
func TestShellCommandStreamsCarryNoEscapeSequences(t *testing.T) {
	const script = "Write-Warning ('gc-' + 'warning')\n" +
		"Write-Verbose ('gc-' + 'verbose') -Verbose\n" +
		"$DebugPreference = 'Continue'\nWrite-Debug ('gc-' + 'debug')\n" +
		"Write-Error ('gc-' + 'error')"
	eachPowerShell(t, func(t *testing.T, shell string) {
		stdout, stderr, err := runShellWith(t, shell, script)
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("error = %v (%T), want the Write-Error to fail the command", err, err)
		}
		if strings.ContainsRune(stdout, '\x1b') || strings.ContainsRune(stderr, '\x1b') {
			t.Fatalf("captured output carries escape sequences:\nstdout=%q\nstderr=%q", stdout, stderr)
		}
		for _, want := range []string{"WARNING: gc-warning", "VERBOSE: gc-verbose", "DEBUG: gc-debug"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("stdout = %q, want it to carry %q", stdout, want)
			}
		}
		if !strings.Contains(stderr, "gc-error") {
			t.Errorf("stderr = %q, want the error record", stderr)
		}
		if strings.Contains(stderr, "gc-warning") || strings.Contains(stderr, "gc-verbose") || strings.Contains(stderr, "gc-debug") {
			t.Errorf("stderr = %q carries a record this test expects on stdout; re-measure where the streams go", stderr)
		}
	})
}

// TestShellCommandScriptsThatMustLeadStillRun pins why scriptPrologue skips a
// script opening with using or param: after a prologue, using is a parse error
// in both interpreters, and param runs as an unknown command (both measured).
func TestShellCommandScriptsThatMustLeadStillRun(t *testing.T) {
	cases := []struct {
		name, command, stdout string
	}{
		{name: "using", command: "using namespace System.Text\n[Console]::Out.Write([StringBuilder]::new('led').ToString())", stdout: "led"},
		{name: "param", command: "param($x = 'led')\n[Console]::Out.Write($x)", stdout: "led"},
	}
	eachPowerShell(t, func(t *testing.T, shell string) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				stdout, stderr, err := runShellWith(t, shell, tc.command)
				// stderr matters as much as the exit code: after the prologue a
				// param block is run as an unknown command named param, which
				// fails loudly but leaves the script running to exit 0.
				if err != nil || stdout != tc.stdout || stderr != "" {
					t.Fatalf("stdout = %q, err = %v, stderr = %q; want %q, no error and nothing on stderr", stdout, err, stderr, tc.stdout)
				}
			})
		}
	})
}

// TestShellCommandScriptReachesEachInterpreterVerbatim is the evidence that a
// script survives -Command intact. Each payload rides in a single-quoted
// here-string, where PowerShell applies no escapes, and comes back as base64,
// so any difference between what was sent and what was read is damage in
// transit. The payloads travel together as one script, the way a real command
// does, so escapes interact across payload boundaries as well as within them.
func TestShellCommandScriptReachesEachInterpreterVerbatim(t *testing.T) {
	payloads := []string{
		`he said "hi" twice`,
		`a""b"""c"`,
		`"`,
		`\"`,
		`\\"`,
		`C:\dir\`,
		`\\server\share\"quoted"\`,
		`trailing\\`,
		"$env:USERPROFILE `$x` `",
		"line1\r\nline2\nline3",
		"a\tb",
		`%PATH% ^ & | < > !x!`,
		"it's",
		"é \U00010437 \U0001F600",
		"  leading and trailing spaces  ",
		"",
	}
	pieces := []string{`"`, `\`, " ", "\t", "'", "`", "$", "\n", "\r\n", "a", "Z", "%", "^", "&", "|", "<", ">", "(", ")", "{", "}", ";", "#", "@", "é", "\U00010437"}
	rng := rand.New(rand.NewPCG(0x5eed, 0xc0de))
	for len(payloads) < 128 {
		var b strings.Builder
		for n := 1 + rng.IntN(24); n > 0; n-- {
			b.WriteString(pieces[rng.IntN(len(pieces))])
		}
		// A line opening with '@ would close the here-string early, and a
		// trailing CR would merge into its closing line break.
		if p := b.String(); !strings.HasPrefix(p, "'@") && !strings.Contains(p, "\n'@") && !strings.HasSuffix(p, "\r") {
			payloads = append(payloads, p)
		}
	}
	var script strings.Builder
	for i, p := range payloads {
		fmt.Fprintf(&script, "$p%d = @'\n%s\n'@\n[Console]::Out.WriteLine([Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($p%d)))\n", i, p, i)
	}

	eachPowerShell(t, func(t *testing.T, shell string) {
		stdout, stderr, err := runShellWith(t, shell, script.String())
		if err != nil {
			t.Fatalf("script failed: %v; stderr=%q", err, stderr)
		}
		lines := strings.Split(strings.TrimRight(stdout, "\r\n"), "\n")
		if len(lines) != len(payloads) {
			t.Fatalf("got %d payloads back, want %d; stdout=%q", len(lines), len(payloads), stdout)
		}
		for i, p := range payloads {
			got, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[i]))
			if err != nil {
				t.Fatalf("payload %d came back as %q: %v", i, lines[i], err)
			}
			if string(got) != p {
				t.Errorf("payload %d = %q, want %q", i, got, p)
			}
		}
	})
}

// TestShellCommandErrorRecordsReachStderrAsText pins that stderr carries the
// command's errors as text under every interpreter present. Started with
// -EncodedCommand, both interpreters wrote them as CLIXML instead, and Windows
// PowerShell also wrote a progress record there whenever a command loaded a
// module, so even a Write-Output that succeeded left serialized objects on
// stderr.
func TestShellCommandErrorRecordsReachStderrAsText(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    string
	}{
		{name: "non-terminating error", command: "Write-Error 'gc-error-record'", want: "gc-error-record"},
		{name: "terminating error", command: "throw 'gc-thrown-error'", want: "gc-thrown-error"},
		{name: "unknown command", command: "gc-no-such-command", want: "gc-no-such-command"},
	}
	eachPowerShell(t, func(t *testing.T, shell string) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, stderr, err := runShellWith(t, shell, tc.command)
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("error = %v (%T), want *exec.ExitError", err, err)
				}
				if strings.Contains(stderr, "CLIXML") || strings.Contains(stderr, "<Objs") {
					t.Fatalf("stderr is serialized objects, not text: %q", stderr)
				}
				if !strings.Contains(stderr, tc.want) {
					t.Fatalf("stderr = %q, want it to contain %q", stderr, tc.want)
				}
			})
		}
		t.Run("success leaves stderr empty", func(t *testing.T) {
			stdout, stderr, err := runShellWith(t, shell, "Write-Output 'ok'")
			if err != nil {
				t.Fatalf("command failed: %v; stderr=%q", err, stderr)
			}
			if strings.TrimSpace(stdout) != "ok" || stderr != "" {
				t.Fatalf("stdout = %q, stderr = %q; want %q and nothing", stdout, stderr, "ok")
			}
		})
	})
}

// TestShellCommandQuotingSurvives is the correctness case the port exists for.
// Windows hands the callee one command line to re-parse, so every one of these
// characters is a chance for the script the interpreter sees to differ from the
// one Gas City wrote.
func TestShellCommandQuotingSurvives(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    string
	}{
		{
			name:    "double quotes in output",
			command: `[Console]::Out.Write('he said "hi" twice')`,
			want:    `he said "hi" twice`,
		},
		{
			name:    "escaped double quote inside a double-quoted string",
			command: "[Console]::Out.Write(\"a`\"b\")",
			want:    `a"b`,
		},
		{
			name:    "windows path backslashes",
			command: `[Console]::Out.Write('C:\Users\test\a b\file.txt')`,
			want:    `C:\Users\test\a b\file.txt`,
		},
		{
			name:    "trailing backslash",
			command: `[Console]::Out.Write('C:\dir\')`,
			want:    `C:\dir\`,
		},
		{
			name:    "literal dollar and backtick",
			command: "[Console]::Out.Write('$env:PATH and `backtick')",
			want:    "$env:PATH and `backtick",
		},
		{
			name:    "multiline with expansion",
			command: "$a = 'one'\n$b = 'two'\n[Console]::Out.Write(\"$a-$b\")",
			want:    "one-two",
		},
		{
			name:    "multiline with crlf separators",
			command: "$a = 'x'\r\n[Console]::Out.Write($a + 'y')",
			want:    "xy",
		},
		{
			name:    "percent and caret survive cmd-style expansion",
			command: `[Console]::Out.Write('100%PATH% ^ok')`,
			want:    `100%PATH% ^ok`,
		},
	}
	eachPowerShell(t, func(t *testing.T, shell string) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				stdout, stderr, err := runShellWith(t, shell, tc.command)
				if err != nil {
					t.Fatalf("command failed: %v; stderr=%q", err, stderr)
				}
				if stdout != tc.want {
					t.Fatalf("stdout = %q, want %q (stderr=%q)", stdout, tc.want, stderr)
				}
			})
		}
	})
}

// TestShellCommandWorkingDirectoryWithSpaces pins the acceptance checklist's
// rig-directory-containing-spaces case: the command runs in the directory the
// caller set, and a relative path in the command resolves against it.
func TestShellCommandWorkingDirectoryWithSpaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rig dir with spaces")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %q: %v", dir, err)
	}

	stdout, stderr, err := runShell(t, dir, nil,
		`[System.IO.File]::WriteAllText((Join-Path $PWD.Path 'out.txt'), $PWD.Path)`+"\n"+
			`[Console]::Out.Write($PWD.Path)`)
	if err != nil {
		t.Fatalf("command failed: %v; stderr=%q", err, stderr)
	}
	if stdout != dir {
		t.Errorf("working directory = %q, want %q", stdout, dir)
	}
	data, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("relative path did not resolve against the working directory: %v", err)
	}
	if string(data) != dir {
		t.Errorf("out.txt = %q, want %q", data, dir)
	}
}

// TestShellCommandExecutableSuffixResolves pins that an executable named
// without its .exe suffix still runs, which POSIX command strings routinely
// assume.
func TestShellCommandExecutableSuffixResolves(t *testing.T) {
	shell, err := findPowerShell()
	if err != nil {
		t.Fatalf("findPowerShell: %v", err)
	}
	bare := strings.TrimSuffix(shell, ".exe")
	inner := encodePowerShellCommand("[Console]::Out.Write('suffixless')")
	command := "& " + psQuote(bare) + " -NoProfile -NonInteractive -EncodedCommand " + inner

	stdout, stderr, err := runShell(t, "", nil, command)
	if err != nil {
		t.Fatalf("command failed: %v; stderr=%q", err, stderr)
	}
	if stdout != "suffixless" {
		t.Errorf("stdout = %q, want %q (stderr=%q)", stdout, "suffixless", stderr)
	}
}

// TestShellCommandExitCodeSurvives pins that the real exit code reaches the
// caller as an *exec.ExitError. PowerShell reports its own success by default,
// so without the epilogue a native program that exited 3 is reported as 0 and
// a failed setup command is recorded as a successful one.
func TestShellCommandExitCodeSurvives(t *testing.T) {
	eachPowerShell(t, func(t *testing.T, shell string) {
		nativeExit7 := "& " + psQuote(shell) + " -NoProfile -NonInteractive -EncodedCommand " +
			encodePowerShellCommand("exit 7")

		cases := []struct {
			name    string
			command string
			want    int
		}{
			{name: "script exits explicitly", command: "exit 3", want: 3},
			{name: "native child exit code", command: nativeExit7, want: 7},
			{name: "native failure then success reports success", command: nativeExit7 + "\n[Console]::Out.Write('after')", want: 0},
			{name: "terminating error", command: `throw "boom"`, want: 1},
			{name: "missing executable", command: `& 'C:\no\such\program.exe'`, want: 1},
			{name: "success", command: `[Console]::Out.Write('ok')`, want: 0},
			// A backtick continues a line, so a command ending in one escapes
			// the newline before the epilogue and swallows its first statement.
			// With a single separator `$gcShellOK = $?` was never assigned, the
			// epilogue read it as false, and a command that had succeeded was
			// reported as exit 1. Both directions are pinned: the swallow must
			// not invert a success, and must not lose a real failure code
			// either.
			{name: "trailing backtick after success", command: `[Console]::Out.Write('ok') ` + "`", want: 0},
			{name: "trailing backtick after failure", command: nativeExit7 + " `", want: 7},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, stderr, err := runShellWith(t, shell, tc.command)
				if tc.want == 0 {
					if err != nil {
						t.Fatalf("command failed: %v; stderr=%q", err, stderr)
					}
					return
				}
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("error = %v (%T), want *exec.ExitError", err, err)
				}
				if got := exitErr.ExitCode(); got != tc.want {
					t.Fatalf("exit code = %d, want %d; stderr=%q", got, tc.want, stderr)
				}
			})
		}
	})
}

// TestShellExitEpilogueHonorsStrictMode pins the epilogue under the script's
// own strict-mode preferences, which are still in force when it runs. Reading
// $LASTEXITCODE when no native program had set it was an error there: a
// succeeding script printed it to stderr, and with $ErrorActionPreference =
// 'Stop' as well exited 1, so a setup command that worked was recorded as a
// failure.
func TestShellExitEpilogueHonorsStrictMode(t *testing.T) {
	const strict = "Set-StrictMode -Version Latest\n"
	const stop = "$ErrorActionPreference = 'Stop'\n"
	eachPowerShell(t, func(t *testing.T, shell string) {
		native3 := "& " + psQuote(shell) + " -NoProfile -NonInteractive -EncodedCommand " +
			encodePowerShellCommand("exit 3")
		cases := []struct {
			name    string
			command string
			want    int
		}{
			{name: "success", command: strict + "[Console]::Out.Write('ok')", want: 0},
			{name: "success under Stop", command: strict + stop + "[Console]::Out.Write('ok')", want: 0},
			{name: "PowerShell failure", command: strict + "Write-Error 'gc-strict-failure'", want: 1},
			{name: "native failure", command: strict + native3, want: 3},
			{name: "native failure under Stop", command: strict + stop + native3, want: 3},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, stderr, err := runShellWith(t, shell, tc.command)
				code := 0
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					code = exitErr.ExitCode()
				} else if err != nil {
					t.Fatalf("running the command: %v", err)
				}
				if code != tc.want {
					t.Fatalf("exit code = %d, want %d; stderr=%q", code, tc.want, stderr)
				}
				if strings.Contains(stderr, "cannot be retrieved") {
					t.Fatalf("the epilogue reported its own strict-mode error: %q", stderr)
				}
				if tc.want == 0 && stderr != "" {
					t.Fatalf("a succeeding command left stderr = %q, want nothing", stderr)
				}
			})
		}
	})
}

// TestShellExitEpilogueReportsAStaleNativeCode pins the second known limit
// recorded on shellExitEpilogue: a PowerShell error after an earlier failed
// native program is reported with that program's code, where PowerShell
// itself reports 1. The verdict is still a failure. If this ever reports 1,
// the limit is gone and the epilogue's documentation should say so.
func TestShellExitEpilogueReportsAStaleNativeCode(t *testing.T) {
	eachPowerShell(t, func(t *testing.T, shell string) {
		native7 := "& " + psQuote(shell) + " -NoProfile -NonInteractive -EncodedCommand " +
			encodePowerShellCommand("exit 7")
		_, stderr, err := runShellWith(t, shell, native7+"\nWrite-Error 'gc-later-failure'")
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("error = %v (%T), want a failure; stderr=%q", err, err, stderr)
		}
		if got := exitErr.ExitCode(); got != 7 {
			t.Fatalf("exit code = %d; the documented limit is 7 (the earlier native code). Re-measure and update shellExitEpilogue's documentation", got)
		}
	})
}

// TestShellExitEpilogueRejectedAlternatives is the evidence behind the known
// limit recorded on shellExitEpilogue: a top-level `return` ends the script
// before the epilogue runs, and PowerShell then reports 1 instead of the real
// code.
//
// Each construct that would catch a `return` was measured rather than reasoned
// about, and each turned out to cost more than it saves. This test keeps that
// result executable, because every one of them looks like an improvement until
// it is run, and two of them convert a failure into a reported success — the
// exact outcome the epilogue exists to prevent.
//
// The flattening that remains is bounded in a way these are not: it never
// reports a failure as a success, only a specific non-zero code as 1.
//
// The script's transport is held to the same standard. Decoding a base64
// script into a scriptblock and invoking that with -Command would need no
// command-line quoting at all, but a top-level `return` then leaves the
// scriptblock instead of the script, and a failure followed by `return` is
// reported as a success.
func TestShellExitEpilogueRejectedAlternatives(t *testing.T) {
	eachPowerShell(t, func(t *testing.T, shell string) {
		testShellExitEpilogueRejectedAlternatives(t, shell)
	})
}

func testShellExitEpilogueRejectedAlternatives(t *testing.T, shell string) {
	// The epilogue without its leading separator, for splicing into a block.
	epilogueBody := strings.TrimLeft(shellExitEpilogue, "\n")

	// runRaw runs script exactly as it is, over the transport production
	// uses, so each probe controls where the epilogue sits.
	runRaw := func(t *testing.T, script string) int {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, shell, "-NoProfile", "-NonInteractive", "-Command", script)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if runErr := cmd.Run(); runErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) {
				t.Fatalf("running probe: %v; stderr=%q", runErr, stderr.String())
			}
			return exitErr.ExitCode()
		}
		return 0
	}

	nativeFailure := "& " + psQuote(shell) + " -NoProfile -NonInteractive -EncodedCommand " +
		encodePowerShellCommand("exit 3")

	t.Run("shipped epilogue reports a native failure", func(t *testing.T) {
		if got := runRaw(t, nativeFailure+shellExitEpilogue); got != 3 {
			t.Fatalf("exit code = %d, want 3", got)
		}
	})

	t.Run("call operator block hides a native failure", func(t *testing.T) {
		// $? after `& { ... }` reports whether invoking the block succeeded,
		// not whether the last command inside it did, so every failing command
		// would be reported as a success.
		script := "& {\n" + nativeFailure + "\n\n}" + shellExitEpilogue
		if got := runRaw(t, script); got == 3 {
			t.Fatal("wrapping in `& { }` now preserves the native exit code; re-measure before adopting it, the reason it was rejected was that it reported 0")
		} else if got != 0 {
			t.Logf("`& { }` reported %d rather than the 0 measured when it was rejected; still not the real code 3", got)
		}
	})

	t.Run("dot sourced block hides a native failure", func(t *testing.T) {
		script := ". {\n" + nativeFailure + "\n\n}" + shellExitEpilogue
		if got := runRaw(t, script); got == 3 {
			t.Fatal("dot sourcing now preserves the native exit code; re-measure before adopting it, the reason it was rejected was that it reported 0")
		}
	})

	t.Run("try finally discards an explicit exit", func(t *testing.T) {
		// finally runs on the way out of `exit`, and its own exit then
		// replaces the code the script chose.
		script := "try {\nexit 5\n\n} finally {\n" + epilogueBody + "}"
		if got := runRaw(t, script); got == 5 {
			t.Fatal("try/finally now preserves an explicit exit; re-measure before adopting it, the reason it was rejected was that it reported 0")
		}
	})

	t.Run("return flattens the code but never inverts the verdict", func(t *testing.T) {
		failed := runRaw(t, nativeFailure+"\nreturn"+shellExitEpilogue)
		if failed == 0 {
			t.Fatalf("a failing command followed by return reported success (%d); the limit is a flattened code, not a lost failure", failed)
		}
		if succeeded := runRaw(t, "[Console]::Out.Write('ok')\nreturn"+shellExitEpilogue); succeeded != 0 {
			t.Fatalf("a succeeding command followed by return reported %d, want 0", succeeded)
		}
	})

	t.Run("scriptblock transport reports a returned failure as success", func(t *testing.T) {
		// $? after invoking the scriptblock reports whether the invocation
		// succeeded, as with `& { }` above.
		script := encodePowerShellCommand(nativeFailure + "\nreturn" + shellExitEpilogue)
		wrapped := "& ([scriptblock]::Create([Text.Encoding]::Unicode.GetString([Convert]::FromBase64String('" + script + "'))))"
		if got := runRaw(t, wrapped); got != 0 {
			t.Fatalf("the scriptblock transport now reports %d for a failure followed by return; re-measure before adopting it, the reason it was rejected was that it reported 0", got)
		}
	})
}

// TestShellCommandEnvironmentReachesCommand pins that the caller's environment
// is the command's environment, including a value carrying the metacharacters
// the command line is most likely to mangle.
func TestShellCommandEnvironmentReachesCommand(t *testing.T) {
	const value = `a "quoted" C:\path\ $dollar`
	env := append(os.Environ(), "GC_SHELL_TEST_VALUE="+value)

	stdout, stderr, err := runShell(t, "", env, `[Console]::Out.Write($env:GC_SHELL_TEST_VALUE)`)
	if err != nil {
		t.Fatalf("command failed: %v; stderr=%q", err, stderr)
	}
	if stdout != value {
		t.Fatalf("env value = %q, want %q", stdout, value)
	}
}

// TestShellCommandSeparatesStreams pins that stderr stays stderr, which is what
// the setup-command failure detail depends on.
func TestShellCommandSeparatesStreams(t *testing.T) {
	eachPowerShell(t, func(t *testing.T, shell string) {
		stdout, stderr, err := runShellWith(t, shell,
			"[Console]::Out.Write('to-stdout')\n[Console]::Error.Write('to-stderr')\nexit 3")
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("error = %v, want *exec.ExitError", err)
		}
		if stdout != "to-stdout" {
			t.Errorf("stdout = %q, want %q", stdout, "to-stdout")
		}
		if stderr != "to-stderr" {
			t.Errorf("stderr = %q, want %q", stderr, "to-stderr")
		}
	})
}

// TestShellCommandRunsWithoutPosixShellOnPath is the acceptance test the port
// is measured by: with no PATH at all, so neither sh.exe nor bash.exe can be
// resolved and no Git Bash or WSL launcher can be reached, a command must still
// run.
func TestShellCommandRunsWithoutPosixShellOnPath(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("PATHEXT", "")

	for _, name := range []string{"sh", "sh.exe", "bash", "bash.exe", "wsl.exe"} {
		if path, err := exec.LookPath(name); err == nil {
			t.Fatalf("%s is still resolvable at %s; the test is not proving anything", name, path)
		}
	}

	stdout, stderr, err := runShell(t, "", os.Environ(), `[Console]::Out.Write('native')`)
	if err != nil {
		t.Fatalf("command failed with no shell on PATH: %v; stderr=%q", err, stderr)
	}
	if stdout != "native" {
		t.Fatalf("stdout = %q, want %q", stdout, "native")
	}
}

// TestShellCommandRejectsOversizedCommand pins that a command too large for a
// Windows command line is reported rather than truncated into a different
// command.
func TestShellCommandRejectsOversizedCommand(t *testing.T) {
	_, err := ShellCommandContext(context.Background(), strings.Repeat("a", commandLineLimit))
	if err == nil {
		t.Fatal("an oversized command must be rejected")
	}
	if !strings.Contains(err.Error(), "does not fit a Windows command line") {
		t.Fatalf("error = %v, want the command-line limit to be named", err)
	}
}

// TestShellCommandLengthLimitIsTheOperatingSystems pins the length check to
// CreateProcess itself: the longest command line Windows accepts runs, and one
// character more is refused by this package and by Windows alike. A check that
// undercounted would hand Windows a line it refuses; one that overcounted would
// refuse commands that fit.
func TestShellCommandLengthLimitIsTheOperatingSystems(t *testing.T) {
	// ERROR_FILENAME_EXCED_RANGE, which CreateProcess reports for a command
	// line past its limit.
	const errorFilenameExcedRange = syscall.Errno(206)

	shell, err := findPowerShell()
	if err != nil {
		t.Fatalf("findPowerShell: %v", err)
	}
	// sized returns a command whose command line is exactly total characters.
	// The quotes around the script already count, so each padding character
	// adds exactly one.
	sized := func(total int) string {
		const prefix = "[Console]::Out.Write('fits') # "
		base := commandLineLength([]string{shell, "-NoProfile", "-NonInteractive", "-Command", scriptPrologue(shell, prefix) + prefix + shellExitEpilogue})
		return prefix + strings.Repeat("a", total-base)
	}

	stdout, stderr, err := runShell(t, "", nil, sized(commandLineLimit-1))
	if err != nil {
		t.Fatalf("a command line of %d characters, the most Windows accepts, failed: %v; stderr=%q", commandLineLimit-1, err, stderr)
	}
	if stdout != "fits" {
		t.Fatalf("stdout = %q, want %q", stdout, "fits")
	}

	over := sized(commandLineLimit)
	if _, err := ShellCommandContext(context.Background(), over); err == nil || !strings.Contains(err.Error(), "does not fit a Windows command line") {
		t.Fatalf("a command line of %d characters was not refused: %v", commandLineLimit, err)
	}
	unchecked := exec.Command(shell, "-NoProfile", "-NonInteractive", "-Command", scriptPrologue(shell, over)+over+shellExitEpilogue)
	if err := unchecked.Run(); !errors.Is(err, errorFilenameExcedRange) {
		t.Fatalf("Windows was handed a command line of %d characters and returned %v, want %v; the limit this package enforces is not the operating system's", commandLineLimit, err, errorFilenameExcedRange)
	}
}

// TestShellCommandRejectsNUL pins that a command a Windows command line cannot
// carry is refused with a reason rather than failing later to start.
func TestShellCommandRejectsNUL(t *testing.T) {
	_, err := ShellCommandContext(context.Background(), "[Console]::Out.Write('a\x00b')")
	if err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("error = %v, want the NUL character to be named", err)
	}
}

// TestShellSessionCommandEmptyRunsBareInterpreter pins the empty-command
// session: a bare interpreter reading stdin, not a nested one, and not a
// process that hangs forever when its stdin is the null device.
func TestShellSessionCommandEmptyRunsBareInterpreter(t *testing.T) {
	cmd, err := ShellSessionCommand("")
	if err != nil {
		t.Fatalf("ShellSessionCommand(\"\"): %v", err)
	}
	if slices.Contains(cmd.Args, "-Command") || slices.Contains(cmd.Args, "-EncodedCommand") {
		t.Errorf("empty session command should run a bare interpreter, got argv %q", cmd.Args)
	}

	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting bare interpreter: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bare interpreter exited with %v, want a clean exit at stdin EOF", err)
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("bare interpreter did not exit at stdin EOF")
	}
}

// TestShellSessionCommandNonEmptyRunsCommand pins that a configured session
// command takes the same path as a setup command.
func TestShellSessionCommandNonEmptyRunsCommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work space")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %q: %v", dir, err)
	}
	cmd, err := ShellSessionCommand(`[System.IO.File]::WriteAllText((Join-Path $PWD.Path 'marker.txt'), 'started')`)
	if err != nil {
		t.Fatalf("ShellSessionCommand: %v", err)
	}
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("session command failed: %v; stderr=%q", err, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "marker.txt"))
	if err != nil {
		t.Fatalf("session command did not run in its working directory: %v", err)
	}
	if string(data) != "started" {
		t.Fatalf("marker.txt = %q, want %q", data, "started")
	}
}

// runPrompt types lines into prompt, a PowerShell reading its commands from
// stdin as a terminal's shell reads typed ones: each line runs as it arrives,
// and a statement that fails leaves the shell reading the next. After lines it
// is sent a marker and `exit 99`, so a prompt that outlives its statement says
// so instead of waiting for input.
func runPrompt(t *testing.T, prompt string, lines ...string) (code int, stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, prompt, "-NoProfile", "-NoLogo", "-Command", "-")
	input := append(append([]string(nil), lines...), "[Console]::Out.Write('"+promptSurvived+"')", "exit 99")
	cmd.Stdin = strings.NewReader(strings.Join(input, "\n") + "\n")
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	runErr := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case errors.As(runErr, &exitErr):
		code = exitErr.ExitCode()
	case runErr != nil:
		t.Fatalf("running the prompt: %v", runErr)
	}
	return code, out.String(), errOut.String()
}

// promptSurvived is what runPrompt's prompt prints if it is still reading
// input after the statement under test.
const promptSurvived = "PROMPT-SURVIVED"

// TestShellCommandPromptRunsTheCommandInsideAPrompt types the statement into a
// prompt under every interpreter present, with every interpreter present as
// the child. The command's output must arrive exactly, double quotes and
// backslashes included, and the prompt must exit with the command's status.
// The output case is the one a literal script fails: typed at a Windows
// PowerShell 5.1 prompt it lost its double quotes and still exited 0.
//
// The prompt runs with the user's preferences, which the statement cannot
// choose. With pwsh's native-error preference and 'Stop', the child's non-zero
// exit became an error that ended the statement before its exit ran, and the
// prompt lived on after its command.
func TestShellCommandPromptRunsTheCommandInsideAPrompt(t *testing.T) {
	const failFast = "Set-StrictMode -Version Latest; $ErrorActionPreference = 'Stop'; $PSNativeCommandUseErrorActionPreference = $true"
	cases := []struct {
		name, init, command string
		stdout              string
		code                int
	}{
		{name: "output survives the prompt", command: `[Console]::Out.Write('he said "hi" | C:\dir\ | a\"b')`, stdout: `he said "hi" | C:\dir\ | a\"b`},
		{name: "explicit exit", command: "exit 7", code: 7},
		{name: "native exit", command: "cmd.exe /c exit 5", code: 5},
		{name: "success", command: "[Console]::Out.Write('ok')", stdout: "ok"},
		{name: "fail-fast prompt, failing command", init: failFast, command: "exit 3", code: 3},
		{name: "fail-fast prompt, succeeding command", init: failFast, command: "[Console]::Out.Write('ok')", stdout: "ok"},
	}
	for _, prompt := range powerShells(t) {
		for _, child := range powerShells(t) {
			t.Run(filepath.Base(prompt)+"/"+filepath.Base(child), func(t *testing.T) {
				for _, tc := range cases {
					t.Run(tc.name, func(t *testing.T) {
						launch, err := shellCommandPromptWith(child, tc.command)
						if err != nil {
							t.Fatalf("shellCommandPromptWith: %v", err)
						}
						var lines []string
						if tc.init != "" {
							lines = append(lines, tc.init)
						}
						code, stdout, stderr := runPrompt(t, prompt, append(lines, launch.Statement)...)
						if code != tc.code || stdout != tc.stdout {
							t.Fatalf("prompt exited %d with stdout %q, want %d and %q; stderr=%q", code, stdout, tc.code, tc.stdout, stderr)
						}
					})
				}
			})
		}
	}
}

// TestShellCommandPromptExitsWhenTheChildCannotStart pins the statement's
// fallback. A child that could not be started sets no exit code, and the
// prompt's $LASTEXITCODE is whatever its last native program left, which a
// profile commonly leaves at 0: read as the command's status it reported a
// launch that never ran as a clean success.
func TestShellCommandPromptExitsWhenTheChildCannotStart(t *testing.T) {
	launch, err := shellCommandPromptWith(`C:\gc-no-such-dir\pwsh.exe`, "exit 0")
	if err != nil {
		t.Fatalf("shellCommandPromptWith: %v", err)
	}
	for _, init := range []string{"cmd.exe /c exit 0", "cmd.exe /c exit 0; $ErrorActionPreference = 'Stop'"} {
		eachPowerShell(t, func(t *testing.T, prompt string) {
			code, stdout, stderr := runPrompt(t, prompt, init, launch.Statement)
			if code != 1 || strings.Contains(stdout, promptSurvived) {
				t.Fatalf("after %q the prompt exited %d with stdout %q, want exit 1; stderr=%q", init, code, stdout, stderr)
			}
		})
	}
}

// TestShellCommandPromptIdentifiesItsChild pins the statement's parts: it
// starts the interpreter it reports, with the encoded command it reports, and
// the child keeps the console's ability to ask for input.
func TestShellCommandPromptIdentifiesItsChild(t *testing.T) {
	launch, err := ShellCommandPrompt("exit 0")
	if err != nil {
		t.Fatalf("ShellCommandPrompt: %v", err)
	}
	if launch.EncodedCommand != encodePowerShellCommand(promptScript("exit 0")) {
		t.Errorf("EncodedCommand = %q, want the base64 of the script", launch.EncodedCommand)
	}
	invocation := "& " + psSingleQuoted(launch.Interpreter) + " -NoProfile -EncodedCommand " + launch.EncodedCommand + " "
	if !strings.Contains(launch.Statement, invocation) {
		t.Errorf("statement %q does not contain %q", launch.Statement, invocation)
	}
	if strings.Contains(launch.Statement, "-NonInteractive") {
		t.Errorf("statement %q makes the command non-interactive", launch.Statement)
	}
}

// TestShellCommandPromptRefusesWhatAPromptCannotRun pins the statement's
// limits against the operating system's. Encoding inflates the script about
// 2.7 times, so a command well inside ShellCommandContext's limit can still
// make a child command line Windows refuses. The largest encodable command
// under the limit runs through a real prompt; one base64 group more is refused
// here, and Windows refuses it too when the check is skipped.
func TestShellCommandPromptRefusesWhatAPromptCannotRun(t *testing.T) {
	if _, err := shellCommandPromptWith(inBoxPowerShell(), "'a\x00b'"); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("error = %v, want the NUL character to be named", err)
	}

	child := inBoxPowerShell()
	const prefix = "[Console]::Out.Write('fits') # "
	length := func(padding int) int {
		return promptChildCommandLineLength(child, encodePowerShellCommand(promptScript(prefix+strings.Repeat("a", padding))))
	}
	padding := (commandLineLimit - length(0)) * 3 / 8
	for padding > 0 && length(padding) >= commandLineLimit {
		padding--
	}
	for length(padding+1) < commandLineLimit {
		padding++
	}
	fits := prefix + strings.Repeat("a", padding)
	over := prefix + strings.Repeat("a", padding+1)
	if n := length(padding); n < commandLineLimit-4 {
		t.Fatalf("largest accepted command line is %d units; the search should end within one base64 group of %d", n, commandLineLimit-1)
	}

	launch, err := shellCommandPromptWith(child, fits)
	if err != nil {
		t.Fatalf("a %d-unit child command line was refused: %v", length(padding), err)
	}
	if code, stdout, stderr := runPrompt(t, child, launch.Statement); code != 0 || stdout != "fits" {
		t.Fatalf("a %d-unit child command line, the largest accepted, exited %d with stdout %q; stderr=%q", length(padding), code, stdout, stderr)
	}

	if _, err := shellCommandPromptWith(child, over); err == nil || !strings.Contains(err.Error(), "does not fit a Windows command line") {
		t.Fatalf("a %d-unit child command line was not refused: %v", length(padding+1), err)
	}
	unchecked := "& " + psSingleQuoted(child) + " -NoProfile -EncodedCommand " + encodePowerShellCommand(promptScript(over)) + "; exit 42"
	if code, stdout, stderr := runPrompt(t, child, unchecked); code != 42 || stdout != "" || !strings.Contains(stderr, "too long") {
		t.Fatalf("a %d-unit child command line, one group over, exited %d with stdout %q, want Windows to refuse to start it; stderr=%q", length(padding+1), code, stdout, stderr)
	}
}

// TestPSSingleQuotedRoundTrips pins the interpreter-path quoting. PowerShell
// reads each typographic single quote as a quote character too, so one left
// single would end the string early.
func TestPSSingleQuotedRoundTrips(t *testing.T) {
	const tricky = "C:\\it's\\\u2018quoted\u2019\\\u201Alow\u201B\\x"
	if got, want := psSingleQuoted(tricky), "'C:\\it''s\\\u2018\u2018quoted\u2019\u2019\\\u201A\u201Alow\u201B\u201B\\x'"; got != want {
		t.Fatalf("psSingleQuoted = %q, want %q", got, want)
	}
	eachPowerShell(t, func(t *testing.T, shell string) {
		stdout, stderr, err := runShellWith(t, shell,
			"[Console]::Out.Write([Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes("+psSingleQuoted(tricky)+")))")
		if err != nil {
			t.Fatalf("command failed: %v; stderr=%q", err, stderr)
		}
		got, err := base64.StdEncoding.DecodeString(stdout)
		if err != nil || string(got) != tricky {
			t.Fatalf("PowerShell read %q (%v), want %q", got, err, tricky)
		}
	})
}
