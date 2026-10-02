package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// stuckCreatingSessionBead builds a session bead in the given state whose
// pending-create attempt began startedAgo before now. claim sets the
// pending_create_claim lease flag the controller stamps while a create is in
// flight.
func stuckCreatingSessionBead(id, template, state string, startedAgo time.Duration, claim bool) beads.Bead {
	started := time.Now().Add(-startedAgo).UTC()
	meta := map[string]string{
		"template":                  template,
		"state":                     state,
		"session_name":              id,
		"pending_create_started_at": started.Format(time.RFC3339),
	}
	if claim {
		meta["pending_create_claim"] = "true"
	}
	return beads.Bead{ID: id, Status: "open", Type: "session", Labels: []string{"gc:session"}, CreatedAt: started, Metadata: meta}
}

func stuckCreatingRoutedBead(id, routedTo, assignee string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Title:    "routed work " + id,
		Type:     "task",
		Status:   "open",
		Assignee: assignee,
		Metadata: map[string]string{"gc.routed_to": routedTo},
	}
}

func stuckCreatingStoreFactory(store beads.Store) func(string) (beads.Store, error) {
	return func(string) (beads.Store, error) { return store, nil }
}

func stuckCreatingBuilderConfig() *config.City {
	return &config.City{Agents: []config.Agent{{Name: "builder", Dir: "gascity"}}}
}

// stuckCreatingFailingReadStore fails every List the predicate selects and
// delegates the rest, so a test can break exactly one of the check's reads.
type stuckCreatingFailingReadStore struct {
	beads.Store
	fail func(beads.ListQuery) bool
}

func (s stuckCreatingFailingReadStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.fail(q) {
		return nil, errors.New("dolt unreachable")
	}
	return s.Store.List(q)
}

func TestStuckCreatingSessionsCheckWarnsOnAbandonedCreate(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		stuckCreatingSessionBead("SESS-1", "gascity/builder", "creating", 72*time.Hour, true),
		stuckCreatingRoutedBead("GA-1", "gascity/builder", ""),
		stuckCreatingRoutedBead("GA-2", "gascity/builder", ""),
		stuckCreatingRoutedBead("GA-3", "gascity/builder", "someone-else"),
		stuckCreatingRoutedBead("GA-4", "gascity/pm", ""),
	}, nil)

	result := newStuckCreatingSessionsCheck(stuckCreatingBuilderConfig(), cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	// doctor prints Message always and Details only under -v, so the facts the
	// operator needs without a flag must be in the Message.
	for _, want := range []string{"SESS-1", "stuck in creating", "3d", "2 unclaimed routed bead(s)"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message missing %q: %s", want, result.Message)
		}
	}
	if !strings.Contains(result.FixHint, "gc session close") {
		t.Errorf("fix hint = %q, want it to name gc session close", result.FixHint)
	}
}

func TestStuckCreatingSessionsCheckReportsZeroRoutedWithoutClaimingStarvation(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		stuckCreatingSessionBead("SESS-1", "gascity/builder", "creating", 72*time.Hour, true),
	}, nil)

	result := newStuckCreatingSessionsCheck(stuckCreatingBuilderConfig(), cityDir, stuckCreatingStoreFactory(store)).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning (an orphaned record is still a fault): %#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "0 unclaimed routed bead(s)") {
		t.Errorf("message = %q, want the measured zero", result.Message)
	}
	everything := strings.ToLower(result.Message + "\n" + strings.Join(result.Details, "\n") + "\n" + result.FixHint)
	if strings.Contains(everything, "starv") {
		t.Errorf("report claims starvation with no routed work behind the session:\n%s", everything)
	}
}

func TestStuckCreatingSessionsCheckOKWhenCreateWithinBound(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		stuckCreatingSessionBead("SESS-1", "gascity/builder", "creating", 10*time.Second, true),
		stuckCreatingRoutedBead("GA-1", "gascity/builder", ""),
	}, nil)

	result := newStuckCreatingSessionsCheck(stuckCreatingBuilderConfig(), cityDir, stuckCreatingStoreFactory(store)).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (a create inside its bound is normal): %#v", result.Status, result)
	}
}

