package herdr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// herdr's agent object carries two identifiers: "name" is the registered agent
// name, omitted for a pane nobody named, and "agent" is the detected kind every
// pane of that kind shares. Two Claude panes must decode to distinct names, and
// a nameless pane must decode as unnamed rather than as its kind.
func TestAgentInfoDecodesNameAcrossHerdrVersions(t *testing.T) {
	type want struct{ name, kind string }
	for _, tc := range []struct {
		label, body string
		want        []want
	}{
		{"named", `[{"agent":"claude","name":"mayor","pane_id":"w1:p1"},{"agent":"claude","name":"worker-1","pane_id":"w1:p2"}]`, []want{{"mayor", "claude"}, {"worker-1", "claude"}}},
		{"nameless", `[{"agent":"claude","pane_id":"w1:p3"}]`, []want{{"", "claude"}}},
	} {
		var got []agentInfo
		if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
			t.Fatalf("%s: %v", tc.label, err)
		}
		for i, w := range tc.want {
			if got[i].Name != w.name || got[i].Kind != w.kind || got[i].PaneID == "" {
				t.Fatalf("%s entry %d = %+v; want name %q kind %q", tc.label, i, got[i], w.name, w.kind)
			}
		}
	}
}

// Every path gc reads herdr's agent object through decodes the same contract:
// `agent list`, the `agent get` wrapper and the socket agent.list.
func TestAgentInfoNamelessEntryDecodesUnnamed(t *testing.T) {
	const (
		named   = `{"agent":"opencode","name":"alpha","pane_id":"w1:p1","agent_status":"working"}`
		unnamed = `{"agent":"claude","pane_id":"w1:p3","agent_status":"idle"}`
	)
	check := func(t *testing.T, via string, got agentInfo, name, kind, pane string) {
		t.Helper()
		if got.Name != name || got.Kind != kind || got.PaneID != pane {
			t.Errorf("%s decoded %+v; want name %q kind %q pane %q", via, got, name, kind, pane)
		}
	}
	ctx := context.Background()

	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *'agent get w1:p1'*) echo '{\"result\":{\"agent\":" + named + "}}' ;;\n" +
		"  *'agent get w1:p3'*) echo '{\"result\":{\"agent\":" + unnamed + "}}' ;;\n" +
		"  *'agent list'*) echo '{\"result\":{\"agents\":[" + named + "," + unnamed + "]}}' ;;\n" +
		"esac\n" +
		"exit 0\n"
	c := &client{session: "gc-test", bin: writeFakeHerdr(t, script)}

	list, err := c.listAgents(ctx)
	if err != nil {
		t.Fatalf("listAgents: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("listAgents = %+v; want 2 entries", list)
	}
	check(t, "agent list", list[0], "alpha", "opencode", "w1:p1")
	check(t, "agent list", list[1], "", "claude", "w1:p3")

	for _, tc := range []struct{ pane, name, kind string }{
		{"w1:p1", "alpha", "opencode"},
		{"w1:p3", "", "claude"},
	} {
		got, ok, err := c.getAgent(ctx, tc.pane)
		if err != nil || !ok {
			t.Fatalf("getAgent(%q) = ok %v, err %v", tc.pane, ok, err)
		}
		check(t, "agent get", got, tc.name, tc.kind, tc.pane)
	}

	f, sock := newFakeHerdrServer(t)
	f.setAgentsJSON(named, unnamed)
	sockList, err := eventTestProvider(t, sock).c.sockAgentList(ctx)
	if err != nil {
		t.Fatalf("sockAgentList: %v", err)
	}
	if len(sockList) != 2 {
		t.Fatalf("sockAgentList = %+v; want 2 entries", sockList)
	}
	check(t, "socket agent.list", sockList[0], "alpha", "opencode", "w1:p1")
	check(t, "socket agent.list", sockList[1], "", "claude", "w1:p3")
}

// Literal herdr agent-list entries: two registered sessions and an unnamed
// pane, all running opencode.
const (
	unnamedOpencodeEntry = `{"agent":"opencode","pane_id":"w3:p1","agent_status":"working"}`
	alphaEntry           = `{"agent":"opencode","name":"alpha","pane_id":"w1:p1","agent_status":"working"}`
	betaEntry            = `{"agent":"opencode","name":"beta","pane_id":"w2:p1","agent_status":"working"}`
)

// lookupFake writes a fake herdr whose `agent get` answers with get, or with
// herdr's not-found envelope when get is "", and whose `agent list` answers
// with the literal entries. Every call's argv is appended to the returned
// trace path.
func lookupFake(t *testing.T, get string, entries ...string) (*client, string) {
	t.Helper()
	getAnswer := `{"error":{"code":"not_found","message":"no such agent"}}`
	if get != "" {
		getAnswer = `{"result":{"agent":` + get + `}}`
	}
	trace := filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> '" + trace + "'\n" +
		"case \"$*\" in\n" +
		"  *'agent get'*) echo '" + getAnswer + "' ;;\n" +
		"  *'agent list'*) echo '{\"result\":{\"agents\":[" + strings.Join(entries, ",") + "]}}' ;;\n" +
		"esac\n" +
		"exit 0\n"
	return &client{session: "gc-test", bin: writeFakeHerdr(t, script)}, trace
}

// A kind is shared by every pane running it, so a target that is only a kind
// names no session: the not-found list scan must not hand back the unnamed
// pane of that kind. An empty target names nothing at all and is not sent.
func TestGetAgentNeverResolvesAKindOrEmptyTarget(t *testing.T) {
	c, trace := lookupFake(t, "", unnamedOpencodeEntry, alphaEntry, betaEntry)
	ctx := context.Background()

	a, ok, err := c.getAgent(ctx, "")
	if err != nil || ok || a.PaneID != "" {
		t.Errorf("getAgent(\"\") = (%+v, %v, %v); want not-found with no pane", a, ok, err)
	}
	if calls, rerr := os.ReadFile(trace); rerr == nil && len(calls) > 0 {
		t.Errorf("getAgent(\"\") called herdr: %s", calls)
	}

	a, ok, err = c.getAgent(ctx, "opencode")
	if err != nil || ok || a.PaneID != "" {
		t.Errorf("getAgent(%q) = (%+v, %v, %v); want not-found with no pane", "opencode", a, ok, err)
	}
	for _, tc := range []struct{ target, pane string }{{"alpha", "w1:p1"}, {"beta", "w2:p1"}} {
		a, ok, err := c.getAgent(ctx, tc.target)
		if err != nil || !ok || a.PaneID != tc.pane {
			t.Errorf("getAgent(%q) = (%+v, %v, %v); want pane %q", tc.target, a, ok, err, tc.pane)
		}
	}
}
