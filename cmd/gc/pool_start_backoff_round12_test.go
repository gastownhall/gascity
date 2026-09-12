package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// The cases codex round 12 found (evidence 07-codex-r12.md).

// TestKeptSessionRecoveryRetriesAfterRepeatedFailure: a kept session's
// resume whose clear fails is recovered next tick — and when THAT clear
// fails too, the tick after still recovers it: the uncommitted start stays
// durable as the bead's start-pending/creating state, put back after the
// heal moved it on, until a clear lands and the start confirms.
func TestKeptSessionRecoveryRetriesAfterRepeatedFailure(t *testing.T) {
	h := newStartBackoffHarness(t, 5)
	if starts := h.advance(15*time.Second, errPreStartFailure); starts != 2 {
		t.Fatalf("starts = %d, want 2\nstderr:\n%s", starts, h.env.stderr.String())
	}
	h.env.clk.Time = h.env.clk.Time.Add(time.Minute)
	h.env.cfg.Agents[0].MaxSessionAge = "1s"
	age := newMaxSessionAgeTracker()
	age.setConfig("sky-kept", time.Second, 0)
	open, unassigned := "open", ""
	if err := h.env.store.Update(h.work.ID, beads.UpdateOpts{Status: &open, Assignee: &unassigned}); err != nil {
		t.Fatal(err)
	}
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
	desired := map[string]TemplateParams{name: {Command: "test-cmd", SessionName: name, TemplateName: backoffHarnessTemplate}}
	h.env.desiredState = desired
	h.env.sp.StartErrors = nil
	realWriter, _ := beads.ConditionalWriterFor(h.env.store)
	refuse := func() {
		h.policy.resolveWriter = func(beads.Store) (beads.ConditionalWriter, error) {
			return round9FailingWriter{ConditionalWriter: realWriter, err: errors.New("work store: write timed out")}, nil
		}
	}
	refuse()
	h.env.startOptions = append(h.env.startOptions, withMaxSessionAgeTracker(age))
	h.env.reconcileWithPoolDesired([]beads.Bead{kept}, map[string]int{backoffHarnessTemplate: 1})
	if !h.env.sp.IsRunning(name) {
		t.Fatalf("the kept session must have resumed\nstderr:\n%s", h.env.stderr.String())
	}
	row, _ := h.env.store.Get(kept.ID)
	info := sessionInfosFromBeads([]beads.Bead{row})[0]
	for key, value := range map[string]string{"GC_SESSION_ID": kept.ID, "GC_INSTANCE_TOKEN": info.InstanceToken} {
		if err := h.env.sp.SetMeta(name, key, value); err != nil {
			t.Fatal(err)
		}
	}
	// A retained resume still carries the previous start's expired age anchor.
	oldAnchor := h.env.clk.Now().Add(-time.Hour).Format(time.RFC3339)
	if err := h.env.store.SetMetadataBatch(kept.ID, map[string]string{"creation_complete_at": oldAnchor}); err != nil {
		t.Fatal(err)
	}
	row, _ = h.env.store.Get(kept.ID)
	// Tick 2: the store is STILL down. Recovery runs and fails; the bead
	// must still say the start is uncommitted afterwards.
	h.installPolicy()
	refuse()
	h.env.startOptions = append(h.env.startOptions, withMaxSessionAgeTracker(age))
	h.env.desiredState = desired
	h.env.clk.Time = h.env.clk.Time.Add(time.Second)
	h.env.reconcileWithPoolDesired([]beads.Bead{row}, map[string]int{backoffHarnessTemplate: 1})
	if got := readWorkStartFailureState(h.reload().Metadata).Failures; got != 2 {
		t.Fatalf("tick 2: the refused clear must leave the record at 2, got %d", got)
	}
	row, _ = h.env.store.Get(kept.ID)
	info = sessionInfosFromBeads([]beads.Bead{row})[0]
	if !pendingCreateQueuedOrCreatingState(info.MetadataState) || info.CreationCompleteAt != oldAnchor || !h.env.sp.IsRunning(name) {
		t.Fatalf("tick 2: the uncommitted start must stay durable as the bead's state, got state=%q creation_complete=%q\nstderr:\n%s", info.MetadataState, info.CreationCompleteAt, h.env.stderr.String())
	}
	if !strings.Contains(h.env.stderr.String(), "metadata repair incomplete") {
		t.Fatalf("tick 2: the failed recovery is said:\n%s", h.env.stderr.String())
	}
	// Tick 3: the store is back. Recovery clears, then confirms.
	h.installPolicy()
	h.env.startOptions = append(h.env.startOptions, withMaxSessionAgeTracker(age))
	h.env.desiredState = desired
	h.env.clk.Time = h.env.clk.Time.Add(time.Second)
	h.env.reconcileWithPoolDesired([]beads.Bead{row}, map[string]int{backoffHarnessTemplate: 1})
	if got := h.reload(); len(workStartFailureClearPatch(got.Metadata)) != 0 {
		t.Fatalf("tick 3: recovery must clear the record: %v\nstderr:\n%s", got.Metadata, h.env.stderr.String())
	}
	row, _ = h.env.store.Get(kept.ID)
	info = sessionInfosFromBeads([]beads.Bead{row})[0]
	if strings.TrimSpace(info.CreationCompleteAt) == "" || session.State(strings.TrimSpace(info.MetadataState)) != session.StateActive {
		t.Fatalf("tick 3: recovery must confirm once the clear landed: state=%q creation_complete=%q\nstderr:\n%s", info.MetadataState, info.CreationCompleteAt, h.env.stderr.String())
	}
	if !h.env.sp.IsRunning(name) || len(h.mails) != 0 {
		t.Fatalf("same runtime, no park, no mail (running=%v mails=%d)", h.env.sp.IsRunning(name), len(h.mails))
	}
}

