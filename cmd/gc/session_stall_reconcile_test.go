package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// stallProvider is the D2-capable fake every D-STALL test uses: runtime.Fake
// plus the fresh-liveness observer and the token-bound unattended stop the
// reset machinery asserts before it touches a runtime.
type stallProvider struct {
	*runtime.Fake
	stops int
}

func (p *stallProvider) ObserveFreshLiveness(target runtime.LivenessTarget) runtime.Liveness {
	running := p.IsRunning(target.SessionName)
	return runtime.Liveness{Running: running, Alive: running, Complete: true}
}

func (p *stallProvider) StopUnattendedSession(name, _ string) error {
	p.stops++
	return p.Stop(name)
}

// claimLookupStore counts the assignee-scoped queries the stall ladder's claim
// lookup makes, and can fail them, so a test can prove the lookup ran (or did
// not) rather than inferring it from the outcome.
type claimLookupStore struct {
	beads.Store
	lookups int
	err     error
}

func (s *claimLookupStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Assignee != "" {
		s.lookups++
		if s.err != nil {
			return nil, s.err
		}
	}
	return s.Store.List(query)
}

// stallCity is the fixture config: one pooled agent template with a provider
// that can rotate a session key, so the restart handoff has something to commit.
func stallCity(progressStall, claimHolderStall string, minActive int) *config.City {
	agent := config.Agent{Name: "worker", StartCommand: "true"}
	if minActive > 0 {
		agent.MinActiveSessions = &minActive
	}
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Session: config.SessionConfig{
			ProgressStallTimeout:    progressStall,
			ClaimHolderStallTimeout: claimHolderStall,
			StartupTimeout:          "60s",
		},
		Agents: []config.Agent{agent},
	}
}

// createStallRow seeds one open awake session row and starts its runtime, with
// quietFor of provider-reported quiet behind it. poolSlot stamps the row as
// pool-managed — the shape .103's own reset family declines and this one must
// carry.
func createStallRow(t *testing.T, store beads.Store, sp *stallProvider, name string, quietFor time.Duration, now time.Time, poolSlot string) beads.Bead {
	t.Helper()
	metadata := map[string]string{
		"session_name":   name,
		"agent_name":     name,
		"template":       "worker",
		"generation":     "1",
		"instance_token": "token-" + name,
		"state":          "active",
		"session_key":    "original-key",
	}
	if poolSlot != "" {
		metadata["pool_slot"] = poolSlot
	}
	bead, err := store.Create(beads.Bead{
		Title:    name,
		Type:     sessionBeadType,
		Status:   "open",
		Labels:   []string{sessionBeadLabel},
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("create session row %q: %v", name, err)
	}
	if err := sp.Start(t.Context(), name, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start runtime %q: %v", name, err)
	}
	if err := sp.SetMeta(name, "GC_INSTANCE_TOKEN", "token-"+name); err != nil {
		t.Fatalf("SetMeta(GC_INSTANCE_TOKEN): %v", err)
	}
	sp.SetActivity(name, now.Add(-quietFor))
	return bead
}

func stallReconcileRows(t *testing.T, store beads.Store, ids ...string) []sessionpkg.ReconcileSession {
	t.Helper()
	rows := make([]sessionpkg.ReconcileSession, 0, len(ids))
	for _, id := range ids {
		info, err := sessionFrontDoor(store).Get(id)
		if err != nil {
			t.Fatalf("project session row %q: %v", id, err)
		}
		bead, err := store.Get(id)
		if err != nil {
			t.Fatalf("read session row %q: %v", id, err)
		}
		rows = append(rows, sessionpkg.ReconcileSession{Info: info, Revision: bead.Revision})
	}
	return rows
}

func stallSweepInput(
	t *testing.T,
	cityPath string,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	now time.Time,
	admit func(string, sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error),
	ids ...string,
) detectorSweepInput {
	t.Helper()
	rows := stallReconcileRows(t, store, ids...)
	// Every row under test is DESIRED. Without that the sweep classifies them as
	// live orphans and D-ORPHAN claims the row before D-STALL is reached
	// (the family precedence WD.1 recorded), which is a different family's
	// condition, not this one's.
	desired := make(map[string]TemplateParams, len(rows))
	for _, row := range rows {
		name := row.Info.SessionNameMetadata
		desired[name] = TemplateParams{Command: "true", SessionName: name, TemplateName: "worker"}
	}
	return detectorSweepInput{
		CityPath: cityPath,
		CityName: "test-city",
		Cfg:      cfg,
		Provider: sp,
		Rows:     rows,
		Desired:  desired,
		Clock:    &clock.Fake{Time: now},
		Trigger:  "patrol",
		Admit:    admit,
	}
}

func stallHandlerParams(cityPath string, cfg *config.City, sp runtime.Provider, store beads.Store) exactSessionStartParams {
	statusWriter, _, statusWriterErr := beads.ResolveConditionalWriter(store)
	return exactSessionStartParams{
		Generation: 1, CityPath: cityPath, CityName: "test-city",
		Config: cfg, Provider: sp, Store: store,
		StatusWriter: statusWriter, StatusWriterError: statusWriterErr,
		Recorder: events.Discard, RolloutMode: rollout.Require,
		Stderr: io.Discard,
	}
}

func stallConditions(result detectorSweepResult) []detectorCondition {
	out := make([]detectorCondition, 0, 1)
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyStall {
			out = append(out, cond)
		}
	}
	return out
}

