package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestGraphTable(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "setup DB"})      // gc-1
	_, _ = store.Create(beads.Bead{Title: "add migration"}) // gc-2
	_, _ = store.Create(beads.Bead{Title: "deploy"})        // gc-3
	_ = store.Close("gc-3")

	// gc-2 blocked by gc-1.
	_ = store.DepAdd("gc-2", "gc-1", "blocks")

	var stdout, stderr bytes.Buffer
	code := doGraph(store, []string{"gc-1", "gc-2", "gc-3"}, graphOpts{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doGraph = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()

	// Header.
	if !strings.Contains(out, "BEAD") || !strings.Contains(out, "BLOCKED BY") {
		t.Errorf("missing table header; got:\n%s", out)
	}
	// gc-1 should be ready (no blockers, not closed).
	if !strings.Contains(out, "gc-1") {
		t.Errorf("missing gc-1 in output:\n%s", out)
	}
	// gc-2 should be blocked by gc-1 — check the gc-2 line specifically.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "gc-2") {
			if !strings.Contains(line, "gc-1") {
				t.Errorf("gc-2 row should show gc-1 as blocker:\n%s", out)
			}
			break
		}
	}
	// gc-3 should show "done".
	if !strings.Contains(out, "done") {
		t.Errorf("closed bead should show done:\n%s", out)
	}
	// Summary line.
	if !strings.Contains(out, "3 bead(s)") {
		t.Errorf("missing summary line:\n%s", out)
	}
}

func TestGraphMermaid(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "task A"}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task B"}) // gc-2
	_ = store.DepAdd("gc-2", "gc-1", "blocks")

	var stdout, stderr bytes.Buffer
	code := doGraph(store, []string{"gc-1", "gc-2"}, graphOpts{Mermaid: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doGraph mermaid = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()

	if !strings.Contains(out, "graph TD") {
		t.Errorf("missing mermaid header:\n%s", out)
	}
	// Edge: gc-1 --> gc-2
	if !strings.Contains(out, "gc-1 --> gc-2") {
		t.Errorf("missing dep edge:\n%s", out)
	}
}

func TestGraphConvoyExpansion(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "my convoy", Type: "convoy"}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "child A", ParentID: "gc-1"}) // gc-2
	_, _ = store.Create(beads.Bead{Title: "child B", ParentID: "gc-1"}) // gc-3

	var stdout, stderr bytes.Buffer
	code := doGraph(store, []string{"gc-1"}, graphOpts{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doGraph convoy = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()

	// Should expand to children, not show the convoy itself.
	if strings.Contains(out, "my convoy") {
		t.Errorf("convoy bead should be expanded, not shown:\n%s", out)
	}
	if !strings.Contains(out, "child A") || !strings.Contains(out, "child B") {
		t.Errorf("should show convoy children:\n%s", out)
	}
}

func TestGraphEpicExpansion(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "my epic", Type: "epic"})     // gc-1
	_, _ = store.Create(beads.Bead{Title: "story 1", ParentID: "gc-1"}) // gc-2
	_, _ = store.Create(beads.Bead{Title: "story 2", ParentID: "gc-1"}) // gc-3

	var stdout, stderr bytes.Buffer
	code := doGraph(store, []string{"gc-1"}, graphOpts{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doGraph epic = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()

	if !strings.Contains(out, "story 1") || !strings.Contains(out, "story 2") {
		t.Errorf("should show epic children:\n%s", out)
	}
}

func TestGraphMissingArgs(t *testing.T) {
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doGraph(store, nil, graphOpts{}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doGraph no args = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "missing") {
		t.Errorf("stderr = %q, want missing-args message", stderr.String())
	}
}

func TestGraphEmptyConvoy(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "empty convoy", Type: "convoy"}) // gc-1

	var stdout, stderr bytes.Buffer
	code := doGraph(store, []string{"gc-1"}, graphOpts{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doGraph empty convoy = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No beads to graph") {
		t.Errorf("stdout = %q, want empty-graph message", stdout.String())
	}
}

func TestGraphDepsFilteredToSet(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "task A"}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task B"}) // gc-2
	_, _ = store.Create(beads.Bead{Title: "task C"}) // gc-3

	// gc-2 depends on gc-1 and gc-3.
	_ = store.DepAdd("gc-2", "gc-1", "blocks")
	_ = store.DepAdd("gc-2", "gc-3", "blocks")

	// Only graph gc-1 and gc-2 — gc-3 dep should be filtered out.
	var stdout, stderr bytes.Buffer
	code := doGraph(store, []string{"gc-1", "gc-2"}, graphOpts{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doGraph = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()

	// gc-2's blocked-by should show gc-1 but NOT gc-3.
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if strings.Contains(line, "gc-2") && strings.Contains(line, "gc-3") {
			t.Errorf("gc-3 should be filtered out (not in set):\n%s", out)
		}
	}
	// Summary: 1 ready (gc-1), 1 blocked (gc-2), 0 closed.
	if !strings.Contains(out, "1 ready") {
		t.Errorf("expected 1 ready:\n%s", out)
	}
}

func TestGraphMermaidClosedStyle(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "done task"}) // gc-1
	_ = store.Close("gc-1")
	_, _ = store.Create(beads.Bead{Title: "ready task"}) // gc-2

	var stdout, stderr bytes.Buffer
	code := doGraph(store, []string{"gc-1", "gc-2"}, graphOpts{Mermaid: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doGraph mermaid = %d, want 0", code)
	}
	out := stdout.String()

	// Closed bead gets green style.
	if !strings.Contains(out, "style gc-1 fill:#90EE90") {
		t.Errorf("missing green style for closed bead:\n%s", out)
	}
	// Ready bead gets gold style.
	if !strings.Contains(out, "style gc-2 fill:#FFD700") {
		t.Errorf("missing gold style for ready bead:\n%s", out)
	}
}

