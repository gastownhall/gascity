package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The cases codex round 7 found missing or wrong (evidence 07-codex-r7.md).

// TestComputePoolDesiredStatesCarriesTheWorkStoreRef: the resume and the
// wake-known-identity requests name the store their bead was counted in, so
// the seat bound to the bead charges THAT copy — a migrated bead's active
// copy in its class store, never the retained copy answering to the same id
// in the work store. Control: unknown refs stay empty.
func TestComputePoolDesiredStatesCarriesTheWorkStoreRef(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "rig", intPtr(3), 0)}}
	work := []beads.Bead{
		workBead("w-resume", "rig/claude", "sess-1", "in_progress", 5),
		workBead("w-wake", "rig/claude", "rig/claude", "in_progress", 5),
	}
	refs := []string{"class:relocated", "rig"}
	sessions := sessionInfosFromBeads([]beads.Bead{sessionBead("sess-1", "open")})

	states := ComputePoolDesiredStatesWithDemandTraced(cfg, work, refs, sessions, nil, nil, nil)
	byBead := map[string]SessionRequest{}
	for _, st := range states {
		for _, r := range st.Requests {
			byBead[r.WorkBeadID] = r
		}
	}
	resume, ok := byBead["w-resume"]
	if !ok || resume.Tier != "resume" {
		t.Fatalf("no resume request for w-resume: %+v", byBead)
	}
	if resume.WorkStoreRef != "class:relocated" {
		t.Fatalf("resume request must carry the class store ref, got %q", resume.WorkStoreRef)
	}
	wake, ok := byBead["w-wake"]
	if !ok || wake.Tier != "wake-known-identity" {
		t.Fatalf("no wake-known-identity request for w-wake: %+v", byBead)
	}
	if wake.WorkStoreRef != "rig" {
		t.Fatalf("wake request must carry the rig store ref, got %q", wake.WorkStoreRef)
	}

	// Control: with no refs the requests carry none (the pre-fix shape).
	for _, st := range ComputePoolDesiredStatesWithDemandTraced(cfg, work, nil, sessions, nil, nil, nil) {
		for _, r := range st.Requests {
			if r.WorkStoreRef != "" {
				t.Fatalf("unknown refs must stay empty, got %q on %s", r.WorkStoreRef, r.WorkBeadID)
			}
		}
	}
}

// TestBuildDesiredState_PoolSeatRecordsTheStoreItsWorkWasCountedIn: the
// production builder, a rig-scoped pool and its in_progress work sitting in
// the RIG store under the bare pool identity (the incident shape, one store
// over): the seat planned for it carries the bead AND the rig store's ref.
func TestBuildDesiredState_PoolSeatRecordsTheStoreItsWorkWasCountedIn(t *testing.T) {
	cityPath := t.TempDir()
	cfg := warmCrossStoreCfg(t, cityPath)
	cfg.Agents[0].WorkQuery = "printf ''"
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{"gascity": rigStore}
	work, err := rigStore.Create(beads.Bead{Title: "rig work", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: warmWorkerTemplate}})
	if err != nil {
		t.Fatal(err)
	}
	inProgress, identity := "in_progress", warmWorkerTemplate
	if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &identity}); err != nil {
		t.Fatal(err)
	}
	snapshot := newSessionBeadSnapshot(nil)
	var stderr bytes.Buffer
	buildDesiredStateWithSessionBeads("gc", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), cityStore, rigStores, snapshot, nil, &stderr)
	var seat *session.Info
	for _, info := range snapshot.OpenInfos() {
		if info.TriggerBeadID == work.ID {
			info := info
			seat = &info
			break
		}
	}
	if seat == nil {
		t.Fatalf("no pool seat planned for the rig-store work bead %s (%d sessions); stderr:\n%s", work.ID, len(snapshot.OpenInfos()), stderr.String())
	}
	if seat.TriggerBeadStoreRef != "gascity" {
		t.Fatalf("the seat must record the store its work was counted in (the assigned-work snapshot's ref for the rig store), got %q", seat.TriggerBeadStoreRef)
	}
}

