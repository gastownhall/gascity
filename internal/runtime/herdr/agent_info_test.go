package herdr

import (
	"encoding/json"
	"testing"
)

// herdr's agent object carries two identifiers: "name" is the registered
// agent name (`agent start <name>`), which herdr omits for a pane whose agent
// it detected but nobody named; "agent" is the detected kind ("claude",
// "opencode"), shared by every pane running that kind. Only the registered
// name is an identity, so a nameless entry must decode as unnamed.
func TestAgentInfoDecodesNameAcrossHerdrVersions(t *testing.T) {
	for _, tc := range []struct {
		version, body string
		want          []agentInfo
	}{
		{
			"0.9.1", `[{"agent":"claude","name":"mayor","pane_id":"w1:p1"},{"agent":"opencode","name":"worker-1","pane_id":"w1:p2"}]`,
			[]agentInfo{{Name: "mayor", Kind: "claude", PaneID: "w1:p1"}, {Name: "worker-1", Kind: "opencode", PaneID: "w1:p2"}},
		},
		{
			"0.9.1 nameless", `[{"agent":"claude","pane_id":"w1:p3"}]`,
			[]agentInfo{{Kind: "claude", PaneID: "w1:p3"}},
		},
	} {
		var got []agentInfo
		if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
			t.Fatalf("herdr %s: %v", tc.version, err)
		}
		for i, want := range tc.want {
			if got[i] != want {
				t.Fatalf("herdr %s entry %d = %+v; want %+v", tc.version, i, got[i], want)
			}
		}
	}
}

// A nameless 0.9.1 entry — herdr detected an agent on the pane but nobody
// registered a name — decodes as unnamed through every shape gc reads it in:
// the agent.list array and the `agent get`/`agent start` object wrapper.
func TestAgentInfoNamelessEntryDecodesUnnamed(t *testing.T) {
	const list = `{"agents":[` +
		`{"agent":"opencode","name":"alpha","pane_id":"w1:p1"},` +
		`{"agent":"claude","pane_id":"w1:p3"}]}`
	var wrap struct {
		Agents []agentInfo `json:"agents"`
	}
	if err := json.Unmarshal([]byte(list), &wrap); err != nil {
		t.Fatalf("agent list decode: %v", err)
	}
	if len(wrap.Agents) != 2 {
		t.Fatalf("decoded %d entries; want 2: %+v", len(wrap.Agents), wrap.Agents)
	}
	if wrap.Agents[0].Name != "alpha" || wrap.Agents[0].Kind != "opencode" {
		t.Errorf("named entry = %+v; want name alpha, kind opencode", wrap.Agents[0])
	}
	if wrap.Agents[1].Name != "" || wrap.Agents[1].Kind != "claude" {
		t.Errorf("nameless entry = %+v; want empty name — the detected kind is never an identity — and kind claude", wrap.Agents[1])
	}

	const get = `{"agent":{"agent":"claude","pane_id":"w1:p3"}}`
	var single struct {
		Agent agentInfo `json:"agent"`
	}
	if err := json.Unmarshal([]byte(get), &single); err != nil {
		t.Fatalf("agent get decode: %v", err)
	}
	if single.Agent.Name != "" || single.Agent.Kind != "claude" {
		t.Errorf("nameless get wrapper = %+v; want empty name, kind claude", single.Agent)
	}
}
