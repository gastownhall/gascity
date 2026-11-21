package beads

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// cursorReportingStore is an in-memory store that also reports a schema
// migration cursor, standing in for a NativeDoltStore whose database is partway
// through bd's migrations.
type cursorReportingStore struct {
	Store
	current, target int
	err             error
}

func (s cursorReportingStore) SchemaCursor(context.Context) (int, int, error) {
	return s.current, s.target, s.err
}

var _ SchemaCursorReporter = cursorReportingStore{}

// TestCheckSchemaAtTargetRefusesAStalledMigration pins the Go-side half of the
// stalled-migration fix. A native store's Ping needs only the issues table
// (migration 1), so it passes while the migration is stalled at v60 of 66 and
// gc's scope-ready wait declared the scope ready.
//
// Ready is the cursor reaching the target. A scope that is behind must not read
// as ready, and the error names both cursors and the explicit consent step
// (`bd migrate schema`, beads#5920): gc never completes the migration itself.
func TestCheckSchemaAtTargetRefusesAStalledMigration(t *testing.T) {
	store := cursorReportingStore{Store: NewMemStore(), current: 60, target: 66}
	err := CheckSchemaAtTarget(context.Background(), store)
	if err == nil {
		t.Fatal("CheckSchemaAtTarget passed a store whose migration cursor is 60 of 66; a stalled migration must not read as ready")
	}
	for _, want := range []string{"60", "66", "bd migrate schema"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// TestCheckSchemaAtTargetAcceptsACursorAtTarget is the positive control.
func TestCheckSchemaAtTargetAcceptsACursorAtTarget(t *testing.T) {
	store := cursorReportingStore{Store: NewMemStore(), current: 66, target: 66}
	if err := CheckSchemaAtTarget(context.Background(), store); err != nil {
		t.Fatalf("CheckSchemaAtTarget rejected a store at its target cursor: %v", err)
	}
}

// TestCheckSchemaAtTargetLeavesSchemaSkewToTheOpenPath pins the other
// direction: a database ahead of this build is schema skew, which the
// library's open already classifies and reports. The readiness wait must not
// turn it into a stalled-migration failure.
func TestCheckSchemaAtTargetLeavesSchemaSkewToTheOpenPath(t *testing.T) {
	store := cursorReportingStore{Store: NewMemStore(), current: 70, target: 66}
	if err := CheckSchemaAtTarget(context.Background(), store); err != nil {
		t.Fatalf("CheckSchemaAtTarget rejected a store ahead of its target cursor: %v", err)
	}
}

// TestCheckSchemaAtTargetSurfacesAFailedCursorRead pins "don't swallow
// errors": a store that cannot say where its migration is has not shown it is
// ready.
func TestCheckSchemaAtTargetSurfacesAFailedCursorRead(t *testing.T) {
	boom := errors.New("read schema_migrations: connection refused")
	store := cursorReportingStore{Store: NewMemStore(), err: boom}
	err := CheckSchemaAtTarget(context.Background(), store)
	if !errors.Is(err, boom) {
		t.Fatalf("CheckSchemaAtTarget error = %v, want it to wrap %v", err, boom)
	}
}

// TestCheckSchemaAtTargetWithoutACursorCapabilityIsANoOp keeps stores with no
// live database handle (the subprocess store, the in-memory store) on their
// own readiness signal.
func TestCheckSchemaAtTargetWithoutACursorCapabilityIsANoOp(t *testing.T) {
	if err := CheckSchemaAtTarget(context.Background(), NewMemStore()); err != nil {
		t.Fatalf("CheckSchemaAtTarget failed for a store with no cursor capability: %v", err)
	}
}

// schemaCursorScriptedPool is a *sql.DB whose connections answer, as a Dolt
// server does, the statements the linked library's schema.CurrentVersion
// issues: the schema_migrations existence probe, the cursor read itself
// (cursor), and the cursor-reality probes of every sentinel table and column
// (all present, so the cursor is believed as read). Any other statement fails,
// so a change in what the read asks surfaces here instead of being answered by
// accident. The pool is the one a server-mode *dolt.DoltStore exports through
// UnderlyingDB (the accessor poolStorage stands in for).
type schemaCursorScriptedPool struct {
	cursor int64
	err    error // when set, every statement fails with it
}

func (p schemaCursorScriptedPool) open(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(schemaCursorConnector{pool: p})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type schemaCursorConnector struct{ pool schemaCursorScriptedPool }

func (c schemaCursorConnector) Connect(context.Context) (driver.Conn, error) {
	return schemaCursorConn(c), nil
}

func (schemaCursorConnector) Driver() driver.Driver { return schemaCursorDriver{} }

type schemaCursorDriver struct{}

func (schemaCursorDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("the scripted cursor pool has no DSN driver")
}

type schemaCursorConn struct{ pool schemaCursorScriptedPool }

func (schemaCursorConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("the scripted cursor pool answers queries directly")
}

func (schemaCursorConn) Close() error { return nil }

func (schemaCursorConn) Begin() (driver.Tx, error) {
	return nil, errors.New("the scripted cursor pool is read-only")
}

func (c schemaCursorConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if c.pool.err != nil {
		return nil, c.pool.err
	}
	lower := strings.ToLower(query)
	switch {
	case strings.Contains(lower, "information_schema"):
		return &schemaCursorRow{value: 1}, nil
	case strings.Contains(lower, "max(version)") && strings.Contains(lower, "schema_migrations"):
		return &schemaCursorRow{value: c.pool.cursor}, nil
	}
	return nil, fmt.Errorf("the scripted cursor pool has no answer for %q", query)
}

// schemaCursorRow is a result of one row and one column.
type schemaCursorRow struct {
	value int64
	read  bool
}

func (*schemaCursorRow) Columns() []string { return []string{"n"} }

func (*schemaCursorRow) Close() error { return nil }

func (r *schemaCursorRow) Next(dest []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	dest[0] = r.value
	return nil
}

// noPoolStorage is a library handle that exports no UnderlyingDB, as the
// embedded engine's store does.
type noPoolStorage struct{ beadslib.Storage }

// TestNativeDoltStoreSchemaCursorReadsTheCursorOfALiveHandle pins the engine
// end of the stalled-migration check, the end a server-mode store takes in
// production: SchemaCursor reads schema_migrations through the handle's pool
// and reports it against the version the linked library migrates to. The
// embedded workspace cmd/gc's engine test opens exports no pool, so that test
// only ever reaches the "unavailable" answer; without this one nothing fails
// when a server-mode store answers "unavailable" for every scope and the
// scope-ready wait waves a store stalled at v60 of v66 through.
func TestNativeDoltStoreSchemaCursorReadsTheCursorOfALiveHandle(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cursor  int
		wantErr string // CheckSchemaAtTarget must name this; "" means it passes
	}{
		{"stalled behind the target", 60, fmt.Sprintf("schema migration stalled at v60 of v%d", SchemaCursorMain)},
		{"at the target", SchemaCursorMain, ""},
		{"ahead of the target", SchemaCursorMain + 4, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := schemaCursorScriptedPool{cursor: int64(tc.cursor)}.open(t)
			store := newNativeDoltStoreForTest(&poolStorage{db: pool})

			current, target, err := store.SchemaCursor(context.Background())
			if err != nil {
				t.Fatalf("SchemaCursor over a live handle: %v", err)
			}
			if current != tc.cursor || target != SchemaCursorMain {
				t.Fatalf("SchemaCursor = (%d, %d), want (%d, %d)", current, target, tc.cursor, SchemaCursorMain)
			}

			err = CheckSchemaAtTarget(context.Background(), store)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("CheckSchemaAtTarget refused a store at cursor %d: %v", tc.cursor, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("CheckSchemaAtTarget passed a store stalled at v%d", tc.cursor)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("CheckSchemaAtTarget error %q does not name %q", err, tc.wantErr)
			}
		})
	}
}

// TestNativeDoltStoreSchemaCursorSurfacesAFailedRead keeps "don't swallow
// errors" on the real read: a handle whose pool cannot answer has not shown the
// scope is ready, and its error must not be the "unavailable" answer that
// CheckSchemaAtTarget skips.
func TestNativeDoltStoreSchemaCursorSurfacesAFailedRead(t *testing.T) {
	boom := errors.New("read schema_migrations: connection refused")
	store := newNativeDoltStoreForTest(&poolStorage{db: schemaCursorScriptedPool{err: boom}.open(t)})

	_, _, err := store.SchemaCursor(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("SchemaCursor error = %v, want it to wrap %v", err, boom)
	}
	if errors.Is(err, ErrSchemaCursorUnavailable) {
		t.Fatalf("SchemaCursor reported a failed read as unavailable (%v); CheckSchemaAtTarget would skip a store it could not read", err)
	}
	if err := CheckSchemaAtTarget(context.Background(), store); !errors.Is(err, boom) {
		t.Fatalf("CheckSchemaAtTarget error = %v, want it to wrap %v", err, boom)
	}
}

// TestNativeDoltStoreSchemaCursorIsUnavailableWithoutAPool pins the other
// answer of the same method: a library handle that exports no UnderlyingDB
// cannot report a cursor, says so with ErrSchemaCursorUnavailable naming the
// handle, and so leaves CheckSchemaAtTarget a no-op on that store.
func TestNativeDoltStoreSchemaCursorIsUnavailableWithoutAPool(t *testing.T) {
	store := newNativeDoltStoreForTest(noPoolStorage{})

	current, target, err := store.SchemaCursor(context.Background())
	if !errors.Is(err, ErrSchemaCursorUnavailable) {
		t.Fatalf("SchemaCursor over a handle with no pool = (%d, %d, %v), want ErrSchemaCursorUnavailable", current, target, err)
	}
	if !strings.Contains(err.Error(), "noPoolStorage") {
		t.Errorf("SchemaCursor error %q does not name the handle type", err)
	}
	if err := CheckSchemaAtTarget(context.Background(), store); err != nil {
		t.Errorf("CheckSchemaAtTarget over a handle with no pool = %v, want nil (the store is not asked)", err)
	}
}
