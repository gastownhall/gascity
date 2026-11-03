package herdr

// The fake herdr CLIs these tests exec are POSIX shell scripts, which Windows
// cannot run: it has no shebang, and the port runs no shell. On Windows each
// fake is this test binary instead, linked into the test's temp dir as
// herdr.exe with a fakeHerdrSpec beside it (fakeherdr_windows_test.go), and
// runs the behavior in fakeherdr_behavior_windows_test.go. This file holds the
// spec, the screens the fakes print, and the recognizer for one-shot scripts.

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// fakeHerdrSpec is what one fake herdr does.
type fakeHerdrSpec struct {
	// Mode is "reply", "start", "provider" or "registry".
	Mode string `json:"mode"`

	// reply writes Stdout and Stderr, with "$*" in either replaced by the
	// arguments joined by spaces, and exits with Exit: a one-shot script
	// passed to writeFakeHerdr.
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	Exit   int    `json:"exit,omitempty"`

	// start is newFakeHerdr's script (start_test.go): Log records each call,
	// and Probe is checked when an agent starts.
	Log   string `json:"log,omitempty"`
	Probe string `json:"probe,omitempty"`

	// provider is newFakeHerdrProviderForSession's script
	// (panebinding_provider_test.go), driven by the flag files in State.
	// WorkspaceExists is the one rewrite of it a test makes (rewriteFake).
	State           string `json:"state,omitempty"`
	MetaDir         string `json:"meta_dir,omitempty"`
	WorkspaceExists bool   `json:"workspace_exists,omitempty"`

	// registry is registryFake's script (no_named_agent_test.go).
	Trace     string `json:"trace,omitempty"`
	List      string `json:"list,omitempty"`
	Rejection string `json:"rejection,omitempty"`
}

// fakeNotReadyLine is the line Copilot CLI 1.0.95 prints when it discards a
// submitted prompt because its managed-account policy has not loaded yet. The
// provider fake's not-ready prompt verdicts append it to the pane's screen.
const fakeNotReadyLine = "! Managed account policy is still loading. Please wait a moment and try again."

// fakeComposer is the provider fake's input prompt: the agent's composer line
// starts with it, followed by whatever the box holds.
const fakeComposer = "\u276f "

// fakeIdleScreen is the provider fake's pane screen before anything is printed
// to it: an agent's empty input prompt.
const fakeIdleScreen = fakeComposer + "\n"

// fakeRejectedScreen is what an agent that discards a turn as not ready adds to
// its screen: the not-ready line, above its composer, now empty.
const fakeRejectedScreen = fakeNotReadyLine + "\n" + fakeIdleScreen

// replyScriptShapes are the one-shot scripts writeFakeHerdr is given, each
// matched against the whole script. Text that a shell would expand or
// interpret — a quote of the same kind, $, a backslash, a % other than one
// printf %s — falls outside every pattern, so no script runs as something
// other than what it says.
var replyScriptShapes = []struct {
	pattern *regexp.Regexp
	spec    func(match []string) (fakeHerdrSpec, bool)
}{
	{ // echo '<text>' >&2; exit <n>
		regexp.MustCompile(`\A#!/bin/sh\necho '([^'\\\n]*)' >&2\nexit (\d+)\n\z`),
		func(m []string) (fakeHerdrSpec, bool) {
			if strings.Contains(m[1], "$*") {
				return fakeHerdrSpec{}, false
			}
			return fakeHerdrSpec{Mode: "reply", Stderr: m[1] + "\n", Exit: atoiFake(m[2])}, true
		},
	},
	{ // echo "<text>$*<text>" >&2; exit <n>
		regexp.MustCompile(`\A#!/bin/sh\necho "([^"$\\\x60\n]*)\$\*([^"$\\\x60\n]*)" >&2\nexit (\d+)\n\z`),
		func(m []string) (fakeHerdrSpec, bool) {
			return fakeHerdrSpec{Mode: "reply", Stderr: m[1] + "$*" + m[2] + "\n", Exit: atoiFake(m[3])}, true
		},
	},
	{ // cat <<'TAG' >&2 / <body> / TAG / exit <n>
		regexp.MustCompile(`(?s)\A#!/bin/sh\ncat <<'(\w+)' >&2\n(.*)\n(\w+)\nexit (\d+)\n\z`),
		func(m []string) (fakeHerdrSpec, bool) {
			if m[1] != m[3] || strings.Contains("\n"+m[2]+"\n", "\n"+m[1]+"\n") || strings.Contains(m[2], "$*") {
				return fakeHerdrSpec{}, false
			}
			return fakeHerdrSpec{Mode: "reply", Stderr: m[2] + "\n", Exit: atoiFake(m[4])}, true
		},
	},
	{ // printf '<format with one %s>' "$*"
		regexp.MustCompile(`\A#!/bin/sh\nprintf '([^'\\\n]*)' "\$\*"\n\z`),
		func(m []string) (fakeHerdrSpec, bool) {
			if strings.Count(m[1], "%") != 1 || strings.Count(m[1], "%s") != 1 || strings.Contains(m[1], "$*") {
				return fakeHerdrSpec{}, false
			}
			return fakeHerdrSpec{Mode: "reply", Stdout: strings.Replace(m[1], "%s", "$*", 1)}, true
		},
	},
	{ // echo '<text>'
		regexp.MustCompile(`\A#!/bin/sh\necho '([^'\\\n]*)'\n\z`),
		func(m []string) (fakeHerdrSpec, bool) {
			if strings.Contains(m[1], "$*") {
				return fakeHerdrSpec{}, false
			}
			return fakeHerdrSpec{Mode: "reply", Stdout: m[1] + "\n"}, true
		},
	},
}

