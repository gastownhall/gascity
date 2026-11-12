package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// TestHoldLabelRoutedToCheck covers ga-fm2vgd.1: a bead carrying a
// hold:<value> label but whose gc.routed_to metadata is missing or does not
// match <value> has silently drifted from its intended route. The check must
// flag such beads across both city and rig stores, --fix must backfill
// gc.routed_to from the label value, hold:external must never be flagged,
// and beads with no hold:* label must be left untouched.
func TestHoldLabelRoutedToCheck(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}

	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		// Mismatch: hold:mayor label, no gc.routed_to at all.
		{ID: "H-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"}},
		// Mismatch: hold:mayor label, gc.routed_to set to something else.
		{
			ID: "H-2", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"},
			Metadata: map[string]string{"gc.routed_to": "reviewer"},
		},
		// Healthy: hold:mayor label, gc.routed_to already matches — must be left alone.
		{
			ID: "H-3", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"},
			Metadata: map[string]string{"gc.routed_to": "mayor"},
		},
		// Excluded by definition: hold:external names a human/out-of-system
		// dependency, never a routing gap, regardless of gc.routed_to state.
		{ID: "H-4", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:external"}},
		// No hold:* label at all — must be ignored even with empty gc.routed_to.
		{ID: "T-1", Title: "work", Type: "task", Status: "open"},
		// Generic over label value — must not require any special-casing of
		// "mayor" specifically; an arbitrary hold value must be caught too.
		{ID: "H-5", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:qa-lead"}},
		// Mismatch on a non-open bead: the scan must not filter to
		// Status=="open" only, or in_progress/blocked/deferred hold-labeled
		// beads silently escape it (ga-fm2vgd.2).
		{ID: "H-6", Title: "held", Type: "task", Status: "in_progress", Labels: []string{"hold:mayor"}},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "RH-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"}},
	}, nil)
	stores := map[string]beads.Store{cityDir: cityStore, rigDir: rigStore}
	factory := func(path string) (beads.Store, error) {
		store, ok := stores[path]
		if !ok {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	}

	check := newHoldLabelRoutedToCheck(cfg, cityDir, factory)

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
	}
	details := strings.Join(res.Details, "\n")
	for _, want := range []string{"H-1", "H-2", "H-5", "H-6", "RH-1"} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	for _, notWant := range []string{"H-3", "H-4", "T-1"} {
		if strings.Contains(details, notWant) {
			t.Fatalf("details should not mention %q:\n%s", notWant, details)
		}
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	// Idempotency: a second Run after Fix must report clean.
	if res2 := check.Run(&doctor.CheckContext{}); res2.Status != doctor.StatusOK {
		t.Fatalf("post-fix Run status = %v, want OK: %#v", res2.Status, res2)
	}

	h1, err := cityStore.Get("H-1")
	if err != nil {
		t.Fatalf("get H-1: %v", err)
	}
	if got := h1.Metadata["gc.routed_to"]; got != "mayor" {
		t.Errorf("H-1 gc.routed_to = %q, want mayor (backfilled from hold:mayor label)", got)
	}
	h2, err := cityStore.Get("H-2")
	if err != nil {
		t.Fatalf("get H-2: %v", err)
	}
	if got := h2.Metadata["gc.routed_to"]; got != "mayor" {
		t.Errorf("H-2 gc.routed_to = %q, want mayor (corrected from stale reviewer)", got)
	}
	h5, err := cityStore.Get("H-5")
	if err != nil {
		t.Fatalf("get H-5: %v", err)
	}
	if got := h5.Metadata["gc.routed_to"]; got != "qa-lead" {
		t.Errorf("H-5 gc.routed_to = %q, want qa-lead (arbitrary hold value, not special-cased)", got)
	}
	h6, err := cityStore.Get("H-6")
	if err != nil {
		t.Fatalf("get H-6: %v", err)
	}
	if got := h6.Metadata["gc.routed_to"]; got != "mayor" {
		t.Errorf("H-6 gc.routed_to = %q, want mayor (backfilled despite in_progress status)", got)
	}
	h4, err := cityStore.Get("H-4")
	if err != nil {
		t.Fatalf("get H-4: %v", err)
	}
	if got := h4.Metadata["gc.routed_to"]; got != "" {
		t.Errorf("H-4 (hold:external) gc.routed_to = %q, want untouched empty", got)
	}
	t1, err := cityStore.Get("T-1")
	if err != nil {
		t.Fatalf("get T-1: %v", err)
	}
	if got := t1.Metadata["gc.routed_to"]; got != "" {
		t.Errorf("T-1 (no hold label) gc.routed_to = %q, want untouched empty", got)
	}
	rh1, err := rigStore.Get("RH-1")
	if err != nil {
		t.Fatalf("get RH-1: %v", err)
	}
	if got := rh1.Metadata["gc.routed_to"]; got != "mayor" {
		t.Errorf("RH-1 gc.routed_to = %q, want mayor", got)
	}
}

