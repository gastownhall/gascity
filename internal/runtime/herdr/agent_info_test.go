package herdr

import (
	"encoding/json"
	"testing"
)

// agentInfo decodes herdr's agent object: "name" is the registered agent
// name, "agent" the detected kind, which every pane running that kind shares.
// A pane nobody named has no "name" key at all and must decode as unnamed —
// the kind is never an identity.
func TestAgentInfoNamelessEntryDecodesUnnamed(t *testing.T) {
	list := `[{"agent":"opencode","name":"alpha","pane_id":"w1:p1"},` +
		`{"agent":"claude","pane_id":"w1:p3"}]`
	var got []agentInfo
	if err := json.Unmarshal([]byte(list), &got); err != nil {
		t.Fatalf("agent list decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("decoded %d entries; want 2", len(got))
	}
	if got[0].Name != "alpha" || got[0].Kind != "opencode" || got[0].PaneID != "w1:p1" {
		t.Errorf("named entry = %+v; want Name alpha, Kind opencode", got[0])
	}
	if got[1].Name != "" || got[1].Kind != "claude" || got[1].PaneID != "w1:p3" {
		t.Errorf(`nameless entry = %+v; want Name "", Kind claude`, got[1])
	}

	// `agent get` nests the same object under a one-key "agent" wrapper.
	var wrap struct {
		Agent agentInfo `json:"agent"`
	}
	if err := json.Unmarshal([]byte(`{"agent":{"agent":"claude","pane_id":"w1:p3"}}`), &wrap); err != nil {
		t.Fatalf("agent get wrapper decode: %v", err)
	}
	if wrap.Agent.Name != "" || wrap.Agent.Kind != "claude" || wrap.Agent.PaneID != "w1:p3" {
		t.Errorf(`get wrapper = %+v; want Name "", Kind claude`, wrap.Agent)
	}
}

// The registered name is the identity and the kind is shared metadata: two
// claude panes decode to distinct names, and a nameless pane decodes to none.
func TestAgentInfoDecodesNameAcrossHerdrVersions(t *testing.T) {
	for _, tc := range []struct {
		shape, body string
		want        []agentInfo
	}{
		{
			"0.9.1 named",
			`[{"agent":"claude","name":"mayor","pane_id":"w1:p1"},{"agent":"claude","name":"worker-1","pane_id":"w1:p2"}]`,
			[]agentInfo{{Name: "mayor", Kind: "claude", PaneID: "w1:p1"}, {Name: "worker-1", Kind: "claude", PaneID: "w1:p2"}},
		},
		{
			"0.9.1 nameless",
			`[{"agent":"claude","pane_id":"w1:p3"}]`,
			[]agentInfo{{Kind: "claude", PaneID: "w1:p3"}},
		},
	} {
		var got []agentInfo
		if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
			t.Fatalf("herdr %s shape: %v", tc.shape, err)
		}
		for i, want := range tc.want {
			if got[i] != want {
				t.Errorf("herdr %s entry %d = %+v; want %+v", tc.shape, i, got[i], want)
			}
		}
	}
}
