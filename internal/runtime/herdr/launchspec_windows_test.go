//go:build windows

package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// TestLaunchSpecForMatchesOnlyBareKindNames pins kind detection on Windows.
// Command names ignore case, so CLAUDE is the claude kind. A name with an
// extension or a path names one particular file, which the kind launch's own
// resolution of the bare name need not reach, so it launches raw.
func TestLaunchSpecForMatchesOnlyBareKindNames(t *testing.T) {
	for _, tc := range []struct {
		command string
		kind    string
		args    []string
	}{
		{command: "claude --resume abc123", kind: "claude", args: []string{"--resume", "abc123"}},
		{command: "CLAUDE --effort max", kind: "claude", args: []string{"--effort", "max"}},
		{command: "Codex --full-auto", kind: "codex", args: []string{"--full-auto"}},
	} {
		got := launchSpecFor(tc.command)
		if got.Kind != tc.kind || got.Raw != "" || !reflect.DeepEqual(got.Args, tc.args) {
			t.Errorf("launchSpecFor(%q) = %+v; want kind %q with args %q", tc.command, got, tc.kind, tc.args)
		}
	}
	for _, command := range []string{
		"claude.exe --resume abc123",
		"claude.cmd",
		"claude.ps1 --resume abc123",
		"codex.bat --full-auto",
		"C:/tools/claude --x",
		"./claude",
		"claudette",
	} {
		if got := launchSpecFor(command); got.Kind != "" || got.Raw != command {
			t.Errorf("launchSpecFor(%q) = %+v; want the raw launch", command, got)
		}
	}
}

// TestLaunchSpecForSendsBackslashCommandsRaw pins the fallback for Windows
// paths: the POSIX splitting that finds the program drops backslashes, so a
// kind launch would pass herdr a different argument from the one configured.
// The raw path runs the command exactly as written.
func TestLaunchSpecForSendsBackslashCommandsRaw(t *testing.T) {
	const withPath = `claude --settings C:\city\settings.json`
	for _, cmd := range []string{withPath, `C:\tools\claude.exe --resume abc123`} {
		if got := launchSpecFor(cmd); got.Kind != "" || got.Raw != cmd {
			t.Errorf("launchSpecFor(%q) = %+v; want the raw fallback", cmd, got)
		}
	}
	// The reason, kept executable: if splitting ever keeps the path intact,
	// the fallback costs a kind launch for nothing and should be re-measured.
	if parts := shellquote.Split(withPath); parts[len(parts)-1] == `C:\city\settings.json` {
		t.Fatalf("shellquote.Split now keeps backslashes (%q); re-measure before keeping the raw fallback", parts)
	}
}

// divergence is argument syntax the kind launch refuses because PowerShell
// passes a program something other than its POSIX split, with the
// interpreters that were measured to do so.
type divergence struct {
	args       string
	pwsh       bool // PowerShell 7, measured on 7.6
	powershell bool // Windows PowerShell 5.1
}

