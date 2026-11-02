package session

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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

			got, err := HasRecordAnsweringTo(store, " repo/nux ")
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
			if _, err := HasRecordAnsweringTo(store, identity); err != nil {
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

			got, err := HasRecordAnsweringTo(store, tc.identity)
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

	got, err := HasRecordAnsweringTo(store, "repo/nux")
	if err != nil {
		t.Fatalf("HasRecordAnsweringTo: %v", err)
	}
	if got {
		t.Fatal("a record under alias Repo/Nux vouched for repo/nux")
	}
}

func TestHasRecordAnsweringTo_NothingToLookUp(t *testing.T) {
	if got, err := HasRecordAnsweringTo(nil, "repo/nux"); got || err != nil {
		t.Fatalf("nil store = %v, %v; want false, nil", got, err)
	}
	store := &recordLookupStore{Store: beads.NewMemStore()}
	if got, err := HasRecordAnsweringTo(store, "  "); got || err != nil || len(store.queries) != 0 {
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

			got, err := RecordAnsweringTo(store, " "+tc.identity+" ")
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
		id, err := RecordAnsweringTo(anyShape, identity)
		if err != nil {
			t.Fatalf("RecordAnsweringTo(%s): %v", identity, err)
		}
		qualified := &getCountingStore{recordLookupStore: &recordLookupStore{Store: mem}}
		found, err := HasRecordAnsweringTo(qualified, identity)
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
		got, err := RecordAnsweringTo(store, "gc-d34d1")
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
		got, err := RecordAnsweringTo(store, "worker-gc-d34d1")
		if got != "" || !errors.Is(err, unreachable) {
			t.Fatalf("RecordAnsweringTo = %q, %v; want \"\" and the listing error", got, err)
		}
	})
	t.Run("partial result with a matching row", func(t *testing.T) {
		mem := beads.NewMemStore()
		record := seedRecordWithID(t, mem, "gc-d34d1", BeadType, true, false, map[string]string{"session_name": "worker-gc-d34d1"})
		store := &getCountingStore{recordLookupStore: &recordLookupStore{Store: mem, err: partial}}
		got, err := RecordAnsweringTo(store, "worker-gc-d34d1")
		if got != record.ID || !errors.Is(err, partial) {
			t.Fatalf("RecordAnsweringTo = %q, %v; want %q and the partial error", got, err, record.ID)
		}
	})
	t.Run("nothing to look up", func(t *testing.T) {
		if got, err := RecordAnsweringTo(nil, "gc-d34d1"); got != "" || err != nil {
			t.Fatalf("nil store = %q, %v; want \"\", nil", got, err)
		}
		store := &getCountingStore{recordLookupStore: &recordLookupStore{Store: beads.NewMemStore()}}
		if got, err := RecordAnsweringTo(store, " "); got != "" || err != nil || store.gets != 0 || len(store.queries) != 0 {
			t.Fatalf("blank identity = %q, %v; want no lookup", got, err)
		}
	})
	t.Run("identities that cannot be a bead ID skip the Get", func(t *testing.T) {
		for _, identity := range []string{"Pat Example", "-odd"} {
			store := &getCountingStore{recordLookupStore: &recordLookupStore{Store: beads.NewMemStore()}}
			if _, err := RecordAnsweringTo(store, identity); err != nil {
				t.Fatalf("RecordAnsweringTo(%q): %v", identity, err)
			}
			if store.gets != 0 {
				t.Fatalf("RecordAnsweringTo(%q) issued %d Get calls, want none", identity, store.gets)
			}
		}
	})
}
