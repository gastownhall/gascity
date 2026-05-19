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

type routeListErrorStore struct {
	beads.Store
	err error
}

func (s routeListErrorStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, s.err
}
