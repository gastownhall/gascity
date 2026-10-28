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

// getAgent must resolve nothing for a kind-only or an empty target. The
// empty leg guards the unnamed decode: once a nameless entry decodes as
// Name "", an unguarded scan would match it and return a stranger's pane as
// the empty session's runtime — so an empty target must not even ask herdr.
func TestGetAgentNeverResolvesAKindOrEmptyTarget(t *testing.T) {
	list := `{"result":{"agents":[` +
		`{"agent":"opencode","name":"alpha","pane_id":"w1:p1","agent_status":"working"},` +
		`{"agent":"opencode","name":"beta","pane_id":"w2:p1","agent_status":"working"},` +
		`{"agent":"opencode","pane_id":"w3:p1","agent_status":"working"}]}}`
	trace := filepath.Join(t.TempDir(), "verbs")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + trace + "\n" +
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

	a, ok, err := c.getAgent(context.Background(), "")
	if err != nil || ok {
		t.Fatalf(`getAgent("") = (%+v, %v, %v); want not-found, no error`, a, ok, err)
	}
	if _, serr := os.Stat(trace); !errors.Is(serr, os.ErrNotExist) {
		b, _ := os.ReadFile(trace)
		t.Errorf(`getAgent("") asked herdr; verbs seen:\n%s`, b)
	}

	if a, ok, err := c.getAgent(context.Background(), "opencode"); err != nil || ok {
		t.Errorf("getAgent(\"opencode\") = (%+v, %v, %v); a kind names no agent in particular", a, ok, err)
	}
}

// targetHasNoNamedAgent's registry scan must not hand a detected-but-unnamed
// pane to the paste fallback as if it were registered under its kind: the
// kind is a class, not a name.
func TestTargetHasNoNamedAgentIgnoresAKindOnlyName(t *testing.T) {
	const agentless = `{"error":{"code":"agent_not_ready","message":"agent w1:p1 is not an active named agent"},"id":"cli:agent:prompt"}`
	bin, _ := registryFake(t, []string{"alpha=w1:p1", "beta=w2:p1", "=w3:p1"}, agentless)
	c := &client{session: "gc-test", bin: bin}

	err := c.agentPrompt(context.Background(), "w1:p1", "proceed")
	if err == nil {
		t.Fatal("fake herdr rejection produced no error")
	}
	if !c.targetHasNoNamedAgent(context.Background(), "opencode", err) {
		t.Error(`targetHasNoNamedAgent("opencode") = false; a kind is not a registered name`)
	}
	// The registered names still hold their own panes.
	if c.targetHasNoNamedAgent(context.Background(), "alpha", err) {
		t.Error(`targetHasNoNamedAgent("alpha") = true; alpha is registered by name`)
	}
}
