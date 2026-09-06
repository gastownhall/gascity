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

func poolIdleWorkSessionBead(template, state, triggerBeadID string) beads.Bead {
	const id = "SESS-1"
	meta := map[string]string{
		"template":     template,
		"state":        state,
		"session_name": id,
	}
	if triggerBeadID != "" {
		meta["gc.trigger_bead_id"] = triggerBeadID
	}
	return beads.Bead{ID: id, Status: "open", Type: "session", Labels: []string{"gc:session"}, Metadata: meta}
}

func poolIdleWorkRoutedBead(routedTo, assignee string) beads.Bead {
	return beads.Bead{
		ID:       "GA-1",
		Title:    "routed work",
		Type:     "task",
		Status:   "open",
		Assignee: assignee,
		Metadata: map[string]string{"gc.routed_to": routedTo},
	}
}

func TestPoolIdleRoutedWorkCheckWarnsOnIdleInstanceWithUnclaimedRoutedWork(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "builder", Dir: "gascity"}},
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		poolIdleWorkSessionBead("gascity/builder", "active", ""),
		poolIdleWorkRoutedBead("gascity/builder", ""),
	}, nil)

	result := newPoolIdleRoutedWorkCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{"gascity/builder", "GA-1", "SESS-1"} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
}

func TestPoolIdleRoutedWorkCheckOKWhenNoUnclaimedRoutedWork(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "builder", Dir: "gascity"}},
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		poolIdleWorkSessionBead("gascity/builder", "active", ""),
	}, nil)

	result := newPoolIdleRoutedWorkCheck(cfg, cityDir, func(_ string) (beads.Store, error) {
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (idle alone is legitimate min-floor capacity): %#v", result.Status, result)
	}
}

func TestPoolIdleRoutedWorkCheckOKWhenNoIdleInstance(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "builder", Dir: "gascity"}},
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		poolIdleWorkSessionBead("gascity/builder", "active", "GA-9"),
		poolIdleWorkRoutedBead("gascity/builder", ""),
	}, nil)

	result := newPoolIdleRoutedWorkCheck(cfg, cityDir, func(_ string) (beads.Store, error) {
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (every instance already busy): %#v", result.Status, result)
	}
}

func TestPoolIdleRoutedWorkCheckOKWhenOnlyInstanceIsAsleep(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "builder", Dir: "gascity"}},
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		poolIdleWorkSessionBead("gascity/builder", "asleep", ""),
		poolIdleWorkRoutedBead("gascity/builder", ""),
	}, nil)

	result := newPoolIdleRoutedWorkCheck(cfg, cityDir, func(_ string) (beads.Store, error) {
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (asleep instance is not live): %#v", result.Status, result)
	}
}

func TestPoolIdleRoutedWorkCheckIgnoresClaimedRoutedWork(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "builder", Dir: "gascity"}},
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		poolIdleWorkSessionBead("gascity/builder", "active", ""),
		poolIdleWorkRoutedBead("gascity/builder", "someone-else"),
	}, nil)

	result := newPoolIdleRoutedWorkCheck(cfg, cityDir, func(_ string) (beads.Store, error) {
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (routed work is already claimed): %#v", result.Status, result)
	}
}

func TestPoolIdleRoutedWorkCheckScansRigScopes(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "builder", Dir: "repo"}},
		Rigs:   []config.Rig{{Name: "repo", Path: rigDir}},
	}
	cityStore := beads.NewMemStoreFrom(0, nil, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		poolIdleWorkSessionBead("repo/builder", "active", ""),
		poolIdleWorkRoutedBead("repo/builder", ""),
	}, nil)
	stores := map[string]beads.Store{cityDir: cityStore, rigDir: rigStore}

	result := newPoolIdleRoutedWorkCheck(cfg, cityDir, func(path string) (beads.Store, error) {
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
	if !strings.Contains(details, "rig repo") {
		t.Fatalf("details missing rig scope label:\n%s", details)
	}
}

func TestPoolIdleRoutedWorkCheckWarnsOnSkippedStoreScopes(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "builder", Dir: "gascity"}},
		Rigs:   []config.Rig{{Name: "repo", Path: rigDir}},
	}

	result := newPoolIdleRoutedWorkCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		switch path {
		case cityDir:
			return nil, errors.New("city offline")
		case rigDir:
			return beads.NewMemStoreFrom(0, nil, nil), nil
		default:
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "city skipped: opening bead store: city offline") {
		t.Fatalf("details missing skipped-scope note:\n%s", details)
	}
}

