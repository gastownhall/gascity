package beads

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func openBatchTestSQLite(t *testing.T) *SQLiteStore {
	t.Helper()
	opened, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = s.CloseStore() })
	return s
}

// TestSQLiteCreateWithForeignIDBatchIsAllOrNothing: the batch keeps every id
// and every field, as CreateWithForeignID does row by row, and one duplicate
// rolls the whole call back.
func TestSQLiteCreateWithForeignIDBatchIsAllOrNothing(t *testing.T) {
	s := openBatchTestSQLite(t)
	at := time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC)
	rows := []Bead{
		{ID: "mc-1", Title: "one", Type: "message", Status: "open", CreatedAt: at, Metadata: StringMap{"k": "v"}, Labels: []string{"a"}},
		{ID: "mc-2", Title: "two", Type: "session", Status: "closed", CreatedAt: at.Add(time.Second), Ephemeral: true},
	}
	if err := s.CreateWithForeignIDBatch(rows); err != nil {
		t.Fatalf("CreateWithForeignIDBatch: %v", err)
	}
	reference := openBatchTestSQLite(t)
	for _, b := range rows {
		want, err := reference.CreateWithForeignID(b)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(b.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", b.ID, err)
		}
		got.Revision, want.Revision = 0, 0
		if !reflect.DeepEqual(got, want) {
			t.Errorf("batch row %s = %+v, want what CreateWithForeignID stores: %+v", b.ID, got, want)
		}
	}

	err := s.CreateWithForeignIDBatch([]Bead{{ID: "mc-3", Title: "three", Type: "message"}, {ID: "mc-1", Title: "again", Type: "message"}})
	if err == nil {
		t.Fatal("a batch holding an id the store already holds was accepted")
	}
	if _, err := s.Get("mc-3"); err == nil {
		t.Fatal("a refused batch left mc-3 behind: the call was not all or nothing")
	}
	if err := s.CreateWithForeignIDBatch([]Bead{{ID: " ", Title: "blank"}}); err == nil || !strings.Contains(err.Error(), "empty id") {
		t.Fatalf("a blank id = %v, want refused", err)
	}
}

