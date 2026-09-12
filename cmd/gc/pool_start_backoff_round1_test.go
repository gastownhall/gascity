package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The cases codex round 1 found missing or wrong (evidence 07-codex-r1.md).

// TestPoolStartBackoffUnparkThenReparkMailsOnceMore: the documented three-key
// unpark starts a clean count; N fresh failures park again and mail exactly
// once more.
func TestPoolStartBackoffUnparkThenReparkMailsOnceMore(t *testing.T) {
	h := newStartBackoffHarness(t, 2)
	h.advance(time.Minute, errPreStartFailure)
	if len(h.mails) != 1 || !readWorkStartFailureState(h.reload().Metadata).Parked() {
		t.Fatalf("first park: mails=%d", len(h.mails))
	}
	// The operator's hold-in-place unpark (bd --unset-metadata deletes; cmd/gc
	// reads an empty value the same).
	if err := h.env.store.SetMetadataBatch(h.work.ID, map[string]string{
		beadmeta.ParkReasonMetadataKey:   "",
		beadmeta.ParkedAtMetadataKey:     "",
		beadmeta.ParkFailuresMetadataKey: "",
	}); err != nil {
		t.Fatal(err)
	}
	h.env.clk.Time = h.env.clk.Time.Add(time.Minute)
	if !h.tick(errPreStartFailure) {
		t.Fatal("an unparked bead is demand again")
	}
	if state := readWorkStartFailureState(h.reload().Metadata); state.Parked() || state.Failures != 1 {
		t.Fatalf("one failure after the unpark is a backoff, not a re-park: %+v", state)
	}
	h.advance(time.Minute, errPreStartFailure)
	state := readWorkStartFailureState(h.reload().Metadata)
	if !state.Parked() || state.ParkFailures != 2 {
		t.Fatalf("second park: %+v", state)
	}
	if len(h.mails) != 2 {
		t.Fatalf("a re-park gets exactly one more mail, got %d in total", len(h.mails))
	}
}

// TestPoolStartBackoffControllerCancellationIsNotCharged: a start interrupted
// by the controller's own shutdown (context.Canceled) is rolled back and
// retried by the next controller; it is not a failure of the lane.
func TestPoolStartBackoffControllerCancellationIsNotCharged(t *testing.T) {
	h := newStartBackoffHarness(t, 1)
	canceled := fmt.Errorf("session %q startup: %w", "sky-1", context.Canceled)
	if !h.tick(canceled) {
		t.Fatal("start")
	}
	work := h.reload()
	for _, key := range beadmeta.WorkStartFailureMetadataKeys {
		if work.Metadata[key] != "" {
			t.Fatalf("a canceled start charged the bead: %s=%q", key, work.Metadata[key])
		}
	}
	if len(h.mails) != 0 {
		t.Fatalf("a canceled start parked and mailed (%d)", len(h.mails))
	}
	// ...but a real failure right after still counts and parks at limit 1.
	h.env.clk.Time = h.env.clk.Time.Add(time.Second)
	if !h.tick(errPreStartFailure) {
		t.Fatal("second start")
	}
	if !readWorkStartFailureState(h.reload().Metadata).Parked() || len(h.mails) != 1 {
		t.Fatal("a real failure after a canceled one must still park")
	}
}

// TestPoolStartBackoffReroutedBeadIsNotCharged: the async commit of a start
// that was planned for template A must not charge a bead that has since been
// re-dispatched to template B (a lost reset would hand B a parked bead
// carrying A's history).
func TestPoolStartBackoffReroutedBeadIsNotCharged(t *testing.T) {
	h := newStartBackoffHarness(t, 1)
	// Between the plan and the commit the operator re-routes the bead.
	if err := h.env.store.SetMetadataBatch(h.work.ID, map[string]string{beadmeta.RoutedToMetadataKey: "other"}); err != nil {
		t.Fatal(err)
	}
	h.policy.recordStartFailure(workTrigger{BeadID: h.work.ID, StoreRef: "city"}, backoffHarnessTemplate, errPreStartFailure, h.env.clk.Now())
	work := h.reload()
	for _, key := range beadmeta.WorkStartFailureMetadataKeys {
		if work.Metadata[key] != "" {
			t.Fatalf("a re-routed bead was charged: %s=%q", key, work.Metadata[key])
		}
	}
	if !strings.Contains(h.env.stderr.String(), "not charged: the bead is now routed to other") {
		t.Fatalf("the skip must be said:\n%s", h.env.stderr.String())
	}
}