var divergentArgs = []divergence{
	{args: `-a.b`, pwsh: true, powershell: true},
	{args: `--x -a.b`, pwsh: true, powershell: true},
	{args: `-a1.5`, pwsh: true, powershell: true},
	{args: `-A.B`, pwsh: true, powershell: true},
	{args: `-a/b.c`, pwsh: true, powershell: true},
	{args: `-?.x`, pwsh: true, powershell: true},
	{args: "-\u00e9.x", pwsh: true, powershell: true},
	{args: "\u2013a.b", pwsh: true, powershell: true},
	{args: "\u2014a.b", pwsh: true, powershell: true},
	{args: "\u2015a.b", pwsh: true, powershell: true},
	{args: `-a[0]`, pwsh: true, powershell: true},
	{args: `-a[`, pwsh: true, powershell: true},
	{args: `-_[x`, pwsh: true, powershell: true},
	{args: "\u2013a[0]", pwsh: true, powershell: true},
	{args: `-a:`, pwsh: true, powershell: true},
	{args: `-a:@b`, pwsh: true, powershell: true},
	{args: `-a:#b`, pwsh: true, powershell: true},
	{args: `-a:]x`, pwsh: true, powershell: true},
	{args: `-a:-b.c`, pwsh: true, powershell: true},
	{args: `-a:--`, pwsh: true, powershell: true},
	{args: `-- -a:b`, pwsh: true, powershell: true},
	{args: `]a`, pwsh: true, powershell: true},
	{args: `]]`, pwsh: true, powershell: true},
	{args: `]9`, pwsh: true, powershell: true},
	{args: `][`, pwsh: true, powershell: true},
	{args: `]-a.b`, pwsh: true, powershell: true},
	{args: `]@a`, pwsh: true, powershell: true},
	{args: `"a"b`, pwsh: true, powershell: true},
	{args: `'a'b`, pwsh: true, powershell: true},
	{args: `"a b"c`, pwsh: true, powershell: true},
	{args: `""`, powershell: true},
	{args: `''`, powershell: true},
	{args: `"a""b"`, pwsh: true},
	{args: `'a''b'`, pwsh: true, powershell: true},
	{args: `'say "hi"'`, powershell: true},
	{args: `@a`, pwsh: true, powershell: true},
	{args: `#a`, pwsh: true, powershell: true},
	{args: `--% x y`, pwsh: true, powershell: true},
	{args: `x --% y`, pwsh: true, powershell: true},
	{args: `"--%"`, pwsh: true, powershell: true},
	{args: `'--%'`, pwsh: true, powershell: true},
	{args: `{a}`, pwsh: true, powershell: true},
	{args: `~`, pwsh: true},
	{args: `~/x`, pwsh: true},
	{args: "\u201ca b\u201d", pwsh: true, powershell: true},
	{args: "\u2018a b\u2019", pwsh: true, powershell: true},
	{args: "a\u00a0b", pwsh: true, powershell: true},
	{args: "a\u2003b", pwsh: true, powershell: true},
	{args: "a\u2028b", pwsh: true, powershell: true},
	{args: `a,`, pwsh: true, powershell: true},
	{args: `,a`, pwsh: true, powershell: true},
	{args: `a,,b`, pwsh: true, powershell: true},
	{args: `-a,b`, pwsh: true, powershell: true},
	{args: `a,#b`, pwsh: true, powershell: true},
	{args: `a,@b`, pwsh: true, powershell: true},
	{args: `a,1kb`, powershell: true},
	{args: `a,0x10`, powershell: true},
	{args: `a,b ,c`, powershell: true},
	{args: `a, b`, powershell: true},
}

// conservativeArgs are refused although PowerShell passes them as the POSIX
// split does: the grammar excludes a whole shape rather than model which of
// its members diverge, which costs these the kind launch and nothing more.
var conservativeArgs = []string{
	`a,b`, `Read,Write`, `a"b c"d`, `-a:b`, `-x:1`, `-a:b.c`, `-a: b`, `-a:0x10`, `-a:]`, `-/b.c`,
}

// TestLaunchSpecForSendsNonPortableSyntaxRaw pins that a command whose words
// PowerShell may read differently from the POSIX split launches raw.
func TestLaunchSpecForSendsNonPortableSyntaxRaw(t *testing.T) {
	refused := append([]string(nil), conservativeArgs...)
	for _, d := range divergentArgs {
		refused = append(refused, d.args)
	}
	for _, args := range refused {
		command := "maki " + args
		if got := launchSpecFor(command); got.Kind != "" || got.Raw != command {
			t.Errorf("launchSpecFor(%q) = %+v; want the raw launch", command, got)
		}
	}
	for _, command := range []string{`"maki" --x`, `'maki' --x`} {
		if got := launchSpecFor(command); got.Kind != "" || got.Raw != command {
			t.Errorf("launchSpecFor(%q) = %+v; want the raw launch: PowerShell reads a quoted program as a string", command, got)
		}
	}
}

