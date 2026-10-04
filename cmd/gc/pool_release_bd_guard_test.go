package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// bdGuardExit stands in for the *exec.ExitError the bd runner wraps; BdStore
// reads the exit status through its ExitCode method.
type bdGuardExit struct{ code int }

func (e bdGuardExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func (e bdGuardExit) ExitCode() int { return e.code }

// bdGuardLedger is a one-bead fake bd CLI that honors `bd update`'s
// --if-status / --if-assignee guards the way the pinned bd does: the guards
// are checked against the stored row inside the write, an empty assignee
// means unassigned, and a mismatch writes nothing and exits 13. noGuards
// models a bd without the flags, and beforeUpdate lands a concurrent write
// just before a guarded update is evaluated.
type bdGuardLedger struct {
	mu           sync.Mutex
	bead         beads.Bead
	noGuards     bool
	beforeUpdate func(*beads.Bead)
	updates      [][]string
}

func (l *bdGuardLedger) run(_, name string, args ...string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
	switch {
	case len(args) == 2 && args[1] == "--help":
		// The conditional-writes probe: this bd has no --if-revision.
		return []byte("Usage: bd " + args[0]), nil
	case args[0] == "show":
		return l.render()
	case args[0] == "update":
		return l.update(args[1:])
	}
	return nil, fmt.Errorf("bd %v: unsupported by the fake", args)
}

func (l *bdGuardLedger) render() ([]byte, error) {
	return json.Marshal([]map[string]any{{
		"id":         l.bead.ID,
		"title":      l.bead.Title,
		"status":     l.bead.Status,
		"assignee":   l.bead.Assignee,
		"issue_type": "task",
		"created_at": nowForBDJSONTest(),
		"metadata":   l.bead.Metadata,
	}})
}

func (l *bdGuardLedger) update(args []string) ([]byte, error) {
	l.updates = append(l.updates, append([]string(nil), args...))
	if l.beforeUpdate != nil && hasBdFlag(args, "--if-assignee") {
		l.beforeUpdate(&l.bead)
		l.beforeUpdate = nil
	}
	next := l.bead
	next.Metadata = make(map[string]string, len(l.bead.Metadata))
	for k, v := range l.bead.Metadata {
		next.Metadata[k] = v
	}
	var ifStatus, ifAssignee *string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json", l.bead.ID:
		case "--if-status", "--if-assignee":
			if l.noGuards {
				return []byte("Error: unknown flag: " + args[i]), bdGuardExit{code: 1}
			}
			i++
			value := args[i]
			if args[i-1] == "--if-status" {
				ifStatus = &value
			} else {
				ifAssignee = &value
			}
		case "--status":
			i++
			next.Status = args[i]
		case "--assignee":
			i++
			next.Assignee = args[i]
		case "--set-metadata":
			i++
			key, value, _ := strings.Cut(args[i], "=")
			next.Metadata[key] = value
		default:
			return nil, fmt.Errorf("bd update: unsupported arg %q in %v", args[i], args)
		}
	}
	if (ifStatus != nil && *ifStatus != l.bead.Status) || (ifAssignee != nil && *ifAssignee != l.bead.Assignee) {
		return []byte("Error: 1 of 1 issues failed to update"), bdGuardExit{code: 13}
	}
	l.bead = next
	return []byte(`{}`), nil
}

// hasBdFlag reports whether args carries flag, whatever its value.
func hasBdFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func (l *bdGuardLedger) row() beads.Bead {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bead
}

func newBdGuardLedger(status, assignee string, extra map[string]string) *bdGuardLedger {
	metadata := map[string]string{
		"gc.routed_to":        "worker",
		"gc.session_affinity": "require",
	}
	for k, v := range extra {
		metadata[k] = v
	}
	return &bdGuardLedger{bead: beads.Bead{
		ID:       "bd-7",
		Title:    "orphaned pool work",
		Status:   status,
		Assignee: assignee,
		Metadata: metadata,
	}}
}

