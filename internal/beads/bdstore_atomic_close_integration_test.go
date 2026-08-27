//go:build integration

package beads_test

import (
	"encoding/json"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// This row settles the question ga-f7v2ft.78.6 hinged on: is the fused terminal
// write BdStore.CloseWithMetadataIfMatch issues — one
// `bd update --status closed --set-metadata … --if-status <observed>` — durably
// equivalent to the `bd close --force` it replaces on the schema-v59 bd both
// incident cohorts ran? It answers by closing two identical fixtures each way
// against a real bd and diffing what a consumer can observe: the persisted row,
// the ready set, and the guard's refusal behavior.
//
// The equivalence is deliberately asserted as an EXACT delta set, not as
// "close enough": if a future bd starts (or stops) writing a column on one path,
// this row fails loudly instead of letting the substitution drift.
//
// Run with: go test -tags integration ./internal/beads/ -run AtomicTerminalClose
// (set GC_TEST_BD_BIN / PATH to the pinned bd; the row skips without one).

// atomicCloseFields are the row fields a consumer can observe that are not
// per-bead identity or wall-clock noise. created_at/updated_at/closed_at and
// id/title differ between two beads by construction, and revision is an opaque
// per-write token, so comparing them would prove nothing.
var atomicCloseVolatileFields = map[string]bool{
	"id": true, "title": true, "created_at": true, "updated_at": true,
	"closed_at": true, "revision": true, "metadata": true,
}

func TestAtomicTerminalCloseMatchesBdCloseOnRealBd(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skipf("bd not on PATH: %v", err)
	}
	store, dir := newConditionalIntegrationBdStore(t)
	if _, ok := beads.AtomicConditionalCloserFor(store); !ok {
		t.Skip("installed bd lacks `bd update --if-status`; the fused terminal close is not available here")
	}
	run := newConditionalIntegrationRunner(dir)

	const reason = "session drained: pool slot retired by reconciler"
	terminal := map[string]string{
		"state":        "drained",
		"close_reason": reason,
		"closed_at":    "2026-08-08T00:00:00Z",
	}

	// Two identical fixtures, each with a dependent, so the ready-set effect of
	// each close path is observable.
	viaClose := mustCreate(t, store, "equivalence fixture: bd close --force")
	viaFused := mustCreate(t, store, "equivalence fixture: fused terminal close")
	depOfClose := mustCreate(t, store, "dependent of the bd-close fixture")
	depOfFused := mustCreate(t, store, "dependent of the fused-close fixture")
	mustDepend(t, store, depOfClose.ID, viaClose.ID)
	mustDepend(t, store, depOfFused.ID, viaFused.ID)

	if readyIDs(t, run, dir)[depOfClose.ID] || readyIDs(t, run, dir)[depOfFused.ID] {
		t.Fatal("dependents are ready before their blockers closed; the fixture proves nothing")
	}

	// Path A: the historical split sequence — metadata first, then `bd close`.
	if err := store.SetMetadataBatch(viaClose.ID, terminal); err != nil {
		t.Fatalf("SetMetadataBatch on the bd-close fixture: %v", err)
	}
	if err := store.Close(viaClose.ID); err != nil {
		t.Fatalf("bd close --force: %v", err)
	}

	// Path B: the fused guarded terminal write.
	current, err := store.Get(viaFused.ID)
	if err != nil {
		t.Fatalf("Get before the fused close: %v", err)
	}
	closed, err := store.CloseWithMetadataIfMatch(viaFused.ID, current.Revision, terminal)
	if err != nil {
		t.Fatalf("CloseWithMetadataIfMatch: %v", err)
	}
	if closed.Status != "closed" || closed.Metadata["state"] != "drained" {
		t.Fatalf("fused close returned status %q state %q, want closed/drained",
			closed.Status, closed.Metadata["state"])
	}

	t.Run("terminal state and metadata are identical", func(t *testing.T) {
		a := showRow(t, run, dir, viaClose.ID)
		b := showRow(t, run, dir, viaFused.ID)
		if a["status"] != "closed" || b["status"] != "closed" {
			t.Fatalf("status: bd close = %v, fused = %v", a["status"], b["status"])
		}
		if a["closed_at"] == nil || b["closed_at"] == nil {
			t.Fatalf("closed_at not stamped server-side: bd close = %v, fused = %v", a["closed_at"], b["closed_at"])
		}
		if !reflect.DeepEqual(a["metadata"], b["metadata"]) {
			t.Fatalf("metadata diverged:\n bd close = %#v\n fused    = %#v", a["metadata"], b["metadata"])
		}
	})

	t.Run("ready-set effect is identical", func(t *testing.T) {
		ready := readyIDs(t, run, dir)
		if !ready[depOfClose.ID] || !ready[depOfFused.ID] {
			t.Fatalf("dependent unblocking diverged: bd-close dependent ready=%v, fused dependent ready=%v",
				ready[depOfClose.ID], ready[depOfFused.ID])
		}
	})

	t.Run("the only observable row delta is the close_reason column", func(t *testing.T) {
		// The known, documented substitution cost: `bd close` writes the
		// top-level close_reason column and `bd update --status closed` does
		// not. Gas City reads the reason from metadata.close_reason (beads.Bead
		// has no CloseReason field), which both paths persist identically —
		// asserted above. Any OTHER field appearing here means the fused write
		// stopped being a faithful substitute and must be re-evaluated.
		a := showRow(t, run, dir, viaClose.ID)
		b := showRow(t, run, dir, viaFused.ID)
		var delta []string
		for _, key := range unionKeys(a, b) {
			if atomicCloseVolatileFields[key] {
				continue
			}
			if !reflect.DeepEqual(a[key], b[key]) {
				delta = append(delta, key)
			}
		}
		sort.Strings(delta)
		if !reflect.DeepEqual(delta, []string{"close_reason"}) {
			t.Fatalf("observable delta = %v, want exactly [close_reason]\n bd close = %#v\n fused    = %#v",
				delta, a, b)
		}
		if a["close_reason"] != reason {
			t.Fatalf("bd close --force did not persist the reason column: %v", a["close_reason"])
		}
	})

	t.Run("the status guard refuses a second close instead of rewriting the row", func(t *testing.T) {
		before := showRow(t, run, dir, viaFused.ID)
		_, err := store.CloseWithMetadataIfMatch(viaFused.ID, 0, map[string]string{"state": "gc_swept"})
		if !beads.IsPreconditionFailed(err) {
			t.Fatalf("re-close of a closed row = %v, want *PreconditionFailedError", err)
		}
		after := showRow(t, run, dir, viaFused.ID)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("a refused re-close mutated the row:\n before = %#v\n after  = %#v", before, after)
		}
	})
}

