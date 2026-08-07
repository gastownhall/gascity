package beads_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// CloseStore releases both database handles, so every later call would
// dereference a nil *sql.DB. These two maps record the only exported methods
// allowed to survive that: everything else must report beads.ErrStoreClosed.
// A method added later lands in neither map and fails the test.
var (
	sqliteMethodsExemptFromClosedGuard = map[string]string{
		"CloseStore":       "idempotent: a second close is a no-op returning nil",
		"SequenceFloor":    "reads the id-floor sidecar file, never the database handle",
		"SetSequenceFloor": "writes the id-floor sidecar file under its own flock",
	}
	sqliteMethodsWithoutErrorResult = map[string]string{
		"AdvanceSequenceFloor": "raises the in-memory id allocator only",
		"AtomicTx":             "reports a constant capability",
		"IDPrefix":             "returns an immutable field",
		"StoreHealthPath":      "returns an immutable field",
	}
)

// TestSQLiteStoreExportedMethodsRejectUseAfterClose walks the exported method
// set by reflection and calls every one of them on a closed store. Each must
// return an error matching beads.ErrStoreClosed, and none may panic.
func TestSQLiteStoreExportedMethodsRejectUseAfterClose(t *testing.T) {
	store, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	sqlite, ok := store.(*beads.SQLiteStore)
	if !ok {
		t.Fatalf("OpenSQLiteStore returned %T, want *beads.SQLiteStore", store)
	}
	if _, err := sqlite.Create(beads.Bead{Title: "before close"}); err != nil {
		t.Fatalf("Create before close: %v", err)
	}
	if err := sqlite.CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}
	if err := sqlite.CloseStore(); err != nil {
		t.Fatalf("second CloseStore = %v, want nil", err)
	}

	value := reflect.ValueOf(sqlite)
	storeType := value.Type()
	guarded := 0
	for i := 0; i < storeType.NumMethod(); i++ {
		name := storeType.Method(i).Name
		if _, exempt := sqliteMethodsExemptFromClosedGuard[name]; exempt {
			continue
		}
		method := value.Method(i)
		errIndex := errorResultIndex(method.Type())
		if errIndex < 0 {
			if _, known := sqliteMethodsWithoutErrorResult[name]; !known {
				t.Errorf("%s returns no error, so it cannot report a closed store; guard it or record why it is safe", name)
			}
			continue
		}
		guarded++
		t.Run(name, func(t *testing.T) {
			results, panicked := callWithZeroArgs(method)
			if panicked != nil {
				t.Fatalf("%s panicked on a closed store: %v", name, panicked)
			}
			got, _ := results[errIndex].Interface().(error)
			if !errors.Is(got, beads.ErrStoreClosed) {
				t.Fatalf("%s on a closed store = %v, want an error matching ErrStoreClosed", name, got)
			}
		})
	}
	if guarded < 30 {
		t.Fatalf("reflection reached only %d error-returning methods; the enumeration is broken", guarded)
	}

	for name := range sqliteMethodsExemptFromClosedGuard {
		if _, found := storeType.MethodByName(name); !found {
			t.Errorf("exempt method %s no longer exists; drop it from the exemption list", name)
		}
	}
	for name := range sqliteMethodsWithoutErrorResult {
		if _, found := storeType.MethodByName(name); !found {
			t.Errorf("method %s no longer exists; drop it from the no-error list", name)
		}
	}
}

// TestSQLiteStoreTxRefusesBeforeBeginningAfterClose proves Tx rejects a closed
// store before it opens a transaction, rather than handing the callback a
// half-built handle.
func TestSQLiteStoreTxRefusesBeforeBeginningAfterClose(t *testing.T) {
	store, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	sqlite, ok := store.(*beads.SQLiteStore)
	if !ok {
		t.Fatalf("OpenSQLiteStore returned %T, want *beads.SQLiteStore", store)
	}
	if err := sqlite.CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}

	called := false
	err = sqlite.Tx("after-close", func(beads.Tx) error {
		called = true
		return nil
	})
	if !errors.Is(err, beads.ErrStoreClosed) {
		t.Fatalf("Tx on a closed store = %v, want an error matching ErrStoreClosed", err)
	}
	if called {
		t.Fatal("Tx ran its callback on a closed store")
	}
}

var errorInterface = reflect.TypeOf((*error)(nil)).Elem()

// errorResultIndex returns the position of a method's error result, or -1 when
// it has none.
func errorResultIndex(method reflect.Type) int {
	for i := 0; i < method.NumOut(); i++ {
		if method.Out(i) == errorInterface {
			return i
		}
	}
	return -1
}

// callWithZeroArgs invokes a bound method with a zero value for each fixed
// parameter and no variadic ones, reporting a panic instead of letting it
// unwind the test binary. Arguments never matter here: a closed store must be
// rejected before any argument is inspected.
func callWithZeroArgs(method reflect.Value) (results []reflect.Value, panicked any) {
	defer func() { panicked = recover() }()
	signature := method.Type()
	fixed := signature.NumIn()
	if signature.IsVariadic() {
		fixed--
	}
	args := make([]reflect.Value, fixed)
	for i := range args {
		args[i] = reflect.New(signature.In(i)).Elem()
	}
	return method.Call(args), nil
}
