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

// TestHoldLabelRoutedToCheckBindingQualifiedRoutes covers ga-tkxbic: in a
// binding-prefixed city, gc.routed_to for a hold:<value> bead is canonical
// in its binding-qualified form (v2-routed-to-namespace rewrites short forms
// to it), so the hold check must accept that qualified form instead of
// demanding the bare hold value, and --fix must write the qualified form
// when it backfills a missing or unrelated route. Covers both city and rig
// stores, named-session bindings (including an agent and session sharing a
// qualified name, which stays unambiguous), dir-qualified hold values, and
// metadata-write error propagation.
func TestHoldLabelRoutedToCheckBindingQualifiedRoutes(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "planner", BindingName: "ops"},
			{Name: "worker", Dir: "repo", BindingName: "ops"},
			{Name: "scribe", BindingName: "ops"},
		},
		NamedSessions: []config.NamedSession{{Name: "scribe", BindingName: "ops"}},
		Rigs:          []config.Rig{{Name: "repo", Path: rigDir}},
	}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		// Healthy: the canonical binding-qualified route must satisfy the
		// check and survive --fix untouched.
		{
			ID: "HQ-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"},
			Metadata: map[string]string{"gc.routed_to": "ops.planner"},
		},
		// Mismatch: missing gc.routed_to — --fix must backfill the qualified form.
		{ID: "HQ-2", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"}},
		// Mismatch: unrelated route — --fix must rewrite it to the qualified form.
		{
			ID: "HQ-3", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"},
			Metadata: map[string]string{"gc.routed_to": "reviewer"},
		},
		// Dir-qualified hold value resolves through its dir-qualified canonical.
		{
			ID: "HQ-4", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:repo/worker"},
			Metadata: map[string]string{"gc.routed_to": "someone-else"},
		},
		// A hold value bound only via a named session (agent scribe exists too,
		// but shares the session's qualified name so the canonical is unique).
		{ID: "HQ-5", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:scribe"}},
		// No hold label — ignored.
		{ID: "T-9", Title: "work", Type: "task", Status: "open"},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "RHQ-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"}},
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
	for _, want := range []string{
		`city bead HQ-2 has hold:planner but gc.routed_to=""; use "ops.planner"`,
		`city bead HQ-3 has hold:planner but gc.routed_to="reviewer"; use "ops.planner"`,
		`city bead HQ-4 has hold:repo/worker but gc.routed_to="someone-else"; use "repo/ops.worker"`,
		`city bead HQ-5 has hold:scribe but gc.routed_to=""; use "ops.scribe"`,
		`rig repo bead RHQ-1 has hold:planner but gc.routed_to=""; use "ops.planner"`,
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	for _, notWant := range []string{"bead HQ-1", "T-9"} {
		if strings.Contains(details, notWant) {
			t.Fatalf("details should not mention %q:\n%s", notWant, details)
		}
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	for id, wantRoute := range map[string]string{
		"HQ-1":  "ops.planner",
		"HQ-2":  "ops.planner",
		"HQ-3":  "ops.planner",
		"HQ-4":  "repo/ops.worker",
		"HQ-5":  "ops.scribe",
		"RHQ-1": "ops.planner",
	} {
		store := cityStore
		if id == "RHQ-1" {
			store = rigStore
		}
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got := b.Metadata["gc.routed_to"]; got != wantRoute {
			t.Errorf("%s gc.routed_to = %q, want %q", id, got, wantRoute)
		}
	}

	// After --fix both the hold check and v2-routed-to-namespace must be OK:
	// the backfill writes exactly the canonical form the namespace check requires.
	if res2 := check.Run(&doctor.CheckContext{}); res2.Status != doctor.StatusOK {
		t.Fatalf("post-fix hold-label-routed-to status = %v, want OK: %#v", res2.Status, res2)
	}
	if nsRes := newV2RoutedToNamespaceCheck(cfg, cityDir, factory).Run(&doctor.CheckContext{}); nsRes.Status != doctor.StatusOK {
		t.Fatalf("post-fix v2-routed-to-namespace status = %v, want OK: %#v", nsRes.Status, nsRes)
	}

	// A named-session-only binding must resolve too — an Agent's absence must
	// not mask session support.
	t.Run("named-session-only-binding", func(t *testing.T) {
		sessDir := t.TempDir()
		sessCfg := &config.City{NamedSessions: []config.NamedSession{{Name: "scribe", BindingName: "ops"}}}
		sessStore := beads.NewMemStoreFrom(0, []beads.Bead{
			{ID: "NS-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:scribe"}},
		}, nil)
		sessFactory := func(path string) (beads.Store, error) {
			if path != sessDir {
				return nil, fmt.Errorf("unexpected store path %q", path)
			}
			return sessStore, nil
		}
		sessCheck := newHoldLabelRoutedToCheck(sessCfg, sessDir, sessFactory)
		if res := sessCheck.Run(&doctor.CheckContext{}); res.Status != doctor.StatusWarning {
			t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
		}
		if err := sessCheck.Fix(&doctor.CheckContext{}); err != nil {
			t.Fatalf("Fix: %v", err)
		}
		b, err := sessStore.Get("NS-1")
		if err != nil {
			t.Fatalf("get NS-1: %v", err)
		}
		if got := b.Metadata["gc.routed_to"]; got != "ops.scribe" {
			t.Errorf("NS-1 gc.routed_to = %q, want ops.scribe (named-session binding)", got)
		}
	})

	// Metadata-write failures keep propagating with bead context.
	t.Run("setmetadata-error-propagates", func(t *testing.T) {
		errDir := t.TempDir()
		errCfg := &config.City{Agents: []config.Agent{{Name: "planner", BindingName: "ops"}}}
		errStore := holdLabelSetErrorStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
			{ID: "E-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"}},
		}, nil)}
		errCheck := newHoldLabelRoutedToCheck(errCfg, errDir, func(path string) (beads.Store, error) {
			if path != errDir {
				return nil, fmt.Errorf("unexpected store path %q", path)
			}
			return errStore, nil
		})
		err := errCheck.Fix(&doctor.CheckContext{})
		if err == nil {
			t.Fatal("Fix error = nil, want setmetadata failure")
		}
		if got := err.Error(); !strings.Contains(got, "city bead E-1") || !strings.Contains(got, "metadata write failed") {
			t.Fatalf("Fix error = %q, want bead context and store error detail", got)
		}
	})
}

