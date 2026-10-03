package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
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

// poolIdleRoutedWorkRecordingStore counts the check's reads against one store:
// session-class enumerations (label-only lists) and the gc.routed_to template
// of every routed-work lookup. Everything else delegates to the embedded store.
type poolIdleRoutedWorkRecordingStore struct {
	beads.Store
	mu           *sync.Mutex
	sessionLists *int
	routedTo     *[]string
}

func newPoolIdleRoutedWorkRecordingStore(seed []beads.Bead) poolIdleRoutedWorkRecordingStore {
	return poolIdleRoutedWorkRecordingStore{
		Store:        beads.NewMemStoreFrom(0, seed, nil),
		mu:           &sync.Mutex{},
		sessionLists: new(int),
		routedTo:     &[]string{},
	}
}

func (s poolIdleRoutedWorkRecordingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	if routed, ok := q.Metadata["gc.routed_to"]; ok {
		*s.routedTo = append(*s.routedTo, routed)
	} else if q.Label == "gc:session" {
		*s.sessionLists++
	}
	s.mu.Unlock()
	return s.Store.List(q)
}

func (s poolIdleRoutedWorkRecordingStore) calls() (sessionLists int, routedTo []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.sessionLists, append([]string(nil), *s.routedTo...)
}

func poolIdleWorkNamedSessionBead(id, template string) beads.Bead {
	b := poolIdleWorkSessionBead(template, "active", "")
	b.ID = id
	b.Metadata["session_name"] = id
	return b
}

// TestPoolIdleRoutedWorkCheckListsSessionsOncePerStore: the check must not
// enumerate sessions once per pool template per store (a full session-label
// scan each). One enumeration per opened store, grouped in memory.
func TestPoolIdleRoutedWorkCheckListsSessionsOncePerStore(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}
	for i := 0; i < 40; i++ {
		cfg.Agents = append(cfg.Agents, config.Agent{Name: fmt.Sprintf("pool%02d", i), Dir: "repo"})
		cfg.Agents = append(cfg.Agents, config.Agent{Name: fmt.Sprintf("hq%02d", i)})
	}
	cityStore := newPoolIdleRoutedWorkRecordingStore(nil)
	rigStore := newPoolIdleRoutedWorkRecordingStore(nil)
	stores := map[string]beads.Store{cityDir: cityStore, rigDir: rigStore}

	result := newPoolIdleRoutedWorkCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		return stores[path], nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok: %#v", result.Status, result)
	}
	for label, store := range map[string]poolIdleRoutedWorkRecordingStore{"city": cityStore, "rig": rigStore} {
		lists, routed := store.calls()
		if lists != 1 {
			t.Errorf("%s store: %d session enumerations, want exactly 1 for %d pool templates", label, lists, len(cfg.Agents))
		}
		if len(routed) != 0 {
			t.Errorf("%s store: routed-work lookups %v with no idle instance anywhere, want none", label, routed)
		}
	}
}

// TestPoolIdleRoutedWorkCheckNeverQueriesRigStoreForAnotherRigsTemplates: a
// rig's store only ever holds work routed to that rig's pools (plus the city
// store's HQ beads), so it must never be asked about another rig's templates or
// city-scoped templates.
func TestPoolIdleRoutedWorkCheckNeverQueriesRigStoreForAnotherRigsTemplates(t *testing.T) {
	cityDir := t.TempDir()
	alphaDir := t.TempDir()
	betaDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "builder", Dir: "alpha"},
			{Name: "builder", Dir: "beta"},
			{Name: "hq"},
		},
		Rigs: []config.Rig{{Name: "alpha", Path: alphaDir}, {Name: "beta", Path: betaDir}},
	}
	// Every pool has an idle instance, so every template gets a routed lookup
	// somewhere; the sessions live in the city (session-class) store.
	cityStore := newPoolIdleRoutedWorkRecordingStore([]beads.Bead{
		poolIdleWorkNamedSessionBead("SESS-A", "alpha/builder"),
		poolIdleWorkNamedSessionBead("SESS-B", "beta/builder"),
		poolIdleWorkNamedSessionBead("SESS-H", "hq"),
	})
	alphaStore := newPoolIdleRoutedWorkRecordingStore(nil)
	betaStore := newPoolIdleRoutedWorkRecordingStore(nil)
	stores := map[string]beads.Store{cityDir: cityStore, alphaDir: alphaStore, betaDir: betaStore}

	result := newPoolIdleRoutedWorkCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		return stores[path], nil
	}).Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok: %#v", result.Status, result)
	}

	for label, tc := range map[string]struct {
		store poolIdleRoutedWorkRecordingStore
		want  []string
	}{
		"alpha": {alphaStore, []string{"alpha/builder"}},
		"beta":  {betaStore, []string{"beta/builder"}},
	} {
		_, routed := tc.store.calls()
		sort.Strings(routed)
		if strings.Join(routed, ",") != strings.Join(tc.want, ",") {
			t.Errorf("rig %s store routed lookups = %v, want only its own templates %v", label, routed, tc.want)
		}
	}
}