// TestExactProgressStalledSessionRecycledOnceByKey is WD.12's primary RED
// (AC a): a stale, claim-less, above-floor session raises exactly one D-STALL
// recycle condition, the sweep hands that exact key to the session-start
// controller under the progress_stall source, and the handler applies exactly
// ONE fenced recycle through .103's reset machinery — the runtime stops under
// its own instance token and the row carries the committed restart handoff.
//
// The second dispatch is the exactly-once half: with the runtime dead the
// decision no longer holds, the seam declines the key, and nothing is written a
// second time. It is the keyed re-point of
// TestReconcileSessionBeads_ProgressStallRecyclesStaleClaimlessHealthySession.
func TestExactProgressStalledSessionRecycledOnceByKey(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	sp := &stallProvider{Fake: runtime.NewFake()}
	cfg := stallCity("30m", "", 0)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	row := createStallRow(t, store, sp, "worker-1", time.Hour, now, "")

	admitter := &recordingDetectorAdmitter{}
	in := stallSweepInput(t, cityPath, cfg, sp, store, now, admitter.admit, row.ID)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	conds := stallConditions(result)
	if len(conds) != 1 {
		t.Fatalf("D-STALL conditions = %d (%#v), want exactly one recycle condition", len(conds), conds)
	}
	if conds[0].Reason != detectorReasonProgressStall || conds[0].Outcome != TraceOutcomeStop {
		t.Fatalf("D-STALL condition = reason %q outcome %q, want %q/%q", conds[0].Reason, conds[0].Outcome, detectorReasonProgressStall, TraceOutcomeStop)
	}
	if conds[0].Site != TraceSiteReconcilerProgressStallExempt {
		t.Fatalf("D-STALL site = %q, want the legacy exempt site %q (the recycle arm has no legacy site of its own)", conds[0].Site, TraceSiteReconcilerProgressStallExempt)
	}
	if len(admitter.keys) != 1 || admitter.keys[0] != row.ID {
		t.Fatalf("sweep enqueued %v, want exactly the stalled key %q", admitter.keys, row.ID)
	}
	if admitter.sources[0] != sessionStartAdmissionProgressStall {
		t.Fatalf("admission source = %q, want %q", admitter.sources[0], sessionStartAdmissionProgressStall)
	}

	params := stallHandlerParams(cityPath, cfg, sp, store)
	params.Clock = &clock.Fake{Time: now}
	info, response, err := getAuthoritativeSessionStartPersistedRecord(store, row.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(),
		sessionStartAdmission{SessionID: row.ID, Source: sessionStartAdmissionProgressStall},
		params, info, response, &clock.Fake{Time: now},
	)
	if !handled {
		t.Fatal("seam did not claim the progress-stalled row for D-STALL")
	}
	if err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
	}

	if sp.IsRunning("worker-1") {
		t.Fatal("progress-stalled runtime still running after the keyed recycle")
	}
	if sp.stops != 1 {
		t.Fatalf("token-bound unattended stops = %d, want exactly 1", sp.stops)
	}
	recycled, err := store.Get(row.ID)
	if err != nil {
		t.Fatalf("read recycled row: %v", err)
	}
	if recycled.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want consumed by the restart handoff", recycled.Metadata["restart_requested"])
	}
	if recycled.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", recycled.Metadata["continuation_reset_pending"])
	}
	if recycled.Metadata[sessionpkg.ResetCommittedAtKey] == "" {
		t.Fatal("reset_committed_at is empty; the restart handoff did not commit")
	}
	if recycled.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want cleared for a first-start path", recycled.Metadata["started_config_hash"])
	}

	// Exactly once: the runtime is dead, so the condition no longer holds and a
	// second admission on the same key is refused with zero effect.
	after, afterResponse, err := getAuthoritativeSessionStartPersistedRecord(store, row.ID)
	if err != nil {
		t.Fatalf("authoritative re-read: %v", err)
	}
	if exactSessionProgressStallCandidate(params, after, afterResponse, &clock.Fake{Time: now}) {
		t.Fatal("seam guard still claims a recycled row whose runtime is dead")
	}
	before, err := store.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	handledAgain, _, err := reconcileExactSessionDetectorFamily(
		t.Context(),
		sessionStartAdmission{SessionID: row.ID, Source: sessionStartAdmissionProgressStall},
		params, after, afterResponse, &clock.Fake{Time: now},
	)
	if handledAgain {
		t.Fatalf("seam claimed the recycled row a second time (err=%v)", err)
	}
	repeat, err := store.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repeat.Revision != before.Revision {
		t.Fatalf("recycled row mutated a second time: revision %d -> %d", before.Revision, repeat.Revision)
	}
	if sp.stops != 1 {
		t.Fatalf("token-bound unattended stops = %d after the second dispatch, want still 1", sp.stops)
	}
}

