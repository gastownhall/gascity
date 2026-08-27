//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sessionWaitDependencyShadowJourneyWitnessTimeout = 10 * time.Second

type sessionWaitDependencyShadowJourneySessionList struct {
	Sessions []sessionWaitDependencyShadowJourneySessionItem `json:"sessions"`
}

type sessionWaitDependencyShadowJourneySessionItem struct {
	ID          string `json:"id"`
	Template    string `json:"template"`
	SessionName string `json:"session_name"`
	State       string `json:"state"`
	Closed      bool   `json:"closed"`
}

type sessionWaitDependencyShadowJourneyBead struct {
	ID string `json:"id"`
}

type sessionWaitDependencyShadowJourneyWaitInspect struct {
	Wait struct {
		ID     string `json:"id"`
		State  string `json:"state"`
		Status string `json:"status"`
	} `json:"wait"`
}

type sessionWaitDependencyShadowJourneyEventEmit struct {
	HasPayload bool `json:"has_payload"`
	Submitted  bool `json:"submitted"`
}

type sessionWaitDependencyShadowJourneyTraceShow struct {
	Records []sessionWaitDependencyShadowJourneyTraceRecord `json:"records"`
}

type sessionWaitDependencyShadowJourneyTraceRecord struct {
	Seq                  uint64 `json:"seq"`
	RecordID             string `json:"record_id"`
	ControllerInstanceID string `json:"controller_instance_id"`
	RecordType           string `json:"record_type"`
	SiteCode             string `json:"site_code"`
	OutcomeCode          string `json:"outcome_code"`
	SessionBeadID        string `json:"session_bead_id"`
	SessionName          string `json:"session_name"`
	Fields               struct {
		Cause                         string `json:"cause"`
		WaitOutcome                   string `json:"wait_outcome"`
		StartOutcome                  string `json:"start_outcome"`
		StartReason                   string `json:"start_reason"`
		WaitID                        string `json:"wait_id"`
		SessionID                     string `json:"session_id"`
		InstanceToken                 string `json:"instance_token"`
		Admission                     string `json:"admission"`
		StartLease                    string `json:"start_lease"`
		StatusOutcome                 string `json:"status_outcome"`
		StatusReason                  string `json:"status_reason"`
		EffectApplied                 *bool  `json:"effect_applied"`
		WorkID                        string `json:"work_id"`
		PoolTarget                    string `json:"pool_target"`
		SourceActor                   string `json:"source_actor"`
		SourceStore                   string `json:"source_store"`
		ContributionPresent           bool   `json:"contribution_present"`
		EventTimestampValid           bool   `json:"event_timestamp_valid"`
		EventToMaterializationNS      int64  `json:"event_to_materialization_ns"`
		EventToShadowDecisionNS       int64  `json:"event_to_shadow_decision_ns"`
		ObservationToShadowDecisionNS int64  `json:"observation_to_shadow_decision_ns"`
		AllocationAction              string `json:"allocation_action"`
		AllocationReason              string `json:"allocation_reason"`
		AllocationStartCount          int    `json:"allocation_start_count"`
		AllocationSupported           bool   `json:"allocation_supported"`
		EffectOwner                   string `json:"effect_owner"`
	} `json:"fields"`
}

type sessionWaitDependencyShadowJourneyTmuxSession struct {
	ID         string
	Name       string
	SocketPath string
}

