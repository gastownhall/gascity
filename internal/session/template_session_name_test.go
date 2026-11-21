package session

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// templateSessionRow is a session row of the shape the session manager
// creates: the session type, the gc:session and template:<template> labels,
// and the template metadata, plus the fields a case adds.
func templateSessionRow(template string, metadata map[string]string, labels ...string) beads.Bead {
	meta := map[string]string{"template": template}
	for k, v := range metadata {
		meta[k] = v
	}
	return beads.Bead{
		Title:    template,
		Type:     BeadType,
		Labels:   append([]string{LabelSession, "template:" + template}, labels...),
		Metadata: meta,
	}
}

func TestFindSessionNameByTemplate(t *testing.T) {
	tests := []struct {
		name     string
		rows     []beads.Bead
		template string
		want     string
	}{
		{
			name:     "the stored name of the agent's own row",
			rows:     []beads.Bead{templateSessionRow("frontend/worker", map[string]string{"agent_name": "frontend/worker", "session_name": "s-stored"})},
			template: "frontend/worker",
			want:     "s-stored",
		},
		{
			name:     "a row of the template without an agent name",
			rows:     []beads.Bead{templateSessionRow("frontend/worker", map[string]string{"session_name": "s-stored"})},
			template: "frontend/worker",
			want:     "s-stored",
		},
		{
			name: "the agent's own row answers ahead of an earlier ad-hoc row of its template",
			rows: []beads.Bead{
				templateSessionRow("worker", map[string]string{"session_name": "s-adhoc", "session_origin": "manual"}),
				templateSessionRow("worker", map[string]string{"agent_name": "worker", "session_name": "s-managed"}, "agent:worker"),
			},
			template: "worker",
			want:     "s-managed",
		},
		{
			name: "a configured named session answers ahead of an earlier row",
			rows: []beads.Bead{
				templateSessionRow("frontend/worker", map[string]string{"session_name": "s-adhoc"}),
				templateSessionRow("frontend/worker", map[string]string{
					"session_name":               "s-named",
					NamedSessionMetadataKey:      "true",
					NamedSessionIdentityMetadata: "frontend/worker",
				}),
			},
			template: "frontend/worker",
			want:     "s-named",
		},
		{
			name:     "a pool_managed row does not answer a template lookup",
			rows:     []beads.Bead{templateSessionRow("worker", map[string]string{"session_name": "s-pool", "pool_managed": "true"})},
			template: "worker",
		},
		{
			name:     "a pool slot row does not answer a template lookup",
			rows:     []beads.Bead{templateSessionRow("worker", map[string]string{"session_name": "s-slot", "pool_slot": "1"})},
			template: "worker",
		},
		{
			name:     "an ephemeral row does not answer a template lookup",
			rows:     []beads.Bead{templateSessionRow("worker", map[string]string{"session_name": "s-eph", "session_origin": "ephemeral"})},
			template: "worker",
		},
		{
			name:     "a dependency-only row does not answer a template lookup",
			rows:     []beads.Bead{templateSessionRow("worker", map[string]string{"session_name": "s-dep", "dependency_only": "true"})},
			template: "worker",
		},
		{
			name:     "a row named like a pool member of its template does not answer",
			rows:     []beads.Bead{templateSessionRow("worker", map[string]string{"session_name": "worker-2"})},
			template: "worker",
		},
		{
			name:     "a manual row named like a pool member answers",
			rows:     []beads.Bead{templateSessionRow("worker", map[string]string{"session_name": "worker-2", "session_origin": "manual"})},
			template: "worker",
			want:     "worker-2",
		},
		{
			name:     "a pool slot row whose agent name is its template does not answer the agent lookup",
			rows:     []beads.Bead{templateSessionRow("worker", map[string]string{"agent_name": "worker", "session_name": "s-slot", "pool_slot": "1"})},
			template: "worker",
		},
		{
			name: "a legacy pool instance answers for its agent label",
			rows: []beads.Bead{{
				Title:    "worker",
				Type:     BeadType,
				Labels:   []string{LabelSession, "agent:myrig/worker-1"},
				Metadata: map[string]string{"template": "worker", "pool_slot": "1", "session_name": "s-legacy-worker-1"},
			}},
			template: "myrig/worker-1",
			want:     "s-legacy-worker-1",
		},
		{
			name: "a canonical singleton pool seat answers when nothing else does",
			rows: []beads.Bead{templateSessionRow("cashmaster/refinery", map[string]string{
				"agent_name":   "cashmaster/refinery",
				"session_name": "s-canonical-refinery",
				"pool_managed": "true",
			}, "agent:cashmaster/refinery")},
			template: "cashmaster/refinery",
			want:     "s-canonical-refinery",
		},
		{
			name: "another row of the agent answers ahead of an earlier canonical seat",
			rows: []beads.Bead{
				templateSessionRow("worker", map[string]string{"agent_name": "worker", "session_name": "s-pool", "pool_managed": "true"}),
				{Title: "worker", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"agent_name": "worker", "session_name": "s-worker"}},
			},
			template: "worker",
			want:     "s-worker",
		},
		{
			name: "a common_name match answers last",
			rows: []beads.Bead{{
				Title:    "worker",
				Type:     BeadType,
				Labels:   []string{LabelSession},
				Metadata: map[string]string{"common_name": "worker", "session_name": "s-common"},
			}},
			template: "worker",
			want:     "s-common",
		},
		{
			name:     "a row of another template does not answer",
			rows:     []beads.Bead{templateSessionRow("worker", map[string]string{"session_name": "s-gc-99"})},
			template: "myrig/worker",
		},
		{
			name:     "a row without a session name does not answer",
			rows:     []beads.Bead{templateSessionRow("worker", nil)},
			template: "worker",
		},
		{
			name:     "a non-session row does not answer",
			rows:     []beads.Bead{{Title: "worker", Type: "task", Metadata: map[string]string{"template": "worker", "session_name": "s-task"}}},
			template: "worker",
		},
		{
			name:     "a blank template answers nothing",
			rows:     []beads.Bead{templateSessionRow("worker", map[string]string{"session_name": "s-worker"})},
			template: "  ",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			for _, row := range tc.rows {
				if _, err := store.Create(row); err != nil {
					t.Fatalf("creating row %q: %v", row.Title, err)
				}
			}
			if got := FindSessionNameByTemplate(beadstest.Sessions(store), tc.template); got != tc.want {
				t.Errorf("FindSessionNameByTemplate(%q) = %q, want %q", tc.template, got, tc.want)
			}
		})
	}
}