// TestPoolStartBackoffFencedWriteYieldsToAConcurrentReset: the record write
// is fenced on the bead's revision. A reassign that lands between the commit's
// read and its write wins; the commit recomputes from the fresh row.
func TestPoolStartBackoffFencedWriteYieldsToAConcurrentReset(t *testing.T) {
	h := newStartBackoffHarness(t, 5)
	h.advance(15*time.Second, errPreStartFailure) // failures at 0s and 10s → count 2
	if got := readWorkStartFailureState(h.reload().Metadata).Failures; got != 2 {
		t.Fatalf("count = %d", got)
	}
	racing := &resetOnFirstGetStore{Store: h.env.store, id: h.work.ID}
	h.policy.workStore = racing
	h.policy.resolveWriter = probeConditionalWriter
	h.policy.recordStartFailure(workTrigger{BeadID: h.work.ID, StoreRef: "city"}, backoffHarnessTemplate, errPreStartFailure, h.env.clk.Now().Add(time.Minute))
	state := readWorkStartFailureState(h.reload().Metadata)
	if state.Failures != 1 {
		t.Fatalf("the failure must be charged against the RESET row (count 1), not the stale snapshot (count 3): %+v\nstderr:\n%s", state, h.env.stderr.String())
	}
	if racing.gets < 3 {
		t.Fatalf("the fenced write must have re-read after the mismatch: lookup + stale read + retry = 3 gets, got %d", racing.gets)
	}
}

// resetOnFirstGetStore hands the policy a stale row on its first Get — right
// after answering it clears the record (the concurrent reassign) — so the
// fenced write's first attempt must mismatch.
type resetOnFirstGetStore struct {
	beads.Store
	id   string
	gets int
}

func (s *resetOnFirstGetStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if err != nil || id != s.id {
		return b, err
	}
	s.gets++
	// The reset lands on the fenced write's OWN read (the policy's lookup is
	// the first Get): the row it reads is then stale, the CAS must mismatch,
	// and the retry must recompute from the reset row.
	if s.gets == 2 {
		if err := s.SetMetadataBatch(id, workStartFailureClearPatch(b.Metadata)); err != nil {
			return beads.Bead{}, err
		}
	}
	return b, nil
}

func (s *resetOnFirstGetStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	return beads.ConditionalWriterFor(s.Store)
}

// TestPoolStartBackoffExistingSessionResumeFailureIsCharged: a session the
// pool KEPT (not a pending create) that fails to resume for its routed work
// is the same failed start from the work bead's side.
func TestPoolStartBackoffExistingSessionResumeFailureIsCharged(t *testing.T) {
	h := newStartBackoffHarness(t, 1)
	h.env.addDesired("kept", backoffHarnessTemplate, false)
	session := h.env.createSessionBead("kept", backoffHarnessTemplate)
	h.env.markSessionCreating(&session) // a kept session the reconciler restarts (no pending_create_claim: not a fresh create)
	h.env.setSessionMetadata(&session, map[string]string{
		beadmeta.TriggerBeadIDMetadataKey:       h.work.ID,
		beadmeta.TriggerBeadStoreRefMetadataKey: "city",
	})
	h.env.sp.StartErrors = map[string]error{"kept": errPreStartFailure}
	h.env.reconcile([]beads.Bead{session})
	got, err := h.env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "closed" {
		t.Fatalf("a kept session is not rolled back: %s", h.env.stderr.String())
	}
	if got.Metadata["wake_attempts"] != "1" {
		t.Fatalf("the per-session damper still applies to the kept session (wake_attempts=%q): %s", got.Metadata["wake_attempts"], h.env.stderr.String())
	}
	state := readWorkStartFailureState(h.reload().Metadata)
	if !state.Parked() || len(h.mails) != 1 {
		t.Fatalf("the resume failure must be charged to the work bead (limit 1 → park + mail): %+v mails=%d\n%s", state, len(h.mails), h.env.stderr.String())
	}
}