// TestSQLiteDepBatchesAgreeWithTheirSingleEdgeForms: DepAddBatch stores each
// edge and payload as DepAddWithMetadata does, DepListBatch lists what DepList
// lists per held anchor and nothing for an absent one, and DepMetadataBatch
// answers what DepMetadata answers edge by edge.
func TestSQLiteDepBatchesAgreeWithTheirSingleEdgeForms(t *testing.T) {
	s := openBatchTestSQLite(t)
	if err := s.CreateWithForeignIDBatch([]Bead{{ID: "a", Title: "a"}, {ID: "b", Title: "b"}, {ID: "c", Title: "c"}}); err != nil {
		t.Fatal(err)
	}
	edges := []DepWithPayload{
		{Dep: Dep{IssueID: "a", DependsOnID: "b", Type: "waits-for"}, Payload: `{"gate":"any-children"}`},
		{Dep: Dep{IssueID: "a", DependsOnID: "c", Type: "blocks"}},
		{Dep: Dep{IssueID: "b", DependsOnID: "c", Type: "blocks"}, Payload: "{}"},
		{Dep: Dep{IssueID: "c", DependsOnID: "external:x", Type: "tracks"}, Payload: `{"why":"x"}`},
	}
	if err := s.DepAddBatch(edges); err != nil {
		t.Fatalf("DepAddBatch: %v", err)
	}
	// An edge row whose anchor the store does not hold (a dangling deps
	// row): DepListBatch keys by held anchor, so it still gets no entry.
	if err := s.DepAddBatch([]DepWithPayload{{Dep: Dep{IssueID: "ghost", DependsOnID: "a", Type: "blocks"}}}); err != nil {
		t.Fatal(err)
	}
	listed, err := s.DepListBatch([]string{"a", "b", "c", "absent", "ghost"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"absent", "ghost"} {
		if _, ok := listed[id]; ok {
			t.Errorf("anchor %s, which the store does not hold, has an entry", id)
		}
	}
	payloads, err := s.DepMetadataBatch([]string{"a", "b", "c", "absent"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		want, err := s.DepList(id, "down")
		if err != nil {
			t.Fatal(err)
		}
		if got := listed[id]; len(got) != len(want) {
			t.Errorf("DepListBatch[%s] = %v, want DepList's %v", id, got, want)
		}
		for _, d := range want {
			payload, carried, err := s.DepMetadata(id, d.DependsOnID)
			if err != nil {
				t.Fatal(err)
			}
			got, inBatch := payloads[DepPair{IssueID: id, DependsOnID: d.DependsOnID}]
			if inBatch != carried || got != payload {
				t.Errorf("DepMetadataBatch %s -> %s = (%q, %v), want DepMetadata's (%q, %v)", id, d.DependsOnID, got, inBatch, payload, carried)
			}
		}
	}
	if len(payloads) != 2 {
		t.Errorf("DepMetadataBatch = %v, want exactly the two carrying edges", payloads)
	}
	if err := s.DepAddBatch([]DepWithPayload{{Dep: Dep{IssueID: "a", DependsOnID: "b", Type: "waits-for"}}}); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.DepMetadataBatch([]string{"a"}); len(again) != 0 {
		t.Errorf("re-adding an edge with no payload left %v: it must clear the sidecar as DepAddWithMetadata does", again)
	}
}

// TestSQLiteDepBatchesAgreeAcrossChunkBoundaries (review R): with more anchors
// than one IN list binds (sqliteBatchQueryChunk), DepListBatch and
// DepMetadataBatch still answer every anchor, edge and payload DepList and
// DepMetadata answer one at a time.
func TestSQLiteDepBatchesAgreeAcrossChunkBoundaries(t *testing.T) {
	const anchors = 2*sqliteBatchQueryChunk + 201
	s := openBatchTestSQLite(t)
	var rows []Bead
	var ids []string
	for i := 0; i < anchors; i++ {
		id := fmt.Sprintf("mc-%04d", i)
		ids = append(ids, id)
		rows = append(rows, Bead{ID: id, Title: id})
	}
	if err := s.CreateWithForeignIDBatch(rows); err != nil {
		t.Fatal(err)
	}
	var edges []DepWithPayload
	for i := 1; i < anchors; i++ {
		e := DepWithPayload{Dep: Dep{IssueID: ids[i], DependsOnID: ids[i-1], Type: "blocks"}}
		if i%3 == 0 {
			e.Type, e.Payload = "waits-for", fmt.Sprintf(`{"n":%d}`, i)
		}
		edges = append(edges, e)
	}
	if err := s.DepAddBatch(edges); err != nil {
		t.Fatal(err)
	}
	listed, err := s.DepListBatch(ids)
	if err != nil {
		t.Fatal(err)
	}
	payloads, err := s.DepMetadataBatch(ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != anchors {
		t.Fatalf("DepListBatch answered %d anchors, want all %d", len(listed), anchors)
	}
	carrying := 0
	for _, id := range ids {
		want, err := s.DepList(id, "down")
		if err != nil {
			t.Fatal(err)
		}
		if len(listed[id]) != len(want) {
			t.Fatalf("DepListBatch[%s] = %v, want DepList's %v", id, listed[id], want)
		}
		for _, d := range want {
			payload, carried, err := s.DepMetadata(id, d.DependsOnID)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := payloads[DepPair{IssueID: id, DependsOnID: d.DependsOnID}]
			if ok != carried || got != payload {
				t.Fatalf("DepMetadataBatch %s -> %s = (%q, %v), want (%q, %v)", id, d.DependsOnID, got, ok, payload, carried)
			}
			if carried {
				carrying++
			}
		}
	}
	if carrying != len(payloads) || carrying != (anchors-1)/3 {
		t.Fatalf("DepMetadataBatch answered %d payloads, the per-edge reads %d, want %d", len(payloads), carrying, (anchors-1)/3)
	}
}

// TestSQLiteDepBatchesReturnNoMapOnAnIterationError: a driver error part way
// through a result (I/O, corruption) ends the iteration as the last row does
// and closes the rows, so only Rows.Err reports it. Each batch read reports it
// and returns no map, as DependencyBatchLister requires, never the rows it
// read before the failure: the caller would read the rest as edges or
// payloads the store does not hold.
func TestSQLiteDepBatchesReturnNoMapOnAnIterationError(t *testing.T) {
	broken := errors.New("disk I/O error")
	listBatch := func(s *SQLiteStore) (bool, error) {
		got, err := s.DepListBatch([]string{"a", "b", "c"})
		return got != nil, err
	}
	metadataBatch := func(s *SQLiteStore) (bool, error) {
		got, err := s.DepMetadataBatch([]string{"a", "b", "c"})
		return got != nil, err
	}
	for _, tc := range []struct {
		name string
		// statement is a fragment of the one statement whose result fails.
		statement string
		read      func(*SQLiteStore) (bool, error)
	}{
		{"DepListBatch held anchors", "SELECT id FROM beads WHERE id IN", listBatch},
		{"DepListBatch edges", "FROM deps WHERE issue_id IN", listBatch},
		{"DepMetadataBatch edges", "FROM deps WHERE issue_id IN", metadataBatch},
		{"DepMetadataBatch payloads", "SELECT key, value FROM kv WHERE key IN", metadataBatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openBatchTestSQLite(t)
			if err := s.CreateWithForeignIDBatch([]Bead{{ID: "a", Title: "a"}, {ID: "b", Title: "b"}, {ID: "c", Title: "c"}}); err != nil {
				t.Fatal(err)
			}
			if err := s.DepAddBatch([]DepWithPayload{
				{Dep: Dep{IssueID: "a", DependsOnID: "b", Type: "waits-for"}, Payload: `{"gate":"any-children"}`},
				{Dep: Dep{IssueID: "a", DependsOnID: "c", Type: "blocks"}, Payload: `{"why":"c"}`},
				{Dep: Dep{IssueID: "b", DependsOnID: "c", Type: "blocks"}, Payload: `{"why":"b"}`},
			}); err != nil {
				t.Fatal(err)
			}
			pool := s.readDB
			s.readDB = sql.OpenDB(failingRowsConnector{driver: pool.Driver(), dsn: sqliteStoreDSN(s.path, false), statement: tc.statement, err: broken})
			if err := pool.Close(); err != nil {
				t.Fatalf("closing the original read pool: %v", err)
			}
			answered, err := tc.read(s)
			if !errors.Is(err, broken) {
				t.Fatalf("the read = %v, want the iteration error %v", err, broken)
			}
			if answered {
				t.Fatal("the read returned a map alongside its error: a partial answer reads the rest as absent")
			}
		})
	}
}

// failingRowsConnector opens the store's driver connections wrapped in
// failingRowsConn.
type failingRowsConnector struct {
	driver    driver.Driver
	dsn       string
	statement string
	err       error
}

func (c failingRowsConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return failingRowsConn{Conn: conn, statement: c.statement, err: c.err}, nil
}

func (c failingRowsConnector) Driver() driver.Driver { return c.driver }

// failingRowsConn fails the result of every query whose text holds statement
// after its first row.
type failingRowsConn struct {
	driver.Conn
	statement string
	err       error
}

func (c failingRowsConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := q.QueryContext(ctx, query, args)
	if err != nil || !strings.Contains(query, c.statement) {
		return rows, err
	}
	return &failingRows{Rows: rows, err: c.err}, nil
}

// failingRows serves its first row, then fails as a driver does on an I/O or
// corruption error part way through a result.
type failingRows struct {
	driver.Rows
	served bool
	err    error
}

func (r *failingRows) Next(dest []driver.Value) error {
	if r.served {
		return r.err
	}
	r.served = true
	return r.Rows.Next(dest)
}

// TestSQLiteDepRemoveBatchRemovesAsDepRemoveDoes: the batch removes each edge
// and its payload sidecar, and an edge that is not there is already removed.
func TestSQLiteDepRemoveBatchRemovesAsDepRemoveDoes(t *testing.T) {
	s := openBatchTestSQLite(t)
	if err := s.CreateWithForeignIDBatch([]Bead{{ID: "a", Title: "a"}, {ID: "b", Title: "b"}, {ID: "c", Title: "c"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DepAddBatch([]DepWithPayload{
		{Dep: Dep{IssueID: "a", DependsOnID: "b", Type: "waits-for"}, Payload: `{"gate":"any-children"}`},
		{Dep: Dep{IssueID: "a", DependsOnID: "c", Type: "blocks"}},
		{Dep: Dep{IssueID: "b", DependsOnID: "c", Type: "blocks"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DepRemoveBatch([]DepPair{{IssueID: "a", DependsOnID: "b"}, {IssueID: "b", DependsOnID: "c"}, {IssueID: "c", DependsOnID: "a"}}); err != nil {
		t.Fatalf("DepRemoveBatch: %v", err)
	}
	for id, want := range map[string]int{"a": 1, "b": 0, "c": 0} {
		if got, err := s.DepList(id, "down"); err != nil || len(got) != want {
			t.Errorf("DepList(%s) = %v (%v), want %d edge(s)", id, got, err, want)
		}
	}
	var sidecars int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM kv WHERE key GLOB ?`, sqliteGraphEdgeMetadataPairPrefix("a", "b")+"*").Scan(&sidecars); err != nil {
		t.Fatal(err)
	}
	if sidecars != 0 {
		t.Fatalf("the batch left %d payload sidecar row(s) for a -> b", sidecars)
	}
}
