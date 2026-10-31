package beads_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func releaseOpts() beads.UpdateOpts {
	open, empty := "open", ""
	return beads.UpdateOpts{
		Status:   &open,
		Assignee: &empty,
		Metadata: map[string]string{"gc.session_affinity": ""},
	}
}

// TestUpdateIfAssignmentStatesBothGuardsOnOneUpdate pins the argv: the update
// BdStore.Update would run, plus both guards, as ONE bd call. An empty
// expected assignee is passed through empty (bd: "must be unassigned").
func TestUpdateIfAssignmentStatesBothGuardsOnOneUpdate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		assignee string
	}{
		{name: "assigned", assignee: "worker-1"},
		{name: "unassigned", assignee: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &releaseVerbRunner{}
			s := beads.NewBdStore("/city", runner.run)

			updated, err := s.UpdateIfAssignment("bd-42", "in_progress", tc.assignee, releaseOpts())
			if err != nil || !updated {
				t.Fatalf("UpdateIfAssignment = (%v, %v), want (true, nil)", updated, err)
			}
			want := []string{
				"bd", "update", "--json", "bd-42", "--status", "open", "--assignee", "",
				"--set-metadata", "gc.session_affinity=", "--if-status", "in_progress", "--if-assignee", tc.assignee,
			}
			calls := runner.releaseVerbArgv()
			if len(calls) != 1 || strings.Join(calls[0], "\x00") != strings.Join(want, "\x00") {
				t.Fatalf("argv = %q\nwant one call %q", calls, want)
			}
		})
	}
}

// TestUpdateIfAssignmentReadsAGuardMissFromExit13 is the lost race: bd wrote
// nothing and exited 13, which is an answer, not an error, and is not retried.
func TestUpdateIfAssignmentReadsAGuardMissFromExit13(t *testing.T) {
	runner := &releaseVerbRunner{reply: func(args []string) ([]byte, error) {
		if isReleaseVerb(args) {
			return []byte("Error: 1 of 1 issues failed to update"), exitErrorWithCode(t, 13)
		}
		return nil, fmt.Errorf("unexpected %v", args)
	}}
	updated, err := beads.NewBdStore("/city", runner.run).UpdateIfAssignment("bd-42", "open", "worker-1", releaseOpts())
	if err != nil || updated {
		t.Fatalf("UpdateIfAssignment = (%v, %v), want (false, nil) on a guard miss", updated, err)
	}
	if len(runner.releaseVerbArgv()) != 1 {
		t.Fatalf("a guard miss must not be retried; calls = %v", runner.argv())
	}
}

// TestUpdateIfAssignmentWithoutTheGuardFlagsIsUnsupported keeps the refusal
// for a bd that predates the guards: ErrConditionalWriteUnsupported with
// nothing written, latched so later calls do not reach bd at all.
func TestUpdateIfAssignmentWithoutTheGuardFlagsIsUnsupported(t *testing.T) {
	runner := &releaseVerbRunner{reply: func(args []string) ([]byte, error) {
		if isReleaseVerb(args) {
			return []byte("Error: unknown flag: --if-status"), exitErrorWithCode(t, 1)
		}
		return nil, fmt.Errorf("unexpected %v", args)
	}}
	s := beads.NewBdStore("/city", runner.run)
	for attempt := 1; attempt <= 2; attempt++ {
		updated, err := s.UpdateIfAssignment("bd-42", "open", "worker-1", releaseOpts())
		if !errors.Is(err, beads.ErrConditionalWriteUnsupported) || updated {
			t.Fatalf("attempt %d = (%v, %v), want (false, ErrConditionalWriteUnsupported)", attempt, updated, err)
		}
	}
	if calls := runner.releaseVerbArgv(); len(calls) != 1 {
		t.Fatalf("calls = %v, want one probe of the flags and then the latch", calls)
	}
}