// TestAtomicTerminalCloseCommitsAsOneWriteOnRealBd proves the property the fix
// exists for: on a real bd the terminal metadata and the close land together,
// so no observer and no crash can leave a closed row carrying a nonterminal
// lifecycle state.
func TestAtomicTerminalCloseCommitsAsOneWriteOnRealBd(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skipf("bd not on PATH: %v", err)
	}
	store, dir := newConditionalIntegrationBdStore(t)
	if _, ok := beads.AtomicConditionalCloserFor(store); !ok {
		t.Skip("installed bd lacks `bd update --if-status`")
	}
	run := newConditionalIntegrationRunner(dir)

	bead := mustCreate(t, store, "single-write terminal close")
	if err := store.SetMetadataBatch(bead.ID, map[string]string{"state": "awake", "keep": "me"}); err != nil {
		t.Fatalf("seeding awake state: %v", err)
	}
	current, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := store.CloseWithMetadataIfMatch(bead.ID, current.Revision, map[string]string{
		"state":        "drained",
		"close_reason": "session drained: pool slot retired by reconciler",
	}); err != nil {
		t.Fatalf("CloseWithMetadataIfMatch: %v", err)
	}

	row := showRow(t, run, dir, bead.ID)
	md, _ := row["metadata"].(map[string]any)
	if row["status"] != "closed" || md["state"] != "drained" {
		t.Fatalf("terminal row = status %v state %v, want closed/drained (closed+awake is the incident signature)",
			row["status"], md["state"])
	}
	if md["keep"] != "me" {
		t.Fatalf("the fused write erased a sibling metadata key: %#v", md)
	}
}

func mustCreate(t *testing.T, store *beads.BdStore, title string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: title})
	if err != nil {
		t.Fatalf("Create(%q): %v", title, err)
	}
	return b
}

func mustDepend(t *testing.T, store *beads.BdStore, id, dependsOn string) {
	t.Helper()
	if err := store.DepAdd(id, dependsOn, "blocks"); err != nil {
		t.Fatalf("DepAdd(%s -> %s): %v", id, dependsOn, err)
	}
}

// showRow returns bd's own detail-view JSON for id. It deliberately bypasses
// beads.Bead: the whole point is to see the columns the Gas City decode drops.
func showRow(t *testing.T, run beads.CommandRunner, dir, id string) map[string]any {
	t.Helper()
	out, err := run(dir, "bd", "show", "--json", id)
	if err != nil {
		t.Fatalf("bd show %s: %v\n%s", id, err, out)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(firstJSONArray(string(out))), &rows); err != nil {
		t.Fatalf("decoding bd show %s: %v\n%s", id, err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("bd show %s returned %d rows", id, len(rows))
	}
	return rows[0]
}

func readyIDs(t *testing.T, run beads.CommandRunner, dir string) map[string]bool {
	t.Helper()
	out, err := run(dir, "bd", "ready", "--json")
	if err != nil {
		t.Fatalf("bd ready: %v\n%s", err, out)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(firstJSONArray(string(out))), &rows); err != nil {
		t.Fatalf("decoding bd ready: %v\n%s", err, out)
	}
	ids := map[string]bool{}
	for _, row := range rows {
		if id, ok := row["id"].(string); ok {
			ids[id] = true
		}
	}
	return ids
}

// firstJSONArray trims bd's log-line prefixes down to the JSON array payload.
func firstJSONArray(s string) string {
	if i := strings.IndexByte(s, '['); i >= 0 {
		return s[i:]
	}
	return s
}

func unionKeys(a, b map[string]any) []string {
	seen := map[string]bool{}
	var keys []string
	for _, m := range []map[string]any{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	return keys
}