// TestMergeScaleCheckDemandDedupesTriggerIDs: two probes answering the same
// routed bead (a custom scale_check row probe and the cold default probe)
// yield ONE seat for it; the count still stands and the extra seat is
// triggerless.
func TestMergeScaleCheckDemandDedupesTriggerIDs(t *testing.T) {
	custom := scaleCheckDemand{WorkBeadIDs: []string{"gp-a"}, StoreRefs: map[string]string{"gp-a": "city"}}
	merged := mergeScaleCheckDemand(scaleCheckDemand{}, custom, 2)
	merged = mergeScaleCheckDemand(merged, scaleCheckDemand{WorkBeadIDs: []string{"gp-a", "gp-b"}, StoreRefs: map[string]string{"gp-a": "city", "gp-b": "city"}}, 2)
	if strings.Join(merged.WorkBeadIDs, ",") != "gp-a,gp-b" {
		t.Fatalf("one bead is one seat's trigger: %v", merged.WorkBeadIDs)
	}
	if again := mergeScaleCheckDemand(merged, custom, 2); strings.Join(again.WorkBeadIDs, ",") != "gp-a,gp-b" {
		t.Fatalf("re-merging the same probe adds nothing: %v", again.WorkBeadIDs)
	}
}

// TestStartIsNotMadeForABeadParkedSinceItWasPlanned: a session planned for
// a bead — a fresh seat under its pending-create claim, or a kept holder
// still carrying it — is not started once that bead is parked or backed
// off at start time: the provider is not called, the park stays, nothing
// is charged, and the outcome is said. The start-time gate is the
// planning gate re-proven on the row.
func TestStartIsNotMadeForABeadParkedSinceItWasPlanned(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]string
		want string
	}{
		{"parked", map[string]string{
			beadmeta.ParkedAtMetadataKey: "2026-09-12T02:00:00Z", beadmeta.ParkReasonMetadataKey: "boom", beadmeta.ParkFailuresMetadataKey: "5", beadmeta.ParkIDMetadataKey: "cafe", beadmeta.ParkMailedAtMetadataKey: "2026-09-12T02:00:00Z",
		}, "parked"},
		{"backed off", map[string]string{
			beadmeta.StartFailuresMetadataKey: "3", beadmeta.StartFailedAtMetadataKey: "2026-03-08T12:00:00Z",
		}, "start_backoff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newStartBackoffHarness(t, 5)
			meta := map[string]string{}
			for k, v := range tc.meta {
				meta[k] = v
			}
			if _, ok := meta[beadmeta.StartFailuresMetadataKey]; ok {
				meta[beadmeta.StartBackoffUntilMetadataKey] = h.env.clk.Now().Add(time.Minute).UTC().Format(time.RFC3339)
			}
			if err := h.env.store.SetMetadataBatch(h.work.ID, meta); err != nil {
				t.Fatal(err)
			}
			if kept := excludeStartDeferredWork([]beads.Bead{h.reload()}, h.env.clk.Now(), nil); len(kept) != 0 {
				t.Fatal("fixture: the planning gate defers this bead")
			}
			for _, shape := range []struct {
				name string
				meta map[string]string
			}{
				{"sky-seat", map[string]string{"state": "creating", "pending_create_claim": "true"}},
				{"sky-held", map[string]string{"state": string(session.StateStartPending)}},
			} {
				sessionMeta := map[string]string{
					"session_name":                          shape.name,
					"session_name_explicit":                 "true",
					"template":                              backoffHarnessTemplate,
					"generation":                            "1",
					"continuation_epoch":                    "1",
					"instance_token":                        "test-token",
					beadmeta.TriggerBeadIDMetadataKey:       h.work.ID,
					beadmeta.TriggerBeadStoreRefMetadataKey: "city",
				}
				for k, v := range shape.meta {
					sessionMeta[k] = v
				}
				sb, err := h.env.store.Create(beads.Bead{Title: backoffHarnessTemplate, Type: sessionBeadType, Labels: []string{sessionBeadLabel, "template:" + backoffHarnessTemplate}, Metadata: sessionMeta})
				if err != nil {
					t.Fatal(err)
				}
				h.env.desiredState = map[string]TemplateParams{shape.name: {Command: "test-cmd", SessionName: shape.name, TemplateName: backoffHarnessTemplate}}
				h.env.reconcileWithPoolDesired([]beads.Bead{sb}, map[string]int{backoffHarnessTemplate: 1})
				if h.env.sp.IsRunning(shape.name) {
					t.Fatalf("%s: the provider must not be asked to start for a %s bead\nstderr:\n%s", shape.name, tc.name, h.env.stderr.String())
				}
				if !strings.Contains(h.env.stderr.String(), "outcome=work_deferred") || !strings.Contains(h.env.stderr.String(), "is "+tc.want) {
					t.Fatalf("%s: the deferral is said with its reason %q:\n%s", shape.name, tc.want, h.env.stderr.String())
				}
				// Round 13: the abandoned start's durable state goes with its
				// lease — a kept holder queued for it is back to asleep (so the
				// pin lifts and the next build can bind it to other work); a
				// fresh seat keeps its claim and expires as never started.
				row, _ := h.env.store.Get(sb.ID)
				after := sessionInfosFromBeads([]beads.Bead{row})[0]
				switch shape.name {
				case "sky-held":
					if session.State(strings.TrimSpace(after.MetadataState)) != session.StateAsleep || startInFlightInfo(after) {
						t.Fatalf("%s: a deferred queued holder must be released to asleep (unpinned), got state=%q", shape.name, after.MetadataState)
					}
				case "sky-seat":
					if !after.PendingCreateClaim || !startInFlightInfo(after) {
						t.Fatalf("%s: a deferred fresh seat keeps its claim, got %+v", shape.name, after)
					}
				}
			}
			state := readWorkStartFailureState(h.reload().Metadata)
			if tc.name == "parked" && (!state.Parked() || state.ParkFailures != 5) {
				t.Fatalf("the park stays exactly as it was: %+v", state)
			}
			if tc.name == "backed off" && state.Failures != 3 {
				t.Fatalf("nothing is charged to a deferred bead: %+v", state)
			}
			if len(h.mails) != 0 {
				t.Fatalf("no mail: %d", len(h.mails))
			}
		})
	}
}

