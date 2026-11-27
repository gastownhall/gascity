package session

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// The assignee-identity codec is the confined vocabulary shared by the
// reconciler orphan-release loops and the API assignee list filter/stamper.
// These tests pin AssigneeIdentities to the cmd/gc sessionBeadAssigneeIdentities
// case table it replaces (via InfoFromPersistedBead, proving bead<->Info
// agreement) and pin AssigneeIdentifier to the internal/api
// sessionBeadAssigneeIdentifier precedence it replaces, so the enumerated term
// set and the stamped identity form stay byte-identical after the dedup. A
// direct-Info case proves the RAW SessionNameMetadata field is read (no
// sessionNameFor(ID) fallback leak).

func TestAssigneeIdentities(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
		want []string
	}{
		{
			name: "empty bead produces no identities",
			bead: beads.Bead{},
			want: []string{},
		},
		{
			name: "id only",
			bead: beads.Bead{ID: "mc-xyz"},
			want: []string{"mc-xyz"},
		},
		{
			name: "session_name only",
			bead: beads.Bead{Metadata: map[string]string{"session_name": "worker-mc-live"}},
			want: []string{"worker-mc-live"},
		},
		{
			name: "configured_named_identity only",
			bead: beads.Bead{Metadata: map[string]string{"configured_named_identity": "reviewer"}},
			want: []string{"reviewer"},
		},
		{
			name: "alias only",
			bead: beads.Bead{Metadata: map[string]string{"alias": "nux"}},
			want: []string{"nux"},
		},
		{
			name: "alias_history single entry",
			bead: beads.Bead{Metadata: map[string]string{"alias_history": "previous"}},
			want: []string{"previous"},
		},
		{
			name: "alias_history multiple entries",
			bead: beads.Bead{Metadata: map[string]string{"alias_history": "first,second,third"}},
			want: []string{"first", "second", "third"},
		},
		{
			name: "all fields populated",
			bead: beads.Bead{
				ID: "mc-xyz",
				Metadata: map[string]string{
					"session_name":              "worker-mc-live",
					"configured_named_identity": "reviewer",
					"alias":                     "rictus",
					"alias_history":             "nux",
				},
			},
			want: []string{"mc-xyz", "worker-mc-live", "reviewer", "rictus", "nux"},
		},
		{
			name: "whitespace-only values are trimmed and skipped",
			bead: beads.Bead{
				ID: "  ",
				Metadata: map[string]string{
					"session_name":              "   ",
					"configured_named_identity": "\t",
					"alias":                     " ",
					"alias_history":             "  ,  , real ,  ",
				},
			},
			want: []string{"real"},
		},
		{
			name: "values with surrounding whitespace are trimmed",
			bead: beads.Bead{
				ID: "  mc-xyz  ",
				Metadata: map[string]string{
					"session_name":              "  worker-mc-live  ",
					"configured_named_identity": "  reviewer  ",
					"alias":                     "  nux  ",
				},
			},
			want: []string{"mc-xyz", "worker-mc-live", "reviewer", "nux"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AssigneeIdentities(infoFromPersistedBead(tt.bead))
			if len(got) != len(tt.want) {
				t.Fatalf("got %d identities %v, want %d %v", len(got), got, len(tt.want), tt.want)
			}
			for i, id := range got {
				if id != tt.want[i] {
					t.Errorf("identity[%d] = %q, want %q (full got=%v, want=%v)", i, id, tt.want[i], got, tt.want)
				}
			}
		})
	}
}

// TestAssigneeIdentitiesReadsRawSessionName proves AssigneeIdentities reads the
// RAW SessionNameMetadata field, not Info.SessionName (which falls back to
// sessionNameFor(ID)). A blank SessionNameMetadata must not leak the derived
// runtime name into the identity set.
func TestAssigneeIdentitiesReadsRawSessionName(t *testing.T) {
	i := Info{ID: "s1", SessionName: "s-gc-derived", SessionNameMetadata: ""}
	if got, want := AssigneeIdentities(i), []string{"s1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AssigneeIdentities = %#v, want %#v (must not leak sessionNameFor(ID))", got, want)
	}
}

