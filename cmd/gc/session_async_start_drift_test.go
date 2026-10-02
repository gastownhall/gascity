package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// The drifted-pending-create specimen: a named session whose persisted command
// predates a model bump. Every tick prepares the start from the current
// template command, so the command-drift gate fires on every attempt.
const (
	driftAlias            = "olivia"
	driftSessionName      = "olivia-rt"
	driftPersistedCommand = "claude --model claude-opus-4-8"
	driftTemplateCommand  = "claude --model claude-opus-5"
	driftToken            = "tok-olivia"
)

func driftNow() *clock.Fake {
	return &clock.Fake{Time: time.Date(2026, 9, 3, 1, 54, 51, 0, time.UTC)}
}

// createDriftedPendingCreate stores the specimen row: a pending create that
// never committed, in creating, holding its claim and its alias.
func createDriftedPendingCreate(t *testing.T, store beads.Store, clk clock.Clock, extra map[string]string) beads.Bead {
	t.Helper()
	meta := creatingMeta(map[string]string{
		"alias":                     driftAlias,
		"session_name":              driftSessionName,
		"template":                  "worker",
		"generation":                "3",
		"continuation_epoch":        "1",
		"instance_token":            driftToken,
		"pending_create_claim":      "true",
		"pending_create_started_at": clk.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		"last_woke_at":              clk.Now().UTC().Format(time.RFC3339),
		"command":                   driftPersistedCommand,
	})
	for k, v := range extra {
		meta[k] = v
	}
	b, err := store.Create(beads.Bead{
		Title:    driftAlias,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: meta,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// startDriftRuntime starts the runtime the attempt spawned, attributed to the
// attempt by session ID and instance token.
func startDriftRuntime(t *testing.T, sp *runtime.Fake, beadID string) {
	t.Helper()
	startDriftRuntimeAtEpoch(t, sp, beadID, driftToken, "3")
}

func startDriftRuntimeAtEpoch(t *testing.T, sp *runtime.Fake, beadID, token, epoch string) {
	t.Helper()
	if err := sp.Start(context.Background(), driftSessionName, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"GC_SESSION_ID": beadID, "GC_INSTANCE_TOKEN": token, "GC_RUNTIME_EPOCH": epoch} {
		if err := sp.SetMeta(driftSessionName, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

// driftStartResult is the async result of the attempt prepared against
// prepared, with the current template command.
func driftStartResult(t *testing.T, prepared beads.Bead, clk clock.Clock) startResult {
	t.Helper()
	return startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				info: sessiontest.SeedBead(t, prepared),
				tp: TemplateParams{
					Command:      driftTemplateCommand,
					SessionName:  driftSessionName,
					TemplateName: "worker",
				},
			},
			coreHash: "core-new",
			liveHash: "live-new",
		},
		outcome:  "success",
		started:  clk.Now(),
		finished: clk.Now(),
	}
}

func commitDrift(t *testing.T, result startResult, sp runtime.Provider, store beads.Store, clk clock.Clock) (bool, string) {
	t.Helper()
	var stderr bytes.Buffer
	committed := commitAsyncStartResultWithContext(context.Background(), result, sp, store, clk, events.Discard, 0, ioDiscard{}, &stderr, nil)
	return committed, stderr.String()
}

// TestCommitAsyncStartResult_DriftedPendingCreateRollsBack is the specimen: a
// pending create whose persisted command never matches the template command
// must be rolled back so it releases its alias. Before the fix the drift gate
// discarded the result, stopped the runtime and left the row in creating,
// holding its claim and alias, on every tick.
func TestCommitAsyncStartResult_DriftedPendingCreateRollsBack(t *testing.T) {
	store := beads.NewMemStore()
	clk := driftNow()
	b := createDriftedPendingCreate(t, store, clk, nil)
	sp := runtime.NewFake()
	startDriftRuntime(t, sp, b.ID)

	committed, stderr := commitDrift(t, driftStartResult(t, b, clk), sp, store, clk)
	if committed {
		t.Fatal("a drifted start must not commit")
	}
	if sp.IsRunning(driftSessionName) {
		t.Fatal("the runtime the drifted start spawned must be stopped")
	}
	got := mustGetBead(t, store, b.ID)
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed: a drifted pending create that cannot converge must be rolled back (stderr=%s)", got.Status, stderr)
	}
	if got.Metadata["state"] != string(session.StateFailedCreate) {
		t.Fatalf("state = %q, want failed-create", got.Metadata["state"])
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared", got.Metadata["pending_create_claim"])
	}
	if err := session.EnsureAliasAvailable(store, driftAlias, ""); err != nil {
		t.Fatalf("alias %q still held after the rollback: %v", driftAlias, err)
	}
	if !strings.Contains(stderr, "async_start_drift_rolled_back") {
		t.Fatalf("stderr does not record the rollback outcome:\n%s", stderr)
	}
}

// TestCommitAsyncStartResult_DriftDuringStartupKeepsRetrying pins the
// convergent case: the persisted command moved after the start was prepared,
// so the next tick prepares the new command and converges. That row keeps the
// discard-and-retry behavior and is not rolled back.
func TestCommitAsyncStartResult_DriftDuringStartupKeepsRetrying(t *testing.T) {
	store := beads.NewMemStore()
	clk := driftNow()
	b := createDriftedPendingCreate(t, store, clk, map[string]string{"command": driftTemplateCommand})
	result := driftStartResult(t, b, clk)
	result.prepared.candidate.tp.Command = driftPersistedCommand
	// Config moved while the start was in flight.
	if err := store.SetMetadata(b.ID, "command", "claude --model claude-opus-6"); err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	startDriftRuntime(t, sp, b.ID)

	if committed, _ := commitDrift(t, result, sp, store, clk); committed {
		t.Fatal("a drifted start must not commit")
	}
	got := mustGetBead(t, store, b.ID)
	if got.Status == "closed" {
		t.Fatal("a create whose command moved during startup converges on retry and must not be rolled back")
	}
	if got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want kept for the retry", got.Metadata["pending_create_claim"])
	}
	if got.Metadata["last_woke_at"] != "" {
		t.Fatalf("last_woke_at = %q, want released so the retry can start", got.Metadata["last_woke_at"])
	}
}

// TestCommitAsyncStartResult_DriftedCommittedRowIsNotRolledBack: a row whose
// create committed (claim cleared, live state) is owned by the config-drift
// lane. The drift gate must not close its bead or free its alias.
func TestCommitAsyncStartResult_DriftedCommittedRowIsNotRolledBack(t *testing.T) {
	store := beads.NewMemStore()
	clk := driftNow()
	b := createDriftedPendingCreate(t, store, clk, map[string]string{
		"state":                     "active",
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	})
	sp := runtime.NewFake()
	startDriftRuntime(t, sp, b.ID)

	if committed, _ := commitDrift(t, driftStartResult(t, b, clk), sp, store, clk); committed {
		t.Fatal("a drifted start must not commit")
	}
	got := mustGetBead(t, store, b.ID)
	if got.Status == "closed" {
		t.Fatal("a committed row must never be rolled back by the drift gate")
	}
	if got.Metadata["alias"] != driftAlias {
		t.Fatalf("alias = %q, want %q kept", got.Metadata["alias"], driftAlias)
	}
}

// TestCommitAsyncStartResult_DriftRollbackRequiresIdentity: a late attempt
// (tok-old) whose row now belongs to a newer incarnation must not close the
// newer incarnation's bead, stop its runtime or clear its in-flight lease.
func TestCommitAsyncStartResult_DriftRollbackRequiresIdentity(t *testing.T) {
	store := beads.NewMemStore()
	clk := driftNow()
	b := createDriftedPendingCreate(t, store, clk, map[string]string{"instance_token": "tok-old"})
	result := driftStartResult(t, b, clk)
	// The reconciler has since started incarnation tok-new.
	newLease := clk.Now().Add(30 * time.Second).UTC().Format(time.RFC3339)
	if err := store.SetMetadataBatch(b.ID, map[string]string{"instance_token": "tok-new", "generation": "4", "last_woke_at": newLease}); err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	startDriftRuntimeAtEpoch(t, sp, b.ID, "tok-new", "4")

	if committed, _ := commitDrift(t, result, sp, store, clk); committed {
		t.Fatal("a stale attempt must not commit")
	}
	got := mustGetBead(t, store, b.ID)
	if got.Status == "closed" {
		t.Fatal("a late attempt closed the bead of a newer incarnation")
	}
	if !sp.IsRunning(driftSessionName) {
		t.Fatal("a late attempt stopped the runtime of a newer incarnation")
	}
	if got.Metadata["last_woke_at"] != newLease {
		t.Fatalf("last_woke_at = %q, want the newer incarnation's lease %q kept", got.Metadata["last_woke_at"], newLease)
	}
}

// TestCommitAsyncStartResult_DriftRollbackRefusesWhenRuntimeSurvives: the
// rollback frees the alias, so a runtime that survives its stop keeps the bead.
func TestCommitAsyncStartResult_DriftRollbackRefusesWhenRuntimeSurvives(t *testing.T) {
	store := beads.NewMemStore()
	clk := driftNow()
	b := createDriftedPendingCreate(t, store, clk, nil)
	sp := runtime.NewFake()
	startDriftRuntime(t, sp, b.ID)
	sp.StopLeavesRunning[driftSessionName] = true

	if committed, _ := commitDrift(t, driftStartResult(t, b, clk), sp, store, clk); committed {
		t.Fatal("a drifted start must not commit")
	}
	got := mustGetBead(t, store, b.ID)
	if got.Status == "closed" {
		t.Fatal("rolled back while the spawned runtime was still running; the alias would be stranded under a live agent")
	}
	if got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want kept", got.Metadata["pending_create_claim"])
	}
}

