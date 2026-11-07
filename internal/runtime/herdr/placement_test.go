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
			// gascity#8933: the default session_template ("{{.Agent}}") makes a
			// town agent's runtime name just its bare agent name -- no "--" and
			// no "__" to split on at all. Falling back to workspace = name
			// would give "mayor" its own one-agent workspace instead of the
			// shared town one. town comes from the herdr session's own city
			// name.
			name:     "bare town agent name groups into town workspace",
			sessName: "mayor",
			env:      nil,
			wantWS:   "gastown",
			wantTab:  "mayor",
		},
		{
			name:     "bare town agent name with GC_ALIAS still groups into town workspace",
			sessName: "deacon",
			env:      map[string]string{"GC_ALIAS": "gastown.deacon"},
			wantWS:   "gastown",
			wantTab:  "deacon",
		},
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
			// The wisp id is a bead id minted by the pouring scope, so it carries
			// that scope's beads prefix — "gc-" only when the city's HQ prefix is
			// literally "gc". Detection must be prefix-agnostic or every other
			// city keeps raw wisp ids in its tabs.
			name:     "wisp with non-gc city prefix gets themed tab",
			sessName: "gastown__polecat-az-wisp-z0pu0",
			env:      map[string]string{"GC_RIG": "gascity", "GC_ALIAS": "gascity/gastown.furiosa"},
			wantWS:   "gascity",
			wantTab:  "polecat-furiosa",
		},
		{
			name:     "wisp id with no role prefix is replaced whole",
			sessName: "gastown__az-wisp-z0pu0",
			env:      map[string]string{"GC_RIG": "gascity", "GC_ALIAS": "gascity/gastown.furiosa"},
			wantWS:   "gascity",
			wantTab:  "furiosa",
		},
		{
			// The deferred pool spawn blanks GC_ALIAS and leaves the session name
			// in GC_AGENT (build_desired_state_pool_info.go); once the alias
			// resolves the reconciler repopulates it.
			name:     "non-gc wisp falls back to GC_AGENT when alias not yet resolved",
			sessName: "gastown__polecat-az-wisp-g8ha3",
			env:      map[string]string{"GC_RIG": "gascity", "GC_ALIAS": "", "GC_AGENT": "gascity/gastown.nux"},
			wantWS:   "gascity",
			wantTab:  "polecat-nux",
		},
		{
			// Guard case: the deferred fallback hands back the session name
			// itself, which still carries the wisp id. Substituting it would
			// produce "polecat-gastown__polecat-az-wisp-z0pu0".
			name:     "non-gc alias that is itself the wisp identity is ignored",
			sessName: "gastown__polecat-az-wisp-z0pu0",
			env:      map[string]string{"GC_RIG": "gascity", "GC_ALIAS": "", "GC_AGENT": "gastown__polecat-az-wisp-z0pu0"},
			wantWS:   "gascity",
			wantTab:  "polecat-az-wisp-z0pu0",
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
			gotWS, gotTab := placementFor(tt.sessName, tt.env, "gastown")
			if gotWS != tt.wantWS || gotTab != tt.wantTab {
				t.Errorf("placementFor(%q, %v, %q) = (%q, %q), want (%q, %q)",
					tt.sessName, tt.env, "gastown", gotWS, gotTab, tt.wantWS, tt.wantTab)
			}
		})
	}
}

// TestWorkspaceTabFor pins workspaceTabFor directly (gascity#8933): a bare
// town-agent name (the default session_template, "{{.Agent}}", produces
// names with neither "--" nor "__") must resolve to the shared town
// workspace, not one named after the agent itself. The rig-qualified case is
// a regression guard that the grouping that already worked correctly for rig
// agents is unchanged.
func TestWorkspaceTabFor(t *testing.T) {
	tests := []struct {
		name     string
		sessName string
		town     string
		wantWS   string
		wantTab  string
	}{
		{
			name:     "bare town agent name falls back to the town workspace",
			sessName: "mayor",
			town:     "gastown",
			wantWS:   "gastown",
			wantTab:  "mayor",
		},
		{
			name:     "rig-qualified name is unaffected by town",
			sessName: "webapp--gastown__refinery",
			town:     "gastown",
			wantWS:   "webapp",
			wantTab:  "refinery",
		},
		{
			name:     "town-qualified name is unaffected by town",
			sessName: "gastown__mayor",
			town:     "gastown",
			wantWS:   "gastown",
			wantTab:  "mayor",
		},
		{
			name:     "bare name with no town falls back to the name itself",
			sessName: "mayor",
			town:     "",
			wantWS:   "mayor",
			wantTab:  "mayor",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotWS, gotTab := workspaceTabFor(tt.sessName, tt.town)
			if gotWS != tt.wantWS || gotTab != tt.wantTab {
				t.Errorf("workspaceTabFor(%q, %q) = (%q, %q), want (%q, %q)",
					tt.sessName, tt.town, gotWS, gotTab, tt.wantWS, tt.wantTab)
			}
		})
	}
}