func TestStuckCreatingSessionsCheckOKWhileStartLeaseIsHeld(t *testing.T) {
	cityDir := t.TempDir()
	// Past the one-minute staleness bound but inside the ten-minute
	// never-started lease: the reconciler still protects this create, so a
	// check that tested staleness alone would cry wolf on a busy start queue.
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		stuckCreatingSessionBead("SESS-1", "gascity/builder", "creating", 5*time.Minute, true),
	}, nil)

	result := newStuckCreatingSessionsCheck(stuckCreatingBuilderConfig(), cityDir, stuckCreatingStoreFactory(store)).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (pending-create lease still held): %#v", result.Status, result)
	}
}

func TestStuckCreatingSessionsCheckIgnoresSessionsNotMidCreate(t *testing.T) {
	cityDir := t.TempDir()
	closedZombie := stuckCreatingSessionBead("SESS-3", "gascity/builder", "creating", 72*time.Hour, true)
	closedZombie.Status = "closed"
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		stuckCreatingSessionBead("SESS-1", "gascity/builder", "active", 72*time.Hour, false),
		stuckCreatingSessionBead("SESS-2", "gascity/builder", "asleep", 72*time.Hour, false),
		closedZombie,
	}, nil)

	result := newStuckCreatingSessionsCheck(stuckCreatingBuilderConfig(), cityDir, stuckCreatingStoreFactory(store)).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (active, asleep and closed sessions are not mid-create): %#v", result.Status, result)
	}
}

func TestStuckCreatingSessionsCheckNamesCapacitySlotAndPoolMax(t *testing.T) {
	cityDir := t.TempDir()
	one := 1
	cfg := &config.City{Agents: []config.Agent{{Name: "builder", Dir: "gascity", MaxActiveSessions: &one}}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		stuckCreatingSessionBead("SESS-1", "gascity/builder", "creating", 72*time.Hour, true),
	}, nil)

	result := newStuckCreatingSessionsCheck(cfg, cityDir, stuckCreatingStoreFactory(store)).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	// One zombie holding the only slot of a max_active_sessions=1 template
	// reads as "no work to do" unless the report says it holds the slot.
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{"counts against capacity", "max_active_sessions=1"} {
		if !strings.Contains(details, want) {
			t.Errorf("details missing %q:\n%s", want, details)
		}
	}
}

