package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// fakeBdStatusRow is one issue in fakeBdStatusCLI's ledger, carrying bd's RAW
// stored status (bd has more statuses than Gas City's open/in_progress/closed).
type fakeBdStatusRow struct {
	id, status string
}

// fakeBdStatusCLI emulates the bd CLI's `list` status semantics that the
// bd-backed store (the maintainer city's path: postgres metadata backend ->
// native_store_unavailable) is served by: `--status=a,b` returns rows whose
// STORED status is literally one of a,b; no --status returns every non-closed
// row; --all adds closed rows. Every row carries the same routing metadata so
// the test can run the workflows pack's root-inventory query shape.
func fakeBdStatusCLI(t *testing.T, rows []fakeBdStatusRow) beads.CommandRunner {
	t.Helper()
	return func(_, _ string, args ...string) ([]byte, error) {
		if len(args) == 0 {
			return []byte(`[]`), nil
		}
		switch args[0] {
		case "list":
			var statuses []string
			all := false
			for _, arg := range args[1:] {
				if v, ok := strings.CutPrefix(arg, "--status="); ok {
					statuses = strings.Split(v, ",")
				}
				if arg == "--all" {
					all = true
				}
			}
			out := make([]map[string]any, 0, len(rows))
			for _, row := range rows {
				switch {
				case len(statuses) > 0 && !slices.Contains(statuses, row.status):
					continue
				case len(statuses) == 0 && !all && row.status == "closed":
					continue
				}
				out = append(out, map[string]any{
					"id":         row.id,
					"title":      row.id,
					"status":     row.status,
					"issue_type": "task",
					"created_at": "2026-09-01T00:00:00Z",
					"metadata":   map[string]string{"gc.workflow_root_for": "src-1"},
				})
			}
			data, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("marshal fake bd rows: %v", err)
			}
			return data, nil
		default:
			// bd query (the ephemeral leg) and anything else: no rows.
			return []byte(`[]`), nil
		}
	}
}

// TestReadyStatusLegsAreExhaustiveOnBdStore pins the workflows pack's root
// inventory contract on the bd-backed store: `gc ready --metadata-field ...
// --status {open,in_progress,blocked,closed}` must, across the four legs, list
// EVERY bead. bd stores richer statuses (blocked, deferred, hooked, pinned,
// custom review/testing) that mapBdStatus folds into Gas City's "open", but the
// open leg pushed `--status=open` down to bd, which matches only the literal
// stored "open" — and the blocked leg's bd rows are then relabeled "open" and
// dropped by the status filter. Such a root was invisible to all four legs, so
// the pack could sling a duplicate child workflow for it.
func TestReadyStatusLegsAreExhaustiveOnBdStore(t *testing.T) {
	rows := []fakeBdStatusRow{
		{id: "gc-open", status: "open"},
		{id: "gc-blocked", status: "blocked"},
		{id: "gc-deferred", status: "deferred"},
		{id: "gc-hooked", status: "hooked"},
		{id: "gc-pinned", status: "pinned"},
		{id: "gc-review", status: "review"},
		{id: "gc-wip", status: "in_progress"},
		{id: "gc-done", status: "closed"},
	}
	store := beads.NewBdStore("/city", fakeBdStatusCLI(t, rows))
	legs := []readyLeg{readyTestLeg("city", store)}

	seen := map[string][]string{}
	for _, status := range readyKnownStatuses {
		got, err := readyBeadsForOpts(legs, readyOpts{
			status:         status,
			metadataFields: []string{"gc.workflow_root_for=src-1"},
		})
		if err != nil {
			t.Fatalf("gc ready --status %s: %v", status, err)
		}
		for _, row := range got {
			seen[row.ID] = append(seen[row.ID], status)
		}
	}
	for _, row := range rows {
		if len(seen[row.id]) == 0 {
			t.Errorf("bead %s (bd status %q) is invisible to every gc ready --status leg %v; the root inventory is not exhaustive", row.id, row.status, readyKnownStatuses)
		}
	}
	// Each bd status folded into Gas City's "open" must be served by the open
	// leg, the same answer the native store gives (it excludes only closed and
	// in_progress), and must not leak into the in_progress or closed legs.
	for _, row := range rows {
		want := "open"
		switch row.status {
		case "in_progress", "closed":
			want = row.status
		}
		if !slices.Contains(seen[row.id], want) {
			t.Errorf("bead %s (bd status %q) listed by legs %v, want it in the %q leg", row.id, row.status, seen[row.id], want)
		}
	}
}