// TestCommitAsyncStartResult_DriftRollbackRefusesOnStopError: a Stop that
// fails for any reason other than "already gone" keeps the bead.
func TestCommitAsyncStartResult_DriftRollbackRefusesOnStopError(t *testing.T) {
	store := beads.NewMemStore()
	clk := driftNow()
	b := createDriftedPendingCreate(t, store, clk, nil)
	sp := runtime.NewFake()
	startDriftRuntime(t, sp, b.ID)
	sp.StopErrors[driftSessionName] = errors.New("kill-session: permission denied")

	if committed, _ := commitDrift(t, driftStartResult(t, b, clk), sp, store, clk); committed {
		t.Fatal("a drifted start must not commit")
	}
	if got := mustGetBead(t, store, b.ID); got.Status == "closed" {
		t.Fatal("rolled back after a failed stop")
	}
}

// unobservableProvider is a provider whose liveness observation fails: the
// three-valued probe answers "could not look", not "gone".
type unobservableProvider struct {
	*runtime.Fake
	listErr error
}

func (p *unobservableProvider) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return runtime.Liveness{}, runtime.ErrRuntimeUnavailable
}

func (p *unobservableProvider) ListRunning(prefix string) ([]string, error) {
	if p.listErr != nil {
		return nil, p.listErr
	}
	return p.Fake.ListRunning(prefix)
}

