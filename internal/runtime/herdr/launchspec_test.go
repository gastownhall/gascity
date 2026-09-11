package herdr

import (
	"reflect"
	"testing"
)

// launchSpecFor decides how Start launches a session's command under herdr
// ≥0.7.5, whose `agent start` no longer execs arbitrary argv: it launches a
// supported agent *kind*'s canonical executable into an existing shell pane
// and waits for TUI detection. Clean invocations of a supported kind take
// that path (registered agent: native detection, prompt, wait, status);
// everything else is typed into the pane shell as `exec /bin/sh -c <cmd>` so
// the pane still dies with the command (tmux parity).

func TestLaunchSpecForCleanClaudeCommandUsesKind(t *testing.T) {
	got := launchSpecFor(`claude --dangerously-skip-permissions --effort max --settings "/city root/.gc/settings.json"`)
	if got.Kind != "claude" {
		t.Fatalf("Kind = %q; want claude", got.Kind)
	}
	want := []string{"--dangerously-skip-permissions", "--effort", "max", "--settings", "/city root/.gc/settings.json"}
	if !reflect.DeepEqual(got.Args, want) {
		t.Errorf("Args = %q; want %q", got.Args, want)
	}
	if got.Raw != "" {
		t.Errorf("Raw = %q; want empty on the kind path", got.Raw)
	}
}

func TestLaunchSpecForPathQualifiedKind(t *testing.T) {
	got := launchSpecFor("/usr/local/bin/claude --resume abc123")
	if got.Kind != "claude" || got.Raw != "" {
		t.Fatalf("spec = %+v; want kind claude via basename", got)
	}
}

// Shell metachars mean the command needs a real shell: fall back to raw even
// when it mentions a known kind. Conservative is correct — the raw path still
// runs it; only herdr-native registration is lost.
func TestLaunchSpecForShellMetacharsFallBackToRaw(t *testing.T) {
	for _, cmd := range []string{
		"claude --flag && echo done",
		"claude -p 'hi'; sleep 1",
		"claude --append-system-prompt \"use $HOME wisely\"",
		"FOO=bar claude --flag",
		"claude | tee log",
		"for i in $(seq 3); do echo $i; done",
	} {
		got := launchSpecFor(cmd)
		if got.Kind != "" || got.Raw != cmd {
			t.Errorf("launchSpecFor(%q) = %+v; want raw fallback", cmd, got)
		}
	}
}

// Unknown executables are raw.
func TestLaunchSpecForUnknownExecutableIsRaw(t *testing.T) {
	got := launchSpecFor("python3 worker.py --queue main")
	if got.Kind != "" || got.Raw != "python3 worker.py --queue main" {
		t.Errorf("spec = %+v; want raw", got)
	}
}

// Empty command: the shell pane itself is the session (old /bin/sh behavior).
func TestLaunchSpecForEmptyCommandIsBareShell(t *testing.T) {
	for _, cmd := range []string{"", "   "} {
		got := launchSpecFor(cmd)
		if got.Kind != "" || got.Raw != "" {
			t.Errorf("launchSpecFor(%q) = %+v; want zero spec (bare shell)", cmd, got)
		}
	}
}