// portableArgs are argument strings the kind launch accepts, whose POSIX split
// is exactly what PowerShell passes a program.
var portableArgs = []string{
	"--resume abc123", "--max-turns 5", "--effort max", "-p hello", "-x", "-a/b", "-a-b", "-?", "-not", "-eq",
	"a.b", "--a.b", "--a:b", "--.x", "a:b", ":a", ":", "::", ":::", "a/b:c", "C:/x/y", "./x", "../x", "x.",
	".", "..", "...", "a..b", "-", "--", "---", "a-", "-- -x", "-- --", "+", "++", "+x", "a+b", "+-", "-+",
	"+9", "+5", "+.5", "-1", "-1.5", "-.5", "-.x", "-9.x", "-9Z", "-1:2", "-1[", "-1.2.3", "-1kb", "-0x10",
	"42", "007", "0x10", "0x", "0xg", "0b11", "0b102", "1e3", "1e", "1e+", "1e-5", "1.e3", "1.", "1..2",
	"1_000", "1kb", "1kbx", "1.5kb", "1gb", "1n", "1d", "1l", "1u", "1.50", ".5", "1.2.3", "v1.2",
	"99999999999999999999", "\u2013x", "\u2014x", "\u2013?", "\u20135", "\u20141.5", "\u20150x10", "\u20131kb",
	"\u2014\u2014", "\u2013-", "-\u2013", "\u2015\u2015", "\u2014-x", "\u2013-%", "\u2014\u2014% x", "-\u2013% x",
	"x_y", "_x", "\u00fc", "\u65e5\u672c", "a\u200bb", "a\u180eb", "a\ufeffb",
	"a@b", "a#b", "a[0]", "[a]", "[9", "a[", "[", "]", "a]", "a]@b", "a]#b", "9]@x", "[@a", "[#a", "[-a.b",
	"a[@b", "[ a", "[ a ]", "[int]", "[int] x", "a [ b ] c", "*", "**", "*-", "?", "??", "a*b", "*a", "?a",
	"/", "//", "%", "%%", "%x%", "!", "!!", "!-", "!a", "a!", "^", "^a",
	"if", "function", "exit", "in", "class", "using", "param", "begin", "return", "throw", "data", "trap",
	`"a b"`, `"a.b"`, "'a b'", `"-1.5"`, `"-a b"`, "'007'", `"it's"`, "'-a.b'", `"a,b"`, "'a, b'", `"#a"`,
	`"@a"`, `" "`, `"a "`, `"-a:b"`, "'-a:b'", `"-a.b"`, `"-a:b c"`, `"-a,b"`, "\"\u00e9\"", `"a#b"`, `"#"`,
	`"@"`, `"]"`, `"]x"`, "'-- x'", "a\tb", "a  b",
}

// sweepWords is every word of one or two runes over the runes that decide
// where PowerShell starts a word, and every such pair after a dash, an en dash
// and a letter: each rune at the start of a word, after each other rune, and
// inside a parameter.
func sweepWords() []string {
	sweepRunes := []string{"a", "9", "-", "\u2013", ".", "[", "]", ":", "@", "#", "!", "%", "*", "+", "/", "?", "^", "_", "\u00e9"}
	var words []string
	for _, a := range sweepRunes {
		words = append(words, a)
		for _, b := range sweepRunes {
			for _, prefix := range []string{"", "-", "\u2013", "a"} {
				words = append(words, prefix+a+b)
			}
		}
	}
	return words
}