// TestMinFloorExemptionIsBoundedByClaimHolderTimeout is AC (b): the paired
// anchors, re-pointed keyed as a pair, plus the asymmetry the pairing exists to
// bound. The exemption suppresses the ENQUEUE for the claim-less family ONLY;
// the claim-holder arm gates on `exempt` alone, so with a positive
// claim_holder_stall_timeout the same floor worker IS enqueued and the handler
// runs the per-session claim lookup legacy runs at session_reconciler.go:2611.
//
// It re-points ProgressStallExemptsMinFloorIdleWorker (:538),
// ProgressStallRecyclesAboveFloorWorker (:569) and
// ClaimHolderStallKeepsPoolClaimForFreshWorker (:431) onto the keyed path.
func TestMinFloorExemptionIsBoundedByClaimHolderTimeout(t *testing.T) {
	for _, tc := range []struct {
		name            string
		claimHolder     string
		minActive       int
		companions      int
		poolSlot        string
		claimInProgress bool
		wantEnqueued    bool
		wantClaimLookup bool
		wantRecycled    bool
		wantReason      TraceReasonCode
	}{
		{
			name:         "claim-less floor worker is exempt and never enqueued",
			minActive:    1,
			wantReason:   detectorReasonProgressStallExempt,
			wantEnqueued: false,
		},
		{
			name:         "above-floor worker recycles",
			minActive:    1,
			companions:   1,
			wantReason:   detectorReasonProgressStall,
			wantEnqueued: true, wantClaimLookup: true, wantRecycled: true,
		},
		{
			name:        "pool floor worker holding a claim is enqueued, looked up and recycled",
			claimHolder: "20m", minActive: 1, poolSlot: "1", claimInProgress: true,
			wantReason:   detectorReasonProgressStall,
			wantEnqueued: true, wantClaimLookup: true, wantRecycled: true,
		},
		{
			name:        "pool floor worker without a claim is enqueued, looked up and refused",
			claimHolder: "20m", minActive: 1, poolSlot: "1",
			wantReason:   detectorReasonProgressStall,
			wantEnqueued: true, wantClaimLookup: true, wantRecycled: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			base := beads.NewMemStore()
			store := &claimLookupStore{Store: base}
			sp := &stallProvider{Fake: runtime.NewFake()}
			cfg := stallCity("30m", tc.claimHolder, tc.minActive)
			now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
			row := createStallRow(t, store, sp, "worker-1", time.Hour, now, tc.poolSlot)
			ids := []string{row.ID}
			for i := 0; i < tc.companions; i++ {
				ids = append(ids, createStallRow(t, store, sp, "worker-companion", time.Minute, now, "").ID)
			}
			if tc.claimInProgress {
				work, err := store.Create(beads.Bead{Title: "claimed pool work", Type: "task", Assignee: row.ID})
				if err != nil {
					t.Fatalf("create claimed work: %v", err)
				}
				status := "in_progress"
				if err := store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
					t.Fatalf("claim work: %v", err)
				}
			}

			admitter := &recordingDetectorAdmitter{}
			in := stallSweepInput(t, cityPath, cfg, sp, store, now, admitter.admit, ids...)
			result := detectSessionConditions(context.Background(), in)
			routeDetectorConditions(in, &result)

			var subject *detectorCondition
			for i := range result.Conditions {
				if result.Conditions[i].Family == detectorFamilyStall && result.Conditions[i].SessionID == row.ID {
					subject = &result.Conditions[i]
				}
			}
			if subject == nil {
				t.Fatalf("no D-STALL condition for %q; conditions=%#v", row.ID, result.Conditions)
			}
			if subject.Reason != tc.wantReason {
				t.Fatalf("D-STALL reason = %q, want %q", subject.Reason, tc.wantReason)
			}
			if subject.Site != TraceSiteReconcilerProgressStallExempt {
				t.Fatalf("D-STALL site = %q, want %q for both arms", subject.Site, TraceSiteReconcilerProgressStallExempt)
			}
			enqueued := false
			for _, key := range admitter.keys {
				if key == row.ID {
					enqueued = true
				}
			}
			if enqueued != tc.wantEnqueued {
				t.Fatalf("enqueued = %v, want %v (keys=%v)", enqueued, tc.wantEnqueued, admitter.keys)
			}
			if !tc.wantEnqueued {
				// The exempt arm is a shadow record: it predicts no effect and
				// carries the detector-shadow owner, never the keyed one.
				if subject.Outcome != TraceOutcomeNoChange || subject.AdmissionSource != "" {
					t.Fatalf("exempt arm = outcome %q source %q, want no-change and no admission", subject.Outcome, subject.AdmissionSource)
				}
				if store.lookups != 0 {
					t.Fatalf("claim lookups = %d during detection of an exempt floor worker, want 0", store.lookups)
				}
			}

			params := stallHandlerParams(cityPath, cfg, sp, store)
			info, response, err := getAuthoritativeSessionStartPersistedRecord(store, row.ID)
			if err != nil {
				t.Fatalf("authoritative read: %v", err)
			}
			store.lookups = 0
			handled, owner, err := reconcileExactSessionDetectorFamily(
				t.Context(),
				sessionStartAdmission{SessionID: row.ID, Source: sessionStartAdmissionProgressStall},
				params, info, response, &clock.Fake{Time: now},
			)
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if handled && owner != exactSessionStartKeyedOwner {
				t.Fatalf("handler owner = %v, want keyed", owner)
			}
			if got := store.lookups > 0; got != tc.wantClaimLookup {
				t.Fatalf("handler ran claim lookup = %v (%d), want %v", got, store.lookups, tc.wantClaimLookup)
			}
			recycled, err := store.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			gotRecycled := recycled.Metadata["continuation_reset_pending"] == "true"
			if gotRecycled != tc.wantRecycled {
				t.Fatalf("recycled = %v, want %v (metadata=%#v)", gotRecycled, tc.wantRecycled, recycled.Metadata)
			}
			if !tc.wantRecycled {
				if !sp.IsRunning("worker-1") {
					t.Fatal("exempt/refused floor worker was stopped")
				}
				if sp.stops != 0 {
					t.Fatalf("unattended stops = %d, want 0 for a refused row", sp.stops)
				}
			}
			if tc.claimInProgress {
				// The re-adoption guarantee (:431): the recycle must leave the
				// claim attached to the canonical row, never reopen it.
				work, err := store.List(beads.ListQuery{Assignee: row.ID})
				if err != nil {
					t.Fatalf("list claimed work: %v", err)
				}
				if len(work) != 1 || work[0].Status != "in_progress" || work[0].Assignee != row.ID {
					t.Fatalf("pool claim did not survive the keyed recycle: %#v", work)
				}
			}
		})
	}
}