// TestReleaseOrphanedPoolAssignment_BdStoreGuardedRelease covers decision (c)
// for the bd-backed city. bd has no --if-revision, so the revision-fenced
// release is unavailable there. Each snapshot shape ReleaseIfCurrent cannot
// take is released as ONE `bd update` that carries the snapshot's status and
// assignee as server-side guards, and also clears the affinity metadata.
func TestReleaseOrphanedPoolAssignment_BdStoreGuardedRelease(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   string
		assignee string
		extra    map[string]string
		wrap     bool
	}{
		{name: "open-status strand", status: "open", assignee: "worker-dead"},
		{name: "assignee-less in_progress", status: "in_progress", assignee: ""},
		{name: "continuation group", status: "in_progress", assignee: "worker-dead", extra: map[string]string{beadmeta.ContinuationGroupMetadataKey: "grp-1"}},
		{name: "through a resolve-target wrapper", status: "open", assignee: "worker-dead", wrap: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newBdGuardLedger(tc.status, tc.assignee, tc.extra)
			var store beads.Store = beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
			if tc.wrap {
				// The typed class wrappers and the cmd/gc policy store declare
				// their target rather than promote the capability.
				store = beads.WorkStore{Store: store}
			}
			wb := ledger.row()

			if !releaseOrphanedPoolAssignment(store, wb, false) {
				t.Fatalf("release = false, want the guarded release to land; bd updates %v", ledger.updates)
			}
			if len(ledger.updates) != 1 {
				t.Fatalf("bd updates = %v, want exactly one guarded write", ledger.updates)
			}
			update := ledger.updates[0]
			if got := testFlagValue(update, "--if-status"); got != tc.status {
				t.Errorf("--if-status = %q, want the snapshot's %q", got, tc.status)
			}
			if got := testFlagValue(update, "--if-assignee"); !hasBdFlag(update, "--if-assignee") || got != tc.assignee {
				t.Errorf("--if-assignee = %q, want the snapshot's %q", got, tc.assignee)
			}
			got := ledger.row()
			if got.Status != "open" || got.Assignee != "" {
				t.Fatalf("row = status %q assignee %q, want open and unassigned", got.Status, got.Assignee)
			}
			for key, want := range clearedSessionAffinityMetadata() {
				if got.Metadata[key] != want {
					t.Errorf("metadata[%q] = %q, want it cleared in the same write", key, got.Metadata[key])
				}
			}
		})
	}
}

// TestReleaseOrphanedPoolAssignment_BdStoreGuardLostToAClaim is the exit-13
// path: a fresh worker claims the bead after the sweep's snapshot. bd checks
// the guard against the claimed row, writes nothing and exits 13, and the
// release skips the bead for the next sweep instead of clobbering the claim.
func TestReleaseOrphanedPoolAssignment_BdStoreGuardLostToAClaim(t *testing.T) {
	ledger := newBdGuardLedger("in_progress", "worker-dead", map[string]string{beadmeta.ContinuationGroupMetadataKey: "grp-1"})
	ledger.beforeUpdate = func(b *beads.Bead) { b.Assignee = "worker-live" }
	store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
	wb := ledger.row()

	var buf bytes.Buffer
	restore := captureLogOutput(&buf)
	defer restore()

	if releaseOrphanedPoolAssignment(store, wb, false) {
		t.Fatal("release = true after bd refused the guard")
	}
	got := ledger.row()
	if got.Status != "in_progress" || got.Assignee != "worker-live" || got.Metadata[beadmeta.ContinuationGroupMetadataKey] != "grp-1" {
		t.Fatalf("row = status %q assignee %q group %q, want the claim untouched", got.Status, got.Assignee, got.Metadata[beadmeta.ContinuationGroupMetadataKey])
	}
	if !strings.Contains(buf.String(), "assignment changed") {
		t.Fatalf("log output = %q, want the lost race logged as a skip", buf.String())
	}
}

