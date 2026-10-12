package beads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// nativeStoreMethodKind classifies one exported method of *NativeDoltStore.
type nativeStoreMethodKind int

const (
	// nativeStoreRead is a method the read-only latch must NOT refuse.
	nativeStoreRead nativeStoreMethodKind = iota
	// nativeStoreMutation is a method the latch must refuse before it reaches
	// storage.
	nativeStoreMutation
	// nativeStoreLifecycle is neither: it operates on the HANDLE rather than
	// on beads (closing the store, reporting a capability). A read-only handle
	// must still be closable, or every proxied open leaks a connection pool.
	nativeStoreLifecycle
)

// nativeStoreMethodKinds is the explicit classification of every exported
// method on *NativeDoltStore.
//
// It is explicit, and the test fails on an UNCLASSIFIED method, because that is
// the only way this fence survives contact with a future change. A heuristic
// ("names starting with Set or Create are writes") would quietly let the next
// mutation through under a name nobody thought of, and the failure mode is a
// native write to a database bd owns — the exact thing PR2 exists not to do.
// Adding a method to *NativeDoltStore therefore forces a deliberate answer
// here.
var nativeStoreMethodKinds = map[string]nativeStoreMethodKind{
	// beads.Store, mutations.
	"Create":           nativeStoreMutation,
	"Update":           nativeStoreMutation,
	"Close":            nativeStoreMutation,
	"Reopen":           nativeStoreMutation,
	"CloseAll":         nativeStoreMutation,
	"SetMetadata":      nativeStoreMutation,
	"SetMetadataBatch": nativeStoreMutation,
	"SetLocalString":   nativeStoreMutation,
	"Tx":               nativeStoreMutation,
	"Delete":           nativeStoreMutation,
	"DepAdd":           nativeStoreMutation,
	"DepRemove":        nativeStoreMutation,

	// beads.Store, reads.
	"Get":            nativeStoreRead,
	"List":           nativeStoreRead,
	"ListOpen":       nativeStoreRead,
	"Ready":          nativeStoreRead,
	"Children":       nativeStoreRead,
	"ListByLabel":    nativeStoreRead,
	"ListByAssignee": nativeStoreRead,
	"ListByMetadata": nativeStoreRead,
	"GetLocalString": nativeStoreRead,
	"Ping":           nativeStoreRead,
	"DepList":        nativeStoreRead,

	// Optional capabilities that write. ApplyGraphPlanWithStorage is the one
	// H4 is about: policy middleware selects the graph applier at wrap time, so
	// an ephemeral graph create can reach a native leaf without going through
	// Create at all.
	"ApplyGraphPlan":              nativeStoreMutation,
	"ApplyGraphPlanWithStorage":   nativeStoreMutation,
	"CreateWithForeignID":         nativeStoreMutation,
	"CloseIfMatch":                nativeStoreMutation,
	"CloseWithMetadataIfMatch":    nativeStoreMutation,
	"CompareAndSetMetadataKey":    nativeStoreMutation,
	"DeleteIfMatch":               nativeStoreMutation,
	"ReleaseIfCurrent":            nativeStoreMutation,
	"UpdateIfMatch":               nativeStoreMutation,
	"WaitForParentProjection":     nativeStoreRead,
	"Count":                       nativeStoreRead,
	"DepMetadata":                 nativeStoreRead,
	"SawRows":                     nativeStoreRead,
	"IDPrefix":                    nativeStoreLifecycle,
	"AtomicTx":                    nativeStoreLifecycle,
	"SupportsEphemeralGraphApply": nativeStoreLifecycle,
	"CloseStore":                  nativeStoreLifecycle,
	"ReadOnly":                    nativeStoreLifecycle,
}

// countingStorageSpy fails the test if the store reaches storage at all.
func countingStorageSpy(t *testing.T, touched *[]string) *nativeDoltStorageSpy {
	t.Helper()
	note := func(name string) { *touched = append(*touched, name) }
	return &nativeDoltStorageSpy{
		createIssue: func(context.Context, *beadslib.Issue, string) error {
			note("CreateIssue")
			return nil
		},
		createIssues: func(context.Context, []*beadslib.Issue, string) error {
			note("CreateIssues")
			return nil
		},
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			note("GetIssue")
			return nil, nil
		},
		updateIssue: func(context.Context, string, map[string]interface{}, string) error {
			note("UpdateIssue")
			return nil
		},
		updateIssueChecked: func(context.Context, string, map[string]interface{}, string, beadslib.UpdateIssueOptions) error {
			note("UpdateIssueChecked")
			return nil
		},
		runInTransaction: func(context.Context, string, func(beadslib.Transaction) error) error {
			note("RunInTransaction")
			return nil
		},
		reopenIssue: func(context.Context, string, string, string) error {
			note("ReopenIssue")
			return nil
		},
		closeIssue: func(context.Context, string, string, string, string) error {
			note("CloseIssue")
			return nil
		},
		closeIssueChecked: func(context.Context, string, string, beadslib.CloseIssueOptions) (beadslib.CloseIssueResult, error) {
			note("CloseIssueChecked")
			return beadslib.CloseIssueResult{}, nil
		},
		deleteIssue: func(context.Context, string) error {
			note("DeleteIssue")
			return nil
		},
		addLabel: func(context.Context, string, string, string) error {
			note("AddLabel")
			return nil
		},
		removeLabel: func(context.Context, string, string, string) error {
			note("RemoveLabel")
			return nil
		},
		addDependency: func(context.Context, *beadslib.Dependency, string) error {
			note("AddDependency")
			return nil
		},
		removeDependency: func(context.Context, string, string, string) error {
			note("RemoveDependency")
			return nil
		},
	}
}