// TestKindLaunchArgsAreWhatPowerShellPasses is the evidence behind
// launchSyntaxPortable, taken under every PowerShell present. Every command
// the grammar accepts must give the kind launch the arguments PowerShell would
// give the program were the command run raw. Every divergent shape must still
// diverge where it was measured to, or refusing it costs kind launches for
// nothing and it should be re-measured; every conservative one must still
// read the same, or it belongs among the divergent.
func TestKindLaunchArgsAreWhatPowerShellPasses(t *testing.T) {
	accepted := append([]string(nil), portableArgs...)
	var line []string
	for _, word := range sweepWords() {
		if launchSpecFor("maki "+word).Kind != "maki" {
			continue
		}
		if line = append(line, word); len(line) == 16 {
			accepted = append(accepted, strings.Join(line, " "))
			line = nil
		}
	}
	if len(line) > 0 {
		accepted = append(accepted, strings.Join(line, " "))
	}
	rng := rand.New(rand.NewPCG(0x6b1d, 0x5eed))
	pieces := []string{"a", "Z", "9", "-", "--x", "\u2014", ".", "/", ":", "+", ",", "*", "?", "!", "%", "^", "[", "]", "_", "\u00e9", "@", "#"}
	for fuzzed := 0; fuzzed < 40; {
		var words []string
		for n := 1 + rng.IntN(4); n > 0; n-- {
			var w strings.Builder
			for m := 1 + rng.IntN(5); m > 0; m-- {
				w.WriteString(pieces[rng.IntN(len(pieces))])
			}
			switch rng.IntN(5) {
			case 0:
				words = append(words, `"`+w.String()+` `+w.String()+`"`)

			case 1:
				words = append(words, "'"+w.String()+"'")

			default:
				words = append(words, w.String())
			}
		}
		if args := strings.Join(words, " "); launchSpecFor("maki "+args).Kind == "maki" {
			accepted = append(accepted, args)
			fuzzed++
		}
	}
	for _, args := range accepted {
		if got := launchSpecFor("maki " + args); got.Kind != "maki" {
			t.Fatalf("launchSpecFor(%q) = %+v; want the maki kind", "maki "+args, got)
		}
	}

	lines := append([]string(nil), accepted...)
	for _, d := range divergentArgs {
		lines = append(lines, d.args)
	}
	lines = append(lines, conservativeArgs...)

	for _, shell := range herdrTestPowerShells(t) {
		isPwsh := strings.EqualFold(filepath.Base(shell), "pwsh.exe")
		t.Run(filepath.Base(shell), func(t *testing.T) {
			t.Parallel()
			readings := powerShellReadings(t, shell, lines)
			for i, args := range accepted {
				if want := launchSpecFor("maki " + args).Args; !readings[i].agrees(want) {
					t.Errorf("maki %s: the kind launch passes %q; PowerShell %s", args, want, readings[i])
				}
			}
			readings = readings[len(accepted):]
			for i, d := range divergentArgs {
				measured := d.powershell
				if isPwsh {
					measured = d.pwsh
				}
				if split := shellquote.Split(d.args); measured && readings[i].agrees(split) {
					t.Errorf("maki %s: PowerShell now passes the POSIX split %q; re-measure before refusing it", d.args, split)
				}
			}
			readings = readings[len(divergentArgs):]
			for i, args := range conservativeArgs {
				if split := shellquote.Split(args); !readings[i].agrees(split) {
					t.Errorf("maki %s: PowerShell %s, not the POSIX split %q; list it among the divergent", args, readings[i], split)
				}
			}
		})
	}
}

// herdrTestPowerShells lists the PowerShell interpreters present.
func herdrTestPowerShells(t *testing.T) []string {
	t.Helper()
	var shells []string
	if path, err := exec.LookPath("pwsh.exe"); err == nil {
		shells = append(shells, path)
	}
	inBox := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(inBox); err == nil {
		shells = append(shells, inBox)
	}
	if len(shells) == 0 {
		t.Fatal("no PowerShell interpreter found")
	}
	return shells
}

// psReading is what PowerShell passed a program for one command line: its
// arguments, or why the line passed none.
type psReading struct {
	argv    []string
	failure string
}

// agrees reports whether PowerShell passed exactly argv.
func (r psReading) agrees(argv []string) bool {
	return r.failure == "" && reflect.DeepEqual(r.argv, argv)
}

func (r psReading) String() string {
	if r.failure != "" {
		return r.failure
	}
	return fmt.Sprintf("passes %q", r.argv)
}

