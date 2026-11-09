package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// scopeSpyStore embeds a MemStore (so it satisfies the full beads.Store
// interface) and records every List query liveWorkAssignmentAssigneeMatches
// emits while delegating to the MemStore for real results.
type scopeSpyStore struct {
	*beads.MemStore
	mu      sync.Mutex
	queries []beads.ListQuery
	listErr error
}

func (s *scopeSpyStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	s.queries = append(s.queries, q)
	err := s.listErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.MemStore.List(q)
}

func newScopeSpyStore(t *testing.T, status, assignee string) (*scopeSpyStore, beads.Bead) {
	t.Helper()
	mem := beads.NewMemStore()
	b, err := mem.Create(beads.Bead{Title: "work", Assignee: assignee})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if status != "open" {
		if err := mem.Update(b.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("Update status: %v", err)
		}
	}
	return &scopeSpyStore{MemStore: mem}, b
}

// TestLiveWorkAssignmentAssigneeMatches_ScopesTheListToTheAssignee pins the
// cost fix (ga-y0ft4d): the live verification lists ONE assignee's rows, not
// every row of the status. A whole-status list costs ~1.5 s per 9k rows per
// candidate per tick.
func TestLiveWorkAssignmentAssigneeMatches_ScopesTheListToTheAssignee(t *testing.T) {
	store, b := newScopeSpyStore(t, "open", "worker-a")

	ok, err := liveWorkAssignmentAssigneeMatches(store, b.ID, "open", "  worker-a ")
	if err != nil || !ok {
		t.Fatalf("liveWorkAssignmentAssigneeMatches = (%v, %v), want (true, nil)", ok, err)
	}

	want := beads.ListQuery{Assignee: "worker-a", Status: "open", Live: true, TierMode: beads.TierBoth}
	if len(store.queries) != 1 {
		t.Fatalf("expected exactly 1 List call, got %d: %#v", len(store.queries), store.queries)
	}
	if !reflect.DeepEqual(store.queries[0], want) {
		t.Fatalf("List query mismatch:\n got  %#v\n want %#v", store.queries[0], want)
	}
}

// TestLiveWorkAssignmentAssigneeMatches_EmptyAssigneeMeansNoFilter keeps the
// "unassigned expected" reading: an empty expected assignee adds no filter.
func TestLiveWorkAssignmentAssigneeMatches_EmptyAssigneeMeansNoFilter(t *testing.T) {
	store, b := newScopeSpyStore(t, "open", "")

	ok, err := liveWorkAssignmentAssigneeMatches(store, b.ID, "open", "  ")
	if err != nil || !ok {
		t.Fatalf("liveWorkAssignmentAssigneeMatches = (%v, %v), want (true, nil)", ok, err)
	}
	if len(store.queries) != 1 || store.queries[0].Assignee != "" {
		t.Fatalf("queries = %#v, want one List with an empty Assignee", store.queries)
	}
}

// TestLiveWorkAssignmentAssigneeMatches_ResultSemantics guards the result
// contract the scoping must not change.
func TestLiveWorkAssignmentAssigneeMatches_ResultSemantics(t *testing.T) {
	tests := []struct {
		name     string
		store    func(t *testing.T) (beads.Store, string)
		status   string
		assignee string
		want     bool
		wantErr  string
	}{
		{
			name: "same assignee",
			store: func(t *testing.T) (beads.Store, string) {
				s, b := newScopeSpyStore(t, "open", "worker-a")
				return s, b.ID
			},
			status: "open", assignee: "worker-a", want: true,
		},
		{
			name: "same assignee with surrounding space",
			store: func(t *testing.T) (beads.Store, string) {
				s, b := newScopeSpyStore(t, "open", "worker-a")
				return s, b.ID
			},
			status: "open", assignee: " worker-a ", want: true,
		},
		{
			name: "in_progress row matches in_progress status",
			store: func(t *testing.T) (beads.Store, string) {
				s, b := newScopeSpyStore(t, "in_progress", "worker-a")
				return s, b.ID
			},
			status: "in_progress", assignee: "worker-a", want: true,
		},
		{
			name: "different assignee",
			store: func(t *testing.T) (beads.Store, string) {
				s, b := newScopeSpyStore(t, "open", "worker-b")
				return s, b.ID
			},
			status: "open", assignee: "worker-a", want: false,
		},
		{
			name: "status differs",
			store: func(t *testing.T) (beads.Store, string) {
				s, b := newScopeSpyStore(t, "in_progress", "worker-a")
				return s, b.ID
			},
			status: "open", assignee: "worker-a", want: false,
		},
		{
			name: "absent from the scoped list",
			store: func(t *testing.T) (beads.Store, string) {
				s, _ := newScopeSpyStore(t, "open", "worker-a")
				return s, "gone-1"
			},
			status: "open", assignee: "worker-a", want: false,
		},
		{
			name:   "nil store",
			store:  func(*testing.T) (beads.Store, string) { return nil, "x-1" },
			status: "open", assignee: "worker-a", want: false,
		},
		{
			name: "empty id",
			store: func(t *testing.T) (beads.Store, string) {
				s, _ := newScopeSpyStore(t, "open", "worker-a")
				return s, "  "
			},
			status: "open", assignee: "worker-a", want: false,
		},
		{
			name: "empty status",
			store: func(t *testing.T) (beads.Store, string) {
				s, b := newScopeSpyStore(t, "open", "worker-a")
				return s, b.ID
			},
			status: " ", assignee: "worker-a", want: false,
		},
		{
			name: "list error is wrapped",
			store: func(t *testing.T) (beads.Store, string) {
				s, b := newScopeSpyStore(t, "open", "worker-a")
				s.listErr = errors.New("dolt down")
				return s, b.ID
			},
			status: "open", assignee: "worker-a", want: false,
			wantErr: "live work-assignment verification of",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, id := tt.store(t)
			got, err := liveWorkAssignmentAssigneeMatches(store, id, tt.status, tt.assignee)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "dolt down") {
					t.Fatalf("err = %v, want it to wrap %q around the list error", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("liveWorkAssignmentAssigneeMatches = %v, want %v", got, tt.want)
			}
		})
	}
}

