package beads

import (
	"context"
	"errors"
	"strings"
	"testing"
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