// TestUpdateIfAssignmentClassifiesOtherFailures covers the rest of the
// verdict table: an unresolvable id holds no assignment (false, nil), any
// other failure is an error, and opts the guard does not cover never reach bd.
func TestUpdateIfAssignmentClassifiesOtherFailures(t *testing.T) {
	t.Run("unresolvable id", func(t *testing.T) {
		runner := &releaseVerbRunner{
			show: func(id string) ([]byte, error) {
				return nil, exitErrorWithDetail(t, 1, `Error resolving `+id+`: no issue found matching "`+id+`"`)
			},
			reply: func([]string) ([]byte, error) {
				return nil, exitErrorWithDetail(t, 1, `Error resolving bd-42: no issue found matching "bd-42"`)
			},
		}
		updated, err := beads.NewBdStore("/city", runner.run).UpdateIfAssignment("bd-42", "open", "", releaseOpts())
		if err != nil || updated {
			t.Fatalf("UpdateIfAssignment = (%v, %v), want (false, nil)", updated, err)
		}
	})
	t.Run("backend failure", func(t *testing.T) {
		runner := &releaseVerbRunner{reply: func([]string) ([]byte, error) {
			return []byte("permission denied"), exitErrorWithCode(t, 1)
		}}
		updated, err := beads.NewBdStore("/city", runner.run).UpdateIfAssignment("bd-42", "open", "", releaseOpts())
		if err == nil || updated || beads.IsConditionalWriteUnsupported(err) {
			t.Fatalf("UpdateIfAssignment = (%v, %v), want a plain error", updated, err)
		}
	})
	t.Run("labels are outside the guard", func(t *testing.T) {
		runner := &releaseVerbRunner{}
		opts := releaseOpts()
		opts.Labels = []string{"x"}
		if _, err := beads.NewBdStore("/city", runner.run).UpdateIfAssignment("bd-42", "open", "", opts); err == nil {
			t.Fatal("UpdateIfAssignment accepted a label edit the guard does not cover")
		}
		if calls := runner.argv(); len(calls) != 0 {
			t.Fatalf("calls = %v, want none", calls)
		}
	})
}

// guardedMemStore is a MemStore whose backend honors the assignment guard, so
// a CachingStore over it exercises the cache side of UpdateIfAssignment.
type guardedMemStore struct {
	*beads.MemStore
}

func (s guardedMemStore) UpdateIfAssignment(id, expectedStatus, expectedAssignee string, opts beads.UpdateOpts) (bool, error) {
	current, err := s.Get(id)
	if err != nil {
		return false, err
	}
	if current.Status != expectedStatus || current.Assignee != expectedAssignee {
		return false, nil
	}
	return true, s.Update(id, opts)
}