// TestPoolStartBackoffFindsTheBeadInARelocatedStore: a work bead that lives
// in a relocated class-binding store (neither the work store nor a rig
// store) is still found by the by-id sweep, so the record lands on it.
func TestPoolStartBackoffFindsTheBeadInARelocatedStore(t *testing.T) {
	relocated := beads.NewMemStore()
	work, err := relocated.Create(beads.Bead{Title: "relocated work", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "helper"}})
	if err != nil {
		t.Fatal(err)
	}
	var mails []parkedWorkNotice
	policy := &workStartFailurePolicy{
		workStore:   beads.NewMemStore(),
		rigStores:   map[string]beads.Store{"rig-a": beads.NewMemStore()},
		extraStores: []beads.Store{relocated},
		limitFor:    func(string) int { return 1 },
		notify:      func(n parkedWorkNotice) error { mails = append(mails, n); return nil },
	}
	policy.recordStartFailure(workTrigger{BeadID: work.ID, StoreRef: "class:graph"}, "helper", errPreStartFailure, time.Now())
	got, err := relocated.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !readWorkStartFailureState(got.Metadata).Parked() || len(mails) != 1 {
		t.Fatalf("the relocated bead was not charged: %v mails=%d", got.Metadata, len(mails))
	}
	// A bead in no store at all is silently not charged (nothing to charge).
	policy.recordStartFailure(workTrigger{BeadID: "gp-nowhere", StoreRef: "city"}, "helper", errPreStartFailure, time.Now())
	if len(mails) != 1 {
		t.Fatal("an unknown trigger bead mailed")
	}
}

// TestPoolStartBackoffRetryReReadsBeforeSending: a retry that works from a
// stale snapshot (park unmailed) re-reads the row and stands down when the
// park's own send has stamped it meanwhile — never two mails for one park.
func TestPoolStartBackoffRetryReReadsBeforeSending(t *testing.T) {
	h := newStartBackoffHarness(t, 1)
	h.mailErr = errors.New("mail down")
	if !h.tick(errPreStartFailure) {
		t.Fatal("start")
	}
	stale := h.reload() // snapshot: parked, unmailed
	// The park's own send lands after the snapshot was taken.
	h.mailErr = nil
	h.policy.mailPark(h.env.store, h.work.ID, backoffHarnessTemplate, true)
	if len(h.mails) != 1 || readWorkStartFailureState(h.reload().Metadata).ParkMailedAt.IsZero() {
		t.Fatalf("the park send must land and stamp: mails=%d", len(h.mails))
	}
	// The tick now processes its stale row.
	h.policy.retry = nil // no throttle: the re-read alone must stop the send
	h.policy.retryUnmailedParks([]beads.Bead{stale}, []string{"city"})
	h.policy.awaitParkMailRetries()
	if len(h.mails) != 1 {
		t.Fatalf("a stale snapshot sent a second park mail: %d", len(h.mails))
	}
	// And a lifted park is not mailed either.
	if err := h.env.store.SetMetadataBatch(h.work.ID, map[string]string{beadmeta.ParkedAtMetadataKey: "", beadmeta.ParkMailedAtMetadataKey: ""}); err != nil {
		t.Fatal(err)
	}
	h.policy.retryUnmailedParks([]beads.Bead{stale}, []string{"city"})
	h.policy.awaitParkMailRetries()
	if len(h.mails) != 1 {
		t.Fatalf("a lifted park was mailed: %d", len(h.mails))
	}
}

// TestPoolStartBackoffSingleFlightParkMail: the park's async send and the
// tick's retry cannot both send for one bead.
func TestPoolStartBackoffSingleFlightParkMail(t *testing.T) {
	r := &parkMailRetryState{}
	if !r.begin("gp-1") {
		t.Fatal("first begin")
	}
	if r.begin("gp-1") {
		t.Fatal("second begin while in flight")
	}
	if !r.begin("gp-2") {
		t.Fatal("another bead is independent")
	}
	r.end("gp-1")
	if !r.begin("gp-1") {
		t.Fatal("begin after end")
	}
	var nilState *parkMailRetryState
	if !nilState.begin("x") {
		t.Fatal("a nil state never blocks")
	}
	nilState.end("x")
}

