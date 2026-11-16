package herdr

import "testing"

// testTown stands in for the city name the Provider holds as its herdr session.
const testTown = "my-city"

// TestPlacementFor covers how a runtime session name + reconciler env map to a
// herdr workspace and tab. The wisp cases are the reason this exists: their
// town-qualified, wisp-id runtime name must be lifted into the originating rig
// workspace under a themed tab.
func TestPlacementFor(t *testing.T) {
	tests := []struct {
		name     string
		sessName string
		town     string // the herdr session name (the city); "" means testTown
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
		{
			// gascity#8933: the default session_template is {{.Agent}}, so a
			// town-level agent's runtime name carries neither "--" nor "__" and no
			// GC_RIG. It must land in the one shared town workspace (the city name),
			// not in a workspace named after itself.
			name:     "default-template town agent shares the town workspace",
			sessName: "mayor",
			env:      map[string]string{"GC_ALIAS": "mayor"},
			wantWS:   testTown,
			wantTab:  "mayor",
		},
		{
			name:     "second default-template town agent lands in the same workspace",
			sessName: "generalist-builder",
			env:      map[string]string{"GC_ALIAS": "generalist-builder"},
			wantWS:   testTown,
			wantTab:  "generalist-builder",
		},
		{
			name:     "default-template town agent with no env shares the town workspace",
			sessName: "mayor",
			env:      nil,
			wantWS:   testTown,
			wantTab:  "mayor",
		},
		{
			// Control: a bare name WITH a GC_RIG is a rig agent and keeps grouping
			// under its rig, exactly as before the town fallback existed.
			name:     "default-template rig agent still groups under its rig",
			sessName: "claude-1",
			env:      map[string]string{"GC_RIG": "my-rig", "GC_ALIAS": "my-rig/claude-1"},
			wantWS:   "my-rig",
			wantTab:  "claude-1",
		},
		{
			name:     "default-template rig wisp still groups under its rig with a themed tab",
			sessName: "claude-gm-wisp-sf4qr",
			env:      map[string]string{"GC_RIG": "my-rig", "GC_ALIAS": "my-rig/furiosa"},
			wantWS:   "my-rig",
			wantTab:  "claude-furiosa",
		},
		{
			name:     "town-qualified name keeps its structural town workspace",
			sessName: "gastown__mayor",
			town:     "other-city",
			env:      map[string]string{"GC_ALIAS": "gastown.mayor"},
			wantWS:   "gastown",
			wantTab:  "mayor",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			town := tt.town
			if town == "" {
				town = testTown
			}
			gotWS, gotTab := placementFor(tt.sessName, town, tt.env)
			if gotWS != tt.wantWS || gotTab != tt.wantTab {
				t.Errorf("placementFor(%q, %q, %v) = (%q, %q), want (%q, %q)",
					tt.sessName, town, tt.env, gotWS, gotTab, tt.wantWS, tt.wantTab)
			}
		})
	}
}

// TestWorkspaceTabFor pins the structural half of placement. A runtime name
// with neither "--" nor "__" is a town-level agent under the default
// session_template ({{.Agent}}); it belongs in the town workspace (the herdr
// session name), not in a workspace named after the agent (gascity#8933). Names
// that do carry a separator keep the placement they always had.
func TestWorkspaceTabFor(t *testing.T) {
	tests := []struct {
		name     string
		sessName string
		town     string
		wantWS   string
		wantTab  string
	}{
		{name: "bare town agent goes to the town workspace", sessName: "mayor", town: testTown, wantWS: testTown, wantTab: "mayor"},
		{name: "another bare town agent goes to the same workspace", sessName: "sie-broker", town: testTown, wantWS: testTown, wantTab: "sie-broker"},
		{name: "town-qualified name keeps its town segment", sessName: "gastown__mayor", town: testTown, wantWS: "gastown", wantTab: "mayor"},
		{name: "rig-qualified name keeps its rig", sessName: "webapp--gastown__witness", town: testTown, wantWS: "webapp", wantTab: "witness"},
		{name: "rig-qualified name without a town segment keeps its rig", sessName: "webapp--witness", town: testTown, wantWS: "webapp", wantTab: "witness"},
		{name: "no town known falls back to the name, as before", sessName: "mayor", town: "", wantWS: "mayor", wantTab: "mayor"},
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
