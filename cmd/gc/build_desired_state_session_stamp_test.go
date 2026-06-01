package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// countingStore wraps a Store and counts SetMetadataBatch calls so a test can
// assert the stamp is idempotent (no writes once the bead already carries the
// resolved identity).
type countingStore struct {
	beads.Store
	writes int
}

func (c *countingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	c.writes++
	return c.Store.SetMetadataBatch(id, kvs)
}

func stampTestSession(name, workDir string) beads.Bead {
	return beads.Bead{
		ID:     "sess-" + name,
		Type:   "session",
		Status: "open",
		Metadata: map[string]string{
			"session_name": name,
			"work_dir":     workDir,
		},
	}
}

func TestStampRunSessionIdentityStampsInProgressAssignedBead(t *testing.T) {
	const sessionName = "codeprobe-worker-gc-1920"
	const workDir = "/home/ds/projects/codeprobe/codeprobe-worker-1"

	run := beads.Bead{ID: "co-run1", Type: "molecule", Status: "in_progress", Assignee: sessionName}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{run}, nil)
	store := &countingStore{Store: mem}
	sessions := newSessionBeadSnapshot([]beads.Bead{stampTestSession(sessionName, workDir)})

	stampRunSessionIdentity([]beads.Bead{run}, []beads.Store{store}, sessions, io.Discard)

	got, err := mem.Get("co-run1")
	if err != nil {
		t.Fatalf("Get(co-run1): %v", err)
	}
	if got.Metadata["gc.session_name"] != sessionName {
		t.Errorf("gc.session_name = %q, want %q", got.Metadata["gc.session_name"], sessionName)
	}
	if got.Metadata["gc.work_dir"] != workDir {
		t.Errorf("gc.work_dir = %q, want %q", got.Metadata["gc.work_dir"], workDir)
	}

	// Idempotent: a second pass over the now-stamped bead writes nothing.
	stamped, _ := mem.Get("co-run1")
	store.writes = 0
	stampRunSessionIdentity([]beads.Bead{stamped}, []beads.Store{store}, sessions, io.Discard)
	if store.writes != 0 {
		t.Errorf("second pass wrote %d times, want 0 (stamp must be idempotent)", store.writes)
	}
}

func TestStampRunSessionIdentityNamedSessionUsesAlias(t *testing.T) {
	// Named sessions (e.g. mayor) carry an empty session_name; their
	// resolvable identifier lives in alias / configured_named_identity.
	mayor := beads.Bead{
		ID: "sess-mayor", Type: "session", Status: "open",
		Metadata: map[string]string{
			"session_name": "", "alias": "mayor",
			"configured_named_identity": "mayor", "work_dir": "/home/ds/gas-city",
		},
	}
	run := beads.Bead{ID: "dr-run", Type: "molecule", Status: "in_progress", Assignee: "mayor"}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{run}, nil)
	store := &countingStore{Store: mem}
	sessions := newSessionBeadSnapshot([]beads.Bead{mayor})

	stampRunSessionIdentity([]beads.Bead{run}, []beads.Store{store}, sessions, io.Discard)

	got, _ := mem.Get("dr-run")
	if got.Metadata["gc.session_name"] != "mayor" {
		t.Errorf("gc.session_name = %q, want %q (alias fallback)", got.Metadata["gc.session_name"], "mayor")
	}
	if got.Metadata["gc.work_dir"] != "/home/ds/gas-city" {
		t.Errorf("gc.work_dir = %q, want /home/ds/gas-city", got.Metadata["gc.work_dir"])
	}
}

func TestStampRunSessionIdentityResolvesByIDAndAliasHistory(t *testing.T) {
	cases := map[string]struct {
		assignee string
		sess     beads.Bead
		wantName string
	}{
		"by bead ID": {
			assignee: "sess-pool",
			sess: beads.Bead{
				ID: "sess-pool", Type: "session", Status: "open",
				Metadata: map[string]string{"session_name": "pool-gc-9", "work_dir": "/wt/9"},
			},
			wantName: "pool-gc-9",
		},
		"by rotated alias_history": {
			// A pool worker whose alias rotated mid-run: the bead's Assignee is
			// a prior alias, still a live assignment identity.
			assignee: "old-alias",
			sess: beads.Bead{
				ID: "sess-rot", Type: "session", Status: "open",
				Metadata: map[string]string{
					"session_name": "pool-gc-10", "alias": "new-alias",
					"alias_history": "old-alias", "work_dir": "/wt/10",
				},
			},
			wantName: "pool-gc-10",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			run := beads.Bead{ID: "r", Type: "molecule", Status: "in_progress", Assignee: tc.assignee}
			mem := beads.NewMemStoreFrom(0, []beads.Bead{run}, nil)
			store := &countingStore{Store: mem}
			sessions := newSessionBeadSnapshot([]beads.Bead{tc.sess})

			stampRunSessionIdentity([]beads.Bead{run}, []beads.Store{store}, sessions, io.Discard)

			got, _ := mem.Get("r")
			if got.Metadata["gc.session_name"] != tc.wantName {
				t.Errorf("gc.session_name = %q, want %q", got.Metadata["gc.session_name"], tc.wantName)
			}
		})
	}
}