// TestHoldLabelRoutedToCheckCleanStore confirms a store with no mismatches
// reports OK and CanFix advertises remediation.
func TestHoldLabelRoutedToCheckCleanStore(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{
			ID: "H-9", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"},
			Metadata: map[string]string{"gc.routed_to": "mayor"},
		},
		{ID: "H-10", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:external"}},
	}, nil)
	check := newHoldLabelRoutedToCheck(nil, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})
	if !check.CanFix() {
		t.Fatal("CanFix() = false, want true")
	}
	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
		t.Fatalf("Run status = %v, want OK: %#v", res.Status, res)
	}
}

// TestHoldLabelRoutedToCheckIgnoresClearedHold covers ga-uu1fpt: `bd set-state
// <id> hold=cleared` is the documented way to release a hold and leaves a
// hold:cleared label behind. "cleared" is the value the hold takes when there
// is no hold, not an agent, so the check must not flag such a bead and --fix
// must not backfill gc.routed_to=cleared over its real route.
func TestHoldLabelRoutedToCheckIgnoresClearedHold(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		// Released hold, no route yet.
		{ID: "C-1", Title: "released", Type: "task", Status: "open", Labels: []string{"hold:cleared"}},
		// Released hold on a bead already routed on: --fix must keep the route.
		{
			ID: "C-2", Title: "released", Type: "task", Status: "open", Labels: []string{"hold:cleared"},
			Metadata: map[string]string{"gc.routed_to": "gascity/deployer"},
		},
	}, nil)
	check := newHoldLabelRoutedToCheck(nil, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})

	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
		t.Fatalf("Run status = %v, want OK (hold:cleared is not a route): %#v", res.Status, res)
	}
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	for id, want := range map[string]string{"C-1": "", "C-2": "gascity/deployer"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got := b.Metadata["gc.routed_to"]; got != want {
			t.Errorf("%s gc.routed_to = %q, want %q (hold:cleared must leave the route alone)", id, got, want)
		}
	}
}

// TestHoldLabelRoutedToCheckClearedHoldDoesNotMaskLiveHold pins the other half of
// treating hold:cleared like hold:external (ga-uu1fpt): the label is skipped, not
// a stop. A hold released with `bd set-state <id> hold=cleared` leaves
// hold:cleared behind, and a hold added afterwards with `bd update --add-label`
// sits beside it, on either side. The live hold must still be read, so the check
// flags the bead and --fix backfills gc.routed_to from it, never from the
// released one.
func TestHoldLabelRoutedToCheckClearedHoldDoesNotMaskLiveHold(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "M-1", Title: "re-held", Type: "task", Status: "open", Labels: []string{"hold:cleared", "hold:mayor"}},
		{ID: "M-2", Title: "re-held", Type: "task", Status: "open", Labels: []string{"hold:mayor", "hold:cleared"}},
	}, nil)
	check := newHoldLabelRoutedToCheck(nil, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning (a live hold:mayor beside hold:cleared is a routing gap): %#v", res.Status, res)
	}
	details := strings.Join(res.Details, "\n")
	for _, id := range []string{"M-1", "M-2"} {
		want := "city bead " + id + ` has hold:mayor but gc.routed_to=""`
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	for _, id := range []string{"M-1", "M-2"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got := b.Metadata["gc.routed_to"]; got != "mayor" {
			t.Errorf("%s gc.routed_to = %q, want mayor (backfilled from the live hold, not from hold:cleared)", id, got)
		}
	}
	if res2 := check.Run(&doctor.CheckContext{}); res2.Status != doctor.StatusOK {
		t.Fatalf("post-fix Run status = %v, want OK: %#v", res2.Status, res2)
	}
}