func TestSessionWaitDependencyCloseStartsSleepingSessionThroughKeyedController(t *testing.T) {
	if usingSubprocess() {
		t.Skip("exact wait-dependency start journey requires tmux")
	}

	cityDir := setupReconcilerCityWithManagedDolt(t, `session_reconciler = "auto"

[[agent]]
name = "database"
start_command = "sleep 3600"
max_active_sessions = 1

[[agent]]
name = "cache"
start_command = "sleep 3600"
max_active_sessions = 1

[[agent]]
name = "worker"
start_command = "sleep 3600"
depends_on = ["database", "cache"]
`, `patrol_interval = "1h"
`, `conditional_writes = "auto"`)

	dependencyTmux := make(map[string]sessionWaitDependencyShadowJourneyTmuxSession, 2)
	for _, template := range []string{"database", "cache"} {
		dependencySessions, err := gc(cityDir, "session", "list", "--state", "all", "--template", template, "--json")
		if err != nil {
			t.Fatalf("list configured singleton %s: %v\n%s", template, err, dependencySessions)
		}
		var dependencyList sessionWaitDependencyShadowJourneySessionList
		if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(dependencySessions))), &dependencyList); err != nil {
			t.Fatalf("decode configured singleton %s: %v\n%s", template, err, dependencySessions)
		}
		var liveDependency sessionWaitDependencyShadowJourneySessionItem
		for _, candidate := range dependencyList.Sessions {
			if candidate.Template == template && !candidate.Closed && candidate.ID != "" && candidate.SessionName != "" {
				liveDependency = candidate
				break
			}
		}
		if liveDependency.ID == "" || liveDependency.SessionName == "" {
			t.Fatalf("configured singleton %s before wait = %+v, want one nonclosed named session", template, dependencyList.Sessions)
		}
		if err := sessionWaitDependencyShadowJourneyWaitForSessionState(
			t.Context(), cityDir, liveDependency.ID, "active", integrationGCCommandTimeout,
		); err != nil {
			t.Fatalf("configured singleton %s was not durably active before target wait: %v", template, err)
		}
		tmuxSession, _, err := sessionWaitDependencyShadowJourneyWaitForExactTmuxSession(
			t.Context(), cityDir, liveDependency.SessionName, time.Now(), integrationGCCommandTimeout,
		)
		if err != nil {
			t.Fatalf("configured singleton %s was not live before target wait: %v", template, err)
		}
		dependencyTmux[template] = tmuxSession
	}

	out, err := gc(cityDir, "session", "new", "worker", "--alias", "manual-waiter", "--no-attach", "--json")
	if err != nil {
		t.Fatalf("create manual waiting session: %v\n%s", err, out)
	}
	var created sessionLifecycleStatusShadowJourneyNew
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &created); err != nil {
		t.Fatalf("decode manual waiting session: %v\n%s", err, out)
	}
	if created.SessionID == "" || created.SessionName == "" {
		t.Fatalf("manual waiting session = %+v, want ID and name", created)
	}
	session := sessionWaitDependencyShadowJourneySessionItem{ID: created.SessionID, Template: "worker", SessionName: created.SessionName}
	if _, _, err := sessionWaitDependencyShadowJourneyWaitForExactTmuxSession(
		t.Context(), cityDir, session.SessionName, time.Now(), integrationGCCommandTimeout,
	); err != nil {
		t.Fatalf("manual waiting session was not live before wait: %v", err)
	}
	if err := sessionWaitDependencyShadowJourneyWaitForSessionState(
		t.Context(), cityDir, session.ID, "active", integrationGCCommandTimeout,
	); err != nil {
		t.Fatalf("manual waiting session was not durably active before wait: %v", err)
	}

	out, err = bdDolt(cityDir, "create", "keyed start dependency", "--json")
	if err != nil {
		t.Fatalf("create durable dependency: %v\n%s", err, out)
	}
	dependencyID := sessionWaitDependencyShadowJourneyBeadID(t, out)

	out, err = gcDolt(cityDir, "session", "wait", session.ID,
		"--on-beads", dependencyID,
		"--note", "keyed dependency closed",
		"--sleep")
	if err != nil {
		t.Fatalf("register exact durable wait: %v\n%s", err, out)
	}
	waitID := sessionWaitDependencyShadowJourneyWaitID(t, out)

	// The integration bd invocation does not run the production hook. Emit its
	// typed event through the same checkout-built gc binary.
	out, err = gcDolt(cityDir, "event", "emit", "bead.created",
		"--subject", waitID,
		"--bead-payload", waitID,
		"--actor", "bd-hook",
		"--json")
	if err != nil {
		t.Fatalf("emit durable wait creation: %v\n%s", err, out)
	}
	var waitCreated sessionWaitDependencyShadowJourneyEventEmit
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &waitCreated); err != nil {
		t.Fatalf("decode durable wait creation event: %v\n%s", err, out)
	}
	if !waitCreated.HasPayload || !waitCreated.Submitted {
		t.Fatalf("durable wait creation event = %+v, want typed payload submitted", waitCreated)
	}
	pendingWait, err := sessionWaitDependencyShadowJourneyInspectWait(cityDir, waitID)
	if err != nil {
		t.Fatalf("inspect pending wait %s: %v", waitID, err)
	}
	if pendingWait.Wait.ID != waitID || pendingWait.Wait.State != "pending" || pendingWait.Wait.Status != "open" {
		t.Fatalf("wait while dependency is open = %+v, want id=%q state=pending status=open", pendingWait.Wait, waitID)
	}
	// Put the fixture at this slice's public precondition: an open, asleep
	// session held by the pending dependency wait. The STOP path has its own
	// real-tmux journey; this test exercises only dependency-ready START.
	out, err = runCommand("", commandEnvForDir(cityDir, false), integrationGCCommandTimeout,
		"tmux", "-L", filepath.Base(cityDir), "kill-session", "-t", session.SessionName)
	if err != nil {
		t.Fatalf("stop waiting-session fixture runtime: %v\n%s", err, out)
	}
	out, err = bdDolt(cityDir, "update", session.ID,
		"--set-metadata", "state=asleep",
		"--set-metadata", "sleep_reason=wait-hold",
		"--set-metadata", "sleep_intent=wait-hold",
		"--set-metadata", "wait_hold=true")
	if err != nil {
		t.Fatalf("persist waiting-session fixture state: %v\n%s", err, out)
	}
	out, err = gcDolt(cityDir, "event", "emit", "bead.updated",
		"--subject", session.ID,
		"--bead-payload", session.ID,
		"--actor", "bd-hook",
		"--json")
	if err != nil {
		t.Fatalf("emit waiting-session update event: %v\n%s", err, out)
	}
	if out, err = gcDolt(cityDir, "trace", "start", "--template", "worker", "--for", "2m", "--level", "detail"); err != nil {
		t.Fatalf("arm dependency start trace: %v\n%s", err, out)
	}
	if err := sessionWaitDependencyShadowJourneyWaitForSessionState(
		t.Context(), cityDir, session.ID, "asleep", sessionWaitDependencyShadowJourneyWitnessTimeout,
	); err != nil {
		t.Fatalf("waiting-session fixture did not become observable: %v", err)
	}

	if err := sessionWaitDependencyShadowJourneyWaitForExactTmuxAbsence(
		t.Context(), cityDir, session.SessionName, sessionWaitDependencyShadowJourneyWitnessTimeout,
	); err != nil {
		t.Fatalf("waiting session runtime remained live: %v\n%s", err, sessionWaitDependencyShadowJourneyDiagnostics(cityDir, waitID, dependencyID))
	}

	out, err = bdDolt(cityDir, "close", dependencyID)
	if err != nil {
		t.Fatalf("close durable dependency: %v\n%s", err, out)
	}
	started := time.Now()
	out, err = gcDolt(cityDir, "event", "emit", "bead.closed",
		"--subject", dependencyID,
		"--bead-payload", dependencyID,
		"--actor", "bd-hook",
		"--json")
	if err != nil {
		t.Fatalf("emit durable dependency close: %v\n%s", err, out)
	}
	var emitted sessionWaitDependencyShadowJourneyEventEmit
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &emitted); err != nil {
		t.Fatalf("decode durable dependency close event: %v\n%s", err, out)
	}
	if !emitted.HasPayload || !emitted.Submitted {
		t.Fatalf("durable dependency close event = %+v, want typed payload submitted", emitted)
	}

	tmuxSession, liveLatency, err := sessionWaitDependencyShadowJourneyWaitForExactTmuxSession(
		t.Context(), cityDir, session.SessionName, started, sessionWaitDependencyShadowJourneyWitnessTimeout,
	)
	if err != nil {
		t.Fatalf("dependency-ready session did not start: %v\n%s", err, sessionWaitDependencyShadowJourneyDiagnostics(cityDir, waitID, dependencyID))
	}
	if err := sessionWaitDependencyShadowJourneyWaitForSessionState(
		t.Context(), cityDir, session.ID, "active", sessionWaitDependencyShadowJourneyWitnessTimeout,
	); err != nil {
		t.Fatalf("manual session %s did not become active: %v", session.ID, err)
	}
	for _, template := range []string{"database", "cache"} {
		live, _, err := sessionWaitDependencyShadowJourneyWaitForExactTmuxSession(
			t.Context(), cityDir, dependencyTmux[template].Name, time.Now(), sessionWaitDependencyShadowJourneyWitnessTimeout,
		)
		if err != nil {
			t.Fatalf("configured singleton %s was not live after the dependency-ready start: %v", template, err)
		}
		if live != dependencyTmux[template] {
			t.Fatalf("configured singleton %s changed during the dependency-ready start: got %+v, want %+v", template, live, dependencyTmux[template])
		}
	}
	out, err = bdDolt(cityDir, "show", session.ID, "--json")
	if err != nil {
		t.Fatalf("show manual session after the dependency-ready start: %v\n%s", err, out)
	}
	var manualSessions []sessionLifecycleStatusShadowJourneyBead
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &manualSessions); err != nil {
		t.Fatalf("decode manual session after the dependency-ready start: %v\n%s", err, out)
	}
	if len(manualSessions) != 1 {
		t.Fatalf("manual session lookup after the dependency-ready start returned %d rows, want 1: %s", len(manualSessions), out)
	}
	manualSession := manualSessions[0]
	if manualSession.Metadata["session_origin"] != "manual" {
		t.Fatalf("manual session after the dependency-ready start metadata = %+v, want preserved session_origin=manual", manualSession.Metadata)
	}
	// The two assertions that the KEYED controller owned this start. Both halves
	// of ga-zo9h3 have landed: the ruled wait-advance stand-down (legacy's
	// boundary consults the session-level wait-dependency claim on current
	// state), and option (b) -- reservation plus certification hoisted onto the
	// bead.closed admission itself. The instrumented run's targets=0 was the
	// CACHED index answering for a wait registered after its census; the
	// admission now resolves the closed bead's waiting dependents from the
	// durable rows, so the certificate exists before the poke ever runs a tick.
	// The patrol redrive stays as the anti-entropy backstop.
	t.Run("keyed_wait_dependency_commit", func(t *testing.T) {
		commit, commitLatency, err := sessionWaitDependencyShadowJourneyWaitForDependencyStartCommit(
			t.Context(), cityDir, session, started, sessionWaitDependencyShadowJourneyWitnessTimeout,
		)
		if err != nil {
			t.Fatalf("dependency-ready keyed start did not commit: %v\n%s", err, sessionWaitDependencyShadowJourneyDiagnostics(cityDir, waitID, dependencyID))
		}
		if commit.Fields.StartLease != "wait_dependency" || commit.Fields.EffectApplied == nil || !*commit.Fields.EffectApplied {
			t.Fatalf("dependency start commit = %+v, want an applied start authorized by the wait_dependency lease", commit)
		}
		durableWait, err := sessionWaitDependencyShadowJourneyInspectWait(cityDir, waitID)
		if err != nil {
			t.Fatalf("inspect durable wait after shadow witness: %v", err)
		}
		if durableWait.Wait.ID != waitID || durableWait.Wait.State != "ready" || durableWait.Wait.Status != "open" {
			t.Fatalf("durable wait after keyed start = %+v, want id=%q state=ready status=open", durableWait.Wait, waitID)
		}
		t.Logf("keyed wait_dependency commit landed in %s", commitLatency)
	})

	t.Logf("dependency close started manual %s through reconciliation in %s (%s|%s|%s)", session.ID, liveLatency, tmuxSession.ID, tmuxSession.Name, tmuxSession.SocketPath)
}

// TestReadyRoutedWorkKeyedMaterializesLiveEphemeralSessionBeforeDebounce proves
// that a schema-59-style routed-work update with no dependency field creates
// and starts one cold ephemeral session through the keyed controller without
// waiting for the fleet-global legacy builder or its debounce.
func TestReadyRoutedWorkKeyedMaterializesLiveEphemeralSessionBeforeDebounce(t *testing.T) {
	for _, test := range []struct {
		name              string
		maxActiveSessions int
	}{
		{name: "unlimited", maxActiveSessions: -1},
		{name: "sole-bounded-member", maxActiveSessions: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			testReadyRoutedWorkKeyedMaterializesLiveEphemeralSessionBeforeDebounce(t, test.maxActiveSessions)
		})
	}
}

