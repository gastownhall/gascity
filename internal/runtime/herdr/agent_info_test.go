package herdr

import (
	"context"
	"encoding/json"
	"testing"
)

// herdr's agent object carries two identifiers: "name" is the registered agent
// name (`agent start <name>`) and is omitted for a pane nobody named; "agent" is
// the detected kind, shared by every pane running that kind. Two Claude panes
// must decode to distinct names, and a nameless pane must decode as unnamed.
func TestAgentInfoDecodesNameAcrossHerdrVersions(t *testing.T) {
	for _, tc := range []struct {
		version, body string
		want          []agentInfo
	}{
		{
			"0.9.1", `[{"agent":"claude","name":"worker-0","pane_id":"w1:p1"},{"agent":"claude","name":"worker-1","pane_id":"w1:p2"}]`,
			[]agentInfo{{Name: "worker-0", Kind: "claude", PaneID: "w1:p1"}, {Name: "worker-1", Kind: "claude", PaneID: "w1:p2"}},
		},
		{
			"0.9.1 nameless", `[{"agent":"claude","pane_id":"w1:p3"}]`,
			[]agentInfo{{Name: "", Kind: "claude", PaneID: "w1:p3"}},
		},
	} {
		var got []agentInfo
		if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
			t.Fatalf("herdr %s: %v", tc.version, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("herdr %s: decoded %d entries; want %d", tc.version, len(got), len(tc.want))
		}
		for i, want := range tc.want {
			if got[i] != want {
				t.Fatalf("herdr %s entry %d = %+v; want %+v", tc.version, i, got[i], want)
			}
		}
	}
}

// Herdr-shaped JSON (0.9.1) for one registered pane and one detected pane that
// nobody named, as `agent list` and the socket `agent.list` report them.
const (
	namedOpencodeAlpha = `{"agent":"opencode","name":"alpha","pane_id":"w1:p1","agent_status":"working"}`
	unnamedClaudePane  = `{"agent":"claude","pane_id":"w1:p3","agent_status":"working"}`
)

// TestAgentInfoNamelessEntryDecodesUnnamed pins the decode contract on every
// path gc reads herdr's agent object from: the CLI `agent list`, the CLI
// `agent get` wrapper, and the socket `agent.list`. The kind is kept apart from
// the name, and a pane nobody registered decodes with no name at all.
func TestAgentInfoNamelessEntryDecodesUnnamed(t *testing.T) {
	alpha := agentInfo{Name: "alpha", Kind: "opencode", PaneID: "w1:p1", AgentStatus: "working"}
	unnamed := agentInfo{Name: "", Kind: "claude", PaneID: "w1:p3", AgentStatus: "working"}
	ctx := context.Background()

	list := `{"result":{"agents":[` + namedOpencodeAlpha + `,` + unnamedClaudePane + `]}}`
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *'agent get alpha'*) echo '{\"result\":{\"agent\":" + namedOpencodeAlpha + "}}' ;;\n" +
		"  *'agent get w1:p3'*) echo '{\"result\":{\"agent\":" + unnamedClaudePane + "}}' ;;\n" +
		"  *'agent list'*) echo '" + list + "' ;;\n" +
		"esac\n" +
		"exit 0\n"
	c := &client{session: "gc-test", bin: writeFakeHerdr(t, script)}

	got, err := c.listAgents(ctx)
	if err != nil {
		t.Fatalf("listAgents: %v", err)
	}
	if len(got) != 2 || got[0] != alpha || got[1] != unnamed {
		t.Errorf("agent list decoded %+v; want [%+v %+v]", got, alpha, unnamed)
	}

	for target, want := range map[string]agentInfo{"alpha": alpha, "w1:p3": unnamed} {
		a, ok, err := c.getAgent(ctx, target)
		if err != nil || !ok || a != want {
			t.Errorf("agent get %s decoded (%+v, %v, %v); want %+v", target, a, ok, err, want)
		}
	}

	f, sock := newFakeHerdrServer(t)
	f.setRawAgents(namedOpencodeAlpha, unnamedClaudePane)
	sc := newClient("gc-test", "")
	sc.sockPath = sock
	got, err = sc.sockAgentList(ctx)
	if err != nil {
		t.Fatalf("sockAgentList: %v", err)
	}
	if len(got) != 2 || got[0] != alpha || got[1] != unnamed {
		t.Errorf("socket agent.list decoded %+v; want [%+v %+v]", got, alpha, unnamed)
	}
}
