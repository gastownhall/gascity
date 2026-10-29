package herdr

import (
	"encoding/json"
	"testing"
)

// herdr's agent object carries two identifiers: "name" is the registered
// agent name (`agent start <name>`), omitted when nobody registered one, and
// "agent" is the detected agent kind ("claude", "opencode"), shared by every
// pane running that kind. Only the registered name is an identity: a
// distinct-name registry must decode to distinct names, and a nameless entry
// must decode as an unnamed agent, never as its kind.
func TestAgentInfoDecodesNameAcrossHerdrVersions(t *testing.T) {
	body := `[{"agent":"claude","name":"mayor","pane_id":"w1:p1"},` +
		`{"agent":"claude","name":"worker-1","pane_id":"w1:p2"},` +
		`{"agent":"opencode","pane_id":"w1:p3"}]`
	var got []agentInfo
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct{ name, kind string }{
		{"mayor", "claude"}, {"worker-1", "claude"}, {"", "opencode"},
	} {
		if got[i].Name != want.name || got[i].Kind != want.kind || got[i].PaneID == "" {
			t.Errorf("entry %d = %+v; want name %q kind %q", i, got[i], want.name, want.kind)
		}
	}
}

// The nameless shape — a pane herdr detected but nobody registered — decodes
// as unnamed on every channel agent entries arrive on: the `agent list`
// array and the `agent get` wrapper.
func TestAgentInfoNamelessEntryDecodesUnnamed(t *testing.T) {
	var list []agentInfo
	if err := json.Unmarshal([]byte(`[{"agent":"claude","name":"alpha","pane_id":"w1:p1"},{"agent":"claude","pane_id":"w1:p3"}]`), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("agent list decoded %d entries; want 2", len(list))
	}
	if list[0].Name != "alpha" || list[0].Kind != "claude" {
		t.Errorf("named entry = %+v; want name %q kind %q", list[0], "alpha", "claude")
	}
	if list[1].Name != "" || list[1].Kind != "claude" {
		t.Errorf("nameless entry = %+v; want an unnamed agent carrying kind %q", list[1], "claude")
	}

	var wrap struct {
		Agent agentInfo `json:"agent"`
	}
	if err := json.Unmarshal([]byte(`{"agent":{"agent":"claude","pane_id":"w1:p3","agent_status":"working"}}`), &wrap); err != nil {
		t.Fatal(err)
	}
	if wrap.Agent.Name != "" || wrap.Agent.Kind != "claude" || wrap.Agent.PaneID != "w1:p3" {
		t.Errorf("agent get wrapper = %+v; want an unnamed claude on w1:p3", wrap.Agent)
	}
}