// TestBuildDesiredState_WorkAssignedToTheHolderItselfBindsItsTrigger: the
// awake pass wakes a retained named holder for work assigned to its session
// bead id or its runtime session name, not only to its configured identity
// (sessionAssigneeMatches); the trigger binding recognizes the same
// identities, so a start that fails for such work is charged to it.
func TestBuildDesiredState_WorkAssignedToTheHolderItselfBindsItsTrigger(t *testing.T) {
	for _, tc := range []struct {
		name     string
		assignee func(holder beads.Bead) string
	}{
		{"the holder's session bead id", func(h beads.Bead) string { return h.ID }},
		{"the holder's runtime session name", func(h beads.Bead) string { return h.Metadata["session_name"] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			store := beads.NewMemStore()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Agents: []config.Agent{{
					Name:              "solo",
					StartCommand:      "true",
					WorkQuery:         "printf ''",
					MaxActiveSessions: intPtr(1),
				}},
				NamedSessions: []config.NamedSession{{Template: "solo", Mode: "on_demand"}},
			}
			identity := cfg.NamedSessions[0].QualifiedName()
			holder, err := store.Create(beads.Bead{
				Title:  "solo holder",
				Type:   sessionBeadType,
				Status: "open",
				Labels: []string{sessionBeadLabel},
				Metadata: map[string]string{
					"template":                           "solo",
					"agent_name":                         identity,
					"alias":                              identity,
					"session_name":                       "solo-holder",
					"state":                              string(session.StateAsleep),
					session.NamedSessionMetadataKey:      "true",
					session.NamedSessionIdentityMetadata: identity,
					session.NamedSessionModeMetadata:     "on_demand",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			work, err := store.Create(beads.Bead{Title: "work for the holder", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "solo"}})
			if err != nil {
				t.Fatal(err)
			}
			inProgress, assignee := "in_progress", tc.assignee(holder)
			if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
				t.Fatal(err)
			}
			buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, io.Discard)
			got, err := store.Get(holder.ID)
			if err != nil {
				t.Fatal(err)
			}
			if trigger := got.Metadata[beadmeta.TriggerBeadIDMetadataKey]; trigger != work.ID {
				t.Fatalf("work assigned to %s (%q) must be bound as the holder's trigger, got %q (metadata %v)", tc.name, assignee, trigger, got.Metadata)
			}
		})
	}
}

// TestRetryUnmailedParksDoesNotWaitForTheMail: the tick hands the owed park
// mails to the background and returns at once; the mail still lands and the
// stamp is written when the send completes.
func TestRetryUnmailedParksDoesNotWaitForTheMail(t *testing.T) {
	h := newStartBackoffHarness(t, 1)
	h.mailErr = errors.New("messaging outage")
	h.advance(time.Minute, errPreStartFailure) // parks; the park's own mail fails
	if st := readWorkStartFailureState(h.reload().Metadata); !st.Parked() || !st.ParkMailedAt.IsZero() {
		t.Fatalf("fixture: want a parked, unmailed bead, got %+v", st)
	}
	h.mailErr = nil
	release := make(chan struct{})
	sent := make(chan parkedWorkNotice, 1)
	h.policy.notify = func(n parkedWorkNotice) error {
		<-release // the mayor's nudge wait, in effect
		sent <- n
		return nil
	}
	began := time.Now()
	h.policy.retryUnmailedParks([]beads.Bead{h.reload()}, []string{"city"})
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("retryUnmailedParks held the tick for %s while a send was blocked", took)
	}
	select {
	case n := <-sent:
		t.Fatalf("the mail was sent before the send was released: %+v", n)
	default:
	}
	if st := readWorkStartFailureState(h.reload().Metadata); !st.ParkMailedAt.IsZero() {
		t.Fatal("the park must not be stamped mailed before the mail landed")
	}
	close(release)
	h.policy.awaitParkMailRetries()
	select {
	case n := <-sent:
		if n.BeadID != h.work.ID {
			t.Fatalf("mail for %q, want %q", n.BeadID, h.work.ID)
		}
	default:
		t.Fatal("no mail after the send was released")
	}
	if st := readWorkStartFailureState(h.reload().Metadata); st.ParkMailedAt.IsZero() {
		t.Fatal("the landed retry must stamp gc.park_mailed_at")
	}
}

// TestAsyncCommitReReadFailureChargesOrdinaryFailuresOnly: the async commit
// whose session re-read fails (the row is gone) after the start itself
// failed still charges the captured work trigger — and charges nothing when
// the failure was a provider rate-limit screen, the way the ordinary commit
// path quarantines the session instead.
func TestAsyncCommitReReadFailureChargesOrdinaryFailuresOnly(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rateLimit    bool
		wantFailures int
	}{
		{"pre_start failure", false, 1},
		{"rate-limit screen", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newStartBackoffHarness(t, 5)
			result := startResult{
				prepared: preparedStart{
					candidate: startCandidate{
						info: session.Info{ID: "sess-vanished"},
						tp:   TemplateParams{TemplateName: backoffHarnessTemplate, SessionName: backoffHarnessTemplate + "-1"},
					},
					workStartFailure: h.policy,
					workTrigger:      workTrigger{BeadID: h.work.ID, StoreRef: "city"},
				},
				err:             errPreStartFailure,
				outcome:         TraceOutcomeProviderError,
				rateLimitScreen: tc.rateLimit,
				started:         time.Now(),
				finished:        time.Now(),
			}
			if commitAsyncStartResultWithContext(context.Background(), result, nil, h.env.store, clock.Real{}, events.Discard, 0, io.Discard, io.Discard, nil) {
				t.Fatal("a start whose session row cannot be re-read must not commit")
			}
			st := readWorkStartFailureState(h.reload().Metadata)
			if st.Failures != tc.wantFailures {
				t.Fatalf("start failures on the work bead = %d, want %d (%+v)", st.Failures, tc.wantFailures, st)
			}
		})
	}
}