// fakeBdRaw is one row of fakeScopedBdCLI's ledger, carrying bd's RAW stored
// status (bd has more statuses than Gas City's open/in_progress/closed) and the
// tier it lives in.
type fakeBdRaw struct {
	id, status, assignee string
	deferUntil           string
	ephemeral            bool
}

// fakeScopedBdCLI emulates the bd CLI's read surface the way bd answers it:
// `list --status=S --assignee=A` and the wisp leg's `query "ephemeral=true AND
// status=S AND assignee=A"` return only rows whose STORED status is literally S
// and whose assignee is literally A. In particular a raw blocked/deferred row is
// never returned for --status=open, though BdStore's read-side normalization
// would later fold it into "open". `show --json <id>` is supported only so a
// regression to a Get-based verification is observed (and fails) rather than
// crashing the fake. Any other command fails the test.
type fakeScopedBdCLI struct {
	t    *testing.T
	rows []fakeBdRaw

	mu   sync.Mutex
	logs []string
}

func (f *fakeScopedBdCLI) run(_, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	f.logs = append(f.logs, line)
	f.mu.Unlock()
	if len(args) == 0 {
		f.t.Errorf("unexpected bare bd command %q", line)
		return nil, errors.New("unexpected bd command")
	}
	switch args[0] {
	case "list":
		var status, assignee string
		all := false
		for _, a := range args[1:] {
			if v, ok := strings.CutPrefix(a, "--status="); ok {
				status = v
			}
			if v, ok := strings.CutPrefix(a, "--assignee="); ok {
				assignee = v
			}
			if a == "--all" {
				all = true
			}
		}
		return f.encode(f.match(false, status, assignee, all))
	case "query":
		var status, assignee string
		if len(args) < 3 {
			f.t.Errorf("malformed bd query %q", line)
			return nil, errors.New("malformed bd query")
		}
		for _, clause := range strings.Split(args[2], " AND ") {
			if v, ok := strings.CutPrefix(clause, "status="); ok {
				status = v
			}
			if v, ok := strings.CutPrefix(clause, "assignee="); ok {
				assignee = v
			}
		}
		return f.encode(f.match(true, status, assignee, false))
	case "show":
		if len(args) >= 3 && args[1] == "--json" {
			var out []fakeBdRaw
			for _, r := range f.rows {
				if r.id == args[2] {
					out = append(out, r)
				}
			}
			return f.encode(out)
		}
	}
	f.t.Errorf("unexpected bd command %q", line)
	return nil, errors.New("unexpected bd command")
}

func (f *fakeScopedBdCLI) match(ephemeral bool, status, assignee string, all bool) []fakeBdRaw {
	var out []fakeBdRaw
	for _, r := range f.rows {
		switch {
		case r.ephemeral != ephemeral:
			continue
		case status != "" && r.status != status:
			continue
		case status == "" && !all && r.status == "closed":
			continue
		case assignee != "" && r.assignee != assignee:
			continue
		}
		out = append(out, r)
	}
	return out
}