func TestHoldLabelRoutedToFixReportsOpenFailures(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "H-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:mayor"}},
	}, nil)
	check := newHoldLabelRoutedToCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path == rigDir {
			return nil, errors.New("permission denied")
		}
		return cityStore, nil
	})

	err := check.Fix(&doctor.CheckContext{})
	if err == nil {
		t.Fatal("Fix error = nil, want skipped scope error")
	}
	if got := err.Error(); !strings.Contains(got, "rig repo skipped") || !strings.Contains(got, "permission denied") {
		t.Fatalf("Fix error = %q, want rig open failure detail", got)
	}
	h1, getErr := cityStore.Get("H-1")
	if getErr != nil {
		t.Fatalf("get H-1: %v", getErr)
	}
	if got := h1.Metadata["gc.routed_to"]; got != "mayor" {
		t.Fatalf("H-1 gc.routed_to = %q, want available repair applied despite skipped rig", got)
	}
}

func TestHoldLabelRoutedToFixReportsListFailures(t *testing.T) {
	cityDir := t.TempDir()
	check := newHoldLabelRoutedToCheck(nil, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return holdLabelListErrorStore{Store: beads.NewMemStore()}, nil
	})

	err := check.Fix(&doctor.CheckContext{})
	if err == nil {
		t.Fatal("Fix error = nil, want skipped scope error")
	}
	if got := err.Error(); !strings.Contains(got, "city skipped") || !strings.Contains(got, "listing failed") {
		t.Fatalf("Fix error = %q, want list failure detail", got)
	}
}

type holdLabelListErrorStore struct {
	beads.Store
}

func (s holdLabelListErrorStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("listing failed")
}

// holdLabelStores returns a store factory over a fixed set of per-path stores.
func holdLabelStores(stores map[string]beads.Store) func(string) (beads.Store, error) {
	return func(path string) (beads.Store, error) {
		store, ok := stores[path]
		if !ok {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	}
}

// holdLabelRoute returns the gc.routed_to metadata of bead id in store.
func holdLabelRoute(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b.Metadata["gc.routed_to"]
}

// holdLabelBoundCity binds planner (as an agent and, with the same canonical,
// a named session) and reviewer (as a named session only) under ops, plus a
// rig-scoped worker under ops in the repo rig at rigDir.
func holdLabelBoundCity(rigDir string) *config.City {
	return &config.City{
		Agents: []config.Agent{
			{Name: "planner", BindingName: "ops"},
			{Name: "worker", Dir: "repo", BindingName: "ops"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "planner", BindingName: "ops"},
			{Template: "reviewer", BindingName: "ops"},
		},
		Rigs: []config.Rig{{Name: "repo", Path: rigDir}},
	}
}

// TestHoldLabelRoutedToCheckAcceptsBindingQualifiedRoute covers ga-tkxbic: in
// a binding-prefixed city the canonical gc.routed_to for hold:<agent> is the
// binding-qualified form, which v2-routed-to-namespace requires. The hold
// check must accept it, and --fix must not regress it to the short form.
func TestHoldLabelRoutedToCheckAcceptsBindingQualifiedRoute(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Agents: []config.Agent{{Name: "dog", BindingName: "gastown"}}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{{
		ID: "H-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:dog"},
		Metadata: map[string]string{"gc.routed_to": "gastown.dog"},
	}}, nil)
	factory := func(string) (beads.Store, error) { return store, nil }

	if res := newV2RoutedToNamespaceCheck(cfg, cityDir, factory).Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
		t.Fatalf("v2-routed-to-namespace status = %v, want OK for canonical route: %#v", res.Status, res)
	}
	check := newHoldLabelRoutedToCheck(cfg, cityDir, factory)
	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
		t.Errorf("hold-label-routed-to status = %v, want OK for binding-qualified gc.routed_to: %#v", res.Status, res)
	}
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	got, err := store.Get("H-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if route := got.Metadata["gc.routed_to"]; route != "gastown.dog" {
		t.Fatalf("after Fix gc.routed_to = %q, want canonical %q", route, "gastown.dog")
	}
}

