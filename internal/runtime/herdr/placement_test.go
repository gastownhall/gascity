package herdr

import "testing"

// TestPlacementFor covers how a runtime session name + reconciler env map to a
// herdr workspace and tab. The wisp cases are the reason this exists: their
// town-qualified, wisp-id runtime name must be lifted into the originating rig
// workspace under a themed tab.
func TestPlacementFor(t *testing.T) {
	tests := []struct {
		name     string
		sessName string
		env      map[string]string
		wantWS   string
		wantTab  string
	}{
		{
			name:     "wisp gets rig workspace and themed tab",
			sessName: "gastown__polecat-gc-wisp-3nvj3yx",
			env:      map[string]string{"GC_RIG": "webapp", "GC_ALIAS": "webapp/gastown.furiosa"},
			wantWS:   "webapp",
			wantTab:  "polecat-furiosa",
		},
		{
			name:     "wisp on mobile with bare alias",
			sessName: "gastown__polecat-gc-wisp-abc123",
			env:      map[string]string{"GC_RIG": "mobile", "GC_ALIAS": "nux"},
			wantWS:   "mobile",
			wantTab:  "polecat-nux",
		},
		{
			name:     "wisp falls back to GC_AGENT when no alias",
			sessName: "gastown__polecat-gc-wisp-abc123",
			env:      map[string]string{"GC_RIG": "webapp", "GC_AGENT": "webapp/gastown.slit"},
			wantWS:   "webapp",
			wantTab:  "polecat-slit",
		},
		{
			name:     "wisp with no alias yet keeps wisp tab but still moves to rig",
			sessName: "gastown__polecat-gc-wisp-abc123",
			env:      map[string]string{"GC_RIG": "webapp"},
			wantWS:   "webapp",
			wantTab:  "polecat-gc-wisp-abc123",
		},
		{
			name:     "alias that is itself the wisp identity is ignored",
			sessName: "gastown__polecat-gc-wisp-abc123",
			env:      map[string]string{"GC_RIG": "webapp", "GC_AGENT": "gastown.polecat-gc-wisp-abc123"},
			wantWS:   "webapp",
			wantTab:  "polecat-gc-wisp-abc123",
		},
		{
			name:     "persistent rig agent unchanged",
			sessName: "webapp--gastown__witness",
			env:      map[string]string{"GC_RIG": "webapp", "GC_ALIAS": "webapp/gastown.witness"},
			wantWS:   "webapp",
			wantTab:  "witness",
		},
		{
			name:     "town-level agent has no rig and stays in town",
			sessName: "gastown__mayor",
			env:      map[string]string{"GC_ALIAS": "gastown.mayor"},
			wantWS:   "gastown",
			wantTab:  "mayor",
		},
		{
			name:     "no env falls back to structural name parsing",
			sessName: "webapp--gastown__refinery",
			env:      nil,
			wantWS:   "webapp",
			wantTab:  "refinery",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotWS, gotTab := placementFor(tt.sessName, tt.env)
			if gotWS != tt.wantWS || gotTab != tt.wantTab {
				t.Errorf("placementFor(%q, %v) = (%q, %q), want (%q, %q)",
					tt.sessName, tt.env, gotWS, gotTab, tt.wantWS, tt.wantTab)
			}
		})
	}
}