func TestStuckCreatingSessionsCheckScansRigScopes(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "builder", Dir: "repo"}},
		Rigs:   []config.Rig{{Name: "repo", Path: rigDir}},
	}
	stores := map[string]beads.Store{
		cityDir: beads.NewMemStoreFrom(0, nil, nil),
		rigDir: beads.NewMemStoreFrom(0, []beads.Bead{
			stuckCreatingSessionBead("SESS-1", "repo/builder", "creating", 72*time.Hour, true),
		}, nil),
	}

	result := newStuckCreatingSessionsCheck(cfg, cityDir, func(path string) (beads.Store, error) {
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

func TestStuckCreatingSessionsCheckWarnsOnSkippedStoreScope(t *testing.T) {
	cityDir := t.TempDir()

	result := newStuckCreatingSessionsCheck(stuckCreatingBuilderConfig(), cityDir, func(string) (beads.Store, error) {
		return nil, errors.New("city offline")
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning (an unreadable store must not read as healthy): %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "city skipped: opening bead store: city offline") {
		t.Fatalf("details missing skipped-scope note:\n%s", details)
	}
}

func TestStuckCreatingSessionsCheckReportsUnknownWhenAReadFails(t *testing.T) {
	stuck := stuckCreatingSessionBead("SESS-1", "gascity/builder", "creating", 72*time.Hour, true)

	t.Run("session listing fails", func(t *testing.T) {
		store := stuckCreatingFailingReadStore{
			Store: beads.NewMemStoreFrom(0, []beads.Bead{stuck}, nil),
			fail:  func(q beads.ListQuery) bool { return q.Label == "gc:session" },
		}
		result := newStuckCreatingSessionsCheck(stuckCreatingBuilderConfig(), t.TempDir(), stuckCreatingStoreFactory(store)).Run(&doctor.CheckContext{})

		if result.Status != doctor.StatusWarning {
			t.Fatalf("status = %v, want warning, not a green result from a store that could not be read: %#v", result.Status, result)
		}
		details := strings.Join(result.Details, "\n")
		for _, want := range []string{"city skipped", "dolt unreachable"} {
			if !strings.Contains(details, want) {
				t.Errorf("details missing %q:\n%s", want, details)
			}
		}
	})

	t.Run("routed work read fails", func(t *testing.T) {
		store := stuckCreatingFailingReadStore{
			Store: beads.NewMemStoreFrom(0, []beads.Bead{stuck}, nil),
			fail:  func(q beads.ListQuery) bool { return len(q.Metadata) > 0 },
		}
		result := newStuckCreatingSessionsCheck(stuckCreatingBuilderConfig(), t.TempDir(), stuckCreatingStoreFactory(store)).Run(&doctor.CheckContext{})

		if result.Status != doctor.StatusWarning {
			t.Fatalf("status = %v, want warning: %#v", result.Status, result)
		}
		if !strings.Contains(result.Message, "SESS-1") {
			t.Errorf("message = %q, want the stuck session still reported", result.Message)
		}
		if !strings.Contains(result.Message, "routed beads unknown") {
			t.Errorf("message = %q, want the routed count reported as unknown", result.Message)
		}
		if strings.Contains(result.Message, "0 unclaimed routed bead(s)") {
			t.Errorf("message = %q, an unread count must never be defaulted to zero", result.Message)
		}
		details := strings.Join(result.Details, "\n")
		if !strings.Contains(details, "dolt unreachable") {
			t.Errorf("details missing the read error:\n%s", details)
		}
	})
}

func TestStuckCreatingSessionsCheckDoesNotCountWorkForSessionWithoutTemplate(t *testing.T) {
	cityDir := t.TempDir()
	// A metadata filter with an empty value matches every bead that lacks the
	// key, so querying routed work for an empty template would report all
	// unrouted work as waiting on this session.
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		stuckCreatingSessionBead("SESS-1", "", "creating", 72*time.Hour, true),
		{ID: "GA-1", Title: "unrouted work", Type: "task", Status: "open"},
	}, nil)

	result := newStuckCreatingSessionsCheck(stuckCreatingBuilderConfig(), cityDir, stuckCreatingStoreFactory(store)).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "SESS-1") || !strings.Contains(result.Message, "routed beads unknown") {
		t.Errorf("message = %q, want the session reported with its routed count unknown", result.Message)
	}
	if strings.Contains(result.Message, "1 unclaimed routed bead(s)") {
		t.Errorf("message = %q, unrouted work was counted against a session with no template", result.Message)
	}
}

func TestStuckCreatingSessionsCheckWarnsWhenItHasNothingToLookAt(t *testing.T) {
	store := beads.NewMemStoreFrom(0, nil, nil)
	for name, check := range map[string]*stuckCreatingSessionsCheck{
		"no config":     newStuckCreatingSessionsCheck(nil, t.TempDir(), stuckCreatingStoreFactory(store)),
		"no bead store": newStuckCreatingSessionsCheck(stuckCreatingBuilderConfig(), t.TempDir(), nil),
	} {
		t.Run(name, func(t *testing.T) {
			result := check.Run(&doctor.CheckContext{})

			if result.Status != doctor.StatusWarning {
				t.Fatalf("status = %v, want warning: a check that cannot look must not report a clean bill: %#v", result.Status, result)
			}
			if !strings.Contains(result.Message, "could not run") {
				t.Errorf("message = %q, want it to say the check could not run", result.Message)
			}
		})
	}
}

func TestStuckCreatingSessionsCheckIsDetectionOnly(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		stuckCreatingSessionBead("SESS-1", "gascity/builder", "creating", 72*time.Hour, true),
	}, nil)
	check := newStuckCreatingSessionsCheck(stuckCreatingBuilderConfig(), cityDir, stuckCreatingStoreFactory(store))

	if check.CanFix() {
		t.Fatal("expected CanFix to return false; releasing a slot is an operator decision")
	}
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	b, err := store.Get("SESS-1")
	if err != nil {
		t.Fatalf("loading SESS-1: %v", err)
	}
	if b.Status != "open" || b.Metadata["state"] != "creating" {
		t.Fatalf("Fix must not mutate the session; got status=%q state=%q", b.Status, b.Metadata["state"])
	}
	if result := check.Run(&doctor.CheckContext{}); result.Status != doctor.StatusWarning {
		t.Fatalf("status after no-op Fix = %v, want still warning: %#v", result.Status, result)
	}
}