func TestPoolIdleRoutedWorkCheckCanFix(t *testing.T) {
	check := newPoolIdleRoutedWorkCheck(&config.City{}, t.TempDir(), nil)
	if check.CanFix() {
		t.Fatal("expected CanFix to return false; this check is detection-only")
	}
}

func TestPoolIdleRoutedWorkCheckFixIsNoop(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "builder", Dir: "gascity"}},
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		poolIdleWorkSessionBead("gascity/builder", "active", ""),
		poolIdleWorkRoutedBead("gascity/builder", ""),
	}, nil)

	check := newPoolIdleRoutedWorkCheck(cfg, cityDir, func(_ string) (beads.Store, error) {
		return store, nil
	})
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}

	b, err := store.Get("GA-1")
	if err != nil {
		t.Fatalf("loading GA-1: %v", err)
	}
	if b.Assignee != "" {
		t.Fatalf("Fix must not mutate beads; GA-1 assignee = %q", b.Assignee)
	}

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status after no-op Fix = %v, want still warning: %#v", result.Status, result)
	}
}

// poolIdleRoutedWorkBlockedStore models the production routed-work read for a
// bead that is blocked in the BACKING store. mapBdStatus collapses bd's blocked
// status into Gas City's "open", so a non-Live read returns it (routedCollapsed)
// while a Live read reaches bd's raw --status=open filter and excludes it
// (routedLive). Only the routed-work query carries a metadata filter; the
// session enumeration is label-only and delegates to the embedded store, as do
// Get and every write. Mirrors blockedDemandStore.
type poolIdleRoutedWorkBlockedStore struct {
	beads.Store
	routedCollapsed []beads.Bead // metadata-filtered, non-Live: blocked row present, collapsed to "open"
	routedLive      []beads.Bead // metadata-filtered, Live: bd's raw filter excluded it
}

func (s poolIdleRoutedWorkBlockedStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if len(q.Metadata) == 0 {
		return s.Store.List(q)
	}
	if q.Live {
		return append([]beads.Bead(nil), s.routedLive...), nil
	}
	return append([]beads.Bead(nil), s.routedCollapsed...), nil
}

func TestPoolIdleRoutedWorkCheckOKWhenRoutedWorkIsBlockedInBackingStore(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "builder", Dir: "gascity"}},
	}
	// The bead is blocked in bd. The idle instance is correct to leave it alone,
	// so the check must not tell the operator to nudge for it.
	store := poolIdleRoutedWorkBlockedStore{
		Store: beads.NewMemStoreFrom(0, []beads.Bead{
			poolIdleWorkSessionBead("gascity/builder", "active", ""),
		}, nil),
		routedCollapsed: []beads.Bead{poolIdleWorkRoutedBead("gascity/builder", "")},
		routedLive:      nil,
	}

	result := newPoolIdleRoutedWorkCheck(cfg, cityDir, func(_ string) (beads.Store, error) {
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (blocked routed work is not claimable; the read must reach bd's raw status filter): %#v", result.Status, result)
	}
}

// poolIdleRoutedWorkTierStore serves routed work that lives on the wisp tier: it
// answers the routed-work query only when the caller asks for both tiers, the
// way a relocated coordination-class store does (see beads.FederatedReadTier).
// A read left at the TierIssues zero value gets nothing back.
type poolIdleRoutedWorkTierStore struct {
	beads.Store
	wispRouted []beads.Bead
}

func (s poolIdleRoutedWorkTierStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if len(q.Metadata) == 0 {
		return s.Store.List(q)
	}
	if q.TierMode != beads.TierBoth {
		return nil, nil
	}
	return append([]beads.Bead(nil), s.wispRouted...), nil
}

func TestPoolIdleRoutedWorkCheckReadsBothTiers(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "builder", Dir: "gascity"}},
	}
	store := poolIdleRoutedWorkTierStore{
		Store: beads.NewMemStoreFrom(0, []beads.Bead{
			poolIdleWorkSessionBead("gascity/builder", "active", ""),
		}, nil),
		wispRouted: []beads.Bead{poolIdleWorkRoutedBead("gascity/builder", "")},
	}

	result := newPoolIdleRoutedWorkCheck(cfg, cityDir, func(_ string) (beads.Store, error) {
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning (routed work on the wisp tier must be read; a TierIssues read drops it silently): %#v", result.Status, result)
	}
	if details := strings.Join(result.Details, "\n"); !strings.Contains(details, "GA-1") {
		t.Fatalf("details missing wisp-tier routed bead:\n%s", details)
	}
}