// TestHoldLabelRoutedToCheckAcceptsCanonicalBoundRoutes covers ga-tkxbic AC-1
// across city and rig stores: a hold value bound under a binding is satisfied
// by its binding-qualified gc.routed_to, whether the binding comes from an
// agent, a named session only, or a dir-qualified rig agent. An already
// qualified hold value and an exact short-form route stay accepted. Neither
// Run nor Fix may touch any of these routes.
func TestHoldLabelRoutedToCheckAcceptsCanonicalBoundRoutes(t *testing.T) {
	cityDir, rigDir := t.TempDir(), t.TempDir()
	held := func(id, hold, route string) beads.Bead {
		return beads.Bead{
			ID: id, Title: "held", Type: "task", Status: "open", Labels: []string{"hold:" + hold},
			Metadata: map[string]string{"gc.routed_to": route},
		}
	}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		held("Q-1", "planner", "ops.planner"),
		held("Q-2", "reviewer", "ops.reviewer"),
		held("Q-3", "ops.planner", "ops.planner"),
		held("Q-4", "planner", "planner"),
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		held("RQ-1", "repo/worker", "repo/ops.worker"),
		held("RQ-2", "planner", "ops.planner"),
	}, nil)
	check := newHoldLabelRoutedToCheck(holdLabelBoundCity(rigDir), cityDir,
		holdLabelStores(map[string]beads.Store{cityDir: cityStore, rigDir: rigStore}))

	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
		t.Fatalf("Run status = %v, want OK for canonical bound routes: %#v", res.Status, res)
	}
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	for _, tc := range []struct {
		store beads.Store
		id    string
		want  string
	}{
		{cityStore, "Q-1", "ops.planner"},
		{cityStore, "Q-2", "ops.reviewer"},
		{cityStore, "Q-3", "ops.planner"},
		{cityStore, "Q-4", "planner"},
		{rigStore, "RQ-1", "repo/ops.worker"},
		{rigStore, "RQ-2", "ops.planner"},
	} {
		if got := holdLabelRoute(t, tc.store, tc.id); got != tc.want {
			t.Errorf("%s gc.routed_to = %q after Fix, want unchanged %q", tc.id, got, tc.want)
		}
	}
}

// TestHoldLabelRoutedToCheckRepairsToCanonicalBoundRoute covers ga-tkxbic
// AC-2: when a bound hold value's gc.routed_to is missing or names another
// route, the check reports it with the binding-qualified repair and Fix
// writes that canonical route, in city and rig stores alike, so both the hold
// check and v2-routed-to-namespace then report OK.
func TestHoldLabelRoutedToCheckRepairsToCanonicalBoundRoute(t *testing.T) {
	cityDir, rigDir := t.TempDir(), t.TempDir()
	cfg := holdLabelBoundCity(rigDir)
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "M-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"}},
		{
			ID: "M-2", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"},
			Metadata: map[string]string{"gc.routed_to": "someone-else"},
		},
		{ID: "M-3", Title: "held", Type: "task", Status: "blocked", Labels: []string{"hold:reviewer"}},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "RM-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:repo/worker"}},
		{
			ID: "RM-2", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"},
			Metadata: map[string]string{"gc.routed_to": "repo/ops.worker"},
		},
	}, nil)
	factory := holdLabelStores(map[string]beads.Store{cityDir: cityStore, rigDir: rigStore})
	check := newHoldLabelRoutedToCheck(cfg, cityDir, factory)

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
	}
	details := strings.Join(res.Details, "\n")
	for _, want := range []string{
		`city bead M-1 has hold:planner but gc.routed_to=""; use "ops.planner"`,
		`city bead M-2 has hold:planner but gc.routed_to="someone-else"; use "ops.planner"`,
		`city bead M-3 has hold:reviewer but gc.routed_to=""; use "ops.reviewer"`,
		`rig repo bead RM-1 has hold:repo/worker but gc.routed_to=""; use "repo/ops.worker"`,
		`rig repo bead RM-2 has hold:planner but gc.routed_to="repo/ops.worker"; use "ops.planner"`,
	} {
		if !strings.Contains(details, want) {
			t.Errorf("details missing %q:\n%s", want, details)
		}
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	for _, tc := range []struct {
		store beads.Store
		id    string
		want  string
	}{
		{cityStore, "M-1", "ops.planner"},
		{cityStore, "M-2", "ops.planner"},
		{cityStore, "M-3", "ops.reviewer"},
		{rigStore, "RM-1", "repo/ops.worker"},
		{rigStore, "RM-2", "ops.planner"},
	} {
		if got := holdLabelRoute(t, tc.store, tc.id); got != tc.want {
			t.Errorf("%s gc.routed_to = %q after Fix, want canonical %q", tc.id, got, tc.want)
		}
	}
	if res := check.Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
		t.Errorf("post-fix hold-label-routed-to status = %v, want OK: %#v", res.Status, res)
	}
	if res := newV2RoutedToNamespaceCheck(cfg, cityDir, factory).Run(&doctor.CheckContext{}); res.Status != doctor.StatusOK {
		t.Errorf("post-fix v2-routed-to-namespace status = %v, want OK: %#v", res.Status, res)
	}
}