// TestDefaultScaleCheckCountsAndDemandReportsDeferred: the probe reports how
// many routed rows it held back, per template, so a custom count can be
// reduced by it.
func TestDefaultScaleCheckCountsAndDemandReportsDeferred(t *testing.T) {
	const template = "hello-world/polecat"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "polecat", Dir: "hello-world", MaxActiveSessions: intPtr(3)}},
	}
	now := time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC)
	store := beads.NewMemStore()
	for _, meta := range []map[string]string{
		{},
		{beadmeta.ParkedAtMetadataKey: now.Format(time.RFC3339)},
		{beadmeta.StartBackoffUntilMetadataKey: now.Add(time.Minute).Format(time.RFC3339)},
	} {
		meta[beadmeta.RoutedToMetadataKey] = template
		if _, err := store.Create(beads.Bead{Title: "w", Type: "task", Status: "open", Metadata: meta}); err != nil {
			t.Fatal(err)
		}
	}
	counts, demand, _, errs := defaultScaleCheckCountsAndDemand(cfg, now, []defaultScaleCheckTarget{{template: template, storeKey: "city", store: store}})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if counts[template] != 1 || demand[template].Deferred != 2 {
		t.Fatalf("count=%d deferred=%d, want 1 and 2", counts[template], demand[template].Deferred)
	}
}

// TestPoolStartBackoffProductionDemandPlansNoStartForParkedWork pins spec
// case (b) through the production demand builder: the same routed
// in_progress bead (the incident shape) makes buildDesiredState create a
// pool session while unparked, and nothing while parked — for the
// wake-known-identity tier and the scale_check tier alike.
func TestPoolStartBackoffProductionDemandPlansNoStartForParkedWork(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
	}
	now := time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC)
	// The gate reads the wall clock (round 16); pin it to this test's now.
	prevNow := workStartDeferralNow
	workStartDeferralNow = func() time.Time { return now }
	defer func() { workStartDeferralNow = prevNow }()
	build := func(store beads.Store) (*sessionBeadSnapshot, DesiredStateResult, string) {
		snapshot := newSessionBeadSnapshot(nil)
		var stderr bytes.Buffer
		result := buildDesiredStateWithSessionBeads("test-city", t.TempDir(), now, cfg, runtime.NewFake(), store, nil, snapshot, nil, &stderr)
		return snapshot, result, stderr.String()
	}
	for _, tc := range []struct {
		name   string
		status string
		claim  bool
	}{
		{"wake-known-identity tier (in_progress, assigned to the pool identity)", "in_progress", true},
		{"scale_check tier (open, unassigned)", "open", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// newWork builds a fresh store holding one routed work bead carrying
			// extra metadata (the record under test).
			newWork := func(extra map[string]string) (beads.Store, beads.Bead) {
				store := beads.NewMemStore()
				meta := map[string]string{beadmeta.RoutedToMetadataKey: "worker"}
				for k, v := range extra {
					meta[k] = v
				}
				work, err := store.Create(beads.Bead{Title: "routed work", Type: "task", Metadata: meta})
				if err != nil {
					t.Fatal(err)
				}
				if tc.claim {
					status, assignee := tc.status, "worker"
					if err := store.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
						t.Fatal(err)
					}
				}
				return store, work
			}
			store, work := newWork(nil)
			snapshot, _, stderr := build(store)
			if got := len(snapshot.OpenInfos()); got != 1 {
				t.Fatalf("unparked: %d pool sessions created, want 1; stderr:\n%s", got, stderr)
			}
			if got := snapshot.OpenInfos()[0].TriggerBeadID; got != work.ID {
				t.Fatalf("the session is for the work bead: trigger=%q", got)
			}
			// Parked (unmailed): no session, and the retry input names it.
			store, work = newWork(map[string]string{
				beadmeta.ParkedAtMetadataKey:     now.Add(-time.Minute).Format(time.RFC3339),
				beadmeta.ParkReasonMetadataKey:   "boom",
				beadmeta.ParkFailuresMetadataKey: "5",
			})
			snapshot, result, stderr := build(store)
			if got := len(snapshot.OpenInfos()); got != 0 {
				t.Fatalf("parked: %d pool sessions created, want 0; stderr:\n%s", got, stderr)
			}
			if !tc.claim {
				if len(result.ParkedUnmailedWorkBeads) != 1 || result.ParkedUnmailedWorkBeads[0].ID != work.ID || len(result.ParkedUnmailedWorkStoreRefs) != 1 {
					t.Fatalf("an open parked unmailed bead must reach the retry input: %+v refs=%v", result.ParkedUnmailedWorkBeads, result.ParkedUnmailedWorkStoreRefs)
				}
			}
			// Backed off: no session either.
			store, _ = newWork(map[string]string{
				beadmeta.StartFailuresMetadataKey:     "1",
				beadmeta.StartBackoffUntilMetadataKey: now.Add(10 * time.Second).Format(time.RFC3339),
			})
			snapshot, _, stderr = build(store)
			if got := len(snapshot.OpenInfos()); got != 0 {
				t.Fatalf("backed off: %d pool sessions created, want 0; stderr:\n%s", got, stderr)
			}
			// Backoff elapsed: a session again.
			store, _ = newWork(map[string]string{
				beadmeta.StartFailuresMetadataKey:     "1",
				beadmeta.StartBackoffUntilMetadataKey: now.Format(time.RFC3339),
			})
			snapshot, _, stderr = build(store)
			if got := len(snapshot.OpenInfos()); got != 1 {
				t.Fatalf("backoff elapsed: %d pool sessions created, want 1; stderr:\n%s", got, stderr)
			}
		})
	}
}