func (f *fakeScopedBdCLI) encode(rows []fakeBdRaw) ([]byte, error) {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m := map[string]any{
			"id":         r.id,
			"title":      r.id,
			"status":     r.status,
			"assignee":   r.assignee,
			"issue_type": "task",
			"created_at": "2026-09-01T00:00:00Z",
		}
		if r.deferUntil != "" {
			m["defer_until"] = r.deferUntil
		}
		out = append(out, m)
	}
	data, err := json.Marshal(out)
	if err != nil {
		f.t.Errorf("marshal fake bd rows: %v", err)
		return nil, err
	}
	return data, nil
}

func (f *fakeScopedBdCLI) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.logs...)
}

func (f *fakeScopedBdCLI) sawShow() bool {
	for _, c := range f.commands() {
		if strings.HasPrefix(c, "bd show ") || strings.HasPrefix(c, "show ") {
			return true
		}
	}
	return false
}

// sawLeg reports whether a command with the given leading verb carried every
// wanted fragment (a flag for `list`, a clause for `query`).
func (f *fakeScopedBdCLI) sawLeg(verb string, want ...string) bool {
	for _, c := range f.commands() {
		if !strings.Contains(c, " "+verb+" ") {
			continue
		}
		all := true
		for _, w := range want {
			if !strings.Contains(c, w) {
				all = false
			}
		}
		if all {
			return true
		}
	}
	return false
}

// TestLiveWorkAssignmentAssigneeMatches_DropsRawNonOpenRows runs the helper over
// the REAL BdStore. bd omits a raw blocked/deferred row from --status=open
// server side, while BdStore's read path would label that row "open"; a Get
// (bd show) would therefore see "open" and release a parked claim. The scoped
// list is the raw-status gate, so every parked shape answers (false, nil) with
// no bd show, and each negative is paired with a positive control.
func TestLiveWorkAssignmentAssigneeMatches_DropsRawNonOpenRows(t *testing.T) {
	const id, owner = "gc-w1", "worker-a"
	tests := []struct {
		name string
		row  fakeBdRaw
		want bool
	}{
		{"raw deferred without a date", fakeBdRaw{id: id, status: "deferred", assignee: owner}, false},
		{"raw deferred with a future defer_until", fakeBdRaw{id: id, status: "deferred", assignee: owner, deferUntil: "2099-01-01T00:00:00Z"}, false},
		{"raw blocked", fakeBdRaw{id: id, status: "blocked", assignee: owner}, false},
		{"control: raw open held by the expected assignee", fakeBdRaw{id: id, status: "open", assignee: owner}, true},
		{"control: raw open held by someone else", fakeBdRaw{id: id, status: "open", assignee: "worker-b"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeScopedBdCLI{t: t, rows: []fakeBdRaw{tt.row}}
			store := beads.NewBdStore("/city", fake.run)

			got, err := liveWorkAssignmentAssigneeMatches(store, id, "open", owner)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("liveWorkAssignmentAssigneeMatches = %v, want %v; bd saw %q", got, tt.want, fake.commands())
			}
			if fake.sawShow() {
				t.Fatalf("verification issued bd show; a Get un-defers a parked row: %q", fake.commands())
			}
			if !fake.sawLeg("list", "--status=open", "--assignee="+owner) {
				t.Fatalf("no `bd list` carried --status=open and --assignee=%s: %q", owner, fake.commands())
			}
		})
	}
}

// TestLiveWorkAssignmentAssigneeMatches_ReadsTheWispTier guards the TierBoth
// reach on the real BdStore: a wisp-tier row is absent from `bd list` and found
// only by the `bd query` leg, which must carry the same status and assignee
// scope.
func TestLiveWorkAssignmentAssigneeMatches_ReadsTheWispTier(t *testing.T) {
	const id, owner = "gc-wisp1", "worker-a"
	tests := []struct {
		name string
		rows []fakeBdRaw
		want bool
	}{
		{"wisp held by the expected assignee", []fakeBdRaw{{id: id, status: "open", assignee: owner, ephemeral: true}}, true},
		{"wisp held by someone else", []fakeBdRaw{{id: id, status: "open", assignee: "worker-b", ephemeral: true}}, false},
		{"wisp absent", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeScopedBdCLI{t: t, rows: tt.rows}
			store := beads.NewBdStore("/city", fake.run)

			got, err := liveWorkAssignmentAssigneeMatches(store, id, "open", owner)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("liveWorkAssignmentAssigneeMatches = %v, want %v; bd saw %q", got, tt.want, fake.commands())
			}
			if !fake.sawLeg("query", "ephemeral=true", "status=open", "assignee="+owner) {
				t.Fatalf("the wisp leg did not carry status=open and assignee=%s: %q", owner, fake.commands())
			}
		})
	}
}