// listFailingProvider answers liveness cleanly but cannot list: the fallback
// existence probe fails.
type listFailingProvider struct {
	*runtime.Fake
}

func (p *listFailingProvider) ListRunning(string) ([]string, error) {
	return nil, errors.New("list-sessions: connection reset")
}

// TestCommitAsyncStartResult_DriftRollbackRefusesUnobservableRuntime: when the
// attempt cannot attribute the runtime and the provider cannot observe it, the
// row keeps its bead. "Could not look" is not "gone".
func TestCommitAsyncStartResult_DriftRollbackRefusesUnobservableRuntime(t *testing.T) {
	for name, sp := range map[string]runtime.Provider{
		"liveness observation fails": &unobservableProvider{Fake: runtime.NewFake()},
		"listing fails":              &listFailingProvider{Fake: runtime.NewFake()},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			clk := driftNow()
			b := createDriftedPendingCreate(t, store, clk, nil)
			if committed, _ := commitDrift(t, driftStartResult(t, b, clk), sp, store, clk); committed {
				t.Fatal("a drifted start must not commit")
			}
			got := mustGetBead(t, store, b.ID)
			if got.Status == "closed" {
				t.Fatal("rolled back although the runtime could not be observed")
			}
			if got.Metadata["pending_create_claim"] != "true" {
				t.Fatalf("pending_create_claim = %q, want kept", got.Metadata["pending_create_claim"])
			}
		})
	}
}

// TestCommitAsyncStartResult_DriftRollbackWaitsOnDeferredOutcome: a deferred
// or still-initializing outcome means the runtime is there but undecided,
// which is no state to close a bead from.
func TestCommitAsyncStartResult_DriftRollbackWaitsOnDeferredOutcome(t *testing.T) {
	for _, outcome := range []TraceOutcomeCode{TraceOutcomeDeferred, TraceOutcomeSessionInitializing} {
		t.Run(string(outcome), func(t *testing.T) {
			store := beads.NewMemStore()
			clk := driftNow()
			b := createDriftedPendingCreate(t, store, clk, nil)
			sp := runtime.NewFake()
			startDriftRuntime(t, sp, b.ID)
			result := driftStartResult(t, b, clk)
			result.outcome = outcome

			if committed, _ := commitDrift(t, result, sp, store, clk); committed {
				t.Fatal("a drifted start must not commit")
			}
			if got := mustGetBead(t, store, b.ID); got.Status == "closed" {
				t.Fatalf("rolled back on outcome %q", outcome)
			}
			if !sp.IsRunning(driftSessionName) {
				t.Fatalf("stopped the runtime on outcome %q", outcome)
			}
		})
	}
}

