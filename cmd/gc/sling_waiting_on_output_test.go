package main

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// blockedSlingFixture is a city whose work bead BL-42 is blocked by the open
// bead BL-41, and opts that sling a graph.v2 formula onto BL-42.
func blockedSlingFixture(t *testing.T) (beads.Store, slingDeps, slingOpts) {
	t.Helper()
	formulaDir := t.TempDir()
	writeGraphV2FormulaForDryRunTest(t, formulaDir)
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	cfg.FormulaLayers.City = []string{formulaDir}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "BL-41", Title: "Blocker", Type: "task", Status: "open"},
		{ID: "BL-42", Title: "Work", Type: "task", Status: "open"},
	}, nil)
	if err := store.DepAdd("BL-42", "BL-41", "blocks"); err != nil {
		t.Fatal(err)
	}
	deps, _, _ := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps = slingDepsWithStore(deps, store)
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "", "frontend")
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "graph-work"
	opts.ScopeKind = "city"
	opts.ScopeRef = "test-city"
	return store, deps, opts
}

func countAllBeads(t *testing.T, store beads.Store) int {
	t.Helper()
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	return len(all)
}

func decodeSlingJSON(t *testing.T, out string) slingJSONResult {
	t.Helper()
	var got slingJSONResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decoding sling JSON %q: %v", out, err)
	}
	return got
}

func TestSlingOnBlockedBeadNamesWhatTheWorkflowWaitsFor(t *testing.T) {
	waitsLine := regexp.MustCompile(`(?m)^Workflow (\S+) waits for BL-41 \(open blockers of BL-42\); add a later blocker with: bd dep add <step> <blocker>$`)

	t.Run("text", func(t *testing.T) {
		store, deps, opts := blockedSlingFixture(t)
		var stdout, stderr bytes.Buffer
		if code := doSlingBatch(opts, deps, deps.Store, &stdout, &stderr); code != 0 {
			t.Fatalf("sling returned %d; stderr: %s", code, stderr.String())
		}
		m := waitsLine.FindStringSubmatch(stdout.String())
		if m == nil {
			t.Fatalf("stdout does not name the waiting blocker:\n%s", stdout.String())
		}
		root, err := store.Get(m[1])
		if err != nil {
			t.Fatalf("named workflow %s: %v", m[1], err)
		}
		if root.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindWorkflow {
			t.Fatalf("named bead %s is not a workflow root: %+v", m[1], root.Metadata)
		}
	})

	t.Run("json", func(t *testing.T) {
		_, deps, opts := blockedSlingFixture(t)
		var stdout, stderr bytes.Buffer
		if code := doSlingBatchWithJSON(opts, deps, deps.Store, depsSlingNudge(deps), true, io.Discard, &stdout, &stderr); code != 0 {
			t.Fatalf("sling returned %d; stderr: %s", code, stderr.String())
		}
		if got := decodeSlingJSON(t, stdout.String()); !slices.Equal(got.WaitingOn, []string{"BL-41"}) {
			t.Fatalf("waiting_on = %v, want [BL-41]; payload %s", got.WaitingOn, stdout.String())
		}
	})

	t.Run("dry run text", func(t *testing.T) {
		store, deps, opts := blockedSlingFixture(t)
		opts.DryRun = true
		before := countAllBeads(t, store)
		var stdout, stderr bytes.Buffer
		if code := doSlingBatch(opts, deps, deps.Store, &stdout, &stderr); code != 0 {
			t.Fatalf("dry run returned %d; stderr: %s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "  Would wait for: BL-41 (open blockers of BL-42)\n") {
			t.Fatalf("dry run does not name the waiting blocker:\n%s", stdout.String())
		}
		if after := countAllBeads(t, store); after != before {
			t.Fatalf("dry run created %d beads", after-before)
		}
	})

	t.Run("dry run json", func(t *testing.T) {
		store, deps, opts := blockedSlingFixture(t)
		opts.DryRun = true
		before := countAllBeads(t, store)
		var stdout, stderr bytes.Buffer
		if code := doSlingBatchWithJSON(opts, deps, deps.Store, depsSlingNudge(deps), true, io.Discard, &stdout, &stderr); code != 0 {
			t.Fatalf("dry run returned %d; stderr: %s", code, stderr.String())
		}
		if got := decodeSlingJSON(t, stdout.String()); !slices.Equal(got.WaitingOn, []string{"BL-41"}) {
			t.Fatalf("waiting_on = %v, want [BL-41]; payload %s", got.WaitingOn, stdout.String())
		}
		if after := countAllBeads(t, store); after != before {
			t.Fatalf("dry run created %d beads", after-before)
		}
	})

	t.Run("dry run on a split city reports the refusal", func(t *testing.T) {
		store, deps, opts := blockedSlingFixture(t)
		deps.GraphStore = beadstest.Graph(beads.NewMemStore())
		opts.DryRun = true
		var stdout, stderr bytes.Buffer
		if code := doSlingBatch(opts, deps, deps.Store, &stdout, &stderr); code != 1 {
			t.Fatalf("dry run returned %d, want 1 as the live launch refuses; stdout: %s", code, stdout.String())
		}
		if !strings.Contains(stderr.String(), "BL-42 is blocked by BL-41, which the workflow's store does not hold") {
			t.Fatalf("dry run does not report the refusal; stderr: %s", stderr.String())
		}
		if got := countAllBeads(t, store); got != 2 {
			t.Fatalf("dry run left %d beads in the work store, want 2", got)
		}
	})
}
