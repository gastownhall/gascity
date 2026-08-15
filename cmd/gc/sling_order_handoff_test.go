package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// T-B: the production order, end to end, with NO by-id claim tier.
//
// Tick 1 counts demand and realizes a seat; the seat's FIRST hook cycle
// discovers and claims through the ordinary generated-query path. The seat knows
// it was spawned from demand (GC_SPAWN_ORIGIN) and knows which row justified it
// (GC_TRIGGER_WORK_BEAD_ID) — and neither may influence what it claims. The pool
// is pull: the controller scales capacity, the worker chooses.

// TestDemandSpawnedSeatClaimsThroughItsOwnQuery is the happy path the agreement
// invariant is supposed to produce.
func TestDemandSpawnedSeatClaimsThroughItsOwnQuery(t *testing.T) {
	h := newHandoffFixture(t)
	work := h.seedRoutedWork(t, "routed graph step")

	result := h.runHook(t, h.workQueryServing(work))

	if result.Action != "work" || result.BeadID != work.ID {
		t.Fatalf("first hook result = %+v, want the routed row claimed", result)
	}
	if result.Assignee != h.sessionName {
		t.Fatalf("assignee = %q, want the session-name form %q", result.Assignee, h.sessionName)
	}
	if got := strings.Join(h.stepStarted, ","); got != work.ID {
		t.Fatalf("execution.step_started = %q, want exactly one for %s", got, work.ID)
	}
	claimed, err := h.store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-reading claimed work: %v", err)
	}
	if !strings.EqualFold(strings.TrimSpace(claimed.Status), "in_progress") || strings.TrimSpace(claimed.Assignee) != h.sessionName {
		t.Fatalf("work bead = status %q assignee %q, want in_progress owned by the seat", claimed.Status, claimed.Assignee)
	}

	// ga-i44k: the same seat's second cycle resolves its OWN claim rather than
	// competing for another row.
	second := h.runHook(t, h.workQueryServing(claimed))
	if second.Action != "work" || second.Reason != "existing_assignment" || second.BeadID != work.ID {
		t.Fatalf("second hook result = %+v, want action=work reason=existing_assignment", second)
	}
}

// THE anti-assignment control. The seat's trigger env names row X, but the query
// serves row Y — and the seat claims Y. If any code path ever re-reads the
// trigger id to decide what to claim, this row fails, which is exactly what the
// operator ruling forbids: "the controller should never be assuming which bead
// is picked up."
func TestDemandSpawnedSeatClaimsWhatItsQueryServesNotItsTrigger(t *testing.T) {
	h := newHandoffFixture(t)
	trigger := h.seedRoutedWork(t, "the row the controller counted")
	served := h.seedRoutedWork(t, "the row this seat's query served")
	h.triggerID = trigger.ID

	result := h.runHook(t, h.workQueryServing(served))

	if result.BeadID != served.ID {
		t.Fatalf("claimed %q, want the row the QUERY served (%q); the trigger id is demand bookkeeping, not an assignment",
			result.BeadID, served.ID)
	}
	untouched, err := h.store.Get(trigger.ID)
	if err != nil {
		t.Fatalf("re-reading the trigger row: %v", err)
	}
	if strings.TrimSpace(untouched.Assignee) != "" {
		t.Fatalf("the trigger row was claimed (assignee %q); nothing may claim by trigger id", untouched.Assignee)
	}
}

// The v1 incident scenario, re-pointed: the work query answers EMPTY on the
// seat's first cycle. With the failure lanes fixed this can only be a genuine
// empty (the sibling-race case), so the seat drains no_work, acks, and is
// reaped — correct pull — while the diagnostics classify what happened.
func TestDemandSpawnedSeatDrainsCleanlyOnAGenuineEmpty(t *testing.T) {
	h := newHandoffFixture(t)
	trigger := h.seedRoutedWork(t, "row a sibling already took")
	h.triggerID = trigger.ID
	// A sibling claimed it between the demand read and this seat's query.
	sibling := "gc__worker-2"
	inProgress := "in_progress"
	if err := h.store.Update(trigger.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &sibling}); err != nil {
		t.Fatalf("sibling claim: %v", err)
	}

	result := h.runHook(t, func() (string, error) { return "[]", nil })

	if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("result = %+v, want a clean no_work drain", result)
	}
	if !h.drainAcked {
		t.Fatal("a seat that lost the sibling race must acknowledge drain and be reaped")
	}
}

type handoffFixture struct {
	store       beads.Store
	cfg         *config.City
	sessionBead beads.Bead
	sessionName string
	triggerID   string
	stepStarted []string
	drainAcked  bool
	dir         string
}

