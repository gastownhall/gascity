package runtime

import (
	"context"
	"os"
	"slices"
	"testing"
)

// hostileShellArgs are values a command string carries as data. Read as code
// rather than as one literal argument, each would end a quoted string,
// substitute a command or start a new statement under sh or under PowerShell,
// and each payload that runs creates a file named pwned in the command's
// working directory.
var hostileShellArgs = []string{
	"plain",
	"",
	"it's",
	"it\u2019s fine; New-Item -ItemType File -Path pwned | Out-Null; \u2019",
	"x'; touch pwned; New-Item -ItemType File -Path pwned; '",
	"\u2018x\u2019; New-Item -ItemType File -Path pwned; \u201Ay\u201B",
	"$(touch pwned)$(New-Item -ItemType File -Path pwned)",
	"`touch pwned` `$(New-Item -ItemType File -Path pwned)` ``",
	"a; touch pwned; New-Item -ItemType File -Path pwned",
	"line1\ntouch pwned\nNew-Item -ItemType File -Path pwned\n",
	"\"dq\" \u201Csmart\u201D \\\"esc\\\" \\",
	"--%",
	"@(1,2) $env:PATH $HOME #comment",
	"& | > < ^ %PATH% !",
	`C:\dir with space\`,
}

// TestShellQuoteCarriesHostileDataAsOneLiteralArgument pins ShellQuote's
// contract under every interpreter a command can run under here: each value
// spliced into command text arrives as exactly one argument, byte for byte,
// and nothing in it runs.
func TestShellQuoteCarriesHostileDataAsOneLiteralArgument(t *testing.T) {
	command := fixtureEchoArgs
	for _, arg := range hostileShellArgs {
		command += " " + ShellQuote(arg)
	}
	eachShell(t, func(t *testing.T, run func(dir, command string) (string, string, error)) {
		dir := t.TempDir()
		stdout, stderr, err := run(dir, command)
		if err != nil {
			t.Fatalf("command failed: %v; stderr=%q", err, stderr)
		}
		got, err := decodeEchoedArgs(stdout)
		if err != nil {
			t.Fatalf("%v; stderr=%q", err, stderr)
		}
		if !slices.Equal(got, hostileShellArgs) {
			t.Errorf("arguments = %q,\nwant %q", got, hostileShellArgs)
		}
		assertNothingRan(t, dir)
	})
}

// TestShellQuotedNULNeverRuns pins the one value ShellQuote cannot carry: a
// command line cannot hold NUL on either platform, so the command must fail
// to start rather than run with its data cut short.
func TestShellQuotedNULNeverRuns(t *testing.T) {
	dir := t.TempDir()
	cmd, err := ShellCommandContext(context.Background(),
		"touch pwned; New-Item -ItemType File -Path pwned; "+fixtureEchoArgs+" "+ShellQuote("a\x00b"))
	if err == nil {
		cmd.Dir = dir
		err = cmd.Run()
	}
	if err == nil {
		t.Fatal("a command carrying NUL ran, want it refused")
	}
	assertNothingRan(t, dir)
}

// assertNothingRan fails if a hostile payload created a file in dir.
func assertNothingRan(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, entry := range entries {
		t.Errorf("payload ran: %s exists in the command's directory", entry.Name())
	}
}
