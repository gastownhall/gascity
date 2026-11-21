package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestSessionNameLookupAgreesWithTheSessionPackage holds cmd/gc's
// findSessionNameByTemplate, which names the controller's agents, and
// session.FindSessionNameByTemplate, which names them for graph routing and
// sling (agentutil.LookupSessionName), to the same answer over every row shape
// either rule tells apart. A change to one rule that the other does not follow
// fails here, so the two cannot name one agent's session differently.
func TestSessionNameLookupAgreesWithTheSessionPackage(t *testing.T) {
	row := func(labels []string, meta map[string]string) beads.Bead {
		return beads.Bead{
			Title:    "worker",
			Type:     session.BeadType,
			Labels:   append([]string{session.LabelSession}, labels...),
			Metadata: meta,
		}
	}
	closed := func(b beads.Bead) beads.Bead {
		b.Status = "closed"
		return b
	}
	repairable := func(b beads.Bead) beads.Bead {
		b.Type = ""
		return b
	}
	tests := []struct {
		name     string
		rows     []beads.Bead
		template string
		want     string
	}{
		{
			name:     "a row of the template",
			rows:     []beads.Bead{row(nil, map[string]string{"template": "worker", "session_name": "s-tmpl"})},
			template: "worker",
			want:     "s-tmpl",
		},
		{
			name: "the agent's own row ahead of an earlier ad-hoc row",
			rows: []beads.Bead{
				row(nil, map[string]string{"template": "worker", "session_name": "s-adhoc", "session_origin": "manual"}),
				row([]string{"agent:worker"}, map[string]string{"template": "worker", "agent_name": "worker", "session_name": "s-managed"}),
			},
			template: "worker",
			want:     "s-managed",
		},
		{
			name: "a configured named session ahead of an earlier row",
			rows: []beads.Bead{
				row(nil, map[string]string{"template": "worker", "session_name": "s-adhoc"}),
				row(nil, map[string]string{"template": "worker", "session_name": "s-named", "configured_named_session": "true", "configured_named_identity": "worker"}),
			},
			template: "worker",
			want:     "s-named",
		},
		{
			name:     "a pool_managed row",
			rows:     []beads.Bead{row(nil, map[string]string{"template": "worker", "session_name": "s-pool", poolManagedMetadataKey: "true"})},
			template: "worker",
		},
		{
			name:     "a pool slot row",
			rows:     []beads.Bead{row(nil, map[string]string{"template": "worker", "session_name": "s-slot", "pool_slot": "1"})},
			template: "worker",
		},
		{
			name:     "an ephemeral row",
			rows:     []beads.Bead{row(nil, map[string]string{"template": "worker", "session_name": "s-eph", "session_origin": "ephemeral"})},
			template: "worker",
		},
		{
			name:     "a dependency-only row",
			rows:     []beads.Bead{row(nil, map[string]string{"template": "worker", "session_name": "s-dep", "dependency_only": "true"})},
			template: "worker",
		},
		{
			name:     "a row with a pool member session name",
			rows:     []beads.Bead{row(nil, map[string]string{"template": "worker", "session_name": "worker-2"})},
			template: "worker",
		},
		{
			name:     "a row with a pool member agent name",
			rows:     []beads.Bead{row(nil, map[string]string{"template": "worker", "agent_name": "worker-gc-3", "session_name": "s-member"})},
			template: "worker",
		},
		{
			name:     "a named row with a pool member session name",
			rows:     []beads.Bead{row(nil, map[string]string{"template": "worker", "session_name": "worker-2", "configured_named_session": "true"})},
			template: "worker",
			want:     "worker-2",
		},
		{
			name:     "a manual-marked row with a pool member session name",
			rows:     []beads.Bead{row(nil, map[string]string{"template": "worker", "session_name": "worker-2", "manual_session": "true"})},
			template: "worker",
			want:     "worker-2",
		},
		{
			name:     "a manual-origin row with a pool member session name",
			rows:     []beads.Bead{row(nil, map[string]string{"template": "worker", "session_name": "worker-2", "session_origin": "manual"})},
			template: "worker",
			want:     "worker-2",
		},
		{
			name:     "a pool slot row whose agent name is its template",
			rows:     []beads.Bead{row(nil, map[string]string{"template": "worker", "agent_name": "worker", "session_name": "s-slot", "pool_slot": "1"})},
			template: "worker",
		},
		{
			name:     "a legacy pool instance by its agent label",
			rows:     []beads.Bead{row([]string{"agent:myrig/worker-1"}, map[string]string{"template": "worker", "pool_slot": "1", "session_name": "s-legacy"})},
			template: "myrig/worker-1",
			want:     "s-legacy",
		},
		{
			name:     "a canonical singleton pool seat alone",
			rows:     []beads.Bead{row([]string{"agent:worker"}, map[string]string{"template": "worker", "agent_name": "worker", "session_name": "s-seat", poolManagedMetadataKey: "true"})},
			template: "worker",
			want:     "s-seat",
		},
		{
			name: "another row of the agent ahead of an earlier canonical seat",
			rows: []beads.Bead{
				row(nil, map[string]string{"template": "worker", "agent_name": "worker", "session_name": "s-seat", poolManagedMetadataKey: "true"}),
				row(nil, map[string]string{"agent_name": "worker", "session_name": "s-worker"}),
			},
			template: "worker",
			want:     "s-worker",
		},
		{
			name: "a template match ahead of an earlier common_name match",
			rows: []beads.Bead{
				row(nil, map[string]string{"common_name": "worker", "session_name": "s-common"}),
				row(nil, map[string]string{"template": "worker", "session_name": "s-tmpl"}),
			},
			template: "worker",
			want:     "s-tmpl",
		},
		{
			name:     "a common_name match",
			rows:     []beads.Bead{row(nil, map[string]string{"common_name": "worker", "session_name": "s-common"})},
			template: "worker",
			want:     "s-common",
		},
		{
			name:     "a closed row",
			rows:     []beads.Bead{closed(row(nil, map[string]string{"template": "worker", "agent_name": "worker", "session_name": "s-closed"}))},
			template: "worker",
		},
		{
			name:     "a row without a session name",
			rows:     []beads.Bead{row(nil, map[string]string{"template": "worker"})},
			template: "worker",
		},
		{
			name:     "a repairable row with an empty type",
			rows:     []beads.Bead{repairable(row(nil, map[string]string{"template": "worker", "session_name": "s-repairable"}))},
			template: "worker",
			want:     "s-repairable",
		},
		{
			name:     "a non-session row",
			rows:     []beads.Bead{{Title: "worker", Type: "task", Metadata: map[string]string{"template": "worker", "session_name": "s-task"}}},
			template: "worker",
		},
		{
			name:     "a row of another template",
			rows:     []beads.Bead{row(nil, map[string]string{"template": "worker", "session_name": "s-gc-99"})},
			template: "myrig/worker",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Each lookup reads its own copy of the rows: cmd/gc's repairs an
			// empty-type row in place, the session package's writes nothing.
			cmdGC := findSessionNameByTemplate(seedSessionNameLookupRows(t, tc.rows), tc.template)
			pkg := session.FindSessionNameByTemplate(beads.SessionStore{Store: seedSessionNameLookupRows(t, tc.rows)}, tc.template)
			if cmdGC != tc.want || pkg != tc.want {
				t.Errorf("lookup of %q: cmd/gc = %q, session = %q, want %q from both", tc.template, cmdGC, pkg, tc.want)
			}
		})
	}
}

// seedSessionNameLookupRows returns a store holding rows, in order, keeping a
// row's closed status and its empty type, which MemStore.Create would replace.
func seedSessionNameLookupRows(t *testing.T, rows []beads.Bead) *beads.MemStore {
	t.Helper()
	store := beads.NewMemStore()
	for _, row := range rows {
		created, err := store.Create(row)
		if err != nil {
			t.Fatalf("seeding row: %v", err)
		}
		if row.Type == "" {
			empty := ""
			if err := store.Update(created.ID, beads.UpdateOpts{Type: &empty}); err != nil {
				t.Fatalf("clearing row %s type: %v", created.ID, err)
			}
		}
		if row.Status == "closed" {
			if err := store.Close(created.ID); err != nil {
				t.Fatalf("closing row %s: %v", created.ID, err)
			}
		}
	}
	return store
}