func TestStampRunSessionIdentitySkipsAmbiguousAssignee(t *testing.T) {
	// Two open sessions claim the same identity ("dupe") — a transient
	// duplicate-alias state. The stamp must skip rather than guess.
	a := beads.Bead{ID: "sa", Type: "session", Status: "open", Metadata: map[string]string{"alias": "dupe", "work_dir": "/a"}}
	b := beads.Bead{ID: "sb", Type: "session", Status: "open", Metadata: map[string]string{"alias": "dupe", "work_dir": "/b"}}
	run := beads.Bead{ID: "r", Type: "molecule", Status: "in_progress", Assignee: "dupe"}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{run}, nil)
	store := &countingStore{Store: mem}
	sessions := newSessionBeadSnapshot([]beads.Bead{a, b})

	stampRunSessionIdentity([]beads.Bead{run}, []beads.Store{store}, sessions, io.Discard)

	if store.writes != 0 {
		t.Errorf("ambiguous assignee must not be stamped, got %d writes", store.writes)
	}
}

func TestStampRunSessionIdentityReassignmentRestamps(t *testing.T) {
	run := beads.Bead{
		ID: "co-run2", Type: "molecule", Status: "in_progress", Assignee: "worker-b",
		Metadata: map[string]string{"gc.session_name": "worker-a", "gc.work_dir": "/old"},
	}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{run}, nil)
	store := &countingStore{Store: mem}
	sessions := newSessionBeadSnapshot([]beads.Bead{stampTestSession("worker-b", "/new")})

	stampRunSessionIdentity([]beads.Bead{run}, []beads.Store{store}, sessions, io.Discard)

	got, _ := mem.Get("co-run2")
	if got.Metadata["gc.session_name"] != "worker-b" || got.Metadata["gc.work_dir"] != "/new" {
		t.Errorf("reassignment not restamped: session_name=%q work_dir=%q",
			got.Metadata["gc.session_name"], got.Metadata["gc.work_dir"])
	}
}

func TestStampRunSessionIdentitySkipsNonExecuting(t *testing.T) {
	sessions := newSessionBeadSnapshot([]beads.Bead{stampTestSession("worker-x", "/wd")})

	cases := map[string]beads.Bead{
		"closed bead":        {ID: "b1", Status: "closed", Assignee: "worker-x"},
		"open (not claimed)": {ID: "b2", Status: "open", Assignee: "worker-x"},
		"no assignee":        {ID: "b3", Status: "in_progress", Assignee: ""},
		"unknown session":    {ID: "b4", Status: "in_progress", Assignee: "ghost"},
	}
	for name, wb := range cases {
		t.Run(name, func(t *testing.T) {
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			store := &countingStore{Store: mem}
			stampRunSessionIdentity([]beads.Bead{wb}, []beads.Store{store}, sessions, io.Discard)
			if store.writes != 0 {
				t.Errorf("expected no stamp, got %d writes", store.writes)
			}
		})
	}
}

func TestStampRunSessionIdentityToleratesLengthMismatchAndNilSnapshot(t *testing.T) {
	run := beads.Bead{ID: "b", Status: "in_progress", Assignee: "w"}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{run}, nil)
	store := &countingStore{Store: mem}

	// Mismatched slice lengths must be a no-op, not a panic.
	stampRunSessionIdentity([]beads.Bead{run}, []beads.Store{}, newSessionBeadSnapshot(nil), io.Discard)
	// Nil snapshot must be a no-op, not a panic.
	stampRunSessionIdentity([]beads.Bead{run}, []beads.Store{store}, nil, io.Discard)
	if store.writes != 0 {
		t.Errorf("expected no writes on degenerate input, got %d", store.writes)
	}
}
