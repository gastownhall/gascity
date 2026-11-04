package proctable

import (
	"strings"
	"testing"
)

func TestDarwinPSCommandIgnoresInlineTmuxEnv(t *testing.T) {
	fields := []string{
		"123",
		"45",
		"/bin/bash",
		"GC_SESSION_ID=ga-123",
		"TMUX=/private/tmp/tmux-501/default,1,0",
	}
	if got := darwinPSCommand(fields); got != "/bin/bash" {
		t.Fatalf("darwinPSCommand() = %q, want executable token only", got)
	}
	if isInfrastructureCommand(darwinPSCommand(fields)) {
		t.Fatal("regular shell with TMUX env was classified as infrastructure")
	}
}

func TestDarwinPSCommandStillIdentifiesTmuxExecutable(t *testing.T) {
	fields := []string{"123", "1", "tmux: server", "GC_SESSION_ID=ga-123"}
	if !isInfrastructureCommand(darwinPSCommand(fields)) {
		t.Fatal("tmux executable was not classified as infrastructure")
	}
}

// The infrastructure match is exact on the path-stripped name: the tmux
// executable as ps reports it on Darwin, and the titles tmux sets through
// setproctitle as Linux comm reports them (newline-terminated). Anything
// else — above all a tmux-* wrapper — is a candidate agent root.
func TestIsInfrastructureCommandMatchesExactNamesOnly(t *testing.T) {
	for _, command := range []string{"tmux", "/opt/homebrew/bin/tmux", "tmux: server", "tmux: client", "tmux: server\n"} {
		if !isInfrastructureCommand(command) {
			t.Errorf("isInfrastructureCommand(%q) = false, want true", command)
		}
	}
	for _, command := range []string{"", "claude", "/bin/bash", "tmux-wrapper", "/usr/local/bin/tmux-wrapper", "tmuxinator", "my-tmux"} {
		if isInfrastructureCommand(command) {
			t.Errorf("isInfrastructureCommand(%q) = true, want false: only an exact tmux name is infrastructure", command)
		}
	}
}

// herdr's session server is shared, long-lived infrastructure exactly as the
// tmux server is: it is launched in a process group of its own and outlives the
// session whose GC_SESSION_ID it inherited, so the orphan sweeps must never
// treat it as an agent root. The match stays exact on the path-stripped name.
func TestIsInfrastructureCommandMatchesHerdrServerExactlyOnly(t *testing.T) {
	for _, command := range []string{"herdr", "/usr/local/bin/herdr", "/home/u/.local/bin/herdr", "herdr\n"} {
		if !isInfrastructureCommand(command) {
			t.Errorf("isInfrastructureCommand(%q) = false, want true", command)
		}
	}
	for _, command := range []string{"herdr-wrapper", "/usr/local/bin/herdr-wrapper", "herdrctl", "my-herdr", "herd"} {
		if isInfrastructureCommand(command) {
			t.Errorf("isInfrastructureCommand(%q) = true, want false: only the exact herdr name is infrastructure", command)
		}
	}
	// ps reports the launched server as its argv[0] followed by its arguments.
	fields := strings.Fields("  100     1 herdr --session gc-city server GC_SESSION_ID=ga-123")
	if !isInfrastructureCommand(darwinPSCommand(fields)) {
		t.Error("a herdr server ps line was not classified as infrastructure")
	}
}

// The exact-name match must hold for values the real parser produces, not just
// for pre-split fixtures: psRecords tokenizes the ps line with strings.Fields,
// so a proctitle'd "tmux: server" reaches isInfrastructureCommand as "tmux:".
func TestIsInfrastructureCommandThroughRealPSTokenization(t *testing.T) {
	infra := []string{
		"  100     1 tmux: server",
		"  101     1 /opt/homebrew/bin/tmux -L hq new-session -d",
	}
	for _, line := range infra {
		if !isInfrastructureCommand(darwinPSCommand(strings.Fields(line))) {
			t.Errorf("ps line %q was not classified as infrastructure", line)
		}
	}
	agent := "  102     1 /usr/local/bin/tmux-wrapper --serve GC_SESSION_ID=hq-session"
	if isInfrastructureCommand(darwinPSCommand(strings.Fields(agent))) {
		t.Errorf("ps line %q was classified as infrastructure; a tmux-* wrapper is a candidate agent root", agent)
	}
}