// TestWorkStartFailurePolicyStartDeferred: the start-time gate's answers —
// a nil policy, no trigger and a bead in no store never defer; a park, a
// live backoff and a record that cannot be read do; an elapsed backoff does
// not.
func TestWorkStartFailurePolicyStartDeferred(t *testing.T) {
	now := time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC)
	var none *workStartFailurePolicy
	if d, _ := none.startDeferred(workTrigger{BeadID: "gp-x"}, now); d {
		t.Fatal("nil policy: not deferred")
	}
	store := beads.NewMemStore()
	policy := &workStartFailurePolicy{workStore: store, limitFor: func(string) int { return 5 }}
	if d, _ := policy.startDeferred(workTrigger{}, now); d {
		t.Fatal("no trigger: not deferred")
	}
	if d, _ := policy.startDeferred(workTrigger{BeadID: "gp-nowhere", StoreRef: "city"}, now); d {
		t.Fatal("a bead in no store is not deferred")
	}
	parked, _ := store.Create(beads.Bead{Title: "parked", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "w", beadmeta.ParkedAtMetadataKey: now.Format(time.RFC3339)}})
	if d, reason := policy.startDeferred(workTrigger{BeadID: parked.ID, StoreRef: "city"}, now); !d || reason != "parked" {
		t.Fatalf("parked: deferred=%v reason=%q", d, reason)
	}
	backoff, _ := store.Create(beads.Bead{Title: "backoff", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "w", beadmeta.StartBackoffUntilMetadataKey: now.Add(10 * time.Second).Format(time.RFC3339)}})
	if d, _ := policy.startDeferred(workTrigger{BeadID: backoff.ID, StoreRef: "city"}, now); !d {
		t.Fatal("inside its backoff: deferred")
	}
	if d, _ := policy.startDeferred(workTrigger{BeadID: backoff.ID, StoreRef: "city"}, now.Add(10*time.Second)); d {
		t.Fatal("backoff elapsed: not deferred")
	}
	failing := &workStartFailurePolicy{workStore: round10ErrStore{Store: beads.NewMemStore(), err: errors.New("down")}, limitFor: func(string) int { return 5 }}
	if d, reason := failing.startDeferred(workTrigger{BeadID: parked.ID, StoreRef: "city"}, now); !d || !strings.Contains(reason, "unreadable") || !strings.Contains(reason, "down") {
		t.Fatalf("a store that fails to answer DEFERS (round 13): deferred=%v reason=%q", d, reason)
	}
}
