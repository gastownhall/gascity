package splittest

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
)

// TestStrictStorePinnedIDFenceConformance runs the shared fenced-Create suite
// against the kit's own class leaf.
//
// This is the whole anti-drift mechanism. pinned_id_fence.go is a SECOND
// spelling of the rule internal/beads/pinned_id_fence.go enforces, and two
// spellings of one rule drift. What keeps them together is that both are held
// to the same PROVIDER contract: the rows below are byte-for-byte the rows the
// shipped SQLite and native-Dolt stores pass, so a leaf that admitted what the
// real binding refuses — or refused what it admits — goes red here first.
//
// The suite runs on SQLiteSemantics because that is the backend a relocated
// class binding runs on, and the suite's unfenced control needs it: an UNFENCED
// bd store rejects a foreign pinned id on the prefix mismatch, which is bd's
// own rule and not the fence's. That a FENCED bd leaf refuses on the fence
// instead is pinned separately, below.
func TestStrictStorePinnedIDFenceConformance(t *testing.T) {
	beadstest.RunPinnedIDFenceConformance(t, func(t *testing.T, mintPrefix string, namespaces ...string) beads.Store {
		t.Helper()
		s := newStrictMemLeaf(t, mintPrefix, SQLiteSemantics, namespaces...)
		// The unfenced control ACCEPTS the foreign pinned id, exactly as an
		// unfenced SQLite store does, and the kit records that acceptance as a
		// residence violation which fails the test at cleanup unless claimed.
		// Claiming it here is claiming the suite's own control, so it is scoped
		// to the unfenced case only: a fenced leaf that started recording
		// instead of refusing must still fail. Cleanups run LIFO, so this one
		// drains before the constructor's unclaimed-violation check reads.
		//
		// It asserts what it drains rather than discarding, because a blind
		// claim would also swallow anything ELSE an unfenced row went on to
		// accept — a cross-store dep, say — and silently retire the backstop
		// for every row the shared suite grows later.
		if len(namespaces) == 0 {
			t.Cleanup(func() {
				for _, v := range TakeResidenceViolations(s) {
					if v.Op != "create" {
						t.Errorf("the unfenced control accepted a %q violation (%s); only the control's own foreign-prefix create belongs here, and claiming the rest hides it", v.Op, v.Detail)
					}
				}
			})
		}
		return s
	})
}

// TestTheFenceHoldsUnderBdSemanticsToo pins the half the conformance run above
// cannot reach.
//
// Both leaves refuse the same foreign pinned id, and the fenced one refuses it
// with beads.ErrPinnedIDOutsideNamespace — the sentinel that will let a caller
// tell "route this to a sibling binding" from "this bead could not be created".
// (Nothing outside internal/beads reads it yet; it is the discrimination the
// error exists to make available.) An unfenced bd leaf refuses too, but on the
// prefix mismatch, which carries no sentinel; without this row the two failures
// are indistinguishable and a fenced bd leaf could quietly answer with the
// wrong one.
//
// The transactional row is here for the same reason: the fence runs before the
// semantics fork, so a single-spelling regression goes red in the SQLite
// conformance run above — but a regression that gated the guard ON a semantics
// would not, and Tx is the surface where that gate is easiest to write.
func TestTheFenceHoldsUnderBdSemanticsToo(t *testing.T) {
	fenced := newStrictMemLeaf(t, "kitn", BdSemantics, "kitn", "kitnq")
	_, err := fenced.Create(beads.Bead{ID: "kitwork-42", Title: "another binding's id"})
	if !errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
		t.Errorf("a fenced BdSemantics leaf refused with %v, want ErrPinnedIDOutsideNamespace; the fence is the binding's rule and does not change with the backend under it", err)
	}

	// The control: unfenced, the same create is bd's prefix mismatch instead —
	// still a refusal, and deliberately NOT the fence sentinel.
	unfenced := newStrictMemLeaf(t, "kitn", BdSemantics)
	_, err = unfenced.Create(beads.Bead{ID: "kitwork-42", Title: "another binding's id"})
	if err == nil {
		t.Fatal("an unfenced BdSemantics leaf accepted a foreign-prefix create; bd rejects the prefix mismatch")
	}
	if errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
		t.Error("an unfenced leaf refused with ErrPinnedIDOutsideNamespace; with no namespaces configured there is no namespace claim to be outside of, and answering with the sentinel tells a caller to go find a binding that does not exist")
	}

	// And the fenced leaf still admits the namespace it holds but never mints,
	// so the row above is not passing on a leaf that refuses everything.
	if _, err := fenced.Create(beads.Bead{ID: "kitnq-7", Title: "held, never minted"}); err != nil {
		t.Errorf("the fenced leaf refused an id in a namespace it holds: %v", err)
	}

	txErr := fenced.Tx("a foreign pinned id inside a transaction", func(tx beads.Tx) error {
		_, createErr := tx.Create(beads.Bead{ID: "kitwork-43", Title: "another binding's id, transactionally"})
		return createErr
	})
	if !errors.Is(txErr, beads.ErrPinnedIDOutsideNamespace) {
		t.Errorf("a fenced BdSemantics Tx create refused with %v, want ErrPinnedIDOutsideNamespace", txErr)
	}
	// Its must-be-silent counterpart, so the row above cannot pass on a Tx that
	// refuses every pinned id.
	if err := fenced.Tx("an in-namespace pinned id inside a transaction", func(tx beads.Tx) error {
		_, createErr := tx.Create(beads.Bead{ID: "kitnq-8", Title: "held, never minted, transactionally"})
		return createErr
	}); err != nil {
		t.Errorf("a fenced BdSemantics Tx refused an id in a namespace it holds: %v", err)
	}
}

