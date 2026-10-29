package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// herdr 0.9.1 reports the registered name under "name" and the detected agent
// kind under "agent". Only the registered name is an identity: a kind-named
// target must never resolve to a pane that merely runs that kind, including a
// detected pane with no registered name.
func TestGetAgentListFallbackMatchesRegisteredNameNotKind(t *testing.T) {
	list := `{"result":{"agents":[` +
		`{"agent":"opencode","name":"alpha","pane_id":"w1:p1","agent_status":"working"},` +
		`{"agent":"opencode","name":"beta","pane_id":"w2:p1","agent_status":"working"},` +
		`{"agent":"opencode","pane_id":"w3:p1","agent_status":"working"}]}}`
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *'agent get'*)\n" +
		"    echo '{\"error\":{\"code\":\"agent_not_found\",\"message\":\"agent not found\"}}'\n" +
		"    ;;\n" +
		"  *'agent list'*)\n" +
		"    echo '" + list + "'\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	c := &client{session: "gc-test", bin: writeFakeHerdr(t, script)}

	for _, tc := range []struct {
		target   string
		wantPane string
	}{
		{"alpha", "w1:p1"},
		{"beta", "w2:p1"},
		{"opencode", ""},
		{"", ""},
	} {
		a, ok, err := c.getAgent(context.Background(), tc.target)
		if err != nil {
			t.Fatalf("getAgent(%q): %v", tc.target, err)
		}
		if tc.wantPane == "" {
			if ok {
				t.Errorf("getAgent(%q) resolved pane %q (decoded name %q); a kind or empty target must resolve nothing", tc.target, a.PaneID, a.Name)
			}
			continue
		}
		if !ok || a.PaneID != tc.wantPane {
			t.Errorf("getAgent(%q) = (%+v, %v); want pane %q", tc.target, a, ok, tc.wantPane)
		}
	}
}

// The kind-only lookup is the issue's own repro shape: IsRunning, liveness
// and Stop all reach getAgent, and a kind-named target must not resolve to
// the first pane running that kind. The empty target must not reach herdr at
// all — the registry holds unnamed entries, and an empty lookup target would
// otherwise resolve to the first one.
func TestGetAgentNeverResolvesAKindOrEmptyTarget(t *testing.T) {
	list := `{"result":{"agents":[` +
		`{"agent":"opencode","pane_id":"w3:p1","agent_status":"working"},` +
		`{"agent":"opencode","name":"alpha","pane_id":"w1:p1","agent_status":"working"},` +
		`{"agent":"opencode","name":"beta","pane_id":"w2:p1","agent_status":"working"}]}}`
	trace := filepath.Join(t.TempDir(), "verbs")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do echo \"$a\" >> " + trace + "; done\n" +
		"echo '---' >> " + trace + "\n" +
		"case \"$*\" in\n" +
		"  *'agent get'*)\n" +
		"    echo '{\"error\":{\"code\":\"agent_not_found\",\"message\":\"agent target not found\"}}'\n" +
		"    ;;\n" +
		"  *'agent list'*)\n" +
		"    echo '" + list + "'\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	c := &client{session: "gc-test", bin: writeFakeHerdr(t, script)}

	// An empty target resolves nothing without consulting herdr.
	a, ok, err := c.getAgent(context.Background(), "")
	if err != nil || ok {
		t.Fatalf("getAgent(\"\") = (%+v, %v, %v); want an immediate empty-handed miss", a, ok, err)
	}
	if _, serr := os.Stat(trace); !errors.Is(serr, os.ErrNotExist) {
		b, _ := os.ReadFile(trace)
		t.Fatalf("empty target reached herdr; verbs seen:\n%s", b)
	}

	// The kind-only target resolves nothing while the named sessions on the
	// same list keep resolving to their own panes.
	a, ok, err = c.getAgent(context.Background(), "opencode")
	if err != nil {
		t.Fatalf("getAgent(\"opencode\"): %v", err)
	}
	if ok {
		t.Errorf("getAgent(\"opencode\") resolved pane %q (decoded name %q); a kind target is not an identity", a.PaneID, a.Name)
	}
	for _, tc := range []struct{ target, wantPane string }{{"alpha", "w1:p1"}, {"beta", "w2:p1"}} {
		a, ok, err := c.getAgent(context.Background(), tc.target)
		if err != nil {
			t.Fatalf("getAgent(%q): %v", tc.target, err)
		}
		if !ok || a.PaneID != tc.wantPane {
			t.Errorf("getAgent(%q) = (%+v, %v); want pane %q", tc.target, a, ok, tc.wantPane)
		}
	}
}

// The registry the paste fallback consults holds the same two identifiers.
// After an agent-state rejection, a kind-only target must still read as "no
// named agent holds it": an unnamed pane of that kind is not a registration,
// so the fallback must go on pasting to the pane it asked about.
func TestTargetHasNoNamedAgentIgnoresAKindOnlyName(t *testing.T) {
	rejection := `{"error":{"code":"agent_not_ready","message":"agent w1:p9 is not an active named agent"},"id":"cli:agent:prompt"}`
	bin, _ := registryFake(t, []string{"=w3:p1"}, rejection)
	c := &client{session: "gc-test", bin: bin}

	err := c.agentPrompt(context.Background(), "w1:p9", "proceed")
	if err == nil {
		t.Fatal("fake herdr rejection produced no error")
	}
	if !c.targetHasNoNamedAgent(context.Background(), "opencode", err) {
		t.Error("targetHasNoNamedAgent = false for a kind-only target; an unnamed pane of that kind is not a named agent")
	}
}