// TestHoldLabelRoutedToFixReportsCanonicalWriteFailure pins that a failed
// canonical repair surfaces as a Fix error naming the bead.
func TestHoldLabelRoutedToFixReportsCanonicalWriteFailure(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Agents: []config.Agent{{Name: "planner", BindingName: "ops"}}}
	store := holdLabelSetMetadataErrorStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "W-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"}},
	}, nil)}
	check := newHoldLabelRoutedToCheck(cfg, cityDir, holdLabelStores(map[string]beads.Store{cityDir: store}))

	err := check.Fix(&doctor.CheckContext{})
	if err == nil {
		t.Fatal("Fix error = nil, want metadata write failure")
	}
	if got := err.Error(); !strings.Contains(got, "city bead W-1") || !strings.Contains(got, "write refused") {
		t.Fatalf("Fix error = %q, want bead and write failure detail", got)
	}
}

type holdLabelSetMetadataErrorStore struct {
	beads.Store
}

func (s holdLabelSetMetadataErrorStore) SetMetadata(string, string, string) error {
	return errors.New("write refused")
}

// TestHoldLabelRoutedToFixConvergesWithV2RoutedToNamespace covers ga-tkxbic
// AC-3: gc doctor --fix runs v2-routed-to-namespace before
// hold-label-routed-to, and each check verifies only itself, so the two
// checks must agree on the canonical route. One fix pass leaves every held
// route binding-qualified, and a second pass finds nothing to fix. Either
// registration order converges.
func TestHoldLabelRoutedToFixConvergesWithV2RoutedToNamespace(t *testing.T) {
	for _, tc := range []struct {
		name          string
		namespaceLast bool
	}{
		{name: "namespace then hold"},
		{name: "hold then namespace", namespaceLast: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			cfg := &config.City{Agents: []config.Agent{{Name: "planner", BindingName: "ops"}}}
			store := beads.NewMemStoreFrom(0, []beads.Bead{
				{
					ID: "C-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"},
					Metadata: map[string]string{"gc.routed_to": ""},
				},
				{
					ID: "C-2", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"},
					Metadata: map[string]string{"gc.routed_to": "planner"},
				},
				{
					ID: "C-3", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"},
					Metadata: map[string]string{"gc.routed_to": "ops.planner"},
				},
			}, nil)
			factory := holdLabelStores(map[string]beads.Store{cityDir: store})
			namespace := newV2RoutedToNamespaceCheck(cfg, cityDir, factory)
			hold := newHoldLabelRoutedToCheck(cfg, cityDir, factory)
			d := &doctor.Doctor{}
			if tc.namespaceLast {
				d.Register(hold)
				d.Register(namespace)
			} else {
				d.Register(namespace)
				d.Register(hold)
			}

			d.RunCollect(&doctor.CheckContext{}, true)
			for _, id := range []string{"C-1", "C-2", "C-3"} {
				if got := holdLabelRoute(t, store, id); got != "ops.planner" {
					t.Errorf("after first fix pass %s gc.routed_to = %q, want ops.planner", id, got)
				}
			}

			second := d.RunCollect(&doctor.CheckContext{}, true)
			if second.Warned != 0 || second.Fixed != 0 {
				t.Errorf("second fix pass Warned=%d Fixed=%d, want 0 and 0: %#v", second.Warned, second.Fixed, second.Results)
			}
			for _, res := range second.Results {
				if res.Status != doctor.StatusOK {
					t.Errorf("second fix pass %s status = %v, want OK: %#v", res.Name, res.Status, res)
				}
			}
		})
	}
}