// newHandoffFixture realizes a pool seat through the production binder, so the
// env the hook runs with is the env the controller actually produces.
func newHandoffFixture(t *testing.T) *handoffFixture {
	t.Helper()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
		}},
	}
	h := &handoffFixture{store: store, cfg: cfg, dir: t.TempDir()}

	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = &sessionBeadSnapshot{}
	desired := map[string]TemplateParams{}
	seedID := h.mustSeed(t, "demand evidence").ID
	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{
		Template: "worker",
		Requests: []SessionRequest{{Template: "worker", Tier: "new", WorkBeadID: seedID, WorkBeadTitle: "demand evidence"}},
	}, desired, &stderr)

	infos := bp.sessionBeads.OpenInfos()
	if len(infos) != 1 {
		t.Fatalf("realized session beads = %d, want 1; stderr=%q", len(infos), stderr.String())
	}
	sessionBead, err := store.Get(infos[0].ID)
	if err != nil {
		t.Fatalf("Get(session bead): %v", err)
	}
	h.sessionBead = sessionBead
	h.sessionName = strings.TrimSpace(sessionBead.Metadata["session_name"])
	h.triggerID = seedID

	// The controller marks a demand-spawned seat: presence only.
	for _, tp := range desired {
		if tp.Env["GC_SPAWN_ORIGIN"] != "demand" {
			t.Fatalf("realized seat env GC_SPAWN_ORIGIN = %q, want demand", tp.Env["GC_SPAWN_ORIGIN"])
		}
	}
	return h
}

func (h *handoffFixture) mustSeed(t *testing.T, title string) beads.Bead {
	t.Helper()
	created, err := h.store.Create(beads.Bead{
		Title:  title,
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:   "worker",
			beadmeta.RootBeadIDMetadataKey: "root-1",
			beadmeta.StepIDMetadataKey:     "step-1",
		},
	})
	if err != nil {
		t.Fatalf("seeding %q: %v", title, err)
	}
	return created
}

func (h *handoffFixture) seedRoutedWork(t *testing.T, title string) beads.Bead {
	t.Helper()
	return h.mustSeed(t, title)
}

// workQueryServing returns a runner answering with exactly the rows a real
// generated query would have served for this seat.
func (h *handoffFixture) workQueryServing(rows ...beads.Bead) func() (string, error) {
	return func() (string, error) {
		encoded, err := json.Marshal(rows)
		return string(encoded), err
	}
}

func (h *handoffFixture) env() []string {
	return []string{
		"GC_SESSION_ID=" + h.sessionBead.ID,
		"GC_SESSION_NAME=" + h.sessionName,
		"GC_TEMPLATE=worker",
		"GC_SPAWN_ORIGIN=demand",
		"GC_TRIGGER_WORK_BEAD_ID=" + h.triggerID,
	}
}

func (h *handoffFixture) runHook(t *testing.T, run func() (string, error)) hookClaimJSONResult {
	t.Helper()
	env := h.env()
	opts := hookClaimOptions{
		Assignee:           h.sessionName,
		IdentityCandidates: []string{h.sessionName, h.sessionBead.ID},
		RouteTargets:       []string{"worker"},
		Env:                env,
		DrainAck:           true,
		JSON:               true,
	}
	stores := []hookStore{{dir: h.dir, env: env}}
	ops := h.ops()

	var stdout, stderr bytes.Buffer
	claimHookWorkWithRunner("work-query", h.dir, env, stores, opts, ops,
		func(string, string, []string) (string, error) { return run() },
		func(string, error) {}, &stdout, &stderr)

	var result hookClaimJSONResult
	trimmed := strings.TrimSpace(stdout.String())
	if trimmed == "" {
		t.Fatalf("hook wrote no result; stderr=%s", stderr.String())
	}
	if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
		t.Fatalf("decoding %q: %v (stderr=%s)", trimmed, err, stderr.String())
	}
	return result
}

func (h *handoffFixture) ops() hookClaimOps {
	return hookClaimOps{
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
			return h.store.Get(beadID)
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			return hookClaimThroughStore(beadID, assignee,
				func() (beads.Bead, bool, error) {
					current, err := h.store.Get(beadID)
					if err != nil {
						return beads.Bead{}, false, err
					}
					if existing := strings.TrimSpace(current.Assignee); existing != "" && existing != assignee {
						return current, false, nil
					}
					status := "in_progress"
					if err := h.store.Update(beadID, beads.UpdateOpts{Assignee: &assignee, Status: &status}); err != nil {
						return beads.Bead{}, false, err
					}
					updated, err := h.store.Get(beadID)
					return updated, err == nil, err
				},
				h.store.Get)
		},
		StampWorkMeta: func(_ context.Context, _ string, _ []string, beadID, _ string, patch map[string]string) error {
			return h.store.Update(beadID, beads.UpdateOpts{Metadata: patch})
		},
		EmitExecutionStepStarted: func(step beads.Bead, _ string, _ []string, _ string) {
			h.stepStarted = append(h.stepStarted, step.ID)
		},
		EmitClaimRejected: func(string, string, string) {},
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return nil, nil
		},
		ResolveWorkBranch: func(string) string { return "" },
		PublishRunMap:     func(string, string, ...string) error { return nil },
		DrainAck:          func(io.Writer) error { h.drainAcked = true; return nil },
	}
}