func TestAssigneeIdentifier(t *testing.T) {
	tests := []struct {
		name string
		info Info
		want string
	}{
		{
			name: "alias wins",
			info: Info{ID: "s1", SessionNameMetadata: "sn", Alias: "al", ConfiguredNamedIdentity: "ni"},
			want: "al",
		},
		{
			name: "configured named identity when no alias",
			info: Info{ID: "s1", SessionNameMetadata: "sn", ConfiguredNamedIdentity: "ni"},
			want: "ni",
		},
		{
			name: "session_name when no public identity",
			info: Info{ID: "s1", SessionNameMetadata: "sn"},
			want: "sn",
		},
		{
			name: "bead id fallback when no name metadata",
			info: Info{ID: "s1"},
			want: "s1",
		},
		{
			name: "whitespace-only values skipped, falls through to id",
			info: Info{ID: "s1", SessionNameMetadata: "  ", Alias: "\t", ConfiguredNamedIdentity: " "},
			want: "s1",
		},
		{
			name: "values trimmed",
			info: Info{ID: "s1", Alias: "  al  ", SessionNameMetadata: "  sn  "},
			want: "al",
		},
		{
			name: "unaliased pool-managed session (pool_managed) claims under its bead id, not the reusable session name",
			info: Info{ID: "s1", SessionNameMetadata: "sn", PoolManaged: true},
			want: "s1",
		},
		{
			name: "unaliased pool-managed session (pool_slot) claims under its bead id",
			info: Info{ID: "s1", SessionNameMetadata: "sn", PoolSlot: "3"},
			want: "s1",
		},
		{
			name: "unaliased pool-managed session (session_origin=ephemeral) claims under its bead id",
			info: Info{ID: "s1", SessionNameMetadata: "sn", SessionOrigin: "ephemeral"},
			want: "s1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AssigneeIdentifier(tt.info); got != tt.want {
				t.Errorf("AssigneeIdentifier = %q, want %q", got, tt.want)
			}
		})
	}
}

// recordLookupStore records every listing HasRecordAnsweringTo issues and fails
// them with err when it is set. A partial-result err keeps the rows, as the bd
// store does when it drops rows it could not parse; the listings are Live, so a
// CachingStore above it passes that through unchanged.
type recordLookupStore struct {
	beads.Store
	queries []beads.ListQuery
	err     error
}

func (s *recordLookupStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, query)
	rows, err := s.Store.List(query)
	switch {
	case err != nil || s.err == nil:
		return rows, err
	case beads.IsPartialResult(s.err):
		return rows, s.err
	default:
		return nil, s.err
	}
}

// seedRecord creates a bead of beadType (labeled gc:session when labeled is
// set) carrying metadata, closed unless open is set. An empty beadType is
// written after the create, which would otherwise default it.
func seedRecord(t *testing.T, store beads.Store, beadType string, labeled, open bool, metadata map[string]string) beads.Bead {
	t.Helper()
	var labels []string
	if labeled {
		labels = []string{LabelSession}
	}
	b, err := store.Create(beads.Bead{Title: "record", Type: beadType, Labels: labels, Metadata: metadata})
	if err != nil {
		t.Fatalf("Create record: %v", err)
	}
	if beadType == "" {
		if err := store.Update(b.ID, beads.UpdateOpts{Type: &beadType}); err != nil {
			t.Fatalf("Clear record type: %v", err)
		}
	}
	if !open {
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("Close record: %v", err)
		}
	}
	return b
}

