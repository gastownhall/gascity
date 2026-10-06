package herdr

import (
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// launchSpec is how Start launches a session's command under herdr ≥0.7.5,
// whose `agent start` no longer execs arbitrary argv: it launches a supported
// agent kind's canonical executable into an existing shell pane and waits for
// TUI detection.
type launchSpec struct {
	// Kind is the herdr agent kind for `agent start --kind` (with Args as the
	// executable's arguments) when the command is a clean invocation of a
	// supported kind. The session gets a registered herdr agent: native
	// detection, prompt/wait delivery, and status-backed liveness.
	Kind string
	Args []string
	// Raw is the fallback: the command is typed into the pane shell as
	// `exec /bin/sh -c <Raw>` so the pane dies with the command (tmux parity).
	// Only pane-level tracking is available; the sidecar pane binding is the
	// session handle.
	Raw string
}

// herdrAgentKinds are the agent kinds herdr 0.7.5 can launch and detect
// (`herdr agent start --help`). A kind here only gates the *attempt*; an
// unsupported invocation surfaces as an agent-start error, and commands that
// need a real shell fall back to Raw before any kind matching.
var herdrAgentKinds = map[string]bool{
	"pi": true, "claude": true, "codex": true, "gemini": true, "cursor": true,
	"devin": true, "agy": true, "cline": true, "omp": true, "mastracode": true,
	"opencode": true, "copilot": true, "kimi": true, "kiro": true, "droid": true,
	"amp": true, "grok": true, "hermes": true, "kilo": true, "qodercli": true,
	"maki": true,
}

// launchShellMetachars are characters whose presence means the command needs a
// real shell (operators, substitution, env-prefix assignments): conservative —
// quoted occurrences also trigger the fallback, which still runs correctly.
const launchShellMetachars = "|&;<>()`$=\n"

// launchSpecFor parses a session command into its herdr launch mode. A blank
// command returns the zero spec: the pane's own shell is the session.
func launchSpecFor(command string) launchSpec {
	command = strings.TrimSpace(command)
	if command == "" {
		return launchSpec{}
	}
	if strings.ContainsAny(command, launchShellMetachars) {
		return launchSpec{Raw: command}
	}
	parts := shellquote.Split(command)
	if len(parts) == 0 {
		return launchSpec{Raw: command}
	}
	if kind := filepath.Base(parts[0]); herdrAgentKinds[kind] {
		return launchSpec{Kind: kind, Args: parts[1:]}
	}
	return launchSpec{Raw: command}
}
