package formula

import "testing"

func TestReferencedVarDefs(t *testing.T) {
	tests := []struct {
		name     string
		recipe   *Recipe
		wantVars []string // sorted var names in result
	}{
		{
			name: "no vars",
			recipe: &Recipe{
				Steps: []RecipeStep{{Title: "hello"}},
			},
			wantVars: nil,
		},
		{
			name: "var in title",
			recipe: &Recipe{
				Steps: []RecipeStep{{Title: "Fix {{component}}"}},
				Vars: map[string]*VarDef{
					"component": {Required: true},
				},
			},
			wantVars: []string{"component"},
		},
		{
			name: "var in description",
			recipe: &Recipe{
				Steps: []RecipeStep{{Description: "Assigned to {{owner}}"}},
				Vars: map[string]*VarDef{
					"owner": {Required: true},
				},
			},
			wantVars: []string{"owner"},
		},
		{
			name: "var in notes",
			recipe: &Recipe{
				Steps: []RecipeStep{{Notes: "See {{ref}}"}},
				Vars: map[string]*VarDef{
					"ref": {},
				},
			},
			wantVars: []string{"ref"},
		},
		{
			name: "var in assignee",
			recipe: &Recipe{
				Steps: []RecipeStep{{Assignee: "{{agent}}"}},
				Vars: map[string]*VarDef{
					"agent": {Required: true},
				},
			},
			wantVars: []string{"agent"},
		},
		{
			name: "var in metadata",
			recipe: &Recipe{
				Steps: []RecipeStep{{Metadata: map[string]string{"gc.scope": "{{scope}}"}}},
				Vars: map[string]*VarDef{
					"scope": {},
				},
			},
			wantVars: []string{"scope"},
		},
		{
			name: "compile-time-only var excluded",
			recipe: &Recipe{
				Steps: []RecipeStep{{Title: "Fix {{component}}"}},
				Vars: map[string]*VarDef{
					"component":  {Required: true},
					"enable_fix": {Required: true}, // condition-only, not in any step text
				},
			},
			wantVars: []string{"component"},
		},
		{
			name: "multiple steps and vars",
			recipe: &Recipe{
				Steps: []RecipeStep{
					{Title: "{{title}}", Description: "{{desc}}"},
					{Title: "Sub-step", Assignee: "{{agent}}"},
				},
				Vars: map[string]*VarDef{
					"title":       {Required: true},
					"desc":        {},
					"agent":       {Required: true},
					"unused_cond": {Required: true},
				},
			},
			wantVars: []string{"agent", "desc", "title"},
		},
		{
			name: "empty steps",
			recipe: &Recipe{
				Vars: map[string]*VarDef{
					"orphan": {Required: true},
				},
			},
			wantVars: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.recipe.ReferencedVarDefs()

			if tt.wantVars == nil {
				if len(got) != 0 {
					t.Errorf("ReferencedVarDefs() returned %d vars, want 0", len(got))
				}
				return
			}

			if len(got) != len(tt.wantVars) {
				t.Errorf("ReferencedVarDefs() returned %d vars, want %d", len(got), len(tt.wantVars))
			}
			for _, name := range tt.wantVars {
				if _, ok := got[name]; !ok {
					t.Errorf("ReferencedVarDefs() missing var %q", name)
				}
			}
		})
	}
}

func TestCollectVarRefs(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"no vars here", nil},
		{"{{foo}}", []string{"foo"}},
		{"a {{b}} c {{d}}", []string{"b", "d"}},
		{"{{a_1}} and {{B_2}}", []string{"a_1", "B_2"}},
		{"{{ bad }}", nil},  // spaces not valid
		{"{{123bad}}", nil}, // starts with digit
		{"{{ok}}{{also_ok}}", []string{"ok", "also_ok"}},
		{"unclosed {{foo", nil},
		{"{{}}", nil}, // empty name
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			seen := make(map[string]bool)
			collectVarRefs(tt.input, seen)

			if tt.want == nil {
				if len(seen) != 0 {
					t.Errorf("collectVarRefs(%q) found %v, want none", tt.input, seen)
				}
				return
			}

			if len(seen) != len(tt.want) {
				t.Errorf("collectVarRefs(%q) found %d refs, want %d", tt.input, len(seen), len(tt.want))
			}
			for _, name := range tt.want {
				if !seen[name] {
					t.Errorf("collectVarRefs(%q) missing %q", tt.input, name)
				}
			}
		})
	}
}