// TestHasRecordAnsweringTo pins which session records vouch for a qualified
// identity: a session bead, open or closed, under its alias, its configured
// named identity or a sole alias-history entry. A prior alias inside a longer
// history is the documented miss; callers keep such an identity protected.
func TestHasRecordAnsweringTo(t *testing.T) {
	for _, tc := range []struct {
		name     string
		beadType string
		labeled  bool
		open     bool
		metadata map[string]string
		want     bool
	}{
		{"closed record under its alias", BeadType, true, false, map[string]string{"alias": "repo/nux"}, true},
		{"open record under its alias", BeadType, true, true, map[string]string{"alias": "repo/nux"}, true},
		{"closed named session", BeadType, true, false, map[string]string{NamedSessionIdentityMetadata: "repo/nux"}, true},
		{"sole alias-history entry", BeadType, true, false, map[string]string{"alias": "repo/furiosa", "alias_history": "repo/nux"}, true},
		{"label-only record with an empty type", "", true, false, map[string]string{"alias": "repo/nux"}, true},
		{"typed record that lost its label", BeadType, false, false, map[string]string{"alias": "repo/nux"}, true},
		{"prior alias inside a longer history", BeadType, true, false, map[string]string{"alias": "repo/furiosa", "alias_history": "repo/rictus,repo/nux"}, false},
		{"work bead carrying the alias", "task", false, true, map[string]string{"alias": "repo/nux"}, false},
		{"record under a longer name", BeadType, true, false, map[string]string{"alias": "repo/nux-2"}, false},
		{"no record at all", BeadType, true, false, map[string]string{"alias": "repo/furiosa"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			record := seedRecord(t, mem, tc.beadType, tc.labeled, tc.open, tc.metadata)
			store := &recordLookupStore{Store: mem}

			got, err := HasRecordAnsweringTo(beadstest.Sessions(store), " repo/nux ")
			if err != nil {
				t.Fatalf("HasRecordAnsweringTo: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasRecordAnsweringTo(repo/nux) = %v, want %v", got, tc.want)
			}
			for _, q := range store.queries {
				if !q.Live || !q.IncludeClosed || len(q.Metadata) != 1 {
					t.Fatalf("listing %+v, want one Live, closed-inclusive exact-metadata lookup", q)
				}
				for key, value := range q.Metadata {
					if key != "alias" && key != NamedSessionIdentityMetadata && key != "alias_history" {
						t.Fatalf("lookup on %q, want only the keys that can hold a qualified identity", key)
					}
					if value != "repo/nux" {
						t.Fatalf("lookup value %q, want the trimmed identity", value)
					}
				}
			}
			stored, err := mem.Get(record.ID)
			if err != nil {
				t.Fatalf("Get record: %v", err)
			}
			if stored.Type != tc.beadType {
				t.Fatalf("stored type = %q, want %q: the lookup must not repair or write", stored.Type, tc.beadType)
			}
		})
	}
}

// TestHasRecordAnsweringTo_CostFollowsMatchesNotTheLedger keeps the lookup's
// reads off the session ledger's size: a store full of closed session records
// costs the same handful of exact-metadata lookups, and they return only the
// records that answer to the identity. What each lookup costs the backend is
// not counted here; bd/Dolt scans the store for one (see HasRecordAnsweringTo).
func TestHasRecordAnsweringTo_CostFollowsMatchesNotTheLedger(t *testing.T) {
	cost := func(ledger int) (lookups, rows int) {
		mem := beads.NewMemStore()
		seedRecord(t, mem, BeadType, true, false, map[string]string{"alias": "repo/nux"})
		for i := 0; i < ledger; i++ {
			seedRecord(t, mem, BeadType, true, false, map[string]string{
				"alias":         fmt.Sprintf("repo/retired-%d", i),
				"session_name":  fmt.Sprintf("worker-gc-retired-%d", i),
				"alias_history": "repo/older",
			})
		}
		store := &recordLookupStore{Store: mem}
		for _, identity := range []string{"repo/nux", "repo/dalinar"} {
			if _, err := HasRecordAnsweringTo(beadstest.Sessions(store), identity); err != nil {
				t.Fatalf("HasRecordAnsweringTo(%s): %v", identity, err)
			}
		}
		for _, q := range store.queries {
			matched, err := mem.List(q)
			if err != nil {
				t.Fatalf("replay %+v: %v", q, err)
			}
			rows += len(matched)
		}
		return len(store.queries), rows
	}
	smallLookups, smallRows := cost(0)
	bigLookups, bigRows := cost(200)
	if smallLookups == 0 || smallLookups > 6 {
		t.Fatalf("two identities cost %d lookups, want 1 to 3 each", smallLookups)
	}
	if bigLookups != smallLookups || bigRows != smallRows || bigRows != 1 {
		t.Fatalf("lookups/rows = %d/%d over 200 closed records, want %d/1 as over none", bigLookups, bigRows, smallLookups)
	}
}