// TestReleaseOrphanedPoolAssignment_BdStoreWithoutGuardsRefuses keeps
// refusal (a) for a bd that has neither --if-revision nor the guard flags:
// nothing is written blind.
func TestReleaseOrphanedPoolAssignment_BdStoreWithoutGuardsRefuses(t *testing.T) {
	ledger := newBdGuardLedger("open", "worker-dead", nil)
	ledger.noGuards = true
	store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
	wb := ledger.row()

	var buf bytes.Buffer
	restore := captureLogOutput(&buf)
	defer restore()

	if releaseOrphanedPoolAssignment(store, wb, false) {
		t.Fatal("release = true on a bd that can fence neither way")
	}
	got := ledger.row()
	if got.Status != "open" || got.Assignee != "worker-dead" || got.Metadata["gc.session_affinity"] != "require" {
		t.Fatalf("row = status %q assignee %q affinity %q, want it left as it was", got.Status, got.Assignee, got.Metadata["gc.session_affinity"])
	}
	for _, update := range ledger.updates {
		if !hasBdFlag(update, "--if-status") || !hasBdFlag(update, "--if-assignee") {
			t.Fatalf("bd update without the guards reached the store: %v", update)
		}
	}
	if !strings.Contains(buf.String(), "cannot release it conditionally") {
		t.Fatalf("log output = %q, want the refusal logged", buf.String())
	}
}

// TestReleaseWorkBead_BdStoreGuardedRelease gives ReleaseWorkBead's tier 2
// the pool release's policy on the bd-backed city, and pins the error contract
// its close-gating callers rely on: a landed or lost-to-a-claim release is
// nil, and a bd that cannot fence it is an error with the bead still assigned.
func TestReleaseWorkBead_BdStoreGuardedRelease(t *testing.T) {
	t.Run("lands as one guarded write", func(t *testing.T) {
		ledger := newBdGuardLedger("open", "retired-session", nil)
		store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
		wa := workAssignmentForStore(beads.WorkStore{Store: store})

		if err := wa.ReleaseWorkBead(ledger.row(), ""); err != nil {
			t.Fatalf("ReleaseWorkBead: %v", err)
		}
		if len(ledger.updates) != 1 || testFlagValue(ledger.updates[0], "--if-status") != "open" || testFlagValue(ledger.updates[0], "--if-assignee") != "retired-session" {
			t.Fatalf("bd updates = %v, want one write guarded on open/retired-session", ledger.updates)
		}
		if got := ledger.row(); got.Assignee != "" || got.Status != "open" {
			t.Fatalf("row = status %q assignee %q, want open and unassigned", got.Status, got.Assignee)
		}
	})
	t.Run("lost to a claim", func(t *testing.T) {
		ledger := newBdGuardLedger("open", "retired-session", nil)
		ledger.beforeUpdate = func(b *beads.Bead) { b.Status, b.Assignee = "in_progress", "fresh-worker" }
		store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
		wa := workAssignmentForStore(beads.WorkStore{Store: store})

		if err := wa.ReleaseWorkBead(ledger.row(), ""); err != nil {
			t.Fatalf("ReleaseWorkBead = %v, want nil: the bead moved on, as with tier 1's lost race", err)
		}
		if got := ledger.row(); got.Assignee != "fresh-worker" || got.Status != "in_progress" {
			t.Fatalf("row = status %q assignee %q, want the claim untouched", got.Status, got.Assignee)
		}
	})
	t.Run("bd without guards", func(t *testing.T) {
		ledger := newBdGuardLedger("open", "retired-session", nil)
		ledger.noGuards = true
		store := beads.NewBdStoreWithPrefix(t.TempDir(), ledger.run, "bd")
		wa := workAssignmentForStore(beads.WorkStore{Store: store})

		err := wa.ReleaseWorkBead(ledger.row(), "")
		if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
			t.Fatalf("ReleaseWorkBead = %v, want an error wrapping ErrConditionalWriteUnsupported so a caller does not close over the bead", err)
		}
		if got := ledger.row(); got.Assignee != "retired-session" {
			t.Fatalf("assignee = %q, want it left assigned", got.Assignee)
		}
	})
}
