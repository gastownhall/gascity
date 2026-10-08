package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestV2RoutedToNamespaceCheckWarnsOnShortBoundRoutes(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog", BindingName: "gastown"},
			{Name: "polecat", Dir: "repo", BindingName: "gastown"},
		},
		Rigs: []config.Rig{
			{Name: "repo", Path: rigDir},
		},
	}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "warrant", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "dog"}},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "RIG-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "repo/polecat"}},
	}, nil)
	stores := map[string]beads.Store{
		cityDir: cityStore,
		rigDir:  rigStore,
	}

	result := newV2RoutedToNamespaceCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		store, ok := stores[path]
		if !ok {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		`city bead CITY-1 has gc.routed_to="dog"; use "gastown.dog"`,
		`rig repo bead RIG-1 has gc.routed_to="repo/polecat"; use "repo/gastown.polecat"`,
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
}

func TestV2RoutedToNamespaceCheckUsesTargetedRouteQueries(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "dog", BindingName: "gastown"}},
	}
	store := &routeQuerySpyStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "warrant", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "dog"}},
	}, nil)}

	result := newV2RoutedToNamespaceCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	if len(store.queries) == 0 {
		t.Fatal("expected at least one route query")
	}
	for _, query := range store.queries {
		if query.AllowScan {
			t.Fatalf("query %+v used AllowScan; route namespace check should use targeted metadata lookups", query)
		}
		if got := query.Metadata["gc.routed_to"]; got == "" {
			t.Fatalf("query %+v missing gc.routed_to metadata filter", query)
		}
	}
}

// TestV2RoutedToNamespaceCheckStopsQueryingAfterAFailedRoute: the scan ends
// at the first failed route, so once one fails no further route query may
// start. On a store that is down, each one would fork bd and run its recovery
// only to have its answer discarded.
func TestV2RoutedToNamespaceCheckStopsQueryingAfterAFailedRoute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := &config.City{}
		for i := range 12 {
			cfg.Agents = append(cfg.Agents, config.Agent{Name: fmt.Sprintf("agent%02d", i), BindingName: "pack"})
		}
		store := &routeGateSpyStore{Store: beads.NewMemStore(), failRoute: "agent00", gate: make(chan struct{})}
		check := newV2RoutedToNamespaceCheck(cfg, t.TempDir(), func(string) (beads.Store, error) {
			return store, nil
		})

		done := make(chan *doctor.CheckResult)
		go func() { done <- check.Run(&doctor.CheckContext{}) }()
		// Every route query that will start has started: the first route's
		// has failed and any others are held at the gate.
		synctest.Wait()
		close(store.gate)
		result := <-done

		if details := strings.Join(result.Details, "\n"); result.Status != doctor.StatusWarning ||
			!strings.Contains(details, "city skipped: listing beads: store unreachable") {
			t.Fatalf("result = %+v, want the city scope skipped with the first route's error", result)
		}
		if len(store.queried) > doctorStoreReadConcurrency {
			t.Fatalf("queried %d of %d routes once the first had failed (%v), want at most the %d already in flight",
				len(store.queried), len(cfg.Agents), store.queried, doctorStoreReadConcurrency)
		}
	})
}

func TestV2RoutedToNamespaceCheckAllowsCanonicalRoutes(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog", BindingName: "gastown"},
			{Name: "human"},
		},
	}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "warrant", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "gastown.dog"}},
		{ID: "CITY-2", Title: "human", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "human"}},
	}, nil)

	result := newV2RoutedToNamespaceCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok: %#v", result.Status, result)
	}
}

func TestV2RoutedToNamespaceCheckWarnsOnBoundNamedSessionShortRoutes(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		NamedSessions: []config.NamedSession{
			{Name: "mayor", BindingName: "gastown"},
		},
	}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "mail", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "mayor"}},
	}, nil)

	result := newV2RoutedToNamespaceCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	want := `city bead CITY-1 has gc.routed_to="mayor"; use "gastown.mayor"`
	if !strings.Contains(details, want) {
		t.Fatalf("details missing %q:\n%s", want, details)
	}
}

func TestV2RoutedToNamespaceCheckAllowsAmbiguousShortRouteForUnboundAgent(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog"},
			{Name: "dog", BindingName: "gastown"},
		},
	}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "warrant", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "dog"}},
	}, nil)

	result := newV2RoutedToNamespaceCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok: %#v", result.Status, result)
	}
}

func TestV2RoutedToNamespaceCheckWarnsOnSkippedStoreScopes(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog", BindingName: "gastown"},
		},
		Rigs: []config.Rig{
			{Name: "repo", Path: rigDir},
		},
	}

	result := newV2RoutedToNamespaceCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		switch path {
		case cityDir:
			return nil, errors.New("city offline")
		case rigDir:
			return routeListErrorStore{err: errors.New("rig offline")}, nil
		default:
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"city skipped: opening bead store: city offline",
		"rig repo skipped: listing beads: rig offline",
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
}