type holdLabelSetErrorStore struct {
	beads.Store
}

func (s holdLabelSetErrorStore) SetMetadata(string, string, string) error {
	return errors.New("metadata write failed")
}

// TestHoldLabelRoutedToCheckLeavesAmbiguousBoundHoldUntouched covers ga-tkxbic:
// when a hold value maps to several binding-qualified canonicals there is no
// single write target, so the hold check must list the candidates, --fix must
// leave the bead's metadata unchanged for manual resolution, and the warning
// must remain on the verifying re-run — while still repairing unambiguous
// targets in the same scan. Routes already equal to either candidate are
// healthy, and a short route equal to the hold suffix stays the namespace
// check's finding, not this one's.
func TestHoldLabelRoutedToCheckLeavesAmbiguousBoundHoldUntouched(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "planner", BindingName: "ops"},
			{Name: "planner", BindingName: "other"},
			{Name: "builder", BindingName: "ops"},
		},
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		// Ambiguous, missing route: report both candidates, never repair.
		{ID: "AM-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"}},
		// Ambiguous, unrelated route: same contract.
		{
			ID: "AM-2", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"},
			Metadata: map[string]string{"gc.routed_to": "reviewer"},
		},
		// Already routed to either candidate: healthy, not a finding.
		{
			ID: "AM-3", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"},
			Metadata: map[string]string{"gc.routed_to": "ops.planner"},
		},
		{
			ID: "AM-4", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"},
			Metadata: map[string]string{"gc.routed_to": "other.planner"},
		},
		// Short route equal to the hold suffix satisfies the hold check; the
		// namespace check owns reporting the ambiguity of migrating it.
		{
			ID: "AM-5", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"},
			Metadata: map[string]string{"gc.routed_to": "planner"},
		},
		// An already-qualified hold value keeps exact-match behavior.
		{
			ID: "AM-6", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:ops.builder"},
			Metadata: map[string]string{"gc.routed_to": "ops.builder"},
		},
		// Uniquely resolvable held bead in the same scan: ambiguity elsewhere
		// must not block its repair.
		{ID: "AM-7", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:builder"}},
	}, nil)
	factory := func(path string) (beads.Store, error) {
		if path != cityDir {
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
	for _, want := range []string{
		`city bead AM-1 has hold:planner but gc.routed_to=""; use one of ops.planner, other.planner`,
		`city bead AM-2 has hold:planner but gc.routed_to="reviewer"; use one of ops.planner, other.planner`,
		`city bead AM-7 has hold:builder but gc.routed_to=""; use "ops.builder"`,
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	for _, notWant := range []string{"bead AM-3", "bead AM-4", "bead AM-5", "bead AM-6"} {
		if strings.Contains(details, notWant) {
			t.Fatalf("details should not mention %q:\n%s", notWant, details)
		}
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v (ambiguity alone must not fail)", err)
	}
	for id, wantRoute := range map[string]string{
		"AM-1": "",
		"AM-2": "reviewer",
		"AM-3": "ops.planner",
		"AM-4": "other.planner",
		"AM-5": "planner",
		"AM-6": "ops.builder",
		"AM-7": "ops.builder",
	} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got := b.Metadata["gc.routed_to"]; got != wantRoute {
			t.Errorf("%s gc.routed_to = %q, want %q", id, got, wantRoute)
		}
	}

	// The unresolved warning remains on verification.
	res2 := check.Run(&doctor.CheckContext{})
	if res2.Status != doctor.StatusWarning {
		t.Fatalf("post-fix Run status = %v, want warning (ambiguous holds stay unresolved): %#v", res2.Status, res2)
	}
	details2 := strings.Join(res2.Details, "\n")
	for _, want := range []string{"bead AM-1", "bead AM-2"} {
		if !strings.Contains(details2, want) {
			t.Fatalf("post-fix details missing %q:\n%s", want, details2)
		}
	}
	if strings.Contains(details2, "bead AM-7") {
		t.Fatalf("post-fix details should not mention repaired bead AM-7:\n%s", details2)
	}

	// The short route equal to the hold suffix is the namespace check's
	// finding, and it reports the same two candidates for manual choice.
	nsRes := newV2RoutedToNamespaceCheck(cfg, cityDir, factory).Run(&doctor.CheckContext{})
	if nsRes.Status != doctor.StatusWarning {
		t.Fatalf("v2-routed-to-namespace status = %v, want warning for short route: %#v", nsRes.Status, nsRes)
	}
	nsDetails := strings.Join(nsRes.Details, "\n")
	if want := `city bead AM-5 has gc.routed_to="planner"; use one of ops.planner, other.planner`; !strings.Contains(nsDetails, want) {
		t.Fatalf("namespace details missing %q:\n%s", want, nsDetails)
	}
}

// TestHoldLabelRoutedToCheckUnboundShadow covers ga-tkxbic AC-5: a hold
// value config does not bind must keep the verbatim backfill contract, even
// when another agent binds the same name — boundRoutedToAliases's
// unbound-owner rule shadows the binding, so the bare hold value stays the
// canonical route and --fix must not qualify it. A hold value with no config
// entry at all backfills verbatim the same way. The namespace check reports
// OK for the backfilled short forms.
func TestHoldLabelRoutedToCheckUnboundShadow(t *testing.T) {
	cityDir := t.TempDir()
	// An unbound planner shadows the bound one in boundRoutedToAliases, and
	// scribe has no config entry at all.
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "planner"},
			{Name: "planner", BindingName: "ops"},
		},
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		// Shadowed value, missing route: backfill the bare hold value.
		{ID: "US-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"}},
		// Shadowed value, unrelated route: rewrite to the bare hold value.
		{
			ID: "US-2", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"},
			Metadata: map[string]string{"gc.routed_to": "reviewer"},
		},
		// Value absent from config entirely: same verbatim contract.
		{ID: "US-3", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:scribe"}},
	}, nil)
	factory := func(path string) (beads.Store, error) {
		if path != cityDir {
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
	for _, want := range []string{
		`city bead US-1 has hold:planner but gc.routed_to=""; use "planner"`,
		`city bead US-2 has hold:planner but gc.routed_to="reviewer"; use "planner"`,
		`city bead US-3 has hold:scribe but gc.routed_to=""; use "scribe"`,
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	for id, wantRoute := range map[string]string{
		"US-1": "planner",
		"US-2": "planner",
		"US-3": "scribe",
	} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got := b.Metadata["gc.routed_to"]; got != wantRoute {
			t.Errorf("%s gc.routed_to = %q, want %q (verbatim backfill, not qualified)", id, got, wantRoute)
		}
	}

	// The hold check converges, and the backfilled short forms are the
	// canonical routes the namespace check requires.
	if res2 := check.Run(&doctor.CheckContext{}); res2.Status != doctor.StatusOK {
		t.Fatalf("post-fix hold-label-routed-to status = %v, want OK: %#v", res2.Status, res2)
	}
	if nsRes := newV2RoutedToNamespaceCheck(cfg, cityDir, factory).Run(&doctor.CheckContext{}); nsRes.Status != doctor.StatusOK {
		t.Fatalf("post-fix v2-routed-to-namespace status = %v, want OK (shadowed and absent values are canonical short): %#v", nsRes.Status, nsRes)
	}
}

// TestHoldLabelRoutedToFixConvergesWithV2RoutedToNamespace covers ga-tkxbic:
// before the fix, hold-label-routed-to's --fix backfilled the bare hold value
// while v2-routed-to-namespace rewrote short forms to the binding-qualified
// canonical, so a doctor run containing both checks (in production order)
// repaired a route and then degraded it again, flip-flopping every pass. Both
// checks must now converge: after one --fix pass every held route is the
// qualified canonical, and the second pass reports both checks OK with zero
// repairs, in production order and reversed.
func TestHoldLabelRoutedToFixConvergesWithV2RoutedToNamespace(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "production-order"
		if reverse {
			name = "reverse-order"
		}
		t.Run(name, func(t *testing.T) {
			cityDir := t.TempDir()
			cfg := &config.City{Agents: []config.Agent{{Name: "planner", BindingName: "ops"}}}
			store := beads.NewMemStoreFrom(0, []beads.Bead{
				{ID: "CV-1", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"}},
				{
					ID: "CV-2", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"},
					Metadata: map[string]string{"gc.routed_to": "planner"},
				},
				{
					ID: "CV-3", Title: "held", Type: "task", Status: "open", Labels: []string{"hold:planner"},
					Metadata: map[string]string{"gc.routed_to": "ops.planner"},
				},
			}, nil)
			factory := func(path string) (beads.Store, error) {
				if path != cityDir {
					return nil, fmt.Errorf("unexpected store path %q", path)
				}
				return store, nil
			}

			d := &doctor.Doctor{}
			nsCheck := newV2RoutedToNamespaceCheck(cfg, cityDir, factory)
			holdCheck := newHoldLabelRoutedToCheck(cfg, cityDir, factory)
			if reverse {
				d.Register(holdCheck)
				d.Register(nsCheck)
			} else {
				d.Register(nsCheck)
				d.Register(holdCheck)
			}

			rep1 := d.RunCollect(&doctor.CheckContext{}, true)
			for _, res := range rep1.Results {
				if res.Status != doctor.StatusOK {
					t.Errorf("first pass check %q status = %v, want OK: %#v", res.Name, res.Status, res)
				}
			}
			for _, id := range []string{"CV-1", "CV-2", "CV-3"} {
				b, err := store.Get(id)
				if err != nil {
					t.Fatalf("get %s: %v", id, err)
				}
				if got := b.Metadata["gc.routed_to"]; got != "ops.planner" {
					t.Errorf("after first --fix pass %s gc.routed_to = %q, want ops.planner", id, got)
				}
			}

			rep2 := d.RunCollect(&doctor.CheckContext{}, true)
			if rep2.Warned != 0 || rep2.Fixed != 0 {
				t.Fatalf("second pass Warned = %d, Fixed = %d, want 0/0 (results: %+v)", rep2.Warned, rep2.Fixed, rep2.Results)
			}
			for _, res := range rep2.Results {
				if res.Status != doctor.StatusOK {
					t.Errorf("second pass check %q status = %v, want OK: %#v", res.Name, res.Status, res)
				}
			}
		})
	}
}