// TestExactProgressStallNegatives is AC (c): the arms that must produce zero
// enqueues, zero claim lookups and zero writes. The disabled-threshold case is
// the AC's "at or below zero across a full sweep" negative; the claim-error
// case is its fail-safe negative.
func TestExactProgressStallNegatives(t *testing.T) {
	for _, tc := range []struct {
		name             string
		progressStall    string
		claimHolder      string
		quietFor         time.Duration
		claimErr         error
		wantConditions   int
		wantClaimLookups int
	}{
		{
			name:          "progressing session raises nothing",
			progressStall: "30m", quietFor: time.Minute,
		},
		{
			name:     "thresholds at or below zero enqueue nothing and look up nothing",
			quietFor: time.Hour,
		},
		{
			name:          "claim lookup error fails safe with zero writes",
			progressStall: "30m", claimHolder: "20m", quietFor: time.Hour,
			claimErr:       errors.New("assigned work query failed"),
			wantConditions: 1, wantClaimLookups: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			base := beads.NewMemStore()
			store := &claimLookupStore{Store: base, err: tc.claimErr}
			sp := &stallProvider{Fake: runtime.NewFake()}
			cfg := stallCity(tc.progressStall, tc.claimHolder, 0)
			now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
			row := createStallRow(t, store, sp, "worker-1", tc.quietFor, now, "")
			before, err := store.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}

			admitter := &recordingDetectorAdmitter{}
			in := stallSweepInput(t, cityPath, cfg, sp, store, now, admitter.admit, row.ID)
			result := detectSessionConditions(context.Background(), in)
			routeDetectorConditions(in, &result)
			if got := len(stallConditions(result)); got != tc.wantConditions {
				t.Fatalf("D-STALL conditions = %d, want %d", got, tc.wantConditions)
			}
			if tc.wantConditions == 0 && len(admitter.keys) != 0 {
				t.Fatalf("sweep enqueued %v, want zero enqueues", admitter.keys)
			}
			if store.lookups != 0 {
				t.Fatalf("detection ran %d claim lookups, want 0 (the sweep is store-read-free for this family)", store.lookups)
			}

			params := stallHandlerParams(cityPath, cfg, sp, store)
			info, response, err := getAuthoritativeSessionStartPersistedRecord(store, row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if exactSessionProgressStallCandidate(params, info, response, &clock.Fake{Time: now}) {
				t.Fatal("seam guard claimed a row that must not be recycled")
			}
			if got := store.lookups; got != tc.wantClaimLookups {
				t.Fatalf("handler-side claim lookups = %d, want %d", got, tc.wantClaimLookups)
			}
			handled, _, err := reconcileExactSessionDetectorFamily(
				t.Context(),
				sessionStartAdmission{SessionID: row.ID, Source: sessionStartAdmissionProgressStall},
				params, info, response, &clock.Fake{Time: now},
			)
			if handled {
				t.Fatalf("seam claimed a non-candidate row (err=%v)", err)
			}
			after, err := store.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Revision != before.Revision {
				t.Fatalf("row mutated with zero effect expected: revision %d -> %d", before.Revision, after.Revision)
			}
			if !sp.IsRunning("worker-1") {
				t.Fatal("runtime stopped with zero effect expected")
			}
		})
	}
}