func testReadyRoutedWorkKeyedMaterializesLiveEphemeralSessionBeforeDebounce(t *testing.T, maxActiveSessions int) {
	if usingSubprocess() {
		t.Skip("exact ready routed-work journey requires tmux")
	}

	cityDir := setupReconcilerCityWithManagedDolt(t, fmt.Sprintf(`session_reconciler = "auto"

[[agent]]
name = "worker"
start_command = "sleep 3600"
min_active_sessions = 0
max_active_sessions = %d
`, maxActiveSessions), `patrol_interval = "1h"
`, `conditional_writes = "auto"`)
	schemaStatus, err := bdDolt(cityDir, "migrate", "schema", "--json")
	if err != nil {
		t.Fatalf("read bd schema status: %v\n%s", err, schemaStatus)
	}
	if !strings.Contains(schemaStatus, "v59") {
		t.Fatalf("bd schema status = %q, want v59", schemaStatus)
	}
	if out, err := gcDolt("", "stop", cityDir); err != nil {
		t.Fatalf("stop empty city before priming routed work: %v\n%s", err, out)
	}
	if err := sessionWaitDependencyShadowJourneyWaitForControllerStop(t.Context(), cityDir, sessionWaitDependencyShadowJourneyWitnessTimeout); err != nil {
		t.Fatalf("wait for empty city controller to stop: %v", err)
	}

	out, err := bdDolt(cityDir, "create", "ready routed-work priority journey", "--json")
	if err != nil {
		t.Fatalf("create ready routed work while city is stopped: %v\n%s", err, out)
	}
	workID := sessionWaitDependencyShadowJourneyBeadID(t, out)
	if out, err = gcDolt("", "start", cityDir); err != nil {
		t.Fatalf("restart city with unrouted work in its initial cache prime: %v\n%s", err, out)
	}

	initial, err := sessionWaitDependencyShadowJourneyListSessions(cityDir)
	if err != nil {
		t.Fatalf("list initial worker sessions: %v", err)
	}
	for _, session := range initial.Sessions {
		if session.Template == "worker" && !session.Closed {
			t.Fatalf("initial worker session = %+v, want no unclosed worker session", session)
		}
	}

	out, err = bdDolt(cityDir, "update", workID, "--set-metadata", "gc.routed_to=worker")
	if err != nil {
		t.Fatalf("route ready work to worker: %v\n%s", err, out)
	}
	if out, err = gcDolt(cityDir, "trace", "start", "--template", "worker", "--for", "2m", "--level", "detail"); err != nil {
		t.Fatalf("arm worker detail trace: %v\n%s", err, out)
	}

	started := time.Now()
	out, err = gcDolt(cityDir, "event", "emit", "bead.updated",
		"--subject", workID,
		"--bead-payload", workID,
		"--actor", "bd-hook",
		"--json")
	if err != nil {
		t.Fatalf("emit routed-work update: %v\n%s", err, out)
	}
	var emitted sessionWaitDependencyShadowJourneyEventEmit
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &emitted); err != nil {
		t.Fatalf("decode routed-work update event: %v\n%s", err, out)
	}
	if !emitted.HasPayload || !emitted.Submitted {
		t.Fatalf("routed-work update event = %+v, want typed payload submitted", emitted)
	}
	if err := sessionWaitDependencyShadowJourneyRequireOmittedDependenciesEvent(cityDir, workID); err != nil {
		t.Fatalf("routed-work update did not retain schema-59 omitted-dependencies shape: %v", err)
	}

	session, projectionLatency, err := sessionWaitDependencyShadowJourneyWaitForWorkerSession(
		t.Context(),
		cityDir,
		started,
		sessionWaitDependencyShadowJourneyWitnessTimeout,
	)
	if err != nil {
		t.Fatalf(
			"ready routed-work session did not materialize before the ten-minute debounce: %v\n%s",
			err,
			sessionWaitDependencyShadowJourneyDiagnostics(cityDir, workID, workID),
		)
	}
	tmuxSession, liveLatency, err := sessionWaitDependencyShadowJourneyWaitForExactTmuxSession(
		t.Context(),
		cityDir,
		session.SessionName,
		started,
		sessionWaitDependencyShadowJourneyWitnessTimeout,
	)
	if err != nil {
		t.Fatalf("keyed session %s did not reach exact live tmux: %v\n%s", session.ID, err, sessionWaitDependencyShadowJourneyDiagnostics(cityDir, workID, workID))
	}
	current, err := sessionWaitDependencyShadowJourneyListSessions(cityDir)
	if err != nil {
		t.Fatalf("list materialized worker sessions: %v", err)
	}
	liveWorkerSessions := 0
	for _, candidate := range current.Sessions {
		if candidate.Template == "worker" && !candidate.Closed {
			liveWorkerSessions++
		}
	}
	if liveWorkerSessions != 1 {
		t.Fatalf("unclosed worker sessions = %d, want exactly 1: %+v", liveWorkerSessions, current.Sessions)
	}
	// WD.10b landed the allocation-ownership seam, so the KEYED allocation is the
	// winner of the materialization on both arms: the reservation opens the moment
	// the exact (workID, poolTarget, sourceStore) key enters the keyed lane, and
	// legacy's member-creation boundary stands down for it on CURRENT state. The
	// count is ARMED here rather than left at the old first-creator-wins zero --
	// un-skipping without arming would prove nothing (council F10), because the
	// post-resume assertion below compares against this variable.
	wantPoolMaterializations := 1
	t.Run("keyed_materialization", func(t *testing.T) {
		trace, commitLatency, err := sessionWaitDependencyShadowJourneyWaitForPoolStartCommit(
			t.Context(),
			cityDir,
			workID,
			session,
			started,
			sessionWaitDependencyShadowJourneyWitnessTimeout,
		)
		if err != nil {
			t.Fatalf("wait for routed-work pool start proof: %v\n%s", err, sessionWaitDependencyShadowJourneyDiagnostics(cityDir, workID, workID))
		}
		materializationRecords := sessionWaitDependencyShadowJourneyPoolMaterializationRecords(trace, workID)
		if len(materializationRecords) != 1 {
			t.Fatalf("routed-work pool materialization records = %d, want 1: %+v\n%s", len(materializationRecords), materializationRecords, sessionWaitDependencyShadowJourneyDiagnostics(cityDir, workID, workID))
		}
		materialized := materializationRecords[0]
		if materialized.Seq == 0 || materialized.RecordID == "" ||
			materialized.RecordType != "operation" ||
			materialized.OutcomeCode != "applied" ||
			materialized.Fields.WorkID != workID ||
			materialized.Fields.PoolTarget != "worker" ||
			materialized.Fields.SessionID != session.ID ||
			materialized.Fields.EffectOwner != "keyed" ||
			materialized.Fields.EffectApplied == nil || !*materialized.Fields.EffectApplied ||
			!materialized.Fields.EventTimestampValid ||
			materialized.Fields.EventToMaterializationNS <= 0 {
			t.Fatalf("routed-work pool materialization record = %+v, want one applied keyed effect", materialized)
		}
		commitRecords := sessionWaitDependencyShadowJourneyPoolStartCommitRecords(trace, session)
		if len(commitRecords) != 1 {
			t.Fatalf("routed-work pool start commit records = %d, want 1: %+v", len(commitRecords), commitRecords)
		}
		commit := commitRecords[0]
		if commit.Seq == 0 || commit.RecordID == "" ||
			commit.RecordType != "operation" ||
			commit.OutcomeCode != "success" ||
			commit.Fields.Admission != "in_process" ||
			commit.Fields.SessionID != session.ID ||
			commit.Fields.InstanceToken == "" ||
			commit.Fields.EffectApplied == nil || !*commit.Fields.EffectApplied {
			t.Fatalf("routed-work pool start commit record = %+v, want one applied in-process exact start", commit)
		}
		t.Logf("keyed materialization %s committed the exact start in %s", time.Duration(materialized.Fields.EventToMaterializationNS), commitLatency)
	})
	materializedBead := sessionWaitDependencyShadowJourneyReadBead(t, cityDir, session.ID)
	if materializedBead.Metadata["pool_managed"] != "true" ||
		materializedBead.Metadata["session_origin"] != "ephemeral" ||
		materializedBead.Metadata["pool_slot"] != "1" ||
		materializedBead.Metadata["agent_name"] != "worker-1" ||
		materializedBead.Metadata["session_name"] != session.SessionName ||
		materializedBead.Metadata["gc.trigger_bead_id"] != workID {
		t.Fatalf("materialized pool metadata = %+v, want strict-default slot 1 bound to work %s", materializedBead.Metadata, workID)
	}

	out, err = bdDolt(cityDir, "create", "pool wait dependency", "--json")
	if err != nil {
		t.Fatalf("create pool wait dependency: %v\n%s", err, out)
	}
	dependencyID := sessionWaitDependencyShadowJourneyBeadID(t, out)
	out, err = gcDolt(cityDir, "session", "wait", session.ID,
		"--on-beads", dependencyID,
		"--note", "resume exact routed worker",
		"--sleep")
	if err != nil {
		t.Fatalf("register pool member wait: %v\n%s", err, out)
	}
	waitID := sessionWaitDependencyShadowJourneyWaitID(t, out)
	out, err = gcDolt(cityDir, "event", "emit", "bead.created",
		"--subject", waitID,
		"--bead-payload", waitID,
		"--actor", "bd-hook",
		"--json")
	if err != nil {
		t.Fatalf("emit pool member wait creation: %v\n%s", err, out)
	}
	pendingWait, err := sessionWaitDependencyShadowJourneyInspectWait(cityDir, waitID)
	if err != nil {
		t.Fatalf("inspect pool member wait: %v", err)
	}
	if pendingWait.Wait.ID != waitID || pendingWait.Wait.State != "pending" || pendingWait.Wait.Status != "open" {
		t.Fatalf("pool member wait before dependency close = %+v, want id=%q state=pending status=open", pendingWait.Wait, waitID)
	}

	out, err = runCommand("", commandEnvForDir(cityDir, false), integrationGCCommandTimeout,
		"tmux", "-L", filepath.Base(cityDir), "kill-session", "-t", session.SessionName)
	if err != nil {
		t.Fatalf("stop waiting pool member runtime: %v\n%s", err, out)
	}
	out, err = bdDolt(cityDir, "update", session.ID,
		"--set-metadata", "state=asleep",
		"--set-metadata", "sleep_reason=wait-hold",
		"--set-metadata", "sleep_intent=wait-hold",
		"--set-metadata", "wait_hold=true")
	if err != nil {
		t.Fatalf("persist waiting pool member state: %v\n%s", err, out)
	}
	out, err = gcDolt(cityDir, "event", "emit", "bead.updated",
		"--subject", session.ID,
		"--bead-payload", session.ID,
		"--actor", "bd-hook",
		"--json")
	if err != nil {
		t.Fatalf("emit waiting pool member update event: %v\n%s", err, out)
	}
	if err := sessionWaitDependencyShadowJourneyWaitForSessionState(
		t.Context(), cityDir, session.ID, "asleep", sessionWaitDependencyShadowJourneyWitnessTimeout,
	); err != nil {
		t.Fatalf("pool member did not become durably asleep: %v\n%s", err,
			sessionWaitDependencyShadowJourneyDiagnostics(cityDir, session.ID, workID))
	}
	if err := sessionWaitDependencyShadowJourneyWaitForExactTmuxAbsence(
		t.Context(), cityDir, session.SessionName, sessionWaitDependencyShadowJourneyWitnessTimeout,
	); err != nil {
		t.Fatalf("waiting pool member runtime remained live: %v\n%s", err, sessionWaitDependencyShadowJourneyDiagnostics(cityDir, waitID, dependencyID))
	}

	out, err = bdDolt(cityDir, "close", dependencyID)
	if err != nil {
		t.Fatalf("close pool wait dependency: %v\n%s", err, out)
	}
	resumedAt := time.Now()
	out, err = gcDolt(cityDir, "event", "emit", "bead.closed",
		"--subject", dependencyID,
		"--bead-payload", dependencyID,
		"--actor", "bd-hook",
		"--json")
	if err != nil {
		t.Fatalf("emit pool wait dependency close: %v\n%s", err, out)
	}
	resumedTmux, resumeLatency, err := sessionWaitDependencyShadowJourneyWaitForExactTmuxSession(
		t.Context(), cityDir, session.SessionName, resumedAt, sessionWaitDependencyShadowJourneyWitnessTimeout,
	)
	if err != nil {
		t.Fatalf("pool member did not resume before the ten-minute debounce: %v\n%s", err, sessionWaitDependencyShadowJourneyDiagnostics(cityDir, waitID, dependencyID))
	}
	resumeCommit, resumeCommitLatency, err := sessionWaitDependencyShadowJourneyWaitForDependencyStartCommit(
		t.Context(), cityDir, session, resumedAt, sessionWaitDependencyShadowJourneyWitnessTimeout,
	)
	if err != nil {
		t.Fatalf("pool member keyed resume did not commit: %v\n%s", err, sessionWaitDependencyShadowJourneyDiagnostics(cityDir, waitID, dependencyID))
	}
	if resumeCommit.Fields.StartLease != "wait_dependency" || resumeCommit.Fields.EffectApplied == nil || !*resumeCommit.Fields.EffectApplied {
		t.Fatalf("pool member resume commit = %+v, want one applied start authorized by the wait_dependency lease", resumeCommit)
	}
	if err := sessionWaitDependencyShadowJourneyWaitForSessionState(
		t.Context(), cityDir, session.ID, "active", sessionWaitDependencyShadowJourneyWitnessTimeout,
	); err != nil {
		t.Fatalf("resumed pool member did not become durably active: %v", err)
	}
	afterResume := sessionWaitDependencyShadowJourneyReadBead(t, cityDir, session.ID)
	for _, key := range []string{"pool_managed", "session_origin", "pool_slot", "agent_name", "session_name", "gc.trigger_bead_id"} {
		if afterResume.Metadata[key] != materializedBead.Metadata[key] {
			t.Fatalf("resumed pool metadata %s = %q, want preserved %q", key, afterResume.Metadata[key], materializedBead.Metadata[key])
		}
	}
	afterSessions, err := sessionWaitDependencyShadowJourneyListSessions(cityDir)
	if err != nil {
		t.Fatalf("list worker sessions after pool resume: %v", err)
	}
	var openWorkers []sessionWaitDependencyShadowJourneySessionItem
	for _, candidate := range afterSessions.Sessions {
		if candidate.Template == "worker" && !candidate.Closed {
			openWorkers = append(openWorkers, candidate)
		}
	}
	if len(openWorkers) != 1 || openWorkers[0].ID != session.ID || openWorkers[0].SessionName != session.SessionName {
		t.Fatalf("open worker sessions after resume = %+v, want only original %+v", openWorkers, session)
	}
	afterTrace, err := sessionWaitDependencyShadowJourneyTrace(cityDir)
	if err != nil {
		t.Fatalf("read trace after pool resume: %v", err)
	}
	if got := len(sessionWaitDependencyShadowJourneyPoolMaterializationRecords(afterTrace, workID)); got != wantPoolMaterializations {
		t.Fatalf("pool materializations after resume = %d, want the original %d only", got, wantPoolMaterializations)
	}
	durableWait, err := sessionWaitDependencyShadowJourneyInspectWait(cityDir, waitID)
	if err != nil {
		t.Fatalf("inspect pool member wait after resume: %v", err)
	}
	if durableWait.Wait.ID != waitID || durableWait.Wait.State != "ready" || durableWait.Wait.Status != "open" {
		t.Fatalf("pool member wait after resume = %+v, want id=%q state=ready status=open", durableWait.Wait, waitID)
	}
	t.Logf(
		"ready routed-work event materialized session %s, projected it in %s, reached live tmux in %s, then resumed the same member through keyed wait_dependency in %s and committed in %s (initial %s|%s|%s; resumed %s|%s|%s)",
		session.ID,
		projectionLatency,
		liveLatency,
		resumeLatency,
		resumeCommitLatency,
		tmuxSession.ID,
		tmuxSession.Name,
		tmuxSession.SocketPath,
		resumedTmux.ID,
		resumedTmux.Name,
		resumedTmux.SocketPath,
	)

	if maxActiveSessions < 0 {
		var action struct {
			OK        bool   `json:"ok"`
			Action    string `json:"action"`
			SessionID string `json:"session_id"`
			State     string `json:"state"`
		}
		out, err = gcDolt(cityDir, "session", "kill", session.ID, "--json")
		if err != nil {
			t.Fatalf("kill exact unlimited pool member: %v\n%s", err, out)
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &action); err != nil {
			t.Fatalf("decode exact unlimited pool kill: %v\n%s", err, out)
		}
		if !action.OK || action.Action != "kill" || action.SessionID != session.ID {
			t.Fatalf("exact unlimited pool kill result = %+v, want successful kill for %q", action, session.ID)
		}
		if err := sessionWaitDependencyShadowJourneyWaitForSessionState(
			t.Context(), cityDir, session.ID, "asleep", sessionWaitDependencyShadowJourneyWitnessTimeout,
		); err != nil {
			t.Fatalf("killed unlimited pool member did not become durably asleep: %v", err)
		}
		if err := sessionWaitDependencyShadowJourneyWaitForExactTmuxAbsence(
			t.Context(), cityDir, session.SessionName, sessionWaitDependencyShadowJourneyWitnessTimeout,
		); err != nil {
			t.Fatalf("killed unlimited pool member remained live: %v\n%s", err, sessionWaitDependencyShadowJourneyDiagnostics(cityDir, workID, workID))
		}
		killedBead := sessionWaitDependencyShadowJourneyReadBead(t, cityDir, session.ID)
		for _, key := range []string{"pool_managed", "session_origin", "pool_slot", "agent_name", "session_name", "gc.trigger_bead_id", "gc.trigger_bead_store_ref"} {
			if killedBead.Metadata[key] != materializedBead.Metadata[key] {
				t.Fatalf("killed unlimited pool metadata %s = %q, want preserved %q", key, killedBead.Metadata[key], materializedBead.Metadata[key])
			}
		}

		beforeWakeTrace, err := sessionWaitDependencyShadowJourneyTrace(cityDir)
		if err != nil {
			t.Fatalf("read detail trace before exact pool wake: %v", err)
		}
		wakeAfterSeq := sessionLifecycleStatusShadowJourneyLastSeq(beforeWakeTrace)
		wakeStarted := time.Now()
		action = struct {
			OK        bool   `json:"ok"`
			Action    string `json:"action"`
			SessionID string `json:"session_id"`
			State     string `json:"state"`
		}{}
		out, err = gcDolt(cityDir, "session", "wake", session.ID, "--json")
		if err != nil {
			t.Fatalf("wake exact unlimited pool member: %v\n%s", err, out)
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &action); err != nil {
			t.Fatalf("decode exact unlimited pool wake: %v\n%s", err, out)
		}
		if !action.OK || action.Action != "wake" || action.SessionID != session.ID || action.State != "wake_requested" {
			t.Fatalf("exact unlimited pool wake result = %+v, want wake_requested for %q", action, session.ID)
		}
		wokenTmux, wakeLatency, err := sessionWaitDependencyShadowJourneyWaitForExactTmuxSession(
			t.Context(), cityDir, session.SessionName, wakeStarted, sessionWaitDependencyShadowJourneyWitnessTimeout,
		)
		if err != nil {
			t.Fatalf("exact unlimited pool member did not wake before the ten-minute debounce: %v\n%s", err, sessionWaitDependencyShadowJourneyDiagnostics(cityDir, workID, workID))
		}
		if wakeLatency >= 10*time.Minute {
			t.Fatalf("exact unlimited pool wake latency = %s, want before ten-minute debounce", wakeLatency)
		}
		if err := sessionWaitDependencyShadowJourneyWaitForSessionState(
			t.Context(), cityDir, session.ID, "active", sessionWaitDependencyShadowJourneyWitnessTimeout,
		); err != nil {
			t.Fatalf("woken unlimited pool member did not become durably active: %v", err)
		}
		socketWakeCommits := func(trace sessionWaitDependencyShadowJourneyTraceShow, sessionID string, afterSeq uint64) []sessionWaitDependencyShadowJourneyTraceRecord {
			var matches []sessionWaitDependencyShadowJourneyTraceRecord
			for _, record := range trace.Records {
				if record.Seq > afterSeq && record.SiteCode == "lifecycle.start.commit" &&
					record.SessionBeadID == sessionID && record.SessionName == session.SessionName &&
					record.Fields.Admission == "socket" {
					matches = append(matches, record)
				}
			}
			return matches
		}
		wakeTrace, wakeCommitLatency, err := sessionLifecycleStatusShadowJourneyWaitForWitness(
			t.Context(), cityDir, session.ID, wakeAfterSeq, sessionWaitDependencyShadowJourneyWitnessTimeout,
			"socket pool wake commit", socketWakeCommits,
		)
		if err != nil {
			t.Fatalf("exact unlimited pool wake commit did not converge: %v\n%s", err, sessionWaitDependencyShadowJourneyDiagnostics(cityDir, workID, workID))
		}
		wakeCommits := socketWakeCommits(wakeTrace, session.ID, wakeAfterSeq)
		wakeCommit := wakeCommits[0]
		if wakeCommit.RecordType != "operation" || wakeCommit.OutcomeCode != "success" ||
			wakeCommit.Fields.SessionID != session.ID || wakeCommit.Fields.InstanceToken == "" ||
			wakeCommit.Fields.EffectApplied == nil || !*wakeCommit.Fields.EffectApplied {
			t.Fatalf("exact unlimited pool wake commit = %+v, want one applied socket start", wakeCommit)
		}
		wokenBead := sessionWaitDependencyShadowJourneyReadBead(t, cityDir, session.ID)
		for _, key := range []string{"pool_managed", "session_origin", "pool_slot", "agent_name", "session_name", "gc.trigger_bead_id", "gc.trigger_bead_store_ref"} {
			if wokenBead.Metadata[key] != materializedBead.Metadata[key] {
				t.Fatalf("woken unlimited pool metadata %s = %q, want preserved %q", key, wokenBead.Metadata[key], materializedBead.Metadata[key])
			}
		}
		afterWakeSessions, err := sessionWaitDependencyShadowJourneyListSessions(cityDir)
		if err != nil {
			t.Fatalf("list worker sessions after exact unlimited pool wake: %v", err)
		}
		var openWorkers []sessionWaitDependencyShadowJourneySessionItem
		for _, candidate := range afterWakeSessions.Sessions {
			if candidate.Template == "worker" && !candidate.Closed {
				openWorkers = append(openWorkers, candidate)
			}
		}
		if len(openWorkers) != 1 || openWorkers[0].ID != session.ID || openWorkers[0].SessionName != session.SessionName {
			t.Fatalf("open worker sessions after exact unlimited pool wake = %+v, want only original %+v", openWorkers, session)
		}
		if got := len(sessionWaitDependencyShadowJourneyPoolMaterializationRecords(wakeTrace, workID)); got != 1 {
			t.Fatalf("pool materializations after exact unlimited wake = %d, want original one only", got)
		}
		t.Logf("public exact wake resumed unlimited pool member %s in %s and committed through socket admission in %s (%s|%s|%s)",
			session.ID, wakeLatency, wakeCommitLatency, wokenTmux.ID, wokenTmux.Name, wokenTmux.SocketPath)
	}
}