// TestPoolStartBackoffTerminalErrorOnKeptSessionIsCharged: the terminal
// provider arm charges the work bead for a kept session's resume too, not
// only for a fresh create's rollback.
func TestPoolStartBackoffTerminalErrorOnKeptSessionIsCharged(t *testing.T) {
	h := newStartBackoffHarness(t, 1)
	h.env.addDesired("kept", backoffHarnessTemplate, false)
	session := h.env.createSessionBead("kept", backoffHarnessTemplate)
	h.env.markSessionCreating(&session)
	h.env.setSessionMetadata(&session, map[string]string{
		beadmeta.TriggerBeadIDMetadataKey:       h.work.ID,
		beadmeta.TriggerBeadStoreRefMetadataKey: "city",
	})
	h.env.sp.StartErrors = map[string]error{"kept": errors.New("provider: model_not_found: claude-nope")}
	h.env.reconcile([]beads.Bead{session})
	if state := readWorkStartFailureState(h.reload().Metadata); !state.Parked() || len(h.mails) != 1 {
		t.Fatalf("terminal error on a kept session must be charged (limit 1 → park + mail): %+v mails=%d\n%s", state, len(h.mails), h.env.stderr.String())
	}
}

// TestPoolStartBackoffStaleSuccessDoesNotClearAnotherDispatchsPark: a
// success of a session started for template A must not clear the record of
// a bead since re-dispatched to (and parked by) template B.
func TestPoolStartBackoffStaleSuccessDoesNotClearAnotherDispatchsPark(t *testing.T) {
	h := newStartBackoffHarness(t, 1)
	h.advance(time.Second, errPreStartFailure) // parked by helper
	if !readWorkStartFailureState(h.reload().Metadata).Parked() {
		t.Fatal("park")
	}
	// Re-dispatched to another template meanwhile (the park is B's now).
	if err := h.env.store.SetMetadataBatch(h.work.ID, map[string]string{beadmeta.RoutedToMetadataKey: "other"}); err != nil {
		t.Fatal(err)
	}
	h.policy.recordStartSuccess(workTrigger{BeadID: h.work.ID, StoreRef: "city"}, backoffHarnessTemplate)
	if !readWorkStartFailureState(h.reload().Metadata).Parked() {
		t.Fatalf("a stale success cleared another dispatch's park:\n%s", h.env.stderr.String())
	}
	if !strings.Contains(h.env.stderr.String(), "record left alone: the bead is now routed to other") {
		t.Fatalf("the skip must be said:\n%s", h.env.stderr.String())
	}
}

// TestPoolStartBackoffLateFirstSendStandsDownAfterARetryLanded: the park's
// own (first) send re-reads too — if a tick's retry already delivered and
// stamped the park, the late first send does not send again.
func TestPoolStartBackoffLateFirstSendStandsDownAfterARetryLanded(t *testing.T) {
	h := newStartBackoffHarness(t, 1)
	h.mailErr = errors.New("mail down")
	if !h.tick(errPreStartFailure) {
		t.Fatal("start")
	}
	h.mailErr = nil
	h.policy.retryUnmailedParks([]beads.Bead{h.reload()}, []string{"city"}) // the tick's retry lands first
	h.policy.awaitParkMailRetries()
	if len(h.mails) != 1 {
		t.Fatalf("retry delivered %d", len(h.mails))
	}
	h.policy.mailPark(h.env.store, h.work.ID, backoffHarnessTemplate, true) // the late first send
	if len(h.mails) != 1 {
		t.Fatalf("the late first send mailed again: %d", len(h.mails))
	}
}