func TestV2RoutedToNamespaceCheckCanFix(t *testing.T) {
	check := newV2RoutedToNamespaceCheck(&config.City{}, t.TempDir(), nil)
	if !check.CanFix() {
		t.Fatal("expected CanFix to return true")
	}
}

func TestV2RoutedToNamespaceCheckFixRewritesUnambiguousShortRoute(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog", BindingName: "gastown"},
		},
	}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "warrant", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "dog"}},
	}, nil)

	check := newV2RoutedToNamespaceCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status after fix = %v, want ok: %#v", result.Status, result)
	}

	items, err := cityStore.List(beads.ListQuery{Metadata: map[string]string{"gc.routed_to": "gastown.dog"}})
	if err != nil {
		t.Fatalf("listing city store: %v", err)
	}
	if len(items) != 1 || items[0].ID != "CITY-1" {
		t.Fatalf("expected CITY-1 rewritten to gastown.dog, got %+v", items)
	}
}

func TestV2RoutedToNamespaceCheckFixLeavesAmbiguousRoutesUntouched(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog", BindingName: "gastown"},
			{Name: "dog", BindingName: "otherpack"},
		},
	}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "warrant", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "dog"}},
	}, nil)

	check := newV2RoutedToNamespaceCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status after fix = %v, want warning (ambiguous route must stay unresolved): %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	want := `city bead CITY-1 has gc.routed_to="dog"; use one of gastown.dog, otherpack.dog`
	if !strings.Contains(details, want) {
		t.Fatalf("details missing %q:\n%s", want, details)
	}

	items, err := cityStore.List(beads.ListQuery{Metadata: map[string]string{"gc.routed_to": "dog"}})
	if err != nil {
		t.Fatalf("listing city store: %v", err)
	}
	if len(items) != 1 || items[0].ID != "CITY-1" {
		t.Fatalf("expected CITY-1 to still carry the unresolved short route, got %+v", items)
	}
}

func TestV2RoutedToNamespaceCheckFixIsPartialFailureTolerant(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "dog", BindingName: "gastown"},
			{Name: "polecat", Dir: "repo", BindingName: "gastown"},
		},
		Rigs: []config.Rig{
			{Name: "repo", Path: rigDir},
		},
	}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "warrant", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "dog"}},
	}, nil)
	failingRigStore := routeSetMetadataErrorStore{
		Store: beads.NewMemStoreFrom(0, []beads.Bead{
			{ID: "RIG-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "repo/polecat"}},
		}, nil),
		err: errors.New("write denied"),
	}
	stores := map[string]beads.Store{
		cityDir: cityStore,
		rigDir:  failingRigStore,
	}

	check := newV2RoutedToNamespaceCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		store, ok := stores[path]
		if !ok {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})

	err := check.Fix(&doctor.CheckContext{})
	if err == nil {
		t.Fatal("expected Fix to return an error for the failing store, got nil")
	}
	if !strings.Contains(err.Error(), "write denied") {
		t.Fatalf("error %q does not mention the underlying SetMetadata failure", err.Error())
	}

	items, listErr := cityStore.List(beads.ListQuery{Metadata: map[string]string{"gc.routed_to": "gastown.dog"}})
	if listErr != nil {
		t.Fatalf("listing city store: %v", listErr)
	}
	if len(items) != 1 || items[0].ID != "CITY-1" {
		t.Fatalf("expected CITY-1 rewritten to gastown.dog despite the rig store's failure, got %+v", items)
	}
}

type routeListErrorStore struct {
	beads.Store
	err error
}

func (s routeListErrorStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, s.err
}

type routeSetMetadataErrorStore struct {
	beads.Store
	err error
}

func (s routeSetMetadataErrorStore) SetMetadata(string, string, string) error {
	return s.err
}

type routeQuerySpyStore struct {
	beads.Store
	queries []beads.ListQuery
}

func (s *routeQuerySpyStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, query)
	return s.Store.List(query)
}

// routeGateSpyStore fails the query for failRoute at once, holds every other
// route query until gate is closed, and records each route queried.
type routeGateSpyStore struct {
	beads.Store
	failRoute string
	gate      chan struct{}

	mu      sync.Mutex
	queried []string
}

func (s *routeGateSpyStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	route := query.Metadata["gc.routed_to"]
	s.mu.Lock()
	s.queried = append(s.queried, route)
	s.mu.Unlock()
	if route == s.failRoute {
		return nil, errors.New("store unreachable")
	}
	<-s.gate
	return s.Store.List(query)
}