// TestHasRecordAnsweringTo_ListingErrors keeps a failed lookup from reading as
// "no record". A hard failure ends the lookup and returns the error with false,
// the answer the caller must treat as unknown. A partial result still credits
// the rows it returned, so true stays reliable alongside the error.
func TestHasRecordAnsweringTo_ListingErrors(t *testing.T) {
	unreachable := errors.New("session store unreachable")
	partial := &beads.PartialResultError{Op: "bd list", Err: errors.New("one row failed to parse")}
	for _, tc := range []struct {
		name        string
		err         error
		identity    string
		want        bool
		wantLookups int
	}{
		{"hard failure stops at the first lookup", unreachable, "repo/nux", false, 1},
		{"partial result with the record", partial, "repo/nux", true, 1},
		{"partial result without one", partial, "repo/dalinar", false, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			seedRecord(t, mem, BeadType, true, false, map[string]string{"alias": "repo/nux"})
			store := &recordLookupStore{Store: mem, err: tc.err}

			got, err := HasRecordAnsweringTo(beadstest.Sessions(store), tc.identity)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want it to wrap %v", err, tc.err)
			}
			if got != tc.want {
				t.Fatalf("HasRecordAnsweringTo(%s) = %v, want %v", tc.identity, got, tc.want)
			}
			if len(store.queries) != tc.wantLookups {
				t.Fatalf("lookups = %d, want %d", len(store.queries), tc.wantLookups)
			}
		})
	}
}

// TestTemplateTenureIn pins which session records count toward a template's
// tenure: a session bead, open or closed, recorded under exactly that
// template, found by one Live, closed-inclusive lookup on the template key that
// writes nothing. Each record dates the tenure by its creation.
func TestTemplateTenureIn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		beadType string
		labeled  bool
		open     bool
		metadata map[string]string
		counts   bool
	}{
		{"closed record of the template", BeadType, true, false, map[string]string{"template": "rig-A/gc.planner"}, true},
		{"open record of the template", BeadType, true, true, map[string]string{"template": "rig-A/gc.planner"}, true},
		{"label-only record with an empty type", "", true, false, map[string]string{"template": "rig-A/gc.planner"}, true},
		{"work bead carrying the template", "task", false, true, map[string]string{"template": "rig-A/gc.planner"}, false},
		{"record of another template", BeadType, true, false, map[string]string{"template": "rig-A/planner"}, false},
		{"record answering to the identity only by alias", BeadType, true, false, map[string]string{"alias": "rig-A/gc.planner", "template": "rig-A/planner"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			record := seedRecord(t, mem, tc.beadType, tc.labeled, tc.open, tc.metadata)
			before, err := mem.Get(record.ID)
			if err != nil {
				t.Fatalf("Get record: %v", err)
			}
			store := &recordLookupStore{Store: mem}

			got, err := TemplateTenureIn(store, " rig-A/gc.planner ")
			if err != nil {
				t.Fatalf("TemplateTenureIn: %v", err)
			}
			var want TemplateTenure
			if tc.counts {
				want.Until = before.CreatedAt
			}
			if !got.Until.Equal(want.Until) {
				t.Fatalf("TemplateTenureIn(rig-A/gc.planner) = %+v, want %+v", got, want)
			}
			wantQuery := beads.ListQuery{Metadata: map[string]string{"template": "rig-A/gc.planner"}, IncludeClosed: true, Live: true}
			if len(store.queries) != 1 || !reflect.DeepEqual(store.queries[0], wantQuery) {
				t.Fatalf("listings = %+v, want exactly %+v", store.queries, wantQuery)
			}
			after, err := mem.Get(record.ID)
			if err != nil {
				t.Fatalf("Get record: %v", err)
			}
			if after.Type != tc.beadType || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("stored type/updated_at = %q/%v, want %q/%v: the lookup must not repair or write", after.Type, after.UpdatedAt, tc.beadType, before.UpdatedAt)
			}
		})
	}

	t.Run("a looser metadata filter is confirmed per row", func(t *testing.T) {
		mem := beads.NewMemStore()
		seedRecord(t, mem, BeadType, true, false, map[string]string{"template": "Rig-A/GC.planner"})
		got, err := TemplateTenureIn(foldedMetadataStore{mem}, "rig-A/gc.planner")
		if err != nil || got != (TemplateTenure{}) {
			t.Fatalf("TemplateTenureIn = %+v, %v; want a record of Rig-A/GC.planner not to count for rig-A/gc.planner", got, err)
		}
	})

	t.Run("nil store or blank template looks nothing up", func(t *testing.T) {
		if got, err := TemplateTenureIn(nil, "rig-A/gc.planner"); got != (TemplateTenure{}) || err != nil {
			t.Fatalf("TemplateTenureIn(nil) = %+v, %v; want the zero tenure, nil", got, err)
		}
		store := &recordLookupStore{Store: beads.NewMemStore()}
		if got, err := TemplateTenureIn(store, "  "); got != (TemplateTenure{}) || err != nil || len(store.queries) != 0 {
			t.Fatalf("TemplateTenureIn(blank) = %+v, %v after %d listings; want the zero tenure, nil, none", got, err, len(store.queries))
		}
	})
}