// TestLegacyResetStalledArmKeepsWatchingKeyedRecycles is AC (d), and it records
// the ownership decision: the legacy ResetStalled arm does NOT yield to
// keyed-owned rows.
//
// It is an observational alarm, not an effect — no store write, no provider
// call, self-deduping through the drain tracker and self-clearing when the
// reset lands — so there is no destructive effect to serialize and no second
// writer to disagree with: the keyed handler emits no alarm of its own. Yielding
// it would blind the fleet to exactly the recycles the new handler owns, which
// is the one failure this alarm exists to report. The pre-commit window is
// covered for free: the marker pair the handler writes before the stop carries
// no reset_committed_at, so it cannot trip the alarm early.
func TestLegacyResetStalledArmKeepsWatchingKeyedRecycles(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	sp := &stallProvider{Fake: runtime.NewFake()}
	cfg := stallCity("30m", "", 0)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	row := createStallRow(t, store, sp, "worker-1", time.Hour, now, "")

	params := stallHandlerParams(cityPath, cfg, sp, store)
	info, response, err := getAuthoritativeSessionStartPersistedRecord(store, row.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Before the handoff commits, the transient marker pair must not trip the
	// alarm: it carries no reset_committed_at.
	requested := info
	requested.RestartRequested = "true"
	requested.ContinuationResetPending = "true"
	pre := events.NewFake()
	recordResetStallIfDue(requested, "worker", "worker-1", false, time.Minute, now.Add(time.Hour), newDrainTracker(), pre, io.Discard, nil)
	if len(pre.Events) != 0 {
		t.Fatalf("pre-commit marker pair raised %d reset-stall alarms, want 0", len(pre.Events))
	}

	if _, err := reconcileExactSessionProgressStallRecycle(
		sessionStartAdmission{SessionID: row.ID, Source: sessionStartAdmissionProgressStall},
		params, info, response, &clock.Fake{Time: now},
	); err != nil {
		t.Fatalf("keyed recycle: %v", err)
	}

	committed, err := sessionFrontDoor(store).Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	dt := newDrainTracker()
	rec := events.NewFake()
	recordResetStallIfDue(committed, "worker", "worker-1", false, time.Minute, now.Add(time.Hour), dt, rec, io.Discard, nil)
	if len(rec.Events) != 1 || rec.Events[0].Type != events.SessionResetStalled {
		t.Fatalf("legacy reset-stall alarm events = %#v, want exactly one %s for a keyed-owned row", rec.Events, events.SessionResetStalled)
	}
	// Idempotent: the drain tracker dedupes, so a second fleet pass adds nothing.
	recordResetStallIfDue(committed, "worker", "worker-1", false, time.Minute, now.Add(time.Hour), dt, rec, io.Discard, nil)
	if len(rec.Events) != 1 {
		t.Fatalf("legacy reset-stall alarm fired %d times, want exactly 1 (self-deduping)", len(rec.Events))
	}
}

// TestLegacyProgressStallArmYieldsToKeyedOwnedRow proves the yield half of the
// act-frontier rule: an acting D-STALL beside a non-yielding legacy would set
// restart_requested twice and race two kills at the same runtime. With the
// bridge installed the legacy arm writes nothing for that key, and without it
// the same fleet pass recycles — so the exclusion is what is being asserted, not
// an unrelated skip.
func TestLegacyProgressStallArmYieldsToKeyedOwnedRow(t *testing.T) {
	for _, tc := range []struct {
		name         string
		excluded     bool
		wantRecycled bool
	}{
		{name: "keyed owns the key: legacy stands down", excluded: true},
		{name: "no keyed admission: legacy recycles", wantRecycled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session, sessionName := newProgressStallTestEnv(t)
			env.startOptions = append(env.startOptions, withLegacyProgressStallRecycleExclusion(
				func(info sessionpkg.Info) bool { return tc.excluded && info.ID == session.ID },
			))

			env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			recycled := got.Metadata["continuation_reset_pending"] == "true"
			if recycled != tc.wantRecycled {
				t.Fatalf("legacy recycled = %v, want %v (stderr=%s)", recycled, tc.wantRecycled, env.stderr.String())
			}
			if running := env.sp.IsRunning(sessionName); running == tc.wantRecycled {
				t.Fatalf("session running = %v, want %v", running, !tc.wantRecycled)
			}
		})
	}
}
