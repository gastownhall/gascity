package nudgepoller

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandArgsMatchCmdlineMatcher(t *testing.T) {
	cityPath := "/tmp/gc-city"
	sessionName := "sess-worker"
	agentName := "agent"

	argv := append([]string{"gc"}, CommandArgs(cityPath, sessionName, agentName)...)
	if !CmdlineMatcher(cityPath, sessionName, agentName)(argv) {
		t.Fatalf("CmdlineMatcher did not match CommandArgs argv: %v", argv)
	}
}

func TestCmdlineMatcherRejectsWrongOwnership(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--city", "/tmp/gc-city", "--session", "sess-worker", "agent"}
	cases := []struct {
		name        string
		cityPath    string
		sessionName string
		agentName   string
	}{
		{name: "empty city", cityPath: "", sessionName: "sess-worker", agentName: "agent"},
		{name: "empty session", cityPath: "/tmp/gc-city", sessionName: "", agentName: "agent"},
		{name: "empty target", cityPath: "/tmp/gc-city", sessionName: "sess-worker", agentName: ""},
		{name: "wrong city", cityPath: "/tmp/other-city", sessionName: "sess-worker", agentName: "agent"},
		{name: "wrong session", cityPath: "/tmp/gc-city", sessionName: "other-session", agentName: "agent"},
		{name: "wrong target", cityPath: "/tmp/gc-city", sessionName: "sess-worker", agentName: "session-id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if CmdlineMatcher(tc.cityPath, tc.sessionName, tc.agentName)(argv) {
				t.Fatalf("CmdlineMatcher(%q, %q, %q) matched %v", tc.cityPath, tc.sessionName, tc.agentName, argv)
			}
		})
	}
}

func TestCmdlineMatcherAcceptsFlagEqualsForm(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--session=sess-worker", "--city=/tmp/gc-city", "agent"}
	if !CmdlineMatcher("/tmp/gc-city", "sess-worker", "agent")(argv) {
		t.Fatalf("CmdlineMatcher did not match equals-form flags: %v", argv)
	}
}

func TestArgvHasPollTargetEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{
			name: "generated target after flags",
			argv: []string{"gc", "nudge", "poll", "--city", "/tmp/gc-city", "--session", "sess-worker", "agent"},
			want: true,
		},
		{
			name: "target before flags",
			argv: []string{"gc", "nudge", "poll", "agent", "--city", "/tmp/gc-city", "--session", "sess-worker"},
			want: true,
		},
		{
			name: "known space form flags before target",
			argv: []string{"gc", "nudge", "poll", "--interval", "1s", "--quiescence", "2s", "agent"},
			want: true,
		},
		{
			name: "known equals form flags before target",
			argv: []string{"gc", "nudge", "poll", "--interval=1s", "--quiescence=2s", "agent"},
			want: true,
		},
		{
			name: "unknown space form flag skips value",
			argv: []string{"gc", "nudge", "poll", "--future", "value", "agent"},
			want: true,
		},
		{
			name: "unknown equals form flag before target",
			argv: []string{"gc", "nudge", "poll", "--future=value", "agent"},
			want: true,
		},
		{
			name: "first positional must be target",
			argv: []string{"gc", "nudge", "poll", "other", "agent"},
			want: false,
		},
		{
			name: "no target",
			argv: []string{"gc", "nudge", "poll", "--city", "/tmp/gc-city"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := argvHasPollTarget(tc.argv, "agent"); got != tc.want {
				t.Fatalf("argvHasPollTarget(%v, agent) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

func TestCmdlineMatcherNormalizesCityPath(t *testing.T) {
	cityPath := t.TempDir()
	argv := []string{"gc", "nudge", "poll", "--city", cityPath, "--session", "sess-worker", "agent"}
	if !CmdlineMatcher(filepath.Join(cityPath, "."), "sess-worker", "agent")(argv) {
		t.Fatalf("CmdlineMatcher did not match equivalent city path spelling: %v", argv)
	}
}

func TestCmdlineMatcherAcceptsAnyMatchingCityFlag(t *testing.T) {
	argv := []string{"gc", "nudge", "poll", "--city", "/tmp/other-city", "--city=/tmp/gc-city", "--session", "sess-worker", "agent"}
	if !CmdlineMatcher("/tmp/gc-city", "sess-worker", "agent")(argv) {
		t.Fatalf("CmdlineMatcher did not match later city flag: %v", argv)
	}
}

func TestPollerFileStemSanitizesSessionPrefix(t *testing.T) {
	stem := PollerFileStem(" ../sess worker/one ", "target")
	if !strings.HasPrefix(stem, "sess-worker-one-") {
		t.Fatalf("PollerFileStem prefix = %q, want sanitized session prefix", stem)
	}
	if strings.ContainsAny(stem, `/\ `) {
		t.Fatalf("PollerFileStem = %q, want filesystem-safe stem", stem)
	}
}

func TestPollerFileStemUsesFallbackPrefixForEmptySession(t *testing.T) {
	stem := PollerFileStem("   ", "target")
	if !strings.HasPrefix(stem, "session-") {
		t.Fatalf("PollerFileStem empty session prefix = %q, want session-*", stem)
	}
}

func TestPollerFileStemTruncatesLongSessionPrefix(t *testing.T) {
	stem := PollerFileStem(strings.Repeat("a", 60), "target")
	prefix, _, ok := strings.Cut(stem, "-")
	if !ok {
		t.Fatalf("PollerFileStem = %q, want prefix and digest", stem)
	}
	if len(prefix) != 48 {
		t.Fatalf("PollerFileStem prefix length = %d, want 48", len(prefix))
	}
}

func TestPollerFileStemDistinguishesSessionTargetTuples(t *testing.T) {
	if PollerFileStem("ab", "c") == PollerFileStem("a", "bc") {
		t.Fatal("PollerFileStem returned the same stem for distinct session/target tuples")
	}
}

func TestSafeFileStemPartTrimsUnsafeEdges(t *testing.T) {
	if got := safeFileStemPart(" ../worker session/. "); got != "worker-session" {
		t.Fatalf("safeFileStemPart() = %q, want worker-session", got)
	}
}

func TestCmdlineMatcherRequiresNudgePollCommand(t *testing.T) {
	argv := []string{"gc", "nudge", "--city", "/tmp/gc-city", "poll", "--session", "sess-worker", "agent"}
	if CmdlineMatcher("/tmp/gc-city", "sess-worker", "agent")(argv) {
		t.Fatalf("CmdlineMatcher matched non-contiguous nudge poll argv: %v", argv)
	}
}
