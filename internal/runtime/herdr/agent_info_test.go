package herdr

import (
	"encoding/json"
	"testing"
)

// herdr 0.7.3 keys the name as "agent"; 0.9.1 keys it as "name" and reuses
// "agent" for the kind, so two Claude panes must still decode to distinct names.
func TestAgentInfoDecodesNameAcrossHerdrVersions(t *testing.T) {
	for _, tc := range []struct {
		version, body string
		want          []string
	}{
		{"0.7.3", `[{"agent":"mayor","pane_id":"w1:p1"},{"agent":"worker-1","pane_id":"w1:p2"}]`, []string{"mayor", "worker-1"}},
		{"0.9.1", `[{"agent":"claude","name":"mayor","pane_id":"w1:p1"},{"agent":"claude","name":"worker-1","pane_id":"w1:p2"}]`, []string{"mayor", "worker-1"}},
	} {
		var got []agentInfo
		if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
			t.Fatalf("herdr %s: %v", tc.version, err)
		}
		for i, want := range tc.want {
			if got[i].Name != want || got[i].PaneID == "" {
				t.Fatalf("herdr %s entry %d = %+v; want name %q", tc.version, i, got[i], want)
			}
		}
	}
}