func TestGraphMermaidLabelEscaping(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: `fix "quotes" issue`}) // gc-1

	var stdout, stderr bytes.Buffer
	code := doGraph(store, []string{"gc-1"}, graphOpts{Mermaid: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doGraph = %d, want 0", code)
	}
	out := stdout.String()

	// Double quotes in titles should be escaped to single quotes in the label.
	if !strings.Contains(out, "'quotes'") {
		t.Errorf("should use single quotes for escaped title:\n%s", out)
	}
}

func TestGraphClosedBlockerIsReady(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "prereq"})    // gc-1
	_, _ = store.Create(beads.Bead{Title: "main task"}) // gc-2
	_ = store.DepAdd("gc-2", "gc-1", "blocks")

	// Close the blocker — gc-2 should now be ready.
	_ = store.Close("gc-1")

	var stdout, stderr bytes.Buffer
	code := doGraph(store, []string{"gc-1", "gc-2"}, graphOpts{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doGraph = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()

	// gc-2 should show as ready (blocker is closed).
	if !strings.Contains(out, "1 ready") {
		t.Errorf("gc-2 should be ready when blocker is closed:\n%s", out)
	}
	// gc-1 should show as done.
	if !strings.Contains(out, "done") {
		t.Errorf("gc-1 should show done:\n%s", out)
	}
	// Summary: 1 done, 1 ready, 0 blocked.
	if strings.Contains(out, "1 blocked") {
		t.Errorf("no beads should be blocked:\n%s", out)
	}
}

func TestGraphDeduplicate(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "task A"}) // gc-1

	var stdout, stderr bytes.Buffer
	// Pass same ID twice — should only appear once.
	code := doGraph(store, []string{"gc-1", "gc-1"}, graphOpts{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doGraph = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "1 bead(s)") {
		t.Errorf("duplicate ID should be deduplicated:\n%s", stdout.String())
	}
}

func TestGraphTree(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "setup DB"})      // gc-1
	_, _ = store.Create(beads.Bead{Title: "add migration"}) // gc-2
	_, _ = store.Create(beads.Bead{Title: "deploy"})        // gc-3
	_ = store.Close("gc-1")

	// gc-2 blocked by gc-1, gc-3 blocked by gc-2.
	_ = store.DepAdd("gc-2", "gc-1", "blocks")
	_ = store.DepAdd("gc-3", "gc-2", "blocks")

	var stdout, stderr bytes.Buffer
	code := doGraph(store, []string{"gc-1", "gc-2", "gc-3"}, graphOpts{Tree: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doGraph tree = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()

	// Root node (gc-1, closed) should use checkmark icon.
	if !strings.Contains(out, "✓ gc-1: setup DB") {
		t.Errorf("root should show ✓ icon for closed bead:\n%s", out)
	}
	// gc-2 should appear as child with tree connector.
	if !strings.Contains(out, "└── ○ gc-2: add migration") {
		t.Errorf("gc-2 should be tree child of gc-1:\n%s", out)
	}
	// gc-3 should be nested under gc-2.
	if !strings.Contains(out, "└── ○ gc-3: deploy") {
		t.Errorf("gc-3 should be tree child of gc-2:\n%s", out)
	}
	// Summary line.
	if !strings.Contains(out, "3 bead(s)") {
		t.Errorf("missing summary:\n%s", out)
	}
}

func TestGraphTreeMultipleRoots(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "root A"})  // gc-1
	_, _ = store.Create(beads.Bead{Title: "root B"})  // gc-2
	_, _ = store.Create(beads.Bead{Title: "child A"}) // gc-3

	// gc-3 blocked by gc-1 only.
	_ = store.DepAdd("gc-3", "gc-1", "blocks")

	var stdout, stderr bytes.Buffer
	code := doGraph(store, []string{"gc-1", "gc-2", "gc-3"}, graphOpts{Tree: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doGraph tree = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()

	// Both gc-1 and gc-2 should be roots (no indent, no connector).
	if !strings.Contains(out, "○ gc-1: root A") {
		t.Errorf("gc-1 should be a root:\n%s", out)
	}
	if !strings.Contains(out, "○ gc-2: root B") {
		t.Errorf("gc-2 should be a root:\n%s", out)
	}
	// gc-3 should be child of gc-1.
	if !strings.Contains(out, "gc-3: child A") {
		t.Errorf("gc-3 should appear:\n%s", out)
	}
}

func TestGraphTreeInProgressIcon(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{Title: "working"}) // gc-1
	_ = store.Update(b.ID, beads.UpdateOpts{Status: strPtr("in_progress")})

	var stdout, stderr bytes.Buffer
	code := doGraph(store, []string{"gc-1"}, graphOpts{Tree: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doGraph tree = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "▶ gc-1: working") {
		t.Errorf("in_progress bead should show ▶:\n%s", stdout.String())
	}
}

func strPtr(s string) *string { return &s }

func TestGraphNonBlockingDepIgnored(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "task A"}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task B"}) // gc-2

	// "tracks" is non-blocking — gc-2 should still be ready.
	_ = store.DepAdd("gc-2", "gc-1", "tracks")

	var stdout, stderr bytes.Buffer
	code := doGraph(store, []string{"gc-1", "gc-2"}, graphOpts{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doGraph = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()

	// Both should be ready — "tracks" doesn't block.
	if !strings.Contains(out, "2 ready") {
		t.Errorf("non-blocking dep should not affect readiness:\n%s", out)
	}
	// No beads should show "blocked" in the READY column.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "gc-2") && strings.Contains(line, "blocked") {
			t.Errorf("gc-2 should not be blocked by non-blocking dep:\n%s", out)
		}
	}
}
