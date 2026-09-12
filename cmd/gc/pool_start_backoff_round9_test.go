package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The round-9 owed-reset marker (gc.start_reset_owed_*) and its sweep were
// REMOVED in round 10: the clear now runs before the confirming batch on the
// one commit path (pool_start_backoff_round10_test.go); round 11 made the
// persisted trigger the one operand a named start is charged to
// (pool_start_backoff_round11_test.go), so the round-9 build's-verdict test
// is gone too.

// The cases codex round 9 found missing or wrong (evidence 07-codex-r9.md).

// round9FailingWriter is a conditional writer whose fenced update fails with
// an ordinary (non-precondition) error: the work store is down for writes.
type round9FailingWriter struct {
	beads.ConditionalWriter
	err error
}

func (w round9FailingWriter) UpdateIfMatch(string, int64, beads.UpdateOpts) error { return w.err }

// TestWriteWorkRecordFailsClosedWhenTheFenceIsRefusedUnderRequire: the
// capability probe passed when the writer was resolved, but the store
// refuses the fence at write time (beads.ErrConditionalWriteUnsupported).
// Under beads.conditional_writes = "require" the record is NOT written
// unfenced; under any other mode the plain re-read path is taken as before.
func TestWriteWorkRecordFailsClosedWhenTheFenceIsRefusedUnderRequire(t *testing.T) {
	for _, require := range []bool{true, false} {
		cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
		store := beads.NewMemStore()
		work, err := store.Create(beads.Bead{Title: "work", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
		if err != nil {
			t.Fatal(err)
		}
		realWriter, _ := beads.ConditionalWriterFor(store)
		var stderr bytes.Buffer
		policy := &workStartFailurePolicy{
			workStore:         store,
			templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
			canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
			limitFor:          func(string) int { return 5 },
			resolveWriter: func(beads.Store) (beads.ConditionalWriter, error) {
				return round9FailingWriter{ConditionalWriter: realWriter, err: beads.ErrConditionalWriteUnsupported}, nil
			},
			requireFenced: func(beads.Store) bool { return require },
			stderr:        &stderr,
		}
		policy.recordStartFailure(workTrigger{BeadID: work.ID, StoreRef: "city"}, "worker", errPreStartFailure, time.Now().UTC())
		row, _ := store.Get(work.ID)
		got := readWorkStartFailureState(row.Metadata).Failures
		switch {
		case require && got != 0:
			t.Fatalf("require: the refused fence must not fall back to an unfenced write, record=%d\nstderr:\n%s", got, stderr.String())
		case require && !strings.Contains(stderr.String(), "conditional_writes=require"):
			t.Fatalf("require: the refusal is said:\n%s", stderr.String())
		case !require && got != 1:
			t.Fatalf("auto/off: the plain path still charges, record=%d\nstderr:\n%s", got, stderr.String())
		}
	}
}

// round9ErrStore fails every read.
type round9ErrStore struct {
	beads.Store
	err error
}

func (s round9ErrStore) Get(string) (beads.Bead, error) { return beads.Bead{}, s.err }

// TestTriggerBeadLookupErrorsAreSaidNotSwallowed: a store that fails to
// answer is not the bead's absence — the dropped charge is logged, and a
// clear that could not find its bead is NOT settled (the start it would
// confirm is not confirmed).
func TestTriggerBeadLookupErrorsAreSaidNotSwallowed(t *testing.T) {
	var stderr bytes.Buffer
	policy := &workStartFailurePolicy{
		workStore: round9ErrStore{Store: beads.NewMemStore(), err: errors.New("backend unavailable")},
		limitFor:  func(string) int { return 5 },
		stderr:    &stderr,
	}
	policy.recordStartFailure(workTrigger{BeadID: "gp-somewhere", StoreRef: "city"}, "worker", errPreStartFailure, time.Now().UTC())
	if !strings.Contains(stderr.String(), "could not be read") || !strings.Contains(stderr.String(), "backend unavailable") {
		t.Fatalf("a lookup failure must be said:\n%s", stderr.String())
	}
	if policy.recordStartSuccess(workTrigger{BeadID: "gp-somewhere", StoreRef: "city"}, "worker") {
		t.Fatal("a clear whose bead could not be read is not settled")
	}
	// A bead in no store at all (every store answers not-found) is settled.
	quiet := &workStartFailurePolicy{workStore: beads.NewMemStore(), limitFor: func(string) int { return 5 }, stderr: &stderr}
	if !quiet.recordStartSuccess(workTrigger{BeadID: "gp-nowhere", StoreRef: "city"}, "worker") {
		t.Fatal("no such bead: nothing to clear, settled")
	}
}