// replySpecForScript returns the reply a one-shot fake script gives, if it has
// one of replyScriptShapes.
func replySpecForScript(script string) (fakeHerdrSpec, bool) {
	for _, shape := range replyScriptShapes {
		if m := shape.pattern.FindStringSubmatch(script); m != nil {
			return shape.spec(m)
		}
	}
	return fakeHerdrSpec{}, false
}

func atoiFake(text string) int {
	n, _ := strconv.Atoi(text)
	return n
}

// TestReplyScriptShapesMatchWholeScriptsOnly pins the recognizer the Windows
// fakes stand on: each shape reproduces its script's reply, and a script with
// anything a shape does not cover is not recognized at all.
func TestReplyScriptShapesMatchWholeScriptsOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   fakeHerdrSpec
		ok     bool
	}{
		{"stderr echo", "#!/bin/sh\necho 'agent not found: %12' >&2\nexit 1\n", fakeHerdrSpec{Mode: "reply", Stderr: "agent not found: %12\n", Exit: 1}, true},
		{"stderr echo of the arguments", echoArgvScript, fakeHerdrSpec{Mode: "reply", Stderr: "error: unexpected argument '$*' found\n", Exit: 2}, true},
		{"stderr heredoc", "#!/bin/sh\ncat <<'MSG' >&2\nwarning\n{\"x\":1}\nMSG\nexit 1\n", fakeHerdrSpec{Mode: "reply", Stderr: "warning\n{\"x\":1}\n", Exit: 1}, true},
		{"printf of the arguments", "#!/bin/sh\nprintf '{\"m\":\"in %s\"}' \"$*\"\n", fakeHerdrSpec{Mode: "reply", Stdout: `{"m":"in $*"}`}, true},
		{"stdout echo", "#!/bin/sh\necho 'not json at all'\n", fakeHerdrSpec{Mode: "reply", Stdout: "not json at all\n"}, true},
		{"a line past the shape", "#!/bin/sh\necho 'a' >&2\nexit 1\necho 'b'\n", fakeHerdrSpec{}, false},
		{"two printf directives", "#!/bin/sh\nprintf '%s %s' \"$*\"\n", fakeHerdrSpec{}, false},
		{"an expansion", "#!/bin/sh\necho \"$HOME\"\n", fakeHerdrSpec{}, false},
		{"a literal $* in single quotes", "#!/bin/sh\necho 'cost $*' >&2\nexit 1\n", fakeHerdrSpec{}, false},
		{"a heredoc holding its own terminator", "#!/bin/sh\ncat <<'A' >&2\nx\nA\ny\nexit 1\n", fakeHerdrSpec{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := replySpecForScript(tc.script)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("replySpecForScript = %+v, %t; want %+v, %t", got, ok, tc.want, tc.ok)
			}
		})
	}
}
