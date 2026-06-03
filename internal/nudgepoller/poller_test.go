package nudgepoller

import (
	"path/filepath"
	"testing"
)

func TestCommandArgsMatchCmdlineMatcher(t *testing.T) {
	cityPath := "/tmp/gc-city"
	sessionName := "sess-worker"
	agentName := "agent"

	argv := append([]string{"gc"}, CommandArgs(cityPath, sessionName, agentName)...)
	if !CmdlineMatcher(cityPath, sessionName)(argv) {
		t.Fatalf("CmdlineMatcher did not match CommandArgs argv: %v", argv)
	}
}

func TestCmdlineMatcherRejectsWrongOwnership(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--city", "/tmp/gc-city", "--session", "sess-worker", "agent"}
	cases := []struct {
		name        string
		cityPath    string
		sessionName string
	}{
		{name: "empty city", cityPath: "", sessionName: "sess-worker"},
		{name: "empty session", cityPath: "/tmp/gc-city", sessionName: ""},
		{name: "wrong city", cityPath: "/tmp/other-city", sessionName: "sess-worker"},
		{name: "wrong session", cityPath: "/tmp/gc-city", sessionName: "other-session"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if CmdlineMatcher(tc.cityPath, tc.sessionName)(argv) {
				t.Fatalf("CmdlineMatcher(%q, %q) matched %v", tc.cityPath, tc.sessionName, argv)
			}
		})
	}
}

func TestCmdlineMatcherAcceptsFlagEqualsForm(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--session=sess-worker", "--city=/tmp/gc-city", "agent"}
	if !CmdlineMatcher("/tmp/gc-city", "sess-worker")(argv) {
		t.Fatalf("CmdlineMatcher did not match equals-form flags: %v", argv)
	}
}

func TestCmdlineMatcherNormalizesCityPath(t *testing.T) {
	cityPath := t.TempDir()
	argv := []string{"gc", "nudge", "poll", "--city", cityPath, "--session", "sess-worker", "agent"}
	if !CmdlineMatcher(filepath.Join(cityPath, "."), "sess-worker")(argv) {
		t.Fatalf("CmdlineMatcher did not match equivalent city path spelling: %v", argv)
	}
}

func TestCmdlineMatcherAcceptsAnyMatchingCityFlag(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--city", "/tmp/other-city", "--city=/tmp/gc-city", "--session", "sess-worker", "agent"}
	if !CmdlineMatcher("/tmp/gc-city", "sess-worker")(argv) {
		t.Fatalf("CmdlineMatcher did not match later city flag: %v", argv)
	}
}

func TestCmdlineMatcherRequiresNudgePollCommand(t *testing.T) {
	argv := []string{"gc", "nudge", "--city", "/tmp/gc-city", "poll", "--session", "sess-worker", "agent"}
	if CmdlineMatcher("/tmp/gc-city", "sess-worker")(argv) {
		t.Fatalf("CmdlineMatcher matched non-contiguous nudge poll argv: %v", argv)
	}
}