// TestPoolStartBackoffStampAcknowledgesOnlyItsOwnPark: a delivery for park
// P1 must not stamp a newer park P2 as mailed. The park changes WHILE P1's
// notify is in flight, so the stamp's park-identity fence is what decides.
func TestPoolStartBackoffStampAcknowledgesOnlyItsOwnPark(t *testing.T) {
	h := newStartBackoffHarness(t, 1)
	h.mailErr = errors.New("mail down")
	if !h.tick(errPreStartFailure) {
		t.Fatal("start")
	}
	p1 := readWorkStartFailureState(h.reload().Metadata)
	if p1.ParkID == "" {
		t.Fatal("a park carries an id")
	}
	// P1's delivery is in flight when the bead is unparked and re-parked (P2,
	// a fresh id): the notify hook performs the swap before returning.
	swapped := false
	h.policy.notify = func(n parkedWorkNotice) error {
		h.mails = append(h.mails, n)
		if !swapped {
			swapped = true
			if err := h.env.store.SetMetadataBatch(h.work.ID, map[string]string{beadmeta.ParkIDMetadataKey: "p2-fresh"}); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	h.policy.mailPark(h.env.store, h.work.ID, backoffHarnessTemplate, true)
	got := readWorkStartFailureState(h.reload().Metadata)
	if len(h.mails) != 1 || h.mails[0].ParkID != p1.ParkID {
		t.Fatalf("P1's delivery: mails=%+v", h.mails)
	}
	if !got.ParkMailedAt.IsZero() {
		t.Fatalf("P1's delivery must not stamp P2: %+v", got)
	}
	if !strings.Contains(h.env.stderr.String(), "park that has since changed; stamp skipped") {
		t.Fatalf("the skipped stamp must be said:\n%s", h.env.stderr.String())
	}
	// P2 is still owed its mail: the retry sends for P2 and stamps P2.
	h.policy.retry = nil
	h.policy.retryUnmailedParks([]beads.Bead{h.reload()}, []string{"city"})
	h.policy.awaitParkMailRetries()
	got = readWorkStartFailureState(h.reload().Metadata)
	if len(h.mails) != 2 || h.mails[1].ParkID != "p2-fresh" || got.ParkMailedAt.IsZero() {
		t.Fatalf("P2 must get its own mail and stamp: mails=%+v state=%+v", h.mails, got)
	}
}

// TestPoolStartBackoffLandedMailFoundAfterRestartIsNotResent: the mail
// landed but the stamp did not persist (a restart in between); the retry
// finds the mail by its park tag and stamps without a second send.
func TestPoolStartBackoffLandedMailFoundAfterRestartIsNotResent(t *testing.T) {
	h := newStartBackoffHarness(t, 1)
	if !h.tick(errPreStartFailure) {
		t.Fatal("start")
	}
	if len(h.mails) != 1 {
		t.Fatalf("mails=%d", len(h.mails))
	}
	// The stamp is lost (restart before it persisted).
	if err := h.env.store.SetMetadataBatch(h.work.ID, map[string]string{beadmeta.ParkMailedAtMetadataKey: ""}); err != nil {
		t.Fatal(err)
	}
	h.installPolicy() // the restart
	h.policy.lookup = func(n parkedWorkNotice) (bool, error) {
		for _, m := range h.mails {
			if m.Tag() == n.Tag() {
				return true, nil
			}
		}
		return false, nil
	}
	h.policy.retryUnmailedParks([]beads.Bead{h.reload()}, []string{"city"})
	h.policy.awaitParkMailRetries()
	if len(h.mails) != 1 {
		t.Fatalf("a landed mail was sent again after the restart: %d", len(h.mails))
	}
	if readWorkStartFailureState(h.reload().Metadata).ParkMailedAt.IsZero() {
		t.Fatal("the found mail must be stamped")
	}
}