// TestNewClassStoreIsFencedToEveryNamespaceItsClassHolds proves the fence the
// conformance suite exercises is actually WIRED into the kit's class stores —
// with the real reserved prefixes, not the suite's synthetic ones.
//
// The suite runs against a leaf the adapter builds by hand, so it says nothing
// about what NewClassStore configures. Nudges is the class that makes the
// distinction observable: it holds a queue prefix it never mints under, which
// is exactly the namespace a mint-only fence would refuse. That such a class
// exists is the test's whole discriminating power, so it is asserted rather
// than assumed — an emptied auxiliary table would otherwise leave every
// subtest green and the mint-only regression invisible.
func TestNewClassStoreIsFencedToEveryNamespaceItsClassHolds(t *testing.T) {
	classes := config.ReservedClassPrefixes()
	holdsMoreThanItMints := false
	for class := range classes {
		if len(config.ReservedClassPrefixesFor(class)) > 1 {
			holdsMoreThanItMints = true
			break
		}
	}
	if !holdsMoreThanItMints {
		t.Fatal("no class holds a namespace it does not mint under; with only mint prefixes left, a fence built from the mint alone passes every row below and the regression this test is for stops being observable")
	}

	for class := range classes {
		t.Run(class, func(t *testing.T) {
			store := NewClassStore(t, class)
			held := config.ReservedClassPrefixesFor(class)
			if len(held) == 0 {
				t.Fatalf("class %q reports no reserved prefixes; NewClassStore should have refused it", class)
			}
			for _, prefix := range held {
				id := prefix + "-fixture"
				created, err := store.Create(beads.Bead{ID: id, Title: "in a namespace this binding holds"})
				if err != nil {
					t.Errorf("Create(%q): %v — the %q binding holds this namespace, so its store must accept a pinned id in it", id, err, class)
					continue
				}
				if created.ID != id {
					t.Errorf("Create(%q) returned %q, want the pinned id verbatim", id, created.ID)
				}
			}
			foreign := "kitwork-1"
			if _, err := store.Create(beads.Bead{ID: foreign, Title: "no binding holds this"}); !errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
				t.Errorf("Create(%q) = %v, want ErrPinnedIDOutsideNamespace; an unfenced class store accepts another ledger's bead and its namespace claim stops holding", foreign, err)
			}
		})
	}
}

// TestNewWorkStoreIsUnfenced is the control for the row above. A work store
// mints under an operator-configured prefix and claims no reserved namespace,
// so fencing it would refuse the foreign-prefix create bd is supposed to answer
// with its own prefix-mismatch rejection — and would make every class store's
// fence indistinguishable from a blanket refusal.
//
// That the work store is not simply refusing everything is the companion pin:
// TestStoreTrioRoutesByPrefix creates "ra-1" on this same constructor, so an
// in-prefix pin is proven to go through.
func TestNewWorkStoreIsUnfenced(t *testing.T) {
	work := NewWorkStore(t, "ra")
	_, err := work.Create(beads.Bead{ID: "kitwork-1", Title: "foreign to this work store"})
	if err == nil {
		t.Fatal("the work store accepted a foreign-prefix create")
	}
	if errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
		t.Errorf("the work store refused with ErrPinnedIDOutsideNamespace: %v — it serves no class binding, so it has no namespace claim, and bd's prefix mismatch is the refusal it models", err)
	}
}