// TestNativeDoltStoreReadOnlyLatchRefusesEveryMutationWithoutTouchingStorage is
// the fence under PR2's central promise: a native handle over bd's proxy serves
// reads and writes NOTHING.
//
// It is driven by reflection over the whole exported method set rather than by
// a hand-written list of calls, because a hand-written list is a census taken
// once. The classification above is the list that must be maintained, and an
// unclassified method fails this test — so adding a mutation to
// *NativeDoltStore cannot land without somebody deciding, in writing, whether
// the read-only latch refuses it.
//
// Two things are asserted per mutation: that the error is (or wraps)
// ErrProxiedNativeReadOnly, and that the storage handle was never touched. The
// second is what makes the guard's POSITION load-bearing rather than
// decorative: a refusal issued after the write reached the transaction would
// satisfy the first assertion and still have written to somebody else's
// database.
func TestNativeDoltStoreReadOnlyLatchRefusesEveryMutationWithoutTouchingStorage(t *testing.T) {
	storeType := reflect.TypeOf((*NativeDoltStore)(nil))
	var unclassified []string
	for i := 0; i < storeType.NumMethod(); i++ {
		name := storeType.Method(i).Name
		if _, ok := nativeStoreMethodKinds[name]; !ok {
			unclassified = append(unclassified, name)
		}
	}
	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Fatalf("unclassified *NativeDoltStore methods: %v.\n"+
			"Classify each one in nativeStoreMethodKinds. If it writes, it needs a readOnlyGuard at the top; "+
			"the proxied lane's whole promise is that a native handle over bd's proxy writes nothing.",
			unclassified)
	}

	// Every beads.Store method must be classified too: the map is keyed by
	// name, so a Store method missing from it would be caught above only if
	// *NativeDoltStore still declared it.
	storeIface := reflect.TypeOf((*Store)(nil)).Elem()
	for i := 0; i < storeIface.NumMethod(); i++ {
		name := storeIface.Method(i).Name
		kind, ok := nativeStoreMethodKinds[name]
		if !ok {
			t.Errorf("beads.Store method %s is unclassified", name)
			continue
		}
		if kind == nativeStoreLifecycle {
			t.Errorf("beads.Store method %s is classified as lifecycle; a Store method is a read or a mutation", name)
		}
	}

	scopeRoot := t.TempDir()
	sidecarPath := filepath.Join(scopeRoot, "local-strings.json")
	var touched []string
	store := newNativeDoltStoreForTest(countingStorageSpy(t, &touched), WithProxiedReadOnly())
	store.localStrings = newLocalSidecar(sidecarPath)

	if !store.ReadOnly() {
		t.Fatal("WithProxiedReadOnly did not latch the handle read-only")
	}

	storeValue := reflect.ValueOf(store)
	refused := 0
	for i := 0; i < storeType.NumMethod(); i++ {
		method := storeType.Method(i)
		if nativeStoreMethodKinds[method.Name] != nativeStoreMutation {
			continue
		}
		t.Run(method.Name, func(t *testing.T) {
			before := len(touched)
			results := storeValue.MethodByName(method.Name).Call(zeroArgsFor(method.Type))
			if len(results) == 0 {
				t.Fatalf("%s returns nothing, so it cannot report a refusal", method.Name)
			}
			last := results[len(results)-1]
			err, _ := last.Interface().(error)
			if err == nil {
				t.Fatalf("%s on a read-only handle returned no error", method.Name)
			}
			if !errors.Is(err, ErrProxiedNativeReadOnly) {
				t.Fatalf("%s refused with %v, want ErrProxiedNativeReadOnly", method.Name, err)
			}
			if got := touched[before:]; len(got) != 0 {
				t.Fatalf("%s reached storage before refusing: %v", method.Name, got)
			}
		})
		refused++
	}
	if refused < 20 {
		t.Fatalf("only %d mutations were exercised; the classification has lost methods", refused)
	}
	if _, err := os.Stat(sidecarPath); !os.IsNotExist(err) {
		t.Errorf("SetLocalString wrote the clone-local sidecar on a read-only handle (stat err = %v)", err)
	}
}

// TestNativeDoltStoreWithoutLatchStillMutates is the other half: the guard must
// be inert on a direct or hosted handle, or P2-06 would have turned every
// native store in the tree read-only and the suite would say so in a hundred
// places at once rather than here.
func TestNativeDoltStoreWithoutLatchStillMutates(t *testing.T) {
	var touched []string
	store := newNativeDoltStoreForTest(countingStorageSpy(t, &touched))
	if store.ReadOnly() {
		t.Fatal("a store opened without WithProxiedReadOnly reports itself read-only")
	}
	if err := store.readOnlyGuard(); err != nil {
		t.Fatalf("readOnlyGuard on an unlatched handle = %v, want nil", err)
	}
	if _, err := store.Create(Bead{ID: "gc-1", Title: "t"}); err != nil && errors.Is(err, ErrProxiedNativeReadOnly) {
		t.Fatalf("an unlatched handle refused a create as read-only: %v", err)
	}
	if len(touched) == 0 {
		t.Fatal("an unlatched Create never reached storage; the guard is refusing a handle it should ignore")
	}
}

// zeroArgsFor builds the zero value of each of a method's inputs, skipping the
// receiver. The guard runs before any argument is read, so zero values are
// enough — and using them is the point: a refusal that depended on a
// well-formed argument would not be a fence.
func zeroArgsFor(methodType reflect.Type) []reflect.Value {
	args := make([]reflect.Value, 0, methodType.NumIn()-1)
	for i := 1; i < methodType.NumIn(); i++ {
		args = append(args, reflect.Zero(methodType.In(i)))
	}
	return args
}
