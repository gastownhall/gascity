package molecule

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// writeChooserFormula lays down a two-step formula whose name is also its file
// name, and returns the search path.
func writeChooserFormula(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	toml := `
formula = "` + name + `"
description = "Store-chooser test"

[[steps]]
id = "implement"
title = "Implement"

[[steps]]
id = "verify"
title = "Verify"
depends_on = ["implement"]
`
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}
	return dir
}

// The whole point of the entry point: the destination is a property of the
// COMPILED recipe, so the chooser is handed that recipe and every bead lands in
// what it picked — including the root, whose id the caller stamps metadata on.
func TestCookChoosingStoreInstantiatesIntoTheStoreTheChooserPicked(t *testing.T) {
	dir := writeChooserFormula(t, "chooser-e2e")
	picked := beads.NewMemStore()
	ignored := beads.NewMemStore()

	var sawRecipe *formula.Recipe
	result, store, err := CookChoosingStore(context.Background(), "chooser-e2e", []string{dir}, Options{Title: "Auth Flow"},
		func(recipe *formula.Recipe) beads.Store {
			sawRecipe = recipe
			return picked
		})
	if err != nil {
		t.Fatalf("CookChoosingStore: %v", err)
	}
	if sawRecipe == nil {
		t.Fatal("the chooser was never called; the store was picked without seeing the recipe, which is the whole reason this entry point exists")
	}
	if sawRecipe.Name != "chooser-e2e" {
		t.Errorf("the chooser saw recipe %q, want the compiled chooser-e2e", sawRecipe.Name)
	}
	if store != beads.Store(picked) {
		t.Errorf("CookChoosingStore returned a store the chooser did not pick; a caller stamping --meta on result.RootID would write to a store that has never held it")
	}
	if _, err := picked.Get(result.RootID); err != nil {
		t.Errorf("the root is not in the store the chooser picked: %v", err)
	}
	if len(result.IDMapping) != 3 {
		t.Errorf("the fixture is a root plus two steps but IDMapping holds %d; the residency loop below would be vacuous", len(result.IDMapping))
	}
	for stepKey, id := range result.IDMapping {
		if _, err := picked.Get(id); err != nil {
			t.Errorf("step %s (%s) is not in the store the chooser picked: %v", stepKey, id, err)
		}
	}
	if beadCount(t, ignored) != 0 {
		t.Errorf("the store the chooser did NOT pick holds %d bead(s)", beadCount(t, ignored))
	}
}

// Exactly once, and after validation. A chooser called per bead, or called on a
// recipe that has not been validated, is a different contract than the one the
// doc comment promises — and callers rely on it: moleculeClassStore is cheap but
// a chooser that opened a store would be opening one per bead.
func TestCookChoosingStoreCallsTheChooserOnceAfterValidation(t *testing.T) {
	dir := writeChooserFormula(t, "chooser-once")
	store := beads.NewMemStore()

	calls := 0
	if _, _, err := CookChoosingStore(context.Background(), "chooser-once", []string{dir}, Options{},
		func(*formula.Recipe) beads.Store {
			calls++
			return store
		}); err != nil {
		t.Fatalf("CookChoosingStore: %v", err)
	}
	if calls != 1 {
		t.Errorf("the chooser ran %d time(s), want exactly 1", calls)
	}

	// The "after validation" half. A recipe that compiles but whose required
	// vars are unsatisfied must not reach the chooser either: the run is already
	// refused, and a chooser is free to be expensive.
	strictDir := t.TempDir()
	strict := `
formula = "chooser-strict"
description = "Required var with no value"

[vars.who]
description = "Who"
required = true

[[steps]]
id = "implement"
title = "Implement {{who}}"
`
	if err := os.WriteFile(filepath.Join(strictDir, "chooser-strict.toml"), []byte(strict), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}
	calls = 0
	_, chosen, err := CookChoosingStore(context.Background(), "chooser-strict", []string{strictDir}, Options{},
		func(*formula.Recipe) beads.Store {
			calls++
			return store
		})
	if err == nil {
		t.Fatal("a recipe with an unsatisfied required var cooked successfully")
	}
	if chosen != nil {
		t.Error("a failed CookChoosingStore returned a non-nil store")
	}
	if calls != 0 {
		t.Errorf("the chooser ran %d time(s) for a recipe that failed validation; the choice happens after validation, not before", calls)
	}
}

// A formula that does not compile must not reach the chooser at all. The chooser
// is where a caller resolves a store, and resolving one for a recipe that will
// never exist is work done for a run that cannot happen.
func TestCookChoosingStoreDoesNotChooseWhenCompileFails(t *testing.T) {
	store := beads.NewMemStore()
	called := false
	_, chosen, err := CookChoosingStore(context.Background(), "no-such-formula", []string{t.TempDir()}, Options{},
		func(*formula.Recipe) beads.Store {
			called = true
			return store
		})
	if err == nil {
		t.Fatal("compiling a formula that does not exist succeeded")
	}
	if called {
		t.Error("the chooser ran for a formula that never compiled")
	}
	if chosen != nil {
		t.Error("a failed CookChoosingStore returned a non-nil store")
	}
	if beadCount(t, store) != 0 {
		t.Errorf("a failed compile wrote %d bead(s)", beadCount(t, store))
	}
}