func TestConfiguredNamedSessionPublicKillRecyclesSameCanonicalSessionBeforeDebounce(t *testing.T) {
	if usingSubprocess() {
		t.Skip("exact configured named kill-recycle journey requires tmux")
	}

	cityDir := setupReconcilerCityWithManagedDolt(t, `session_reconciler = "auto"

[[agent]]
name = "worker"
start_command = "sleep 3600"
`, `patrol_interval = "1h"
`, `conditional_writes = "auto"`)
	schemaStatus, err := bdDolt(cityDir, "migrate", "schema", "--json")
	if err != nil || !strings.Contains(schemaStatus, "v59") {
		t.Fatalf("bd schema status = %q, err=%v, want v59", schemaStatus, err)
	}

	session, _, err := sessionWaitDependencyShadowJourneyWaitForWorkerSession(
		t.Context(), cityDir, time.Now(), sessionWaitDependencyShadowJourneyWitnessTimeout,
	)
	if err != nil {
		t.Fatalf("canonical named session did not materialize: %v", err)
	}
	if err := sessionWaitDependencyShadowJourneyWaitForSessionState(t.Context(), cityDir, session.ID, "active", sessionWaitDependencyShadowJourneyWitnessTimeout); err != nil {
		t.Fatalf("canonical named session was not durably active: %v", err)
	}
	beforeTmux, _, err := sessionWaitDependencyShadowJourneyWaitForExactTmuxSession(t.Context(), cityDir, session.SessionName, time.Now(), sessionWaitDependencyShadowJourneyWitnessTimeout)
	if err != nil {
		t.Fatalf("canonical named session was not live: %v", err)
	}
	before := sessionWaitDependencyShadowJourneyReadBead(t, cityDir, session.ID)
	beforeToken := before.Metadata["instance_token"]
	if beforeToken == "" {
		t.Fatal("canonical named session instance_token is empty")
	}
	identity := before.Metadata["configured_named_identity"]
	if identity == "" || before.Metadata["session_name"] != session.SessionName || before.Metadata["configured_named_mode"] != "always" ||
		before.Metadata["template"] != "worker" || before.Metadata["session_origin"] != "named" {
		t.Fatalf("canonical named metadata = %+v, want stable named worker identity", before.Metadata)
	}
	if out, err := gcDolt(cityDir, "trace", "start", "--template", "worker", "--for", "2m", "--level", "detail"); err != nil {
		t.Fatalf("arm configured named kill trace: %v\n%s", err, out)
	}
	beforeTrace, err := sessionWaitDependencyShadowJourneyTrace(cityDir)
	if err != nil {
		t.Fatalf("read trace before configured named kill: %v", err)
	}
	afterSeq := sessionLifecycleStatusShadowJourneyLastSeq(beforeTrace)

	type sessionAction struct {
		OK        bool   `json:"ok"`
		Action    string `json:"action"`
		SessionID string `json:"session_id"`
		State     string `json:"state"`
	}
	killStarted := time.Now()
	out, err := gcDolt(cityDir, "session", "kill", session.ID, "--json")
	var action sessionAction
	if err != nil || json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &action) != nil ||
		!action.OK || action.Action != "kill" || action.SessionID != session.ID {
		t.Fatalf("kill canonical named session: action=%+v err=%v\n%s", action, err, out)
	}
	live, latency, err := sessionWaitDependencyShadowJourneyWaitForExactTmuxSession(t.Context(), cityDir, session.SessionName, killStarted, integrationGCCommandTimeout)
	if err != nil || latency >= 10*time.Minute {
		t.Fatalf("configured named kill did not recycle live tmux before debounce: latency=%s err=%v\n%s", latency, err, sessionWaitDependencyShadowJourneyDiagnostics(cityDir, session.ID, session.ID))
	}
	if live.ID == beforeTmux.ID {
		t.Fatalf("recycled named tmux ID = %q, want replacement of %q", live.ID, beforeTmux.ID)
	}
	if err := sessionWaitDependencyShadowJourneyWaitForSessionState(t.Context(), cityDir, session.ID, "active", sessionWaitDependencyShadowJourneyWitnessTimeout); err != nil {
		t.Fatalf("recycled named session did not become durably active: %v", err)
	}
	socketCommits := func(trace sessionWaitDependencyShadowJourneyTraceShow, sessionID string, after uint64) []sessionWaitDependencyShadowJourneyTraceRecord {
		var matches []sessionWaitDependencyShadowJourneyTraceRecord
		for _, record := range trace.Records {
			if record.Seq > after && record.SiteCode == "lifecycle.start.commit" && record.SessionBeadID == sessionID &&
				record.SessionName == session.SessionName && record.Fields.Admission == "socket" {
				matches = append(matches, record)
			}
		}
		return matches
	}
	trace, _, err := sessionLifecycleStatusShadowJourneyWaitForWitness(t.Context(), cityDir, session.ID, afterSeq, sessionWaitDependencyShadowJourneyWitnessTimeout, "socket configured named kill commit", socketCommits)
	if err != nil {
		t.Fatalf("configured named socket start did not commit exactly once: %v", err)
	}
	commits := socketCommits(trace, session.ID, afterSeq)
	if len(commits) != 1 {
		t.Fatalf("configured named socket commits = %d, want exactly 1", len(commits))
	}
	commit := commits[0]
	if commit.OutcomeCode != "success" || commit.Fields.EffectApplied == nil || !*commit.Fields.EffectApplied {
		t.Fatalf("configured named kill commit = %+v, want applied success", commit)
	}
	for _, record := range trace.Records {
		if record.Seq > afterSeq && record.SessionBeadID == session.ID && record.OutcomeCode == "start_enqueued" {
			t.Fatalf("configured named kill trace contains legacy enqueue after exact handoff: %+v", record)
		}
	}

	after := sessionWaitDependencyShadowJourneyReadBead(t, cityDir, session.ID)
	for _, key := range []string{"session_name", "configured_named_identity", "configured_named_mode", "template", "session_origin"} {
		if after.Metadata[key] != before.Metadata[key] {
			t.Fatalf("recycled named metadata %s = %q, want preserved %q", key, after.Metadata[key], before.Metadata[key])
		}
	}
	if after.Metadata["instance_token"] == "" || after.Metadata["instance_token"] == beforeToken {
		t.Fatalf("recycled named instance_token = %q, want a new nonempty value distinct from %q", after.Metadata["instance_token"], beforeToken)
	}
	current, err := sessionWaitDependencyShadowJourneyListSessions(cityDir)
	if err != nil {
		t.Fatalf("list sessions after configured named kill recycle: %v", err)
	}
	var openWorkers []sessionWaitDependencyShadowJourneySessionItem
	for _, candidate := range current.Sessions {
		if candidate.Template == "worker" && !candidate.Closed {
			openWorkers = append(openWorkers, candidate)
		}
	}
	if len(openWorkers) != 1 || openWorkers[0].ID != session.ID || openWorkers[0].SessionName != session.SessionName {
		t.Fatalf("open worker sessions after configured named kill recycle = %+v, want only original %+v", openWorkers, session)
	}
	t.Logf("public kill recycled configured named session %s through one socket commit in %s (%s|%s|%s)", session.ID, latency, live.ID, live.Name, live.SocketPath)
}

