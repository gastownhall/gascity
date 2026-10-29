package herdr

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestAgentAliveFromStatus pins the core of the claude-2.1.x singleton
// restart-loop fix: liveness is derived from herdr's own agent_status, so an
// active status (idle/working/done/…) reads alive without any dependency on
// matching a process name or reading GC_SESSION_ID out of the agent's
// environment. Only an explicit terminal status reads dead, and unknown/empty
// statuses fail safe toward alive (a live singleton misread as dead is the
// destructive direction this fix exists to prevent).
func TestAgentAliveFromStatus(t *testing.T) {
	alive := []string{"idle", "working", "done", "running", "Idle", "  WORKING ", "thinking", "busy", "", "  "}
	for _, s := range alive {
		if !agentAliveFromStatus(s) {
			t.Errorf("agentAliveFromStatus(%q) = false; want true (active/unknown status must read alive)", s)
		}
	}
	dead := []string{"exited", "stopped", "dead", "gone", "terminated", "closed", "crashed", "EXITED", " Stopped "}
	for _, s := range dead {
		if agentAliveFromStatus(s) {
			t.Errorf("agentAliveFromStatus(%q) = true; want false (terminal status must read dead)", s)
		}
	}
}

// TestLivenessFromAgentAbsentOrError is the required negative case: a failed
// `agent get` or an agent herdr reports absent must be neither running nor
// alive, so a genuinely-gone session is still eligible for restart.
func TestLivenessFromAgentAbsentOrError(t *testing.T) {
	if got := livenessFromAgent(agentInfo{}, false, nil); got.Running || got.Alive {
		t.Errorf("absent agent: got %+v; want Running=false Alive=false", got)
	}
	if got := livenessFromAgent(agentInfo{AgentStatus: "idle"}, false, errors.New("herdr transport failure")); got.Running || got.Alive {
		t.Errorf("query error: got %+v; want Running=false Alive=false", got)
	}
}

// TestLivenessFromAgentPresent covers the incident case: a present, idle
// singleton mayor must report Running=true and Alive=true (pre-fix it reported
// Alive=false via the process-table walk and looped). A present agent herdr
// reports terminal reads running-but-dead so the reconciler can restart it.
func TestLivenessFromAgentPresent(t *testing.T) {
	live := livenessFromAgent(agentInfo{Name: "mayor", AgentStatus: "idle"}, true, nil)
	if !live.Running || !live.Alive {
		t.Errorf("present idle mayor: got %+v; want Running=true Alive=true", live)
	}
	terminal := livenessFromAgent(agentInfo{Name: "mayor", AgentStatus: "exited"}, true, nil)
	if !terminal.Running || terminal.Alive {
		t.Errorf("present exited agent: got %+v; want Running=true Alive=false", terminal)
	}
}

// A session whose registered herdr name literally equals an agent kind must
// resolve to exactly its own pane — both when `agent get` answers directly and
// through the not-found list scan — so the fix that stops kind-named lookups
// from hitting an unnamed pane of that kind cannot over-correct into missing
// real registrations. IsRunning, ObserveLiveness and Stop all resolve through
// getAgent, so a mis-resolution here is another session's pane reported as
// this one's runtime.
func TestGetAgentResolvesASessionNamedLikeAKindToItsOwnPane(t *testing.T) {
	list := `{"result":{"agents":[` +
		`{"agent":"opencode","pane_id":"w3:p1","agent_status":"idle"},` +
		`{"agent":"opencode","name":"alpha","pane_id":"w1:p1","agent_status":"idle"},` +
		`{"agent":"opencode","name":"beta","pane_id":"w2:p1","agent_status":"idle"},` +
		`{"agent":"opencode","name":"opencode","pane_id":"w5:p1","agent_status":"idle"}]}}`

	// `agent get` answers directly.
	direct := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *'agent get'*)\n" +
		"    echo '{\"result\":{\"agent\":{\"agent\":\"opencode\",\"name\":\"opencode\",\"pane_id\":\"w5:p1\",\"agent_status\":\"idle\"}}}'\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	c := &client{session: "gc-test", bin: writeFakeHerdr(t, direct)}
	a, ok, err := c.getAgent(context.Background(), "opencode")
	if err != nil {
		t.Fatalf("getAgent(\"opencode\"): %v", err)
	}
	if !ok || a.PaneID != "w5:p1" {
		t.Errorf("getAgent(\"opencode\") = (%+v, %v); want the registered session's own pane w5:p1", a, ok)
	}

	// `agent get` answers not-found; the list scan, holding an unnamed
	// opencode pane ahead of the genuine registration, must still find it.
	fallback := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *'agent get'*)\n" +
		"    echo '{\"error\":{\"code\":\"agent_not_found\",\"message\":\"agent not found\"}}'\n" +
		"    ;;\n" +
		"  *'agent list'*)\n" +
		"    echo '" + list + "'\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	c = &client{session: "gc-test", bin: writeFakeHerdr(t, fallback)}
	a, ok, err = c.getAgent(context.Background(), "opencode")
	if err != nil {
		t.Fatalf("getAgent(\"opencode\") list scan: %v", err)
	}
	if !ok || a.PaneID != "w5:p1" {
		t.Errorf("getAgent(\"opencode\") list scan = (%+v, %v); want pane w5:p1, not an unnamed pane of the same kind", a, ok)
	}
}

// ListRunning is not complete here: a bound session whose pane binding does
// not resolve is omitted with no error. An error-free listing is therefore no
// proof of absence, and the provider must stay unattested until its listing
// reports that gap.
// Kills: a listing attestation declared while ListRunning still omits live
// sessions.
func TestListRunningIsNotAttested(t *testing.T) {
	for _, sp := range []any{(*Provider)(nil)} {
		if _, ok := sp.(runtime.ListingAttestation); ok {
			t.Errorf("%T declares runtime.ListingAttestation", sp)
		}
	}
}