// TestCachingStoreUpdateIfAssignment forwards the guard to the backing store
// and keeps the cache honest: a landed update is visible through the cache,
// a refused one evicts the stale cached row, and a backing without the guard
// is unsupported with nothing written.
func TestCachingStoreUpdateIfAssignment(t *testing.T) {
	seed := func(t *testing.T, mem *beads.MemStore) beads.Bead {
		t.Helper()
		b, err := mem.Create(beads.Bead{Title: "work", Assignee: "worker-dead"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return b
	}
	t.Run("landed", func(t *testing.T) {
		mem := beads.NewMemStore()
		b := seed(t, mem)
		cache := beads.NewCachingStoreForTest(guardedMemStore{mem}, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		updated, err := cache.UpdateIfAssignment(b.ID, "open", "worker-dead", releaseOpts())
		if err != nil || !updated {
			t.Fatalf("UpdateIfAssignment = (%v, %v), want (true, nil)", updated, err)
		}
		got, err := cache.Get(b.ID)
		if err != nil || got.Assignee != "" {
			t.Fatalf("cached row = %+v (%v), want the release visible", got, err)
		}
	})
	t.Run("refused", func(t *testing.T) {
		mem := beads.NewMemStore()
		b := seed(t, mem)
		cache := beads.NewCachingStoreForTest(guardedMemStore{mem}, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		live := "worker-live"
		if err := mem.Update(b.ID, beads.UpdateOpts{Assignee: &live}); err != nil {
			t.Fatalf("out-of-band claim: %v", err)
		}
		updated, err := cache.UpdateIfAssignment(b.ID, "open", "worker-dead", releaseOpts())
		if err != nil || updated {
			t.Fatalf("UpdateIfAssignment = (%v, %v), want (false, nil)", updated, err)
		}
		got, err := cache.Get(b.ID)
		if err != nil || got.Assignee != "worker-live" {
			t.Fatalf("row through the cache = %+v (%v), want the out-of-band claim", got, err)
		}
	})
	t.Run("backing without the guard", func(t *testing.T) {
		mem := beads.NewMemStore()
		b := seed(t, mem)
		cache := beads.NewCachingStoreForTest(mem, nil)
		updated, err := cache.UpdateIfAssignment(b.ID, "open", "worker-dead", releaseOpts())
		if !errors.Is(err, beads.ErrConditionalWriteUnsupported) || updated {
			t.Fatalf("UpdateIfAssignment = (%v, %v), want (false, ErrConditionalWriteUnsupported)", updated, err)
		}
		if got, _ := mem.Get(b.ID); got.Assignee != "worker-dead" {
			t.Fatalf("assignee = %q, want it untouched", got.Assignee)
		}
	})
}

func TestBdStoreBlockedReleaseAcceptsChildID(t *testing.T) {
	runner := &releaseVerbRunner{show: func(id string) ([]byte, error) {
		if id != "bd-42.1" {
			t.Fatalf("read ID = %q", id)
		}
		return []byte(`[{"id":"bd-42.1","status":"blocked","assignee":"dead-session"}]`), nil
	}}
	released, handled, err := beads.NewBdStore("/city", runner.run).ReleaseBlockedIfCurrent("bd-42.1", "dead-session", map[string]string{"gc.continuation_group": "", "gc.run_target": "repo/worker", "gc.session_affinity": ""})
	if err != nil || !released || !handled {
		t.Fatalf("child release = %v, %v, %v", released, handled, err)
	}
	want := []string{"bd", "update", "--json", "bd-42.1", "--assignee", "", "--set-metadata", "gc.continuation_group=", "--set-metadata", "gc.run_target=repo/worker", "--set-metadata", "gc.session_affinity=", "--if-status", "blocked", "--if-assignee", "dead-session"}
	calls := runner.releaseVerbArgv()
	if len(calls) != 1 || strings.Join(calls[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("mutations = %q, want one combined guarded update %q", calls, want)
	}
}

func TestBdStoreBlockedReleaseRefusesUnresolvedRows(t *testing.T) {
	for _, tc := range []struct {
		name, id, holder, row string
		showMiss, backendErr  bool
		wantErr, wantQuery    bool
	}{
		{name: "empty ID", holder: "dead-session", wantErr: true},
		{name: "empty holder", id: "bd-42.1", holder: " ", wantErr: true},
		{name: "child miss cannot enter wisp query", id: "bd-42.1", holder: "dead-session", showMiss: true},
		{name: "exact ID collision", id: "bd-42.1", holder: "dead-session", row: `[{"id":"bd-42.2","status":"blocked","assignee":"dead-session"}]`, wantErr: true},
		{name: "holder changed", id: "bd-42.1", holder: "dead-session", row: `[{"id":"bd-42.1","status":"blocked","assignee":"live-session"}]`},
		{name: "absent", id: "bd-42.1", holder: "dead-session", row: `[]`},
		{name: "ambiguous", id: "bd-42.1", holder: "dead-session", row: `[{"id":"bd-42.1"},{"id":"bd-42.2"}]`},
		{name: "operational failure", id: "bd-42.1", holder: "dead-session", backendErr: true, wantErr: true},
		{name: "wisp absent", id: "bd-42", holder: "dead-session", showMiss: true, row: `[]`, wantQuery: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &releaseVerbRunner{show: func(id string) ([]byte, error) {
				if tc.showMiss {
					return nil, exitErrorWithDetail(t, 1, `no issue found matching "`+id+`"`)
				}
				if tc.backendErr {
					return nil, errors.New("permission denied")
				}
				return []byte(tc.row), nil
			}, reply: func(args []string) ([]byte, error) {
				if tc.wantQuery && strings.Join(args, "\x00") == strings.Join([]string{"query", "--json", "ephemeral=true AND id=bd-42", "--all", "--limit", "1"}, "\x00") {
					return []byte(tc.row), nil
				}
				t.Fatalf("unexpected fallback or mutation: %v", args)
				return nil, errors.New("unexpected command")
			}}
			released, handled, err := beads.NewBdStore("/city", runner.run).ReleaseBlockedIfCurrent(tc.id, tc.holder, map[string]string{"gc.continuation_group": "", "gc.run_target": "repo/worker"})
			if released || !handled || (err != nil) != tc.wantErr {
				t.Fatalf("refusal = %v, %v, %v", released, handled, err)
			}
			queries := 0
			for _, call := range runner.argv() {
				if call[1] == "query" {
					queries++
				}
				if call[1] == "update" {
					t.Fatalf("refusal reached update: %v", call)
				}
			}
			if (queries == 1) != tc.wantQuery {
				t.Fatalf("query count = %d", queries)
			}
		})
	}
}