// psLiteral quotes s as a PowerShell single-quoted string.
func psLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// powerShellReadings runs, under shell, one command line per entry of
// argLines: this test binary as an argument echo, named as a raw command names
// its program, followed by the entry. Each line is parsed and run on its own,
// so one that PowerShell refuses spoils only its own reading.
func powerShellReadings(t *testing.T, shell string, argLines []string) []psReading {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}
	list := filepath.Join(t.TempDir(), "args.txt")
	if err := os.WriteFile(list, []byte(strings.Join(argLines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `$ErrorActionPreference = 'Stop'
$program = ` + psLiteral("& "+psLiteral(self)+" ") + `
foreach ($argText in (Get-Content -LiteralPath ` + psLiteral(list) + ` -Encoding UTF8)) {
    $line = $program + $argText
    $tokens = $null
    $parseErrors = $null
    [void][System.Management.Automation.Language.Parser]::ParseInput($line, [ref]$tokens, [ref]$parseErrors)
    if ($parseErrors.Count -gt 0) {
        '!does not parse: ' + $parseErrors[0].ErrorId
        continue
    }
    try {
        $out = @(Invoke-Expression $line)
    } catch {
        '!fails: ' + $_.FullyQualifiedErrorId
        continue
    }
    if ($out.Count -ne 1) {
        '!prints ' + $out.Count + ' lines'
        continue
    }
    $out[0]
}
`
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), argvEchoEnv+"=1", "NO_COLOR=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || stderr.Len() > 0 {
		t.Fatalf("running the argument echo under %s: %v\nstderr:\n%s\nstdout:\n%s", shell, err, stderr.String(), out)
	}
	var readings []psReading
	for _, line := range strings.Split(strings.TrimRight(string(out), "\r\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if failure, ok := strings.CutPrefix(line, "!"); ok {
			readings = append(readings, psReading{failure: failure})
			continue
		}
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatalf("under %s the argument echo printed %q: %v", shell, line, err)
		}
		readings = append(readings, psReading{argv: argv})
	}
	if len(readings) != len(argLines) {
		t.Fatalf("under %s: %d readings from %d lines; output:\n%s", shell, len(readings), len(argLines), out)
	}
	return readings
}

// argvEchoEnv makes this test binary print its arguments as JSON and exit, as
// a stand-in for the program a command names.
const argvEchoEnv = "GC_HERDR_TEST_ARGV_ECHO"

// asciiJSON encodes argv as JSON with every non-ASCII rune escaped, so what
// the echo prints survives any console code page between it and the test.
func asciiJSON(argv []string) string {
	raw, _ := json.Marshal(argv)
	var b strings.Builder
	for _, r := range string(raw) {
		if r < utf8.RuneSelf {
			b.WriteRune(r)
			continue
		}
		for _, u := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&b, `\u%04x`, u)
		}
	}
	return b.String()
}

// TestProcessAliveMatchesWindowsImageNames pins the foreground match on
// Windows: herdr names the pane's shell by its image, powershell.exe, while a
// configuration names programs the Unix way, and Windows names ignore case.
func TestProcessAliveMatchesWindowsImageNames(t *testing.T) {
	p, _ := newFakeHerdrProvider(t)
	bindTestPane(t, p, "gastown__shell", bindModeShell)
	// Only the foreground list can match: the process-table walk sees nothing.
	prev := snapshotProcesses
	snapshotProcesses = func() ([]proctable.ProcessRecord, error) { return nil, nil }
	t.Cleanup(func() { snapshotProcesses = prev })

	for _, want := range []string{"powershell", "PowerShell", "powershell.exe"} {
		if !p.ProcessAlive("gastown__shell", []string{want}) {
			t.Errorf("ProcessAlive([%s]) = false for a pane whose shell is powershell.exe", want)
		}
	}
	if p.ProcessAlive("gastown__shell", []string{"pwsh"}) {
		t.Error("ProcessAlive([pwsh]) = true for a pane that does not run it")
	}
}
