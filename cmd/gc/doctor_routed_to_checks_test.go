package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

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

// TestV2RoutedToNamespaceCheckReadsEachStoreOnceWhateverTheRouteCount: the
// check lists each store once and matches routes in memory, so its reads do
// not grow with the city's bound routes (a 215-route city with four stores
// paid 1,720 bd forks for one read per route per store). A store path shared
// by two scopes is listed once and still reported for both.
func TestV2RoutedToNamespaceCheckReadsEachStoreOnceWhateverTheRouteCount(t *testing.T) {
	for _, routes := range []int{1, 250} {
		t.Run(fmt.Sprintf("%d-routes", routes), func(t *testing.T) {
			cityDir := t.TempDir()
			rigDir := t.TempDir()
			cfg := &config.City{
				Rigs: []config.Rig{
					{Name: "repo", Path: rigDir},
					{Name: "alias", Path: rigDir},
				},
			}
			for i := range routes {
				cfg.Agents = append(cfg.Agents, config.Agent{Name: fmt.Sprintf("agent%03d", i), BindingName: "pack"})
			}
			stores := map[string]*routeQuerySpyStore{
				cityDir: {Store: beads.NewMemStoreFrom(0, []beads.Bead{
					{ID: "CITY-1", Title: "short", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "agent000"}},
					{ID: "CITY-2", Title: "canonical", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "pack.agent000"}},
					{ID: "CITY-3", Title: "closed", Type: "task", Status: "closed", Metadata: map[string]string{"gc.routed_to": "agent000"}},
					{ID: "CITY-4", Title: "padded", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": " agent000"}},
				}, nil)},
				rigDir: {Store: beads.NewMemStoreFrom(0, []beads.Bead{
					{ID: "RIG-1", Title: "short", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": fmt.Sprintf("agent%03d", routes-1)}},
				}, nil)},
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
			last := fmt.Sprintf("agent%03d", routes-1)
			want := []string{
				`city bead CITY-1 has gc.routed_to="agent000"; use "pack.agent000"`,
				fmt.Sprintf(`rig alias bead RIG-1 has gc.routed_to=%q; use "pack.%s"`, last, last),
				fmt.Sprintf(`rig repo bead RIG-1 has gc.routed_to=%q; use "pack.%s"`, last, last),
			}
			if got := strings.Join(result.Details, "\n"); got != strings.Join(want, "\n") {
				t.Fatalf("details:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
			}
			for path, store := range stores {
				if len(store.queries) != 1 {
					t.Fatalf("store %s listed %d times for %d routes, want 1: %+v", path, len(store.queries), routes, store.queries)
				}
			}
		})
	}
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
	mu      sync.Mutex
	queries []beads.ListQuery
}

func (s *routeQuerySpyStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	s.queries = append(s.queries, query)
	s.mu.Unlock()
	return s.Store.List(query)
}