// TestPoolIdleRoutedWorkCheckFindsRigPoolWithSessionsInCityStore models the
// production layout: pool session beads live in the city's session-class
// store while the work routed to a rig pool lives in that rig's store. The
// idle instance and the routed work sit in different stores and must still be
// joined into one finding scoped to the rig.
func TestPoolIdleRoutedWorkCheckFindsRigPoolWithSessionsInCityStore(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "builder", Dir: "repo"}},
		Rigs:   []config.Rig{{Name: "repo", Path: rigDir}},
	}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		poolIdleWorkSessionBead("repo/builder", "active", ""),
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		poolIdleWorkRoutedBead("repo/builder", ""),
	}, nil)
	stores := map[string]beads.Store{cityDir: cityStore, rigDir: rigStore}

	result := newPoolIdleRoutedWorkCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		return stores[path], nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{"rig repo pool repo/builder", "GA-1", "SESS-1"} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
}

// TestPoolIdleRoutedWorkCheckFindsHQBeadRoutedToRigPool: sling keeps an
// HQ-prefixed bead in the city store even when it routes it to a rig pool, so
// the city store is still asked about rig templates that have idle instances.
func TestPoolIdleRoutedWorkCheckFindsHQBeadRoutedToRigPool(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "builder", Dir: "repo"}},
		Rigs:   []config.Rig{{Name: "repo", Path: rigDir}},
	}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		poolIdleWorkSessionBead("repo/builder", "active", ""),
		poolIdleWorkRoutedBead("repo/builder", ""),
	}, nil)
	stores := map[string]beads.Store{cityDir: cityStore, rigDir: beads.NewMemStoreFrom(0, nil, nil)}

	result := newPoolIdleRoutedWorkCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		return stores[path], nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	if details := strings.Join(result.Details, "\n"); !strings.Contains(details, "city pool repo/builder") {
		t.Fatalf("details missing city-scoped finding for the rig pool:\n%s", details)
	}
}

// TestPoolIdleRoutedWorkCheckStopsWhenAbandoned: once the doctor runner
// abandons the check at its timeout (CheckContext.Done closed), the check must
// stop issuing bead-store calls.
func TestPoolIdleRoutedWorkCheckStopsWhenAbandoned(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "builder", Dir: "gascity"}},
	}
	store := newPoolIdleRoutedWorkRecordingStore([]beads.Bead{
		poolIdleWorkSessionBead("gascity/builder", "active", ""),
		poolIdleWorkRoutedBead("gascity/builder", ""),
	})
	opened := 0
	done := make(chan struct{})
	close(done)

	newPoolIdleRoutedWorkCheck(cfg, cityDir, func(_ string) (beads.Store, error) {
		opened++
		return store, nil
	}).Run(&doctor.CheckContext{Done: done})

	lists, routed := store.calls()
	if opened != 0 || lists != 0 || len(routed) != 0 {
		t.Fatalf("abandoned check kept issuing store calls: opened=%d sessionLists=%d routed=%v", opened, lists, routed)
	}
}
