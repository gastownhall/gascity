package beadstest

// The pinned-id fence, as a PROVIDER contract.
//
// A class binding claims an id namespace, and a store serving one is fenced to
// the namespaces it claims. Minting settles the ids a store GENERATES; it says
// nothing about the ids a caller PINS, and Create honors a pinned id verbatim.
// So an unfenced binding accepts a foreign bead and its namespace claim quietly
// stops holding: the bead is then unreachable by every id-shaped lookup of the
// namespace it actually sits in.
//
// The rows below are behavior a caller can observe, never an implementation. A
// suite that demanded particular error PROSE would be pinning one store's
// wording, which is why every refusal is matched on
// beads.ErrPinnedIDOutsideNamespace.
//
// Half the rows are over-restriction controls, grouped under a subtest that
// says so: each passes on a build with no fence at all, which is the point.
// Without them a store satisfies this suite by refusing every pinned id, and
// the bindings that legitimately pin one — a wisp root, a nudge-queue record, a
// work bead under an operator-configured prefix — stop working.

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// RunPinnedIDFenceConformance exercises the fenced-Create contract every store
// serving a class binding must satisfy.
//
// openFenced must return a fresh, empty store minting under mintPrefix and
// fenced to exactly the given namespaces, forwarding them VERBATIM to the same
// configuration surface production wiring uses. In particular the adapter must
// not branch on emptiness: passing no namespaces has to reach production's own
// no-namespaces case, which is an UNFENCED store — the shipped default, and the
// control this suite needs to tell a fence from a blanket refusal. An adapter
// that special-cases the empty set passes this suite while the production path
// it stands in for refuses every work bead.
func RunPinnedIDFenceConformance(t *testing.T, openFenced func(t *testing.T, mintPrefix string, namespaces ...string) beads.Store) {
	t.Helper()

	// A binding mints under one namespace and may hold others without ever
	// minting them — the nudges store holds the nudge queue's records that way.
	// The values are synthetic on purpose: a store that consulted the tree's own
	// reserved-prefix table instead of the namespaces it was configured with
	// would pass every row below if the suite reused live prefixes, and would be
	// wrong for every binding outside that table. What is NOT arbitrary is that
	// the mint namespace is a proper string prefix of the auxiliary one, which
	// is what makes the separator rows able to fail.
	const (
		mint    = "kitn"
		aux     = "kitnq"
		foreign = "kitwork"
	)

	fenced := func(t *testing.T) beads.Store {
		t.Helper()
		return openFenced(t, mint, mint, aux)
	}

	t.Run("RefusesAPinnedIDOutsideTheNamespacesItServes", func(t *testing.T) {
		s := fenced(t)
		_, err := s.Create(beads.Bead{ID: foreign + "-42", Title: "another binding's id, pinned into this one"})
		if err == nil {
			t.Fatal("Create accepted a foreign pinned id; the binding's namespace claim is now false and the bead is unreachable by every id-shaped lookup")
		}
		if !errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
			t.Errorf("refusal is %v, which does not wrap ErrPinnedIDOutsideNamespace; a caller cannot tell 'route this to a sibling binding' from 'this bead could not be created'", err)
		}
	})

	// Membership is tested on the separator, not on string containment. Both
	// rows here fail a store that asks only whether the id starts with a
	// declared namespace: such a store admits "kitnx-1" — which belongs to no
	// binding at all — and, worse, admits every "kitnq-" id through a fence
	// declaring only "kitn", collapsing two namespaces into one.
	t.Run("MembershipIsTestedOnTheSeparator", func(t *testing.T) {
		s := fenced(t)
		for _, id := range []string{mint, mint + "x-1"} {
			_, err := s.Create(beads.Bead{ID: id, Title: "adjacent to the namespace, not inside it"})
			if err == nil {
				t.Errorf("Create(%q) was accepted; prefix containment is not namespace membership", id)
				continue
			}
			if !errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
				t.Errorf("Create(%q) = %v, want ErrPinnedIDOutsideNamespace", id, err)
			}
		}
	})

	// The id is tested exactly as it will be STORED. A store that trims before
	// checking certifies a membership the persisted row does not have: " kitn-1"
	// passes the check and lands under an id no lookup of this namespace
	// reaches, and an id that is nothing but spaces is neither a mint request
	// nor in any namespace, yet passes both tests on the way in.
	t.Run("MembershipIsTestedOnTheIDAsStored", func(t *testing.T) {
		s := fenced(t)
		for _, id := range []string{"   ", " " + mint + "-1", "\t" + mint + "-2"} {
			_, err := s.Create(beads.Bead{ID: id, Title: "not the id it would be stored under"})
			if err == nil {
				t.Errorf("Create(%q) was accepted; the store validated an id it does not persist", id)
				continue
			}
			if !errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
				t.Errorf("Create(%q) = %v, want ErrPinnedIDOutsideNamespace", id, err)
			}
		}
	})

	// The refusal has to be a refusal. A store that "helpfully" re-minted the
	// bead under its own namespace returns no error at all, so the pin is that
	// nothing was written under EITHER id.
	t.Run("ARefusalWritesNothing", func(t *testing.T) {
		s := fenced(t)
		id := foreign + "-7"
		if _, err := s.Create(beads.Bead{ID: id, Title: "refused"}); err == nil {
			t.Fatal("Create accepted a foreign pinned id")
		}
		if _, err := s.Get(id); !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("after the refusal Get(%q) = %v, want ErrNotFound; the store kept the row it said it would not take", id, err)
		}
		rows, err := s.List(beads.ListQuery{AllowScan: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Errorf("the store holds %d bead(s) after refusing every create: %v; a fence that re-mints under its own namespace is not a fence, it is a silent rename", len(rows), titlesOf(rows))
		}
	})

	// A refused id must not move the mint sequence. The hazard is concrete: a
	// store that normalizes the incoming bead before checking the fence lifts
	// its sequence floor to the FOREIGN id's suffix, and the next mint jumps to
	// it — the refused caller has renumbered a binding it was not allowed to
	// write to. The check therefore has to run before normalization.
	//
	// The claim is "the refusal moved nothing", so the reading is against a
	// control store that saw no refusal. Asserting a particular first suffix
	// instead would bake in "sequences start at 1", which is not part of this
	// contract.
	t.Run("ARefusalDoesNotConsumeTheMintSequence", func(t *testing.T) {
		control, err := openFenced(t, mint, mint, aux).Create(beads.Bead{Title: "first mint, no refusal before it"})
		if err != nil {
			t.Fatalf("Create(mint) on the control store: %v", err)
		}
		s := fenced(t)
		if _, err := s.Create(beads.Bead{ID: foreign + "-424242", Title: "refused, with a high suffix"}); err == nil {
			t.Fatal("Create accepted a foreign pinned id")
		}
		minted, err := s.Create(beads.Bead{Title: "first mint, after a refusal"})
		if err != nil {
			t.Fatalf("Create(mint): %v", err)
		}
		want, ok := numericSuffixOf(control.ID)
		if !ok {
			t.Skipf("this store mints %q, which carries no numeric suffix; the sequence claim is not observable here", control.ID)
		}
		got, ok := numericSuffixOf(minted.ID)
		if !ok {
			t.Fatalf("the control minted %q but the store under test minted %q, which carries no numeric suffix", control.ID, minted.ID)
		}
		if got != want {
			t.Errorf("the first minted id is %q (suffix %d), want suffix %d as on a store that saw no refusal; the refused id's suffix leaked into the mint sequence", minted.ID, got, want)
		}
	})

	t.Run("OverRestrictionControls", func(t *testing.T) {
		// Every row in this subtest passes on a build with NO fence. That is
		// what they are for — delete them and refusing every pinned id becomes
		// a conforming implementation. Keep them even when they look idle.

		t.Run("AdmitsEveryNamespaceTheBindingHolds", func(t *testing.T) {
			s := fenced(t)
			for _, id := range []string{mint + "-7", aux + "-abc-q"} {
				created, err := s.Create(beads.Bead{ID: id, Title: "held"})
				if err != nil {
					t.Fatalf("Create(%q): %v — a binding holds more than it mints, and a fence admitting only the mint namespace breaks the subsystems that derive their own ids", id, err)
				}
				if created.ID != id {
					t.Errorf("Create(%q) returned %q; a pinned id inside the namespace must be honored verbatim", id, created.ID)
				}
			}
		})

		// Membership folds case; the stored id does not. A store that admitted
		// the id and then wrote it lowercased would satisfy a bare error check
		// and leave Get(the caller's id) missing — the same unreachable-bead
		// outcome the fence exists to prevent, arrived at from the other side.
		t.Run("MembershipIsCaseInsensitiveWithoutRewritingTheID", func(t *testing.T) {
			s := fenced(t)
			in := strings.ToUpper(mint) + "-9"
			created, err := s.Create(beads.Bead{ID: in, Title: "shouting, but in-namespace"})
			if err != nil {
				t.Fatalf("Create(%q) was refused: %v — case is not part of the namespace", in, err)
			}
			if created.ID != in {
				t.Errorf("Create(%q) returned %q; folding case for the membership test must not fold the stored id", in, created.ID)
			}
			if _, err := s.Get(in); err != nil {
				t.Errorf("Get(%q) after creating it: %v", in, err)
			}
			// The other side: folding case must not fold the namespace away.
			out := strings.ToUpper(foreign) + "-9"
			if _, err := s.Create(beads.Bead{ID: out, Title: "shouting, and foreign"}); !errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
				t.Errorf("Create(%q) = %v, want ErrPinnedIDOutsideNamespace", out, err)
			}
		})

		t.Run("AMintIsNeverFenced", func(t *testing.T) {
			s := fenced(t)
			created, err := s.Create(beads.Bead{Title: "minted"})
			if err != nil {
				t.Fatalf("Create with no pinned id was refused: %v — the fence inspects only ids the CALLER supplied", err)
			}
			if !strings.HasPrefix(strings.ToLower(created.ID), mint+"-") {
				t.Errorf("minted id %q does not carry the store's own namespace", created.ID)
			}
		})

		// The fence governs the id being created, never the ids the bead REFERS
		// to. Those are weak cross-store references: a split city routinely
		// hangs a graph-class step off a work-class root in another ledger, and
		// a fence reaching into them would refuse every one of those creates.
		t.Run("TheFenceGovernsTheIDNotTheReferences", func(t *testing.T) {
			s := fenced(t)
			foreignParent := foreign + "-99"
			foreignNeed := foreign + "-100"
			created, err := s.Create(beads.Bead{
				ID:       mint + "-step",
				Title:    "a step whose molecule and blocker both live in another ledger",
				ParentID: foreignParent,
				Needs:    []string{foreignNeed},
			})
			if err != nil {
				t.Fatalf("Create with an in-namespace id and foreign references was refused: %v — the fence reached past the id, which breaks every cross-store molecule", err)
			}
			if created.ParentID != foreignParent {
				t.Errorf("ParentID came back %q, want %q verbatim", created.ParentID, foreignParent)
			}
			read, err := s.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if read.ParentID != foreignParent {
				t.Errorf("after a round trip ParentID is %q, want %q verbatim", read.ParentID, foreignParent)
			}
		})

		t.Run("TheForeignIDCreateStaysOpenForTheMigrationCopy", func(t *testing.T) {
			s := fenced(t)
			creator, ok := s.(beads.ForeignIDCreator)
			if !ok {
				t.Skip("store is not a ForeignIDCreator")
			}
			id := foreign + "-42"
			created, err := creator.CreateWithForeignID(beads.Bead{ID: id, Title: "a relic carried across by gc storage migrate"})
			if err != nil {
				t.Fatalf("CreateWithForeignID refused a preserved id: %v — the migration copy path cannot do its job, and the beads it carries would end up nowhere at all", err)
			}
			if created.ID != id {
				t.Errorf("got id %q, want the preserved %q", created.ID, id)
			}
		})

		// The control that keeps the whole suite from describing a store which
		// refuses pinned ids outright: no namespaces means unfenced, which is
		// the shipped default and how a binding serving the work class is
		// opened (work beads carry the operator's configured prefix, which is
		// not a namespace anything here can name).
		t.Run("AnUnfencedStoreHonorsAnyPinnedID", func(t *testing.T) {
			s := openFenced(t, mint)
			id := foreign + "-42"
			created, err := s.Create(beads.Bead{ID: id, Title: "no fence configured"})
			if err != nil {
				t.Fatalf("an unfenced store refused a pinned id: %v", err)
			}
			if created.ID != id {
				t.Errorf("got id %q, want %q", created.ID, id)
			}
		})
	})

	// The fence covers the transactional write surface too.
	//
	// Store.Tx is a Create in every sense the invariant cares about: the bead it
	// writes is as resident, and as unreachable by an id-shaped lookup of the
	// namespace it lands in, as one written outside a transaction. A store that
	// fences only the standalone Create passes every row above and still lets a
	// caller write a foreign bead into a binding that claims not to hold one —
	// which is how this row came to exist (ga-qdt5y.14 review, both shipped
	// providers had the hole).
	//
	// Tx is on Store, so this row can neither skip nor be satisfied by omission.
	t.Run("TheFenceCoversTheTransactionalCreate", func(t *testing.T) {
		s := fenced(t)
		id := foreign + "-42"
		err := s.Tx("a foreign pinned id inside a transaction", func(tx beads.Tx) error {
			_, createErr := tx.Create(beads.Bead{ID: id, Title: "pinned inside a transaction"})
			return createErr
		})
		if err == nil {
			t.Fatal("Tx accepted a foreign pinned id; a transaction is not an exemption from the namespace claim")
		}
		if !errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
			t.Errorf("refusal is %v, which does not wrap ErrPinnedIDOutsideNamespace", err)
		}
		if _, err := s.Get(id); !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("after the refusal Get(%q) = %v, want ErrNotFound", id, err)
		}

		// The must-be-silent counterpart: an in-namespace pin inside a
		// transaction still goes through, honored verbatim. Without it a store
		// conforms by making Tx.Create refuse everything.
		held := aux + "-inside-a-tx"
		if err := s.Tx("an in-namespace pinned id inside a transaction", func(tx beads.Tx) error {
			created, createErr := tx.Create(beads.Bead{ID: held, Title: "held by this binding"})
			if createErr != nil {
				return createErr
			}
			if created.ID != held {
				t.Errorf("Tx Create(%q) returned %q; a pinned id inside the namespace must be honored verbatim", held, created.ID)
			}
			return nil
		}); err != nil {
			t.Fatalf("Tx refused an in-namespace pinned id: %v", err)
		}
		if _, err := s.Get(held); err != nil {
			t.Errorf("Get(%q) after committing it in a transaction: %v", held, err)
		}
	})

	// Fence before existence. A relic under a foreign id can already be
	// resident, and a later create pinning that id must still be told the store
	// does not serve the namespace — not that the row is a duplicate. The
	// ordering is the contract: a refusal about a disclaimed namespace must not
	// double as a probe for what the store holds.
	t.Run("TheNamespaceIsCheckedBeforeExistence", func(t *testing.T) {
		s := fenced(t)
		creator, ok := s.(beads.ForeignIDCreator)
		if !ok {
			t.Skip("store is not a ForeignIDCreator")
		}
		id := foreign + "-42"
		if _, err := creator.CreateWithForeignID(beads.Bead{ID: id, Title: "relic"}); err != nil {
			t.Fatalf("CreateWithForeignID: %v", err)
		}
		_, err := s.Create(beads.Bead{ID: id, Title: "a second create pinning the same foreign id"})
		if err == nil {
			t.Fatal("Create accepted a foreign pinned id")
		}
		if !errors.Is(err, beads.ErrPinnedIDOutsideNamespace) {
			t.Errorf("refusal is %v, want ErrPinnedIDOutsideNamespace; answering with a duplicate-id error tells the caller this store holds the row, which is exactly what a disclaimed namespace must not reveal", err)
		}
	})
}

// numericSuffixOf reads the trailing numeric segment of a minted id. It reports
// false for an id that has none, so a store minting under some other scheme
// skips the sequence row instead of failing it.
func numericSuffixOf(id string) (int, bool) {
	idx := strings.LastIndex(id, "-")
	if idx < 0 || idx == len(id)-1 {
		return 0, false
	}
	n, err := strconv.Atoi(id[idx+1:])
	if err != nil {
		return 0, false
	}
	return n, true
}