// TestTemplateTenureIn_EveryRecordCounts: the tenure is taken over every
// record of the template, not the first one found. Its Until is the latest
// record's creation, open or closed. A record's later writes, its close
// included, do not extend it: a pre-migration session bead a city keeps as a
// seat of the renamed agent is written on every wake and sleep while it runs
// the new identity, and closed while it runs it too. A record with no creation
// time dates nothing, however it was last written.
func TestTemplateTenureIn_EveryRecordCounts(t *testing.T) {
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	record := func(id, status string, created, updated time.Time) beads.Bead {
		return beads.Bead{
			ID: id, Title: "record", Type: BeadType, Status: status, CreatedAt: created, UpdatedAt: updated,
			Metadata: map[string]string{"template": "rig-A/gc.planner"},
		}
	}
	for _, tc := range []struct {
		name    string
		records []beads.Bead
		want    TemplateTenure
	}{
		{"the latest creation among closed records", []beads.Bead{
			record("s-1", "closed", base.Add(3*time.Hour), base.Add(4*time.Hour)),
			record("s-2", "closed", base.Add(5*time.Hour), base.Add(6*time.Hour)),
			record("s-3", "closed", base.Add(time.Hour), base.Add(9*time.Hour)),
		}, TemplateTenure{Until: base.Add(5 * time.Hour)}},
		{"a closed record by its creation, not its close", []beads.Bead{
			record("s-1", "closed", base.Add(2*time.Hour), base.Add(98*time.Hour)),
		}, TemplateTenure{Until: base.Add(2 * time.Hour)}},
		{"a closed record the store reports no update for", []beads.Bead{
			record("s-1", "closed", base.Add(2*time.Hour), time.Time{}),
		}, TemplateTenure{Until: base.Add(2 * time.Hour)}},
		{"an open record by its creation, not its last write", []beads.Bead{
			record("s-1", "open", base.Add(2*time.Hour), base.Add(98*time.Hour)),
		}, TemplateTenure{Until: base.Add(2 * time.Hour)}},
		{"an open record among closed ones", []beads.Bead{
			record("s-1", "closed", base, base.Add(time.Hour)),
			record("s-2", "open", base.Add(2*time.Hour), base.Add(98*time.Hour)),
			record("s-3", "closed", base.Add(time.Hour), base.Add(4*time.Hour)),
		}, TemplateTenure{Until: base.Add(2 * time.Hour)}},
		{"an open record created before a closed one", []beads.Bead{
			record("s-1", "open", base, base.Add(98*time.Hour)),
			record("s-2", "closed", base.Add(3*time.Hour), base.Add(4*time.Hour)),
		}, TemplateTenure{Until: base.Add(3 * time.Hour)}},
		{"a closed record with no time", []beads.Bead{
			record("s-1", "closed", time.Time{}, time.Time{}),
		}, TemplateTenure{}},
		{"a closed record with no creation time", []beads.Bead{
			record("s-1", "closed", time.Time{}, base.Add(98*time.Hour)),
		}, TemplateTenure{}},
		{"an open record with no creation time", []beads.Bead{
			record("s-1", "open", time.Time{}, base.Add(98*time.Hour)),
		}, TemplateTenure{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TemplateTenureIn(beads.NewMemStoreFrom(0, tc.records, nil), "rig-A/gc.planner")
			if err != nil {
				t.Fatalf("TemplateTenureIn: %v", err)
			}
			if !got.Until.Equal(tc.want.Until) {
				t.Fatalf("TemplateTenureIn = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestTemplateTenureCovers pins how a row is dated against a tenure: it is
// covered when it was created no later than Until, the latest creation of a
// record of the template. A row with no creation time is never covered, and
// nothing is covered when no record dates the tenure.
func TestTemplateTenureCovers(t *testing.T) {
	until := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		tenure  TemplateTenure
		created time.Time
		want    bool
	}{
		{"no record", TemplateTenure{}, until, false},
		{"no record, undated row", TemplateTenure{}, time.Time{}, false},
		{"row created before the latest record", TemplateTenure{Until: until}, until.Add(-time.Hour), true},
		{"row created as the latest record", TemplateTenure{Until: until}, until, true},
		{"row created after the latest record", TemplateTenure{Until: until}, until.Add(time.Second), false},
		{"undated row", TemplateTenure{Until: until}, time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tenure.Covers(tc.created); got != tc.want {
				t.Fatalf("%+v.Covers(%v) = %v, want %v", tc.tenure, tc.created, got, tc.want)
			}
		})
	}
}

// TestTemplateTenureIn_ListingErrors keeps a failed lookup from reading as
// "no record": a hard failure returns the error with the zero tenure, and a
// partial result still counts the rows it returned.
func TestTemplateTenureIn_ListingErrors(t *testing.T) {
	unreachable := errors.New("session store unreachable")
	partial := &beads.PartialResultError{Op: "bd list", Err: errors.New("one row failed to parse")}
	for _, tc := range []struct {
		name     string
		err      error
		template string
		counts   bool
	}{
		{"hard failure", unreachable, "rig-A/gc.planner", false},
		{"partial result with the record", partial, "rig-A/gc.planner", true},
		{"partial result without one", partial, "rig-A/gc.worker", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			record := seedRecord(t, mem, BeadType, true, false, map[string]string{"template": "rig-A/gc.planner"})
			stored, err := mem.Get(record.ID)
			if err != nil {
				t.Fatalf("Get record: %v", err)
			}
			store := &recordLookupStore{Store: mem, err: tc.err}

			got, err := TemplateTenureIn(store, tc.template)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want it to wrap %v", err, tc.err)
			}
			var want TemplateTenure
			if tc.counts {
				want.Until = stored.CreatedAt
			}
			if !got.Until.Equal(want.Until) {
				t.Fatalf("TemplateTenureIn(%s) = %+v, want %+v", tc.template, got, want)
			}
			if len(store.queries) != 1 {
				t.Fatalf("lookups = %d, want 1", len(store.queries))
			}
		})
	}
}

// foldedMetadataStore answers a metadata filter case-insensitively, as a
// backend under a case-insensitive collation can.
type foldedMetadataStore struct{ beads.Store }

func (s foldedMetadataStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	want := query.Metadata
	query.Metadata = nil
	query.Label = LabelSession
	rows, err := s.Store.List(query)
	var out []beads.Bead
	for _, b := range rows {
		matched := true
		for key, value := range want {
			matched = matched && strings.EqualFold(b.Metadata[key], value)
		}
		if matched {
			out = append(out, b)
		}
	}
	return out, err
}

// TestHasRecordAnsweringTo_ConfirmsEachRow keeps the answer exact when the
// store's metadata filter is looser than the identity match: a returned row
// counts only when the record's own identities include the identity.
func TestHasRecordAnsweringTo_ConfirmsEachRow(t *testing.T) {
	mem := beads.NewMemStore()
	seedRecord(t, mem, BeadType, true, false, map[string]string{"alias": "Repo/Nux"})
	store := foldedMetadataStore{mem}
	rows, err := store.List(beads.ListQuery{Metadata: map[string]string{"alias": "repo/nux"}, IncludeClosed: true})
	if err != nil || len(rows) != 1 {
		t.Fatalf("folded lookup = %d rows, %v; want the one loose match", len(rows), err)
	}

	got, err := HasRecordAnsweringTo(beadstest.Sessions(store), "repo/nux")
	if err != nil {
		t.Fatalf("HasRecordAnsweringTo: %v", err)
	}
	if got {
		t.Fatal("a record under alias Repo/Nux vouched for repo/nux")
	}
}

func TestHasRecordAnsweringTo_NothingToLookUp(t *testing.T) {
	if got, err := HasRecordAnsweringTo(beads.SessionStore{}, "repo/nux"); got || err != nil {
		t.Fatalf("nil store = %v, %v; want false, nil", got, err)
	}
	store := &recordLookupStore{Store: beads.NewMemStore()}
	if got, err := HasRecordAnsweringTo(beadstest.Sessions(store), "  "); got || err != nil || len(store.queries) != 0 {
		t.Fatalf("blank identity = %v, %v after %d lookups; want false, nil and no lookup", got, err, len(store.queries))
	}
}

// getCountingStore counts Get calls and fails them with err when it is set,
// on top of recordLookupStore's listing record.
type getCountingStore struct {
	*recordLookupStore
	gets   int
	getErr error
}

func (s *getCountingStore) Get(id string) (beads.Bead, error) {
	s.gets++
	if s.getErr != nil {
		return beads.Bead{}, s.getErr
	}
	return s.recordLookupStore.Get(id)
}

// seedRecordWithID is seedRecord for a record whose bead ID the test pins.
func seedRecordWithID(t *testing.T, store *beads.MemStore, id, beadType string, labeled, open bool, metadata map[string]string) beads.Bead {
	t.Helper()
	store.HonorExplicitIDs = true
	var labels []string
	if labeled {
		labels = []string{LabelSession}
	}
	b, err := store.Create(beads.Bead{ID: id, Title: "record", Type: beadType, Labels: labels, Metadata: metadata})
	if err != nil {
		t.Fatalf("Create record %s: %v", id, err)
	}
	if !open {
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("Close record: %v", err)
		}
	}
	return b
}

// TestRecordAnsweringTo pins which session records vouch for an identity of
// any shape, and that the answer is the record's ID: a session bead, open or
// closed, under its bead ID, session_name, alias, configured named identity or
// sole alias-history entry. A bead that is not a session record never counts.
func TestRecordAnsweringTo(t *testing.T) {
	for _, tc := range []struct {
		name     string
		id       string
		beadType string
		labeled  bool
		open     bool
		metadata map[string]string
		identity string
		want     bool
	}{
		{"open record by its ID", "gc-live1", BeadType, true, true, nil, "gc-live1", true},
		{"closed record by its ID", "gc-dead2", BeadType, true, false, nil, "gc-dead2", true},
		{"closed record by session_name", "gc-d34d1", BeadType, true, false, map[string]string{"session_name": "worker-gc-d34d1"}, "worker-gc-d34d1", true},
		{"closed record by bare alias", "gc-a1", BeadType, true, false, map[string]string{"alias": "nux"}, "nux", true},
		{"closed record by configured named identity", "gc-n1", BeadType, true, false, map[string]string{NamedSessionIdentityMetadata: "lana"}, "lana", true},
		{"closed record by sole alias-history entry", "gc-h1", BeadType, true, false, map[string]string{"alias": "furiosa", "alias_history": "nux"}, "nux", true},
		{"qualified identity by alias", "gc-q1", BeadType, true, false, map[string]string{"alias": "repo/nux"}, "repo/nux", true},
		{"non-session bead whose ID is the identity", "gc-task1", "task", false, true, nil, "gc-task1", false},
		{"work bead carrying the session_name", "gc-task2", "task", false, true, map[string]string{"session_name": "worker-gc-d34d1"}, "worker-gc-d34d1", false},
		{"no record answers", "gc-other", BeadType, true, false, map[string]string{"session_name": "worker-gc-other"}, "bt-q7m3v", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			record := seedRecordWithID(t, mem, tc.id, tc.beadType, tc.labeled, tc.open, tc.metadata)
			store := &getCountingStore{recordLookupStore: &recordLookupStore{Store: mem}}

			got, err := RecordAnsweringTo(beadstest.Sessions(store), " "+tc.identity+" ")
			if err != nil {
				t.Fatalf("RecordAnsweringTo(%s): %v", tc.identity, err)
			}
			want := ""
			if tc.want {
				want = record.ID
			}
			if got != want {
				t.Fatalf("RecordAnsweringTo(%s) = %q, want %q", tc.identity, got, want)
			}
			for _, q := range store.queries {
				if !q.Live || !q.IncludeClosed || len(q.Metadata) != 1 {
					t.Fatalf("listing %+v, want one Live, closed-inclusive exact-metadata lookup", q)
				}
			}
		})
	}
}

