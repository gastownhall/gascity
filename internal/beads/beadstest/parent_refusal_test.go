package beadstest

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// unresolvableParent is an id in a reserved namespace no store under test
// mints: the cross-store shape the ParentID conformance row exists to pin.
const unresolvableParent = "gcg-70b1e5f2-a"

// createHook wraps a Store with a replacement for the Create that names a
// parent, the one call the refusal check drives. Every other call, including a
// Create with no parent, passes through to the wrapped store.
type createHook struct {
	beads.Store
	create func(next beads.Store, b beads.Bead) (beads.Bead, error)
}

func (h createHook) Create(b beads.Bead) (beads.Bead, error) {
	if b.ParentID == "" {
		return h.Store.Create(b)
	}
	return h.create(h.Store, b)
}

// listFails is a Store whose List always errors.
type listFails struct{ beads.Store }

func (listFails) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("list backend down")
}

// parentNotFound is the error bd create --parent returns for an id it cannot
// resolve.
func parentNotFound(parent string) error {
	return fmt.Errorf("parent issue %s not found", parent)
}

// parentResolvingStore refuses a Create whose parent the store does not hold,
// the way BdStore does through bd, and otherwise behaves as a MemStore.
func parentResolvingStore() beads.Store {
	return createHook{Store: beads.NewMemStore(), create: func(next beads.Store, b beads.Bead) (beads.Bead, error) {
		if _, err := next.Get(b.ParentID); err != nil {
			return beads.Bead{}, parentNotFound(b.ParentID)
		}
		return next.Create(b)
	}}
}

func TestCheckRefusesUnresolvableParent(t *testing.T) {
	for _, tt := range []struct {
		name    string
		store   beads.Store
		wantErr []string // every substring the error must contain; nil means no error
	}{
		{
			name:  "documented refusal leaves nothing behind",
			store: parentResolvingStore(),
		},
		{
			name:    "a store that accepts the parent",
			store:   beads.NewMemStore(),
			wantErr: []string{"accepts foreign parents", "RefusesUnresolvableParent"},
		},
		{
			name: "refused for another reason",
			store: createHook{Store: beads.NewMemStore(), create: func(beads.Store, beads.Bead) (beads.Bead, error) {
				return beads.Bead{}, errors.New("bd create: exit status 1")
			}},
			wantErr: []string{"not found", "exit status 1"},
		},
		{
			name: "refusal that leaves a half-created child",
			store: createHook{Store: beads.NewMemStore(), create: func(next beads.Store, b beads.Bead) (beads.Bead, error) {
				if _, err := next.Create(b); err != nil {
					return beads.Bead{}, err
				}
				return beads.Bead{}, parentNotFound(b.ParentID)
			}},
			wantErr: []string{"left 1 bead"},
		},
		{
			name:    "a store that cannot list",
			store:   listFails{Store: beads.NewMemStore()},
			wantErr: []string{"list backend down"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The ParentID row creates its control bead first, so the count it
			// compares against starts from a non-empty store.
			if _, err := tt.store.Create(beads.Bead{Title: "control"}); err != nil {
				t.Fatal(err)
			}
			err := checkRefusesUnresolvableParent(tt.store, unresolvableParent)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("checkRefusesUnresolvableParent() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkRefusesUnresolvableParent() = nil, want an error containing %q", tt.wantErr)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("checkRefusesUnresolvableParent() = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestConformanceRunsTheRefusalHalfWhenDeclared proves Options wires the
// capability into the ParentID row: a store that, like BdStore, refuses a
// parent it cannot resolve passes the whole suite once it declares so. Without
// the declaration the row demands the full weak-reference contract and this
// store fails it.
func TestConformanceRunsTheRefusalHalfWhenDeclared(t *testing.T) {
	RunStoreTestsWithOptions(t, parentResolvingStore, Options{RefusesUnresolvableParent: true})
}
