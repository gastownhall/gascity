package herdr

import (
	"context"
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

// An empty target resolves nothing and must not even reach herdr: a nameless
// registry entry decodes with no registered name, so a list scan that matched
// it would hand an empty target the first pane nobody named.
func TestGetAgentNeverResolvesAKindOrEmptyTarget(t *testing.T) {
	list := `{"result":{"agents":[` +
		`{"agent":"opencode","pane_id":"w3:p1","agent_status":"working"},` +
		`{"agent":"opencode","name":"alpha","pane_id":"w1:p1","agent_status":"working"},` +
		`{"agent":"opencode","name":"beta","pane_id":"w2:p1","agent_status":"working"}]}}`
	trace := filepath.Join(t.TempDir(), "verbs")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do echo \"$a\" >> " + trace + "; done\n" +
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

	// A kind-only target names no agent: it is the label every pane running
	// that kind shares, not an identity.
	a, ok, err := c.getAgent(context.Background(), "opencode")
	if err != nil || ok {
		t.Errorf("getAgent(opencode) = (%+v, %v, %v); want not-found, no error", a, ok, err)
	}
	// An empty target resolves nothing without asking herdr at all: the verb
	// trace the kind lookup above already touched must not grow.
	before, rerr := os.ReadFile(trace)
	if rerr != nil {
		t.Fatalf("reading the verb trace: %v", rerr)
	}
	a, ok, err = c.getAgent(context.Background(), "")
	if err != nil || ok {
		t.Errorf("getAgent(\"\") = (%+v, %v, %v); want not-found, no error", a, ok, err)
	}
	if after, rerr := os.ReadFile(trace); rerr != nil || string(after) != string(before) {
		t.Errorf("the empty target reached herdr; verbs seen:\n%s", after)
	}
	// Registered names still resolve through the list fallback.
	for _, tc := range []struct{ target, pane string }{
		{"alpha", "w1:p1"},
		{"beta", "w2:p1"},
	} {
		a, ok, err := c.getAgent(context.Background(), tc.target)
		if err != nil || !ok || a.PaneID != tc.pane {
			t.Errorf("getAgent(%q) = (%+v, %v, %v); want pane %q", tc.target, a, ok, err, tc.pane)
		}
	}
}