// TestRecordAnsweringTo_QualifiedIdentityMatchesHasRecordAnsweringTo keeps a
// "<rig>/<name>" identity on exactly HasRecordAnsweringTo's lookups: no Get
// (a bead ID never contains "/") and no session_name listing.
func TestRecordAnsweringTo_QualifiedIdentityMatchesHasRecordAnsweringTo(t *testing.T) {
	for _, identity := range []string{"repo/nux", "repo/dalinar"} {
		mem := beads.NewMemStore()
		seedRecord(t, mem, BeadType, true, false, map[string]string{"alias": "repo/nux"})

		anyShape := &getCountingStore{recordLookupStore: &recordLookupStore{Store: mem}}
		id, err := RecordAnsweringTo(beadstest.Sessions(anyShape), identity)
		if err != nil {
			t.Fatalf("RecordAnsweringTo(%s): %v", identity, err)
		}
		qualified := &getCountingStore{recordLookupStore: &recordLookupStore{Store: mem}}
		found, err := HasRecordAnsweringTo(beadstest.Sessions(qualified), identity)
		if err != nil {
			t.Fatalf("HasRecordAnsweringTo(%s): %v", identity, err)
		}
		if (id != "") != found {
			t.Fatalf("%s: RecordAnsweringTo = %q, HasRecordAnsweringTo = %v; want agreement", identity, id, found)
		}
		if anyShape.gets != 0 {
			t.Fatalf("%s: %d Get calls, want none for a qualified identity", identity, anyShape.gets)
		}
		if !reflect.DeepEqual(anyShape.queries, qualified.queries) {
			t.Fatalf("%s: listings %+v, want HasRecordAnsweringTo's %+v", identity, anyShape.queries, qualified.queries)
		}
	}
}