// TestFindSessionNameByTemplateSkipsClosedRows pins that only an open row
// answers: a closed row of the agent keeps its session_name, but no live
// session carries it.
func TestFindSessionNameByTemplateSkipsClosedRows(t *testing.T) {
	store := beads.NewMemStore()
	row, err := store.Create(templateSessionRow("worker", map[string]string{"agent_name": "worker", "session_name": "s-closed"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(row.ID); err != nil {
		t.Fatal(err)
	}
	if got := FindSessionNameByTemplate(beadstest.Sessions(store), "worker"); got != "" {
		t.Errorf("FindSessionNameByTemplate(worker) = %q over a closed row, want none", got)
	}
}

func TestFindSessionNameByTemplateZeroStoreAnswersNothing(t *testing.T) {
	if got := FindSessionNameByTemplate(beads.SessionStore{}, "worker"); got != "" {
		t.Errorf("FindSessionNameByTemplate over the zero store = %q, want none", got)
	}
}

// targetedOnlySessionStore fails the test on a list that would read every
// session row (the gc:session label with no metadata selector), and can fail
// the listings that select one metadata key.
type targetedOnlySessionStore struct {
	*beads.MemStore
	t       *testing.T
	failKey string
}

func (s targetedOnlySessionStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == LabelSession && len(query.Metadata) == 0 {
		s.t.Fatalf("the template session-name lookup scanned every session row: %+v", query)
	}
	if _, ok := query.Metadata[s.failKey]; ok && s.failKey != "" {
		return nil, errors.New("listing failed")
	}
	return s.MemStore.List(query)
}

// TestFindSessionNameByTemplateReadsTargetedRowsAndWritesNothing pins the
// lookup's cost and side effects: it selects rows by metadata or the agent
// label, never by scanning the session label, and a repairable row (empty
// type, gc:session label) answers without the lookup repairing it.
func TestFindSessionNameByTemplateReadsTargetedRowsAndWritesNothing(t *testing.T) {
	mem := beads.NewMemStore()
	row, err := mem.Create(beads.Bead{
		Title:    "worker",
		Labels:   []string{LabelSession},
		Metadata: map[string]string{"template": "worker", "session_name": "s-repairable"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// MemStore.Create defaults an empty type to "task"; clear it so the row
	// has the repairable shape.
	empty := ""
	if err := mem.Update(row.ID, beads.UpdateOpts{Type: &empty}); err != nil {
		t.Fatal(err)
	}
	store := targetedOnlySessionStore{MemStore: mem, t: t}

	if got := FindSessionNameByTemplate(beads.SessionStore{Store: store}, "worker"); got != "s-repairable" {
		t.Fatalf("FindSessionNameByTemplate(worker) = %q, want the repairable row's s-repairable", got)
	}
	after, err := mem.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Type != "" {
		t.Errorf("row %s type = %q after the lookup, want it left empty: the lookup reads, it does not repair", row.ID, after.Type)
	}
}

// TestFindSessionNameByTemplateAsksTheNextLookupWhenAListingFails pins the
// read-error rule: a lookup whose listing fails answers nothing, and the next
// lookup is still asked.
func TestFindSessionNameByTemplateAsksTheNextLookupWhenAListingFails(t *testing.T) {
	mem := beads.NewMemStore()
	if _, err := mem.Create(templateSessionRow("worker", map[string]string{"agent_name": "worker", "session_name": "s-worker"})); err != nil {
		t.Fatal(err)
	}
	store := targetedOnlySessionStore{MemStore: mem, t: t, failKey: "agent_name"}

	if got := FindSessionNameByTemplate(beads.SessionStore{Store: store}, "worker"); got != "s-worker" {
		t.Errorf("FindSessionNameByTemplate(worker) with the agent_name listing failing = %q, want the template lookup's s-worker", got)
	}
}