// TestCommitAsyncStartResult_DriftRollbackFailureIsNotReported: a rollback
// transaction that fails must not be logged as a rollback, and the row must
// keep retrying (its in-flight lease released) rather than go silent.
func TestCommitAsyncStartResult_DriftRollbackFailureIsNotReported(t *testing.T) {
	store := &failingCloseStore{MemStore: beads.NewMemStore()}
	clk := driftNow()
	b := createDriftedPendingCreate(t, store, clk, nil)
	sp := runtime.NewFake()
	startDriftRuntime(t, sp, b.ID)

	committed, stderr := commitDrift(t, driftStartResult(t, b, clk), sp, store, clk)
	if committed {
		t.Fatal("a drifted start must not commit")
	}
	if got := mustGetBead(t, store, b.ID); got.Status == "closed" {
		t.Fatal("precondition: the injected close failure did not fail the rollback")
	}
	if strings.Contains(stderr, "async_start_drift_rolled_back") {
		t.Fatalf("a rollback that did not land was reported as rolled back:\n%s", stderr)
	}
	if !strings.Contains(stderr, "async_start_refresh_failed") {
		t.Fatalf("a failed rollback must be reported as a failed refresh:\n%s", stderr)
	}
}

// TestPreWakeCommitKeepsPendingCreateEpisodeMarker: a retried start of a
// claimed pending create must not renew pending_create_started_at, or the
// stale-create bound never expires for a row that retries every tick. A
// claimless wake keeps the per-attempt stamp.
func TestPreWakeCommitKeepsPendingCreateEpisodeMarker(t *testing.T) {
	clk := driftNow()
	episodeStart := clk.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name  string
		claim string
		want  string
	}{
		{name: "claimed retry keeps the episode start", claim: "true", want: episodeStart},
		{name: "claimless wake stamps a fresh marker", claim: "", want: clk.Now().UTC().Format(time.RFC3339)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{
				Title:  "worker",
				Type:   sessionBeadType,
				Labels: []string{sessionBeadLabel},
				Metadata: creatingMeta(map[string]string{
					"session_name":              "worker-rt",
					"generation":                "1",
					"pending_create_claim":      tc.claim,
					"pending_create_started_at": episodeStart,
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := preWakeCommit(sessiontest.SeedBead(t, b), sessionFrontDoor(store), clk); err != nil {
				t.Fatal(err)
			}
			got := mustGetBead(t, store, b.ID)
			if got.Metadata["pending_create_started_at"] != tc.want {
				t.Fatalf("pending_create_started_at = %q, want %q", got.Metadata["pending_create_started_at"], tc.want)
			}
		})
	}
}

// TestPendingCreateRetriesDoNotRenewStaleBound drives the retry loop from the
// incident: a start attempt every tick for longer than the stale-create bound.
// Once the in-flight lease of the latest attempt lapses, the row must read
// stale so the reconciler's pending-create rollback can collect it.
func TestPendingCreateRetriesDoNotRenewStaleBound(t *testing.T) {
	store := beads.NewMemStore()
	clk := driftNow()
	b, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name":              "worker-rt",
			"generation":                "1",
			"pending_create_claim":      "true",
			"pending_create_started_at": clk.Now().UTC().Format(time.RFC3339),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sessFront := sessionFrontDoor(store)
	for i := 0; i < 10; i++ {
		info, _, err := sessFront.GetPersistedResponse(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := preWakeCommit(info, sessFront, clk); err != nil {
			t.Fatal(err)
		}
		clk.Advance(15 * time.Second)
	}
	info, _, err := sessFront.GetPersistedResponse(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	startupTimeout := 30 * time.Second
	clk.Advance(startupTimeout + staleKeyDetectDelay + 10*time.Second)
	if pendingCreateLeaseActiveInfo(info, clk, startupTimeout) {
		t.Fatalf("pending create still leased %s after its episode began; retries renewed the stale bound (pending_create_started_at=%s)",
			clk.Now().Sub(driftNow().Now()), info.PendingCreateStartedAt)
	}
	if !pendingCreateLeaseExpiredForRollbackInfo(info, clk, startupTimeout) {
		t.Fatal("the reconciler's pending-create rollback cannot collect a row that retried for longer than the stale bound")
	}
}

// TestRescuePendingCreateForReset covers the reset rescue gates: it rolls back
// only an unfinished create that is past its lease, stale, and has no running
// runtime, and it reports a rollback that did not land as an error.
func TestRescuePendingCreateForReset(t *testing.T) {
	const startupTimeout = time.Minute
	stale := func(clk clock.Clock) map[string]string {
		return map[string]string{
			"last_woke_at":              clk.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			"pending_create_started_at": clk.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		}
	}

	t.Run("stuck pending create with no runtime is rolled back", func(t *testing.T) {
		store := beads.NewMemStore()
		clk := driftNow()
		b := createDriftedPendingCreate(t, store, clk, stale(clk))
		rolledBack, err := rescuePendingCreateForReset(store, runtime.NewFake(), startupTimeout, b.ID, clk, ioDiscard{})
		if err != nil || !rolledBack {
			t.Fatalf("rescue = (%v, %v), want (true, nil)", rolledBack, err)
		}
		if got := mustGetBead(t, store, b.ID); got.Status != "closed" {
			t.Fatalf("status = %q, want closed", got.Status)
		}
		if err := session.EnsureAliasAvailable(store, driftAlias, ""); err != nil {
			t.Fatalf("alias still held after reset rescue: %v", err)
		}
	})

	notRescued := func(t *testing.T, store beads.Store, sp runtime.Provider, clk clock.Clock, id string) {
		t.Helper()
		rolledBack, err := rescuePendingCreateForReset(store, sp, startupTimeout, id, clk, ioDiscard{})
		if err != nil || rolledBack {
			t.Fatalf("rescue = (%v, %v), want (false, nil) so reset restarts in place", rolledBack, err)
		}
		got := mustGetBead(t, store, id)
		if got.Status == "closed" {
			t.Fatal("reset rescue closed a bead it must leave alone")
		}
	}

	t.Run("create still inside its start lease", func(t *testing.T) {
		store := beads.NewMemStore()
		clk := driftNow()
		b := createDriftedPendingCreate(t, store, clk, map[string]string{
			"last_woke_at": clk.Now().Add(-3 * time.Second).UTC().Format(time.RFC3339),
		})
		notRescued(t, store, runtime.NewFake(), clk, b.ID)
	})
	t.Run("runtime is running", func(t *testing.T) {
		store := beads.NewMemStore()
		clk := driftNow()
		b := createDriftedPendingCreate(t, store, clk, stale(clk))
		sp := runtime.NewFake()
		startDriftRuntime(t, sp, b.ID)
		notRescued(t, store, sp, clk, b.ID)
		if !sp.IsRunning(driftSessionName) {
			t.Fatal("reset rescue stopped a runtime; it must only probe")
		}
	})
	t.Run("runtime cannot be observed", func(t *testing.T) {
		store := beads.NewMemStore()
		clk := driftNow()
		b := createDriftedPendingCreate(t, store, clk, stale(clk))
		notRescued(t, store, &unobservableProvider{Fake: runtime.NewFake()}, clk, b.ID)
	})
	t.Run("runtime listing fails", func(t *testing.T) {
		store := beads.NewMemStore()
		clk := driftNow()
		b := createDriftedPendingCreate(t, store, clk, stale(clk))
		notRescued(t, store, &listFailingProvider{Fake: runtime.NewFake()}, clk, b.ID)
	})
	t.Run("not a pending create", func(t *testing.T) {
		store := beads.NewMemStore()
		clk := driftNow()
		extra := stale(clk)
		extra["state"] = "awake"
		extra["pending_create_claim"] = ""
		b := createDriftedPendingCreate(t, store, clk, extra)
		notRescued(t, store, runtime.NewFake(), clk, b.ID)
	})
	t.Run("rollback that does not land is an error", func(t *testing.T) {
		store := &failingCloseStore{MemStore: beads.NewMemStore()}
		clk := driftNow()
		b := createDriftedPendingCreate(t, store, clk, stale(clk))
		rolledBack, err := rescuePendingCreateForReset(store, runtime.NewFake(), startupTimeout, b.ID, clk, ioDiscard{})
		if err == nil || rolledBack {
			t.Fatalf("rescue = (%v, %v), want an error: reporting success while the row keeps its claim and alias is a silent no-op", rolledBack, err)
		}
	})
}