// TestRecordAnsweringTo_Errors keeps a failed lookup from reading as "no
// record": a non-NotFound Get error and a hard listing error both return ""
// with the error, while a partial result still credits a matching row.
func TestRecordAnsweringTo_Errors(t *testing.T) {
	unreachable := errors.New("session store unreachable")
	partial := &beads.PartialResultError{Op: "bd list", Err: errors.New("one row failed to parse")}

	t.Run("Get error", func(t *testing.T) {
		mem := beads.NewMemStore()
		store := &getCountingStore{recordLookupStore: &recordLookupStore{Store: mem}, getErr: unreachable}
		got, err := RecordAnsweringTo(beadstest.Sessions(store), "gc-d34d1")
		if got != "" || !errors.Is(err, unreachable) {
			t.Fatalf("RecordAnsweringTo = %q, %v; want \"\" and the Get error", got, err)
		}
		if len(store.queries) != 0 {
			t.Fatalf("%d listings after a hard Get error, want none", len(store.queries))
		}
	})
	t.Run("hard listing error", func(t *testing.T) {
		mem := beads.NewMemStore()
		seedRecordWithID(t, mem, "gc-d34d1", BeadType, true, false, map[string]string{"session_name": "worker-gc-d34d1"})
		store := &getCountingStore{recordLookupStore: &recordLookupStore{Store: mem, err: unreachable}}
		got, err := RecordAnsweringTo(beadstest.Sessions(store), "worker-gc-d34d1")
		if got != "" || !errors.Is(err, unreachable) {
			t.Fatalf("RecordAnsweringTo = %q, %v; want \"\" and the listing error", got, err)
		}
	})
	t.Run("partial result with a matching row", func(t *testing.T) {
		mem := beads.NewMemStore()
		record := seedRecordWithID(t, mem, "gc-d34d1", BeadType, true, false, map[string]string{"session_name": "worker-gc-d34d1"})
		store := &getCountingStore{recordLookupStore: &recordLookupStore{Store: mem, err: partial}}
		got, err := RecordAnsweringTo(beadstest.Sessions(store), "worker-gc-d34d1")
		if got != record.ID || !errors.Is(err, partial) {
			t.Fatalf("RecordAnsweringTo = %q, %v; want %q and the partial error", got, err, record.ID)
		}
	})
	t.Run("nothing to look up", func(t *testing.T) {
		if got, err := RecordAnsweringTo(beads.SessionStore{}, "gc-d34d1"); got != "" || err != nil {
			t.Fatalf("nil store = %q, %v; want \"\", nil", got, err)
		}
		store := &getCountingStore{recordLookupStore: &recordLookupStore{Store: beads.NewMemStore()}}
		if got, err := RecordAnsweringTo(beadstest.Sessions(store), " "); got != "" || err != nil || store.gets != 0 || len(store.queries) != 0 {
			t.Fatalf("blank identity = %q, %v; want no lookup", got, err)
		}
	})
	t.Run("identities that cannot be a bead ID skip the Get", func(t *testing.T) {
		for _, identity := range []string{"Pat Example", "-odd"} {
			store := &getCountingStore{recordLookupStore: &recordLookupStore{Store: beads.NewMemStore()}}
			if _, err := RecordAnsweringTo(beadstest.Sessions(store), identity); err != nil {
				t.Fatalf("RecordAnsweringTo(%q): %v", identity, err)
			}
			if store.gets != 0 {
				t.Fatalf("RecordAnsweringTo(%q) issued %d Get calls, want none", identity, store.gets)
			}
		}
	})
}