// TestHoldLabelRoutedToCheckLeavesAmbiguousBoundHoldUntouched covers
// ga-tkxbic AC-4: when the hold value is bound under two bindings, a route
// naming neither candidate has no single repair. Run lists both choices, Fix
// leaves the metadata alone without failing, and the warning survives
// doctor's verifying re-run. A route already equal to either candidate, or to
// the hold value itself, satisfies the hold check, and an ambiguous hold does
// not block repairing a uniquely bound one in the same scan.
func TestHoldLabelRoutedToCheckLeavesAmbiguousBoundHoldUntouched(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Agents: []config.Agent{
		{Name: "planner", BindingName: "ops"},
		{Name: "planner", BindingName: "other"},
		{Name: "worker", BindingName: "ops"},
	}}
	held := func(id, hold, route string) beads.Bead {
		return beads.Bead{
			ID: id, Title: "held", Type: "task", Status: "open", Labels: []string{"hold:" + hold},
			Metadata: map[string]string{"gc.routed_to": route},
		}
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		held("A-1", "planner", ""),
		held("A-2", "planner", "someone-else"),
		held("A-3", "planner", "ops.planner"),
		held("A-4", "planner", "other.planner"),
		held("A-5", "worker", ""),
		held("A-6", "planner", "planner"),
	}, nil)
	factory := holdLabelStores(map[string]beads.Store{cityDir: store})
	check := newHoldLabelRoutedToCheck(cfg, cityDir, factory)

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
	}
	details := strings.Join(res.Details, "\n")
	for _, want := range []string{
		`city bead A-1 has hold:planner but gc.routed_to=""; use one of ops.planner, other.planner`,
		`city bead A-2 has hold:planner but gc.routed_to="someone-else"; use one of ops.planner, other.planner`,
		`city bead A-5 has hold:worker but gc.routed_to=""; use "ops.worker"`,
	} {
		if !strings.Contains(details, want) {
			t.Errorf("details missing %q:\n%s", want, details)
		}
	}
	for _, notWant := range []string{"A-3", "A-4", "A-6"} {
		if strings.Contains(details, notWant) {
			t.Errorf("details should not mention %q:\n%s", notWant, details)
		}
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v (ambiguity alone is not an operational error)", err)
	}
	for id, want := range map[string]string{
		"A-1": "", "A-2": "someone-else", "A-3": "ops.planner",
		"A-4": "other.planner", "A-5": "ops.worker", "A-6": "planner",
	} {
		if got := holdLabelRoute(t, store, id); got != want {
			t.Errorf("%s gc.routed_to = %q after Fix, want %q", id, got, want)
		}
	}

	d := &doctor.Doctor{}
	d.Register(check)
	report := d.RunCollect(&doctor.CheckContext{}, true)
	if report.Warned != 1 || report.Fixed != 0 || len(report.Results) != 1 {
		t.Fatalf("doctor --fix Warned=%d Fixed=%d results=%d, want the unresolved warning kept: %#v",
			report.Warned, report.Fixed, len(report.Results), report.Results)
	}
	verified := strings.Join(report.Results[0].Details, "\n")
	for _, id := range []string{"A-1", "A-2"} {
		if !strings.Contains(verified, "city bead "+id+" ") {
			t.Errorf("verified details missing %s:\n%s", id, verified)
		}
	}

	namespace := newV2RoutedToNamespaceCheck(cfg, cityDir, factory).Run(&doctor.CheckContext{})
	want := `city bead A-6 has gc.routed_to="planner"; use one of ops.planner, other.planner`
	if !strings.Contains(strings.Join(namespace.Details, "\n"), want) {
		t.Errorf("v2-routed-to-namespace details missing %q: %#v", want, namespace)
	}
}