func sessionWaitDependencyShadowJourneyWaitForControllerStop(ctx context.Context, cityDir string, timeout time.Duration) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if controllerAlive(cityDir) == 0 {
			return nil
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("controller still answered after stop: %w", deadline.Err())
		case <-ticker.C:
		}
	}
}

func sessionWaitDependencyShadowJourneyRequireOmittedDependenciesEvent(cityDir, workID string) error {
	data, err := os.ReadFile(filepath.Join(cityDir, ".gc", "events.jsonl"))
	if err != nil {
		return fmt.Errorf("read event log: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var event struct {
			Type    string          `json:"type"`
			Actor   string          `json:"actor"`
			Subject string          `json:"subject"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &event); err != nil ||
			event.Type != "bead.updated" || event.Actor != "bd-hook" || event.Subject != workID {
			continue
		}
		var envelope struct {
			Bead map[string]json.RawMessage `json:"bead"`
		}
		if err := json.Unmarshal(event.Payload, &envelope); err != nil {
			return fmt.Errorf("decode event payload: %w", err)
		}
		if _, ok := envelope.Bead["dependencies"]; ok {
			return fmt.Errorf("payload unexpectedly contains dependencies")
		}
		if _, ok := envelope.Bead["needs"]; ok {
			return fmt.Errorf("payload unexpectedly contains needs")
		}
		return nil
	}
	return fmt.Errorf("typed bead.updated event for %s not found", workID)
}

func sessionWaitDependencyShadowJourneyListSessions(cityDir string) (sessionWaitDependencyShadowJourneySessionList, error) {
	out, err := gc(cityDir, "session", "list", "--state", "all", "--json")
	if err != nil {
		return sessionWaitDependencyShadowJourneySessionList{}, fmt.Errorf("gc session list: %w: %s", err, out)
	}
	var result sessionWaitDependencyShadowJourneySessionList
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &result); err != nil {
		return sessionWaitDependencyShadowJourneySessionList{}, fmt.Errorf("decode gc session list: %w: %s", err, out)
	}
	return result, nil
}

func sessionWaitDependencyShadowJourneyWaitForWorkerSession(
	ctx context.Context,
	cityDir string,
	started time.Time,
	timeout time.Duration,
) (sessionWaitDependencyShadowJourneySessionItem, time.Duration, error) {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var last sessionWaitDependencyShadowJourneySessionList
	var lastErr error
	for {
		current, err := sessionWaitDependencyShadowJourneyListSessions(cityDir)
		if err != nil {
			lastErr = err
		} else {
			last = current
			lastErr = nil
			for _, session := range current.Sessions {
				if session.Template == "worker" && !session.Closed && session.ID != "" && session.SessionName != "" {
					return session, time.Since(started), nil
				}
			}
		}

		select {
		case <-deadline.Done():
			return sessionWaitDependencyShadowJourneySessionItem{}, time.Since(started), fmt.Errorf(
				"waiting for an unclosed worker session: %w; last error: %v; last sessions: %+v",
				deadline.Err(),
				lastErr,
				last.Sessions,
			)
		case <-ticker.C:
		}
	}
}

func sessionWaitDependencyShadowJourneyWaitForExactTmuxSession(
	ctx context.Context,
	cityDir string,
	sessionName string,
	started time.Time,
	timeout time.Duration,
) (sessionWaitDependencyShadowJourneyTmuxSession, time.Duration, error) {
	if strings.TrimSpace(sessionName) == "" {
		return sessionWaitDependencyShadowJourneyTmuxSession{}, time.Since(started), fmt.Errorf("durable session name is empty")
	}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var lastOutput string
	var lastErr error
	for {
		out, err := runCommand("", commandEnvForDir(cityDir, false), integrationGCCommandTimeout,
			"tmux", "-L", filepath.Base(cityDir), "list-sessions", "-F",
			"#{session_id}|#{session_name}|#{socket_path}")
		lastOutput, lastErr = out, err
		if err == nil {
			var matches []sessionWaitDependencyShadowJourneyTmuxSession
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				parts := strings.Split(line, "|")
				if len(parts) != 3 || parts[1] != sessionName {
					continue
				}
				matches = append(matches, sessionWaitDependencyShadowJourneyTmuxSession{
					ID: parts[0], Name: parts[1], SocketPath: parts[2],
				})
			}
			if len(matches) > 1 {
				return sessionWaitDependencyShadowJourneyTmuxSession{}, time.Since(started), fmt.Errorf("exact tmux sessions named %q = %d, want 1: %q", sessionName, len(matches), out)
			}
			if len(matches) == 1 {
				match := matches[0]
				if match.ID == "" || match.Name == "" || match.SocketPath == "" {
					return sessionWaitDependencyShadowJourneyTmuxSession{}, time.Since(started), fmt.Errorf("exact tmux session has empty identity field: %+v", match)
				}
				return match, time.Since(started), nil
			}
		}

		select {
		case <-deadline.Done():
			return sessionWaitDependencyShadowJourneyTmuxSession{}, time.Since(started), fmt.Errorf(
				"waiting for exact tmux session %q: %w; last error: %v; last sessions: %q",
				sessionName,
				deadline.Err(),
				lastErr,
				lastOutput,
			)
		case <-ticker.C:
		}
	}
}

func sessionWaitDependencyShadowJourneyWaitForExactTmuxAbsence(
	ctx context.Context,
	cityDir string,
	sessionName string,
	timeout time.Duration,
) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var lastOutput string
	for {
		out, err := runCommand("", commandEnvForDir(cityDir, false), integrationGCCommandTimeout,
			"tmux", "-L", filepath.Base(cityDir), "list-sessions", "-F", "#{session_name}")
		lastOutput = out
		if err != nil || !slicesContainLine(out, sessionName) {
			return nil
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("waiting for exact tmux session %q to disappear: %w; last sessions: %q", sessionName, deadline.Err(), lastOutput)
		case <-ticker.C:
		}
	}
}

func sessionWaitDependencyShadowJourneyWaitForSessionState(
	ctx context.Context,
	cityDir string,
	sessionID string,
	wantState string,
	timeout time.Duration,
) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var last sessionWaitDependencyShadowJourneySessionList
	var lastErr error
	for {
		current, err := sessionWaitDependencyShadowJourneyListSessions(cityDir)
		if err != nil {
			lastErr = err
		} else {
			last, lastErr = current, nil
			for _, session := range current.Sessions {
				if session.ID == sessionID && session.State == wantState {
					return nil
				}
			}
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("waiting for session %s state %s: %w; last error: %v; last sessions: %+v", sessionID, wantState, deadline.Err(), lastErr, last.Sessions)
		case <-ticker.C:
		}
	}
}

func slicesContainLine(output, want string) bool {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

func sessionWaitDependencyShadowJourneyWaitForDependencyStartCommit(
	ctx context.Context,
	cityDir string,
	session sessionWaitDependencyShadowJourneySessionItem,
	started time.Time,
	timeout time.Duration,
) (sessionWaitDependencyShadowJourneyTraceRecord, time.Duration, error) {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		trace, err := sessionWaitDependencyShadowJourneyTrace(cityDir)
		if err != nil {
			lastErr = err
		} else {
			matches := sessionWaitDependencyShadowJourneyWaitDependencyStartCommitRecords(trace, session)
			switch len(matches) {
			case 1:
				return matches[0], time.Since(started), nil
			case 0:
				lastErr = fmt.Errorf("no exact start commit for %s", session.ID)
			default:
				return sessionWaitDependencyShadowJourneyTraceRecord{}, time.Since(started), fmt.Errorf("exact start commits for %s = %d, want 1: %+v", session.ID, len(matches), matches)
			}
		}
		select {
		case <-deadline.Done():
			return sessionWaitDependencyShadowJourneyTraceRecord{}, time.Since(started), fmt.Errorf("waiting for exact dependency start commit: %w; last error: %v", deadline.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func sessionWaitDependencyShadowJourneyWaitDependencyStartCommitRecords(
	trace sessionWaitDependencyShadowJourneyTraceShow,
	session sessionWaitDependencyShadowJourneySessionItem,
) []sessionWaitDependencyShadowJourneyTraceRecord {
	var matches []sessionWaitDependencyShadowJourneyTraceRecord
	for _, record := range sessionWaitDependencyShadowJourneyPoolStartCommitRecords(trace, session) {
		// The LEASE, not the admission source, is the ownership proof. The
		// source is sticky to whichever entry admitted the key FIRST
		// (sessionStartController.admit), so a dependency-ready start whose
		// BeadUpdated admission landed first traces as `in_process` even though
		// the wait-dependency lease is what authorized it (ga-f7v2ft.116 finding
		// 3, ratified on ga-ij8mh).
		if record.Fields.StartLease == "wait_dependency" {
			matches = append(matches, record)
		}
	}
	return matches
}

func sessionWaitDependencyShadowJourneyWaitForPoolStartCommit(
	ctx context.Context,
	cityDir string,
	workID string,
	session sessionWaitDependencyShadowJourneySessionItem,
	started time.Time,
	timeout time.Duration,
) (sessionWaitDependencyShadowJourneyTraceShow, time.Duration, error) {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var lastTrace sessionWaitDependencyShadowJourneyTraceShow
	var lastErr error
	for {
		trace, err := sessionWaitDependencyShadowJourneyTrace(cityDir)
		if err != nil {
			lastErr = err
		} else {
			lastTrace = trace
			materializations := sessionWaitDependencyShadowJourneyPoolMaterializationRecords(trace, workID)
			commits := sessionWaitDependencyShadowJourneyPoolStartCommitRecords(trace, session)
			if len(materializations) > 1 || len(commits) > 1 {
				return trace, time.Since(started), fmt.Errorf(
					"pool materializations/commits = %d/%d, want at most 1 each: materializations=%+v commits=%+v",
					len(materializations), len(commits), materializations, commits,
				)
			}
			if len(materializations) == 1 && len(commits) == 1 {
				return trace, time.Since(started), nil
			}
			lastErr = fmt.Errorf("pool materializations/commits = %d/%d, want 1/1", len(materializations), len(commits))
		}

		select {
		case <-deadline.Done():
			return lastTrace, time.Since(started), fmt.Errorf(
				"waiting for one keyed materialization and exact start commit: %w; last error: %v",
				deadline.Err(),
				lastErr,
			)
		case <-ticker.C:
		}
	}
}

func sessionWaitDependencyShadowJourneySession(t *testing.T, cityDir string) sessionWaitDependencyShadowJourneySessionItem {
	t.Helper()
	result, err := sessionWaitDependencyShadowJourneyListSessions(cityDir)
	if err != nil {
		t.Fatalf("list live worker session: %v", err)
	}
	for _, session := range result.Sessions {
		if session.Template == "worker" && !session.Closed && session.ID != "" && session.SessionName != "" {
			return session
		}
	}
	t.Fatalf("live worker session absent from typed session list: %+v", result)
	return sessionWaitDependencyShadowJourneySessionItem{}
}

func sessionWaitDependencyShadowJourneyTmuxIdentity(t *testing.T, cityDir, sessionName string) string {
	t.Helper()
	out, err := runCommand("", commandEnvForDir(cityDir, false), integrationGCCommandTimeout,
		"tmux", "-L", filepath.Base(cityDir), "display-message", "-p", "-t", "="+sessionName,
		"#{session_id}|#{session_name}|#{socket_path}")
	if err != nil {
		t.Fatalf("read tmux identity for %s: %v\n%s", sessionName, err, out)
	}
	identity := strings.TrimSpace(out)
	if identity == "" {
		t.Fatalf("tmux identity for %s is empty", sessionName)
	}
	return identity
}

func sessionWaitDependencyShadowJourneyBeadID(t *testing.T, output string) string {
	t.Helper()
	const createdPrefix = "Created bead:"
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, createdPrefix) {
			continue
		}
		if id := strings.TrimSpace(strings.TrimPrefix(line, createdPrefix)); id != "" {
			return id
		}
	}
	payload := []byte(strings.TrimSpace(extractJSONPayload(output)))
	var bead sessionWaitDependencyShadowJourneyBead
	if err := json.Unmarshal(payload, &bead); err == nil && bead.ID != "" {
		return bead.ID
	}
	var beads []sessionWaitDependencyShadowJourneyBead
	if err := json.Unmarshal(payload, &beads); err == nil && len(beads) == 1 && beads[0].ID != "" {
		return beads[0].ID
	}
	t.Fatalf("decode created dependency ID from %q", output)
	return ""
}

func sessionWaitDependencyShadowJourneyReadBead(t *testing.T, cityDir, beadID string) sessionLifecycleStatusShadowJourneyBead {
	t.Helper()
	out, err := bdDolt(cityDir, "show", beadID, "--json")
	if err != nil {
		t.Fatalf("show durable session bead %s: %v\n%s", beadID, err, out)
	}
	var rows []sessionLifecycleStatusShadowJourneyBead
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &rows); err != nil {
		t.Fatalf("decode durable session bead %s: %v\n%s", beadID, err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("durable session bead %s returned %d rows, want 1\n%s", beadID, len(rows), out)
	}
	return rows[0]
}

func sessionWaitDependencyShadowJourneyWaitID(t *testing.T, output string) string {
	t.Helper()
	const prefix = "Registered wait "
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, prefix))
		if len(fields) > 0 && fields[0] != "" {
			return fields[0]
		}
	}
	t.Fatalf("decode registered wait ID from %q", output)
	return ""
}

func sessionWaitDependencyShadowJourneyInspectWait(
	cityDir string,
	waitID string,
) (sessionWaitDependencyShadowJourneyWaitInspect, error) {
	out, err := gc(cityDir, "wait", "inspect", waitID, "--json")
	if err != nil {
		return sessionWaitDependencyShadowJourneyWaitInspect{}, fmt.Errorf("gc wait inspect %s: %w: %s", waitID, err, out)
	}
	var result sessionWaitDependencyShadowJourneyWaitInspect
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &result); err != nil {
		return sessionWaitDependencyShadowJourneyWaitInspect{}, fmt.Errorf("decode gc wait inspect %s: %w: %s", waitID, err, out)
	}
	return result, nil
}

func sessionWaitDependencyShadowJourneyWaitForDependencyCommit(
	ctx context.Context,
	cityDir string,
	waitID string,
	sessionID string,
	timeout time.Duration,
) (sessionWaitDependencyShadowJourneyTraceShow, time.Duration, error) {
	started := time.Now()
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var lastTrace sessionWaitDependencyShadowJourneyTraceShow
	var lastWait sessionWaitDependencyShadowJourneyWaitInspect
	var lastErr error
	for {
		trace, traceErr := sessionWaitDependencyShadowJourneyTrace(cityDir)
		if traceErr != nil {
			lastErr = traceErr
		} else {
			lastTrace = trace
			lastErr = nil
			matches := sessionWaitDependencyShadowJourneyDependencyCommitRecords(trace, waitID, sessionID)
			switch len(matches) {
			case 1:
				wait, waitErr := sessionWaitDependencyShadowJourneyInspectWait(cityDir, waitID)
				if waitErr != nil {
					lastErr = waitErr
					break
				}
				lastWait = wait
				if wait.Wait.ID == waitID && wait.Wait.State == "ready" && wait.Wait.Status == "open" {
					return trace, time.Since(started), nil
				}
				lastErr = fmt.Errorf("durable wait = %+v, want id=%q state=ready status=open", wait.Wait, waitID)
			case 0:
			default:
				return trace, time.Since(started), fmt.Errorf(
					"dependency-commit shadow records for wait %s and session %s = %d, want exactly 1: %+v",
					waitID,
					sessionID,
					len(matches),
					matches,
				)
			}
		}

		select {
		case <-deadline.Done():
			return lastTrace, time.Since(started), fmt.Errorf(
				"waiting for dependency-commit shadow record and durable ready wait for wait %s and session %s: %w; last error: %v; last wait: %+v; exact records: %+v",
				waitID,
				sessionID,
				deadline.Err(),
				lastErr,
				lastWait.Wait,
				sessionWaitDependencyShadowJourneyExactRecords(lastTrace, waitID, sessionID),
			)
		case <-ticker.C:
		}
	}
}

func sessionWaitDependencyShadowJourneyDiagnostics(cityDir, waitID, dependencyID string) string {
	var sections []string
	if out, err := gc(cityDir, "session", "list", "--state", "all", "--json"); err != nil {
		sections = append(sections, fmt.Sprintf("session list: %v: %s", err, out))
	} else {
		sections = append(sections, "session list:\n"+tailText(out, 100))
	}

	eventsPath := filepath.Join(cityDir, ".gc", "events.jsonl")
	if data, err := os.ReadFile(eventsPath); err != nil {
		sections = append(sections, fmt.Sprintf("relevant events: read %s: %v", eventsPath, err))
	} else {
		var relevant []string
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, waitID) || strings.Contains(line, dependencyID) {
				relevant = append(relevant, line)
			}
		}
		sections = append(sections, "relevant event tail:\n"+tailText(strings.Join(relevant, "\n"), 30))
	}

	env := parseEnvList(commandEnvForDir(cityDir, false))
	logPath := filepath.Join(env["GC_HOME"], "supervisor.log")
	if data, err := os.ReadFile(logPath); err != nil {
		sections = append(sections, fmt.Sprintf("supervisor log: read %s: %v", logPath, err))
	} else {
		sections = append(sections, "supervisor log tail:\n"+tailText(string(data), 100))
	}

	if out, err := gc(cityDir, "trace", "status", "--json"); err != nil {
		sections = append(sections, fmt.Sprintf("trace status: %v: %s", err, out))
	} else {
		sections = append(sections, "trace status:\n"+tailText(out, 20))
	}
	if out, err := gc(cityDir, "trace", "show", "--since", "2m"); err != nil {
		sections = append(sections, fmt.Sprintf("recent trace: %v: %s", err, out))
	} else {
		sections = append(sections, "recent trace tail:\n"+tailText(out, 100))
	}

	return strings.Join(sections, "\n\n")
}

func sessionWaitDependencyShadowJourneyTrace(cityDir string) (sessionWaitDependencyShadowJourneyTraceShow, error) {
	out, err := gc(cityDir, "trace", "show", "--json")
	if err != nil {
		return sessionWaitDependencyShadowJourneyTraceShow{}, fmt.Errorf("gc trace show: %w: %s", err, out)
	}
	var result sessionWaitDependencyShadowJourneyTraceShow
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &result); err != nil {
		return sessionWaitDependencyShadowJourneyTraceShow{}, fmt.Errorf("decode gc trace show: %w: %s", err, out)
	}
	return result, nil
}

func sessionWaitDependencyShadowJourneyExactRecords(
	trace sessionWaitDependencyShadowJourneyTraceShow,
	waitID string,
	sessionID string,
) []sessionWaitDependencyShadowJourneyTraceRecord {
	var matches []sessionWaitDependencyShadowJourneyTraceRecord
	for _, record := range trace.Records {
		if record.SiteCode == "lifecycle.wait_dependency.shadow" &&
			record.Fields.WaitID == waitID &&
			record.Fields.SessionID == sessionID {
			matches = append(matches, record)
		}
	}
	return matches
}

func sessionWaitDependencyShadowJourneyRoutedWorkDemandRecords(
	trace sessionWaitDependencyShadowJourneyTraceShow,
	workID string,
) []sessionWaitDependencyShadowJourneyTraceRecord {
	var matches []sessionWaitDependencyShadowJourneyTraceRecord
	for _, record := range trace.Records {
		if record.SiteCode == "pool_demand.contribution.shadow" && record.Fields.WorkID == workID {
			matches = append(matches, record)
		}
	}
	return matches
}

func sessionWaitDependencyShadowJourneyPoolMaterializationRecords(
	trace sessionWaitDependencyShadowJourneyTraceShow,
	workID string,
) []sessionWaitDependencyShadowJourneyTraceRecord {
	var matches []sessionWaitDependencyShadowJourneyTraceRecord
	for _, record := range trace.Records {
		if record.SiteCode == "pool_allocation.materialize" && record.Fields.WorkID == workID {
			matches = append(matches, record)
		}
	}
	return matches
}

func sessionWaitDependencyShadowJourneyPoolStartCommitRecords(
	trace sessionWaitDependencyShadowJourneyTraceShow,
	session sessionWaitDependencyShadowJourneySessionItem,
) []sessionWaitDependencyShadowJourneyTraceRecord {
	var matches []sessionWaitDependencyShadowJourneyTraceRecord
	for _, record := range trace.Records {
		if record.SiteCode == "lifecycle.start.commit" &&
			record.SessionBeadID == session.ID &&
			record.SessionName == session.SessionName {
			matches = append(matches, record)
		}
	}
	return matches
}

func sessionWaitDependencyShadowJourneyDependencyCommitRecords(
	trace sessionWaitDependencyShadowJourneyTraceShow,
	waitID string,
	sessionID string,
) []sessionWaitDependencyShadowJourneyTraceRecord {
	var matches []sessionWaitDependencyShadowJourneyTraceRecord
	for _, record := range sessionWaitDependencyShadowJourneyExactRecords(trace, waitID, sessionID) {
		if record.RecordType == "operation" &&
			record.Fields.Cause == "dependency_commit" &&
			record.Fields.WaitOutcome == "ready" &&
			record.Fields.StartOutcome == "noop" &&
			record.Fields.StartReason == "already_running" &&
			record.Fields.EffectApplied != nil &&
			!*record.Fields.EffectApplied {
			matches = append(matches, record)
		}
	}
	return matches
}