// A chooser that answers nil is a caller bug, and it has to surface as one
// rather than as a nil-store panic inside Instantiate — the call is between
// validation and the first write, so there is nothing to roll back but also
// nothing yet written.
func TestCookChoosingStoreRefusesANilChoice(t *testing.T) {
	dir := writeChooserFormula(t, "chooser-nil")
	_, store, err := CookChoosingStore(context.Background(), "chooser-nil", []string{dir}, Options{},
		func(*formula.Recipe) beads.Store { return nil })
	if err == nil {
		t.Fatal("a nil store choice was accepted")
	}
	if !strings.Contains(err.Error(), "chooser-nil") {
		t.Errorf("the refusal does not name the formula: %v", err)
	}
	if store != nil {
		t.Error("a failed CookChoosingStore returned a non-nil store")
	}
}

// The formula must COMPILE, or this row proves nothing: a name that does not
// resolve fails before the guard is reached, and "some error came back" is then
// true whether the nil chooser is refused or called.
func TestCookChoosingStoreRequiresAChooser(t *testing.T) {
	dir := writeChooserFormula(t, "chooser-required")
	_, store, err := CookChoosingStore(context.Background(), "chooser-required", []string{dir}, Options{}, nil)
	if err == nil {
		t.Fatal("a nil chooser was accepted")
	}
	if !strings.Contains(err.Error(), "StoreChooser") {
		t.Errorf("the refusal does not name the missing chooser: %v", err)
	}
	if store != nil {
		t.Error("a failed CookChoosingStore returned a non-nil store")
	}
}

// Cook is CookChoosingStore with a constant chooser, so this pins the one thing
// that rewrite could have changed: Cook still writes into the store it was
// handed, whatever the recipe turns out to be.
func TestCookStillWritesIntoTheStoreItWasGiven(t *testing.T) {
	dir := writeChooserFormula(t, "chooser-cook")
	store := beads.NewMemStore()

	result, err := Cook(context.Background(), store, "chooser-cook", []string{dir}, Options{Title: "Auth Flow"})
	if err != nil {
		t.Fatalf("Cook: %v", err)
	}
	if _, err := store.Get(result.RootID); err != nil {
		t.Fatalf("the root is not in the store Cook was given: %v", err)
	}
}

// The last path on which "the store is nil when err is not" could be broken: the
// chooser answered, the store is in hand, and instantiation then fails. Handing
// that store back would invite the caller to stamp metadata onto a root the
// failure means does not exist.
func TestCookChoosingStoreReturnsNoStoreWhenInstantiateFails(t *testing.T) {
	dir := writeChooserFormula(t, "chooser-instantiate-fail")
	// Second create is the first step bead; the root is already down by then, so
	// the failure lands mid-molecule rather than before any write.
	picked := &errStore{Store: beads.NewMemStore(), failOnCreate: 2}

	result, store, err := CookChoosingStore(context.Background(), "chooser-instantiate-fail", []string{dir}, Options{},
		func(*formula.Recipe) beads.Store { return picked })
	if err == nil {
		t.Fatal("instantiating into a store whose create fails succeeded")
	}
	if result != nil {
		t.Error("a failed CookChoosingStore returned a non-nil result")
	}
	if store != nil {
		t.Error("a failed CookChoosingStore returned the store it chose; the caller would stamp metadata onto a root the failure says is not there")
	}
}

// Cook refuses a nil store in its own vocabulary. Routing straight to the
// chooser's nil check would answer a Cook caller with an error naming a store
// chooser that appears nowhere in the call they wrote.
func TestCookRefusesANilStoreInItsOwnTerms(t *testing.T) {
	dir := writeChooserFormula(t, "chooser-cook-nil")
	_, err := Cook(context.Background(), nil, "chooser-cook-nil", []string{dir}, Options{})
	if err == nil {
		t.Fatal("Cook accepted a nil store")
	}
	if !strings.Contains(err.Error(), "chooser-cook-nil") {
		t.Errorf("the refusal does not name the formula: %v", err)
	}
	if strings.Contains(err.Error(), "store chooser") {
		t.Errorf("Cook's nil-store refusal talks about a store chooser its caller never wrote: %v", err)
	}
}

func beadCount(t *testing.T, store beads.Store) int {
	t.Helper()
	// Not ListQuery{AllowScan: true} alone: that drops closed rows and, with the
	// zero TierMode, wisps — so a molecule written into the wrong store could be
	// counted as zero beads by the very row asserting the store is empty.
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("listing the store: %v", err)
	}
	return len(all)
}
