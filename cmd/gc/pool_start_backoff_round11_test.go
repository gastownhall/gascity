package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The cases codex round 11 found (evidence 07-codex-r11.md): the one operand
// — the trigger the session bead carries, pinned while a start is in flight
// — on the paths round 10 left uncovered: a KEPT session's resume, a named
// bind that did not land, and a pool seat re-pointed under recovery.

// TestKeptSessionResumeClearsTheRecordBeforeItConfirms: a retained pool
// session (no pending-create claim) resumes for work with a record;
// the clear is refused, so the resume is NOT confirmed — the bead stays
// creating with its runtime alive — and the next tick's recovery (the same
// path a fresh create takes) clears the record and then confirms. No
// controller death, no marker: the pre-heal state is the gate.
func TestKeptSessionResumeClearsTheRecordBeforeItConfirms(t *testing.T) {
	h := newStartBackoffHarness(t, 5)
	if starts := h.advance(15*time.Second, errPreStartFailure); starts != 2 {
		t.Fatalf("starts = %d, want 2\nstderr:\n%s", starts, h.env.stderr.String())
	}
	h.env.clk.Time = h.env.clk.Time.Add(time.Minute)
	// The kept session: retained (no pending-create claim), its trigger the
	// work bead, marked start-pending — the resume the build asks for.
	const name = "sky-kept"
	kept, err := h.env.store.Create(beads.Bead{
		Title:  backoffHarnessTemplate,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:" + backoffHarnessTemplate},
		Metadata: map[string]string{
			"session_name":                          name,
			"session_name_explicit":                 "true",
			"template":                              backoffHarnessTemplate,
			"state":                                 string(session.StateStartPending),
			"generation":                            "1",
			"continuation_epoch":                    "1",
			"instance_token":                        "kept-token",
			beadmeta.TriggerBeadIDMetadataKey:       h.work.ID,
			beadmeta.TriggerBeadStoreRefMetadataKey: "city",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.env.desiredState = map[string]TemplateParams{name: {Command: "test-cmd", SessionName: name, TemplateName: backoffHarnessTemplate}}
	h.env.sp.StartErrors = nil
	realWriter, _ := beads.ConditionalWriterFor(h.env.store)
	h.policy.resolveWriter = func(beads.Store) (beads.ConditionalWriter, error) {
		return round9FailingWriter{ConditionalWriter: realWriter, err: errors.New("work store: write timed out")}, nil
	}
	h.env.reconcileWithPoolDesired([]beads.Bead{kept}, map[string]int{backoffHarnessTemplate: 1})
	if !h.env.sp.IsRunning(name) {
		t.Fatalf("the kept session must have resumed\nstderr:\n%s", h.env.stderr.String())
	}
	if got := readWorkStartFailureState(h.reload().Metadata).Failures; got != 2 {
		t.Fatalf("the refused clear must leave the record at 2, got %d", got)
	}
	row, err := h.env.store.Get(kept.ID)
	if err != nil {
		t.Fatal(err)
	}
	info := sessionInfosFromBeads([]beads.Bead{row})[0]
	if strings.TrimSpace(info.CreationCompleteAt) != "" || session.State(strings.TrimSpace(info.MetadataState)) != session.StateCreating || info.PendingCreateClaim {
		t.Fatalf("a resume whose clear failed is not confirmed: state=%q creation_complete=%q claim=%v\nstderr:\n%s", info.MetadataState, info.CreationCompleteAt, info.PendingCreateClaim, h.env.stderr.String())
	}
	if !strings.Contains(h.env.stderr.String(), "work_record_clear_failed") {
		t.Fatalf("the refused clear is said:\n%s", h.env.stderr.String())
	}
	for key, value := range map[string]string{"GC_SESSION_ID": kept.ID, "GC_INSTANCE_TOKEN": info.InstanceToken} {
		if err := h.env.sp.SetMeta(name, key, value); err != nil {
			t.Fatal(err)
		}
	}
	// The next tick, a fresh policy (the store back): the live runtime whose
	// resume did not commit is recovered — clear, then confirm.
	h.installPolicy()
	h.env.desiredState = map[string]TemplateParams{name: {Command: "test-cmd", SessionName: name, TemplateName: backoffHarnessTemplate}}
	h.env.clk.Time = h.env.clk.Time.Add(time.Second)
	h.env.reconcileWithPoolDesired([]beads.Bead{row}, map[string]int{backoffHarnessTemplate: 1})
	if got := h.reload(); len(workStartFailureClearPatch(got.Metadata)) != 0 {
		t.Fatalf("recovery must clear the kept session's record: %v\nstderr:\n%s", got.Metadata, h.env.stderr.String())
	}
	row, _ = h.env.store.Get(kept.ID)
	info = sessionInfosFromBeads([]beads.Bead{row})[0]
	if strings.TrimSpace(info.CreationCompleteAt) == "" || session.State(strings.TrimSpace(info.MetadataState)) != session.StateActive {
		t.Fatalf("recovery must confirm the resume once the clear landed: state=%q creation_complete=%q\nstderr:\n%s", info.MetadataState, info.CreationCompleteAt, h.env.stderr.String())
	}
	if !h.env.sp.IsRunning(name) {
		t.Fatal("the runtime is the same one, still running")
	}
	if len(h.mails) != 0 {
		t.Fatalf("no park, no mail: %d", len(h.mails))
	}
}

// failMetadataOnStore fails every metadata batch on one bead id.
type failMetadataOnStore struct {
	beads.Store
	id  string
	err error
}

func (s *failMetadataOnStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if s.err != nil && id == s.id {
		return s.err
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *failMetadataOnStore) Update(id string, opts beads.UpdateOpts) error {
	if s.err != nil && id == s.id {
		return s.err
	}
	return s.Store.Update(id, opts)
}

// TestBuildDesiredState_NamedBindThatDidNotLandKeepsThePersistedTrigger:
// the holder carries A, the wake request is B, and persisting B fails. The
// start prepared this tick reads its trigger env off the bead (A), so the
// params say A too and the charge (workTriggerForStart, the persisted
// trigger) names A — one operand, never a start that runs for A and parks
// B. Once the store answers, the bind lands and everything says B.
func TestBuildDesiredState_NamedBindThatDidNotLandKeepsThePersistedTrigger(t *testing.T) {
	cityPath := t.TempDir()
	mem := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "solo",
			StartCommand:      "true",
			WorkQuery:         "printf ''",
			MaxActiveSessions: intPtr(1),
			MaxStartFailures:  intPtr(5),
		}},
		NamedSessions: []config.NamedSession{{Template: "solo", Mode: "always"}},
	}
	identity := cfg.NamedSessions[0].QualifiedName()
	inProgress := "in_progress"
	mkWork := func(title string) beads.Bead {
		w, err := mem.Create(beads.Bead{Title: title, Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "solo"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.Update(w.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &identity}); err != nil {
			t.Fatal(err)
		}
		return w
	}
	a := mkWork("work A: the holder's persisted trigger")
	holder, err := mem.Create(beads.Bead{
		Title:  "solo holder",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":                              "solo",
			"agent_name":                            identity,
			"alias":                                 identity,
			"session_name":                          "solo-holder",
			"state":                                 string(session.StateAsleep),
			session.NamedSessionMetadataKey:         "true",
			session.NamedSessionIdentityMetadata:    identity,
			session.NamedSessionModeMetadata:        "always",
			beadmeta.TriggerBeadIDMetadataKey:       a.ID,
			beadmeta.TriggerBeadStoreRefMetadataKey: "city",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Close(a.ID); err != nil {
		t.Fatal(err)
	}
	b := mkWork("work B: the wake request the store refuses to persist")
	store := &failMetadataOnStore{Store: mem, id: holder.ID, err: fmt.Errorf("session store: write timed out")}
	trigger := func() string {
		row, err := mem.Get(holder.ID)
		if err != nil {
			t.Fatal(err)
		}
		return row.Metadata[beadmeta.TriggerBeadIDMetadataKey]
	}
	desiredFor := func(res DesiredStateResult) TemplateParams {
		for _, tp := range res.State {
			if tp.ConfiguredNamedIdentity == identity {
				return tp
			}
		}
		t.Fatalf("no desired state for the always holder %s: %v", identity, mapKeys(res.State))
		return TemplateParams{}
	}
	var stderr bytes.Buffer
	res := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)
	if tp := desiredFor(res); tp.TriggerBeadID != a.ID {
		t.Fatalf("a bind that did not land leaves the params on the PERSISTED trigger %s, got %q (B = %s)\nstderr:\n%s", a.ID, tp.TriggerBeadID, b.ID, stderr.String())
	}
	if got := trigger(); got != a.ID {
		t.Fatalf("fixture: the bead still carries %s, got %q", a.ID, got)
	}
	if !strings.Contains(stderr.String(), "keeps trigger "+fmt.Sprintf("%q", a.ID)) {
		t.Fatalf("the unlanded bind is said with the trigger kept:\n%s", stderr.String())
	}
	// The charge a start prepared off this bead would make names A — the
	// same bead its trigger env names.
	row, _ := mem.Get(holder.ID)
	info := sessionInfosFromBeads([]beads.Bead{row})[0]
	if got := workTriggerForStart(info); got != (workTrigger{BeadID: a.ID, StoreRef: "city"}) {
		t.Fatalf("the start is charged to the persisted trigger, got %+v", got)
	}
	if env := sessionTriggerBeadEnv(info); env["GC_TRIGGER_BEAD_ID"] != a.ID {
		t.Fatalf("the trigger env names the same bead: %v", env)
	}
	// The store answers: the bind lands and the params follow.
	store.err = nil
	res = buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)
	if tp := desiredFor(res); tp.TriggerBeadID != b.ID {
		t.Fatalf("once the bind lands the params carry %s, got %q\nstderr:\n%s", b.ID, tp.TriggerBeadID, stderr.String())
	}
	if got := trigger(); got != b.ID {
		t.Fatalf("once the bind lands the bead carries %s, got %q", b.ID, got)
	}
}

// TestBindPoolSessionTriggerBeadPinsAnInFlightStart: a pool seat whose
// start is in flight (its pending-create claim set, or creating /
// start-pending) keeps the trigger that start ran for; a resume request
// for other work it claimed meanwhile does not move it (recovery would
// otherwise clear that other bead and leave the started bead's failures).
// A seat between starts is re-pointed as before.
func TestBindPoolSessionTriggerBeadPinsAnInFlightStart(t *testing.T) {
	store := beads.NewMemStore()
	seat, err := store.Create(beads.Bead{
		Title:  "seat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":                          "worker-1",
			"state":                                 string(session.StateCreating),
			"pending_create_claim":                  "true",
			beadmeta.TriggerBeadIDMetadataKey:       "gp-a",
			beadmeta.TriggerBeadStoreRefMetadataKey: "city",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgAgent := &config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	bp := &agentBuildParams{beadStore: store}
	request := SessionRequest{WorkBeadID: "gp-b", WorkStoreRef: "city"}
	for _, inFlight := range []map[string]string{
		{"state": string(session.StateCreating), "pending_create_claim": "true"},
		{"state": string(session.StateCreating), "pending_create_claim": ""},
		{"state": string(session.StateStartPending), "pending_create_claim": ""},
	} {
		if err := store.SetMetadataBatch(seat.ID, inFlight); err != nil {
			t.Fatal(err)
		}
		row, _ := store.Get(seat.ID)
		info := sessionInfosFromBeads([]beads.Bead{row})[0]
		bound, err := bindPoolSessionTriggerBead(bp, cfgAgent, "worker-1", info, request)
		if err != nil {
			t.Fatal(err)
		}
		if bound.TriggerBeadID != "gp-a" {
			t.Fatalf("%v: an in-flight start pins the trigger, got %q", inFlight, bound.TriggerBeadID)
		}
		row, _ = store.Get(seat.ID)
		if row.Metadata[beadmeta.TriggerBeadIDMetadataKey] != "gp-a" {
			t.Fatalf("%v: nothing written while the start is in flight: %v", inFlight, row.Metadata)
		}
	}
	// Committed: the seat follows the request.
	if err := store.SetMetadataBatch(seat.ID, map[string]string{"state": string(session.StateActive), "pending_create_claim": ""}); err != nil {
		t.Fatal(err)
	}
	row, _ := store.Get(seat.ID)
	bound, err := bindPoolSessionTriggerBead(bp, cfgAgent, "worker-1", sessionInfosFromBeads([]beads.Bead{row})[0], request)
	if err != nil || bound.TriggerBeadID != "gp-b" {
		t.Fatalf("between starts the seat is re-pointed: %q err=%v", bound.TriggerBeadID, err)
	}
	row, _ = store.Get(seat.ID)
	if row.Metadata[beadmeta.TriggerBeadIDMetadataKey] != "gp-b" {
		t.Fatalf("the re-point persists: %v", row.Metadata)
	}
}
