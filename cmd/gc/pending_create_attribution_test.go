package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// errIdentityReadFailed is a transient GetMeta failure: neither a missing
// metadata store nor a missing session.
var errIdentityReadFailed = errors.New("apiserver unreachable")

// failIdentityReads makes GetMeta of each key under name fail transiently.
func failIdentityReads(sp *runtime.Fake, name string, keys ...string) {
	if sp.GetMetaErrors[name] == nil {
		sp.GetMetaErrors[name] = map[string]error{}
	}
	for _, key := range keys {
		sp.GetMetaErrors[name][key] = errIdentityReadFailed
	}
}

func TestAttributePendingCreateRuntime(t *testing.T) {
	const name = "worker"
	info := sessionpkg.Info{ID: "gc-1", SessionNameMetadata: name, Generation: "2", InstanceToken: "tok-1"}
	gone := fmt.Errorf("reading meta: %w", runtime.ErrSessionNotFound)
	tests := []struct {
		name   string
		info   sessionpkg.Info
		meta   map[string]string
		errs   map[string]error
		want   pendingCreateAttribution
		reason string
	}{
		{name: "token only", meta: map[string]string{"GC_INSTANCE_TOKEN": "tok-1"}, want: pendingCreateRuntimeOurs},
		{name: "id only", meta: map[string]string{"GC_SESSION_ID": "gc-1"}, want: pendingCreateRuntimeOurs},
		{name: "id and token", meta: map[string]string{"GC_SESSION_ID": "gc-1", "GC_INSTANCE_TOKEN": "tok-1"}, want: pendingCreateRuntimeOurs},
		{
			name: "id match, token unreadable", meta: map[string]string{"GC_SESSION_ID": "gc-1"},
			errs: map[string]error{"GC_INSTANCE_TOKEN": errIdentityReadFailed}, want: pendingCreateRuntimeUnknown,
			reason: "a newer incarnation of the same bead carries the same ID",
		},
		{
			name: "id match, token unreadable, generation matches", meta: map[string]string{"GC_SESSION_ID": "gc-1", "GC_RUNTIME_EPOCH": "2"},
			errs: map[string]error{"GC_INSTANCE_TOKEN": errIdentityReadFailed}, want: pendingCreateRuntimeOurs,
			reason: "preWakeCommit bumps the generation with the token, so a re-woken incarnation carries another",
		},
		{
			name: "id match, token unreadable, generation conflicts", meta: map[string]string{"GC_SESSION_ID": "gc-1", "GC_RUNTIME_EPOCH": "3"},
			errs: map[string]error{"GC_INSTANCE_TOKEN": errIdentityReadFailed}, want: pendingCreateRuntimeUnknown,
		},
		{
			name: "id match, token and generation unreadable", meta: map[string]string{"GC_SESSION_ID": "gc-1"},
			errs: map[string]error{"GC_INSTANCE_TOKEN": errIdentityReadFailed, "GC_RUNTIME_EPOCH": errIdentityReadFailed}, want: pendingCreateRuntimeUnknown,
		},
		{
			name: "id unreadable, token unreadable, generation matches", meta: map[string]string{"GC_RUNTIME_EPOCH": "2"},
			errs: map[string]error{"GC_SESSION_ID": errIdentityReadFailed, "GC_INSTANCE_TOKEN": errIdentityReadFailed}, want: pendingCreateRuntimeUnknown,
		},
		{
			name: "both unreadable", errs: map[string]error{"GC_SESSION_ID": errIdentityReadFailed, "GC_INSTANCE_TOKEN": errIdentityReadFailed},
			want: pendingCreateRuntimeUnknown,
		},
		{
			name: "id unreadable, token matches", meta: map[string]string{"GC_INSTANCE_TOKEN": "tok-1"},
			errs: map[string]error{"GC_SESSION_ID": errIdentityReadFailed}, want: pendingCreateRuntimeOurs,
		},
		{
			name: "id unreadable, token unset", errs: map[string]error{"GC_SESSION_ID": errIdentityReadFailed},
			want: pendingCreateRuntimeUnknown,
		},
		{
			name: "id unreadable, token mismatch", meta: map[string]string{"GC_INSTANCE_TOKEN": "tok-2"},
			errs: map[string]error{"GC_SESSION_ID": errIdentityReadFailed}, want: pendingCreateRuntimeUnknown,
			reason: "a matching ID would make a non-conflicting token drift ours",
		},
		{
			name: "id unreadable, token mismatch, generation conflicts", meta: map[string]string{"GC_INSTANCE_TOKEN": "tok-2", "GC_RUNTIME_EPOCH": "3"},
			errs: map[string]error{"GC_SESSION_ID": errIdentityReadFailed}, want: pendingCreateRuntimeForeign,
		},
		{
			name: "other id, token unreadable", meta: map[string]string{"GC_SESSION_ID": "gc-9"},
			errs: map[string]error{"GC_INSTANCE_TOKEN": errIdentityReadFailed}, want: pendingCreateRuntimeForeign,
		},
		{name: "nothing recorded", want: pendingCreateRuntimeForeign},
		{
			name: "no id, no expected token, token unreadable", info: sessionpkg.Info{ID: "gc-1", SessionNameMetadata: name},
			errs: map[string]error{"GC_INSTANCE_TOKEN": errIdentityReadFailed}, want: pendingCreateRuntimeForeign,
		},
		{name: "id read gone", errs: map[string]error{"GC_SESSION_ID": gone}, want: pendingCreateRuntimeForeign},
		{name: "token read gone", meta: map[string]string{"GC_SESSION_ID": "gc-1"}, errs: map[string]error{"GC_INSTANCE_TOKEN": gone}, want: pendingCreateRuntimeForeign},
		{
			name: "no metadata store", errs: map[string]error{"GC_SESSION_ID": runtime.ErrMetaUnsupported, "GC_INSTANCE_TOKEN": runtime.ErrMetaUnsupported},
			want: pendingCreateRuntimeForeign,
		},
		{
			name: "gone-looking text without the sentinel", meta: map[string]string{"GC_SESSION_ID": "gc-1"},
			errs: map[string]error{"GC_INSTANCE_TOKEN": errors.New("session not found in cache")}, want: pendingCreateRuntimeUnknown,
			reason: "classification uses errors.Is, never message matching",
		},
		{name: "id match, token drift, same generation", meta: map[string]string{"GC_SESSION_ID": "gc-1", "GC_INSTANCE_TOKEN": "tok-2", "GC_RUNTIME_EPOCH": "2"}, want: pendingCreateRuntimeOurs},
		{name: "id match, token drift, generation conflicts", meta: map[string]string{"GC_SESSION_ID": "gc-1", "GC_INSTANCE_TOKEN": "tok-2", "GC_RUNTIME_EPOCH": "3"}, want: pendingCreateRuntimeForeign},
		{
			name: "id match, token drift, generation unreadable", meta: map[string]string{"GC_SESSION_ID": "gc-1", "GC_INSTANCE_TOKEN": "tok-2"},
			errs: map[string]error{"GC_RUNTIME_EPOCH": errIdentityReadFailed}, want: pendingCreateRuntimeUnknown,
		},
		{name: "no id, token mismatch", meta: map[string]string{"GC_INSTANCE_TOKEN": "tok-2"}, want: pendingCreateRuntimeForeign},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := runtime.NewFake()
			for key, value := range tt.meta {
				if err := sp.SetMeta(name, key, value); err != nil {
					t.Fatal(err)
				}
			}
			sp.GetMetaErrors[name] = tt.errs
			subject := info
			if tt.info.ID != "" {
				subject = tt.info
			}
			if got := attributePendingCreateRuntime(subject, name, sp); got != tt.want {
				t.Fatalf("attribution = %d, want %d %s", got, tt.want, tt.reason)
			}
		})
	}
	if got := attributePendingCreateRuntime(info, name, nil); got != pendingCreateRuntimeForeign {
		t.Fatalf("nil provider attribution = %d, want foreign", got)
	}
}

// TestStaleAsyncStartRuntimeAttribution_BeadScopedPoolRow pins ga-vcjr9: a
// bead-scoped pool runtime with no attribution is still stoppable, a positively
// newer incarnation is not, and one whose identity cannot be read is unknown.
func TestStaleAsyncStartRuntimeAttribution_BeadScopedPoolRow(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 0, 1, 0, time.UTC)
	tests := []struct {
		name string
		meta map[string]string
		errs map[string]error
		want pendingCreateAttribution
	}{
		{name: "unset attribution", want: pendingCreateRuntimeOurs},
		{
			name: "no metadata store", errs: map[string]error{"GC_SESSION_ID": runtime.ErrMetaUnsupported, "GC_INSTANCE_TOKEN": runtime.ErrMetaUnsupported},
			want: pendingCreateRuntimeOurs,
		},
		{name: "newer instance token", meta: map[string]string{"GC_INSTANCE_TOKEN": "newer-generation-token"}, want: pendingCreateRuntimeForeign},
		{name: "token unreadable", errs: map[string]error{"GC_INSTANCE_TOKEN": errIdentityReadFailed}, want: pendingCreateRuntimeUnknown},
		{name: "id unreadable", errs: map[string]error{"GC_SESSION_ID": errIdentityReadFailed}, want: pendingCreateRuntimeUnknown},
		{
			name: "id unreadable, newer token", meta: map[string]string{"GC_INSTANCE_TOKEN": "newer-generation-token"},
			errs: map[string]error{"GC_SESSION_ID": errIdentityReadFailed}, want: pendingCreateRuntimeUnknown,
		},
		{name: "own id, token unset", meta: map[string]string{"GC_SESSION_ID": "own"}, want: pendingCreateRuntimeOurs},
		{name: "other id", meta: map[string]string{"GC_SESSION_ID": "gc-other"}, want: pendingCreateRuntimeForeign},
		{name: "gone", errs: map[string]error{"GC_SESSION_ID": fmt.Errorf("x: %w", runtime.ErrSessionNotFound)}, want: pendingCreateRuntimeForeign},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := runtime.NewFake()
			info := createBeadScopedPoolRowWithBox(t, beads.NewMemStore(), sp, now)
			name := info.SessionNameMetadata
			for key, value := range tt.meta {
				if value == "own" {
					value = info.ID
				}
				if err := sp.SetMeta(name, key, value); err != nil {
					t.Fatal(err)
				}
			}
			sp.GetMetaErrors[name] = tt.errs
			if got := staleAsyncStartRuntimeAttribution(info, name, sp); got != tt.want {
				t.Fatalf("attribution = %d, want %d", got, tt.want)
			}
		})
	}
	if got := staleAsyncStartRuntimeAttribution(sessionpkg.Info{ID: "gc-1"}, "worker", nil); got != pendingCreateRuntimeForeign {
		t.Fatalf("nil provider attribution = %d, want foreign", got)
	}
}

// TestCommitAsyncStart_StaleResultSparesRewokenIncarnationWithUnreadableToken
// covers a late async-start result arriving after the bead was re-woken: the
// box under the bead-scoped name is the newer incarnation (same GC_SESSION_ID,
// rotated token), and its token read fails transiently. The stale cleanup must
// not stop it.
func TestCommitAsyncStart_StaleResultSparesRewokenIncarnationWithUnreadableToken(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 0, 1, 0, time.UTC)
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	info := createBeadScopedPoolRowWithBox(t, store, sp, now)
	name := info.SessionNameMetadata
	if err := store.SetMetadata(info.ID, "instance_token", "rewoken-token"); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta(name, "GC_SESSION_ID", info.ID); err != nil {
		t.Fatal(err)
	}
	failIdentityReads(sp, name, "GC_INSTANCE_TOKEN")
	var stderr strings.Builder

	if commitAsyncStartResultWithContext(context.Background(), successfulPoolStartResult(info, now), sp, store, &clock.Fake{Time: now}, events.Discard, 0, io.Discard, &stderr, nil) {
		t.Fatal("stale async start against a re-woken row reported committed")
	}
	if !sp.IsRunning(name) {
		t.Fatalf("re-woken incarnation %q was stopped by a stale start result although its token was unreadable", name)
	}
	if !strings.Contains(stderr.String(), "attribution_unknown") {
		t.Fatalf("stderr = %q, want the attribution_unknown deferral", stderr.String())
	}
}

// TestCommitAsyncStart_SessionExistsWithUnknownAttributionDefers covers the
// async commit's converge-or-rollback decision: a failed start whose runtime
// cannot be attributed neither converges on it nor rolls back the create, which
// would stop the bead-scoped box by name.
func TestCommitAsyncStart_SessionExistsWithUnknownAttributionDefers(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 0, 1, 0, time.UTC)
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	info := createBeadScopedPoolRowWithBox(t, store, sp, now)
	name := info.SessionNameMetadata
	failIdentityReads(sp, name, "GC_SESSION_ID", "GC_INSTANCE_TOKEN")
	result := successfulPoolStartResult(info, now)
	result.err = fmt.Errorf("%w: session %q", runtime.ErrSessionExists, name)
	result.outcome = TraceOutcomeSessionExists
	result.rollbackPending = true
	result.provider = sp

	if commitAsyncStartResultWithContext(context.Background(), result, sp, store, &clock.Fake{Time: now}, events.Discard, 0, io.Discard, io.Discard, nil) {
		t.Fatal("unattributable session-exists result reported committed")
	}
	got, err := store.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "closed" || got.Metadata["pending_create_claim"] != boolMetadata(true) {
		t.Fatalf("row status=%q claim=%q, want the pending create kept", got.Status, got.Metadata["pending_create_claim"])
	}
	if !sp.IsRunning(name) {
		t.Fatalf("box %q was stopped although its identity could not be read", name)
	}
}

// TestCommitAsyncStart_UnknownAttributionKeepsRateLimitAndCapacityArms matches
// runPreparedStartCandidate: a rate-limit screen or a capacity refusal keeps its
// own commit arm even when the runtime cannot be attributed.
func TestCommitAsyncStart_UnknownAttributionKeepsRateLimitAndCapacityArms(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 0, 1, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		rateLimitScreen bool
		capacityRefused bool
	}{
		{name: "rate-limit screen", rateLimitScreen: true},
		{name: "capacity refused", capacityRefused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			info := createBeadScopedPoolRowWithBox(t, store, sp, now)
			name := info.SessionNameMetadata
			failIdentityReads(sp, name, "GC_SESSION_ID", "GC_INSTANCE_TOKEN")
			result := successfulPoolStartResult(info, now)
			result.err = errors.New("launcher exited")
			result.outcome = TraceOutcomeProviderError
			result.rollbackPending = true
			result.rateLimitScreen = tc.rateLimitScreen
			result.capacityRefused = tc.capacityRefused
			result.provider = sp
			var stderr strings.Builder

			commitAsyncStartResultWithContext(context.Background(), result, sp, store, &clock.Fake{Time: now}, events.Discard, 0, io.Discard, &stderr, nil)
			if strings.Contains(stderr.String(), "outcome="+string(TraceOutcomeDeferred)) {
				t.Fatalf("stderr = %q, want the %s arm, not the attribution deferral", stderr.String(), tc.name)
			}
			got, err := store.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.rateLimitScreen && got.Metadata["sleep_reason"] != string(sessionpkg.SleepReasonRateLimit):
				t.Fatalf("sleep_reason = %q, want the rate-limit hold", got.Metadata["sleep_reason"])
			case tc.capacityRefused && !strings.Contains(stderr.String(), "endpoint refused (capacity)"):
				t.Fatalf("stderr = %q, want the capacity refusal", stderr.String())
			}
		})
	}
}

// TestExecutePreparedStartWave_StartErrorWithUnknownAttributionDefers is the
// synchronous twin: runPreparedStartCandidate defers instead of handing the
// failure to the rollback.
func TestExecutePreparedStartWave_StartErrorWithUnknownAttributionDefers(t *testing.T) {
	city := t.TempDir()
	sp := runtime.NewFake()
	sp.StartErrors["worker"] = errors.New("launcher exited")
	failIdentityReads(sp, "worker", "GC_SESSION_ID", "GC_INSTANCE_TOKEN")
	item := unattributedWorkerStart()

	results := executePreparedStartWaveForCity(context.Background(), []preparedStart{item}, city, sp, nil, nil, 10*time.Second, 1)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if sp.CountCalls("Start", "worker") != 1 {
		t.Fatalf("Start calls = %+v, want the injected start error reached", sp.SnapshotCalls())
	}
	if r := results[0]; r.err != nil || r.rollbackPending || r.outcome != TraceOutcomeDeferred {
		t.Fatalf("result err=%v rollbackPending=%v outcome=%q, want a deferral", r.err, r.rollbackPending, r.outcome)
	}
	if r := results[0]; !errors.Is(r.unattributedErr, sp.StartErrors["worker"]) || !strings.Contains(r.unattributedErr.Error(), "GC_SESSION_ID: apiserver unreachable") {
		t.Fatalf("unattributedErr = %v, want the start error with the failed identity reads", r.unattributedErr)
	}

	delete(sp.GetMetaErrors, "worker")
	results = executePreparedStartWaveForCity(context.Background(), []preparedStart{item}, city, sp, nil, nil, 10*time.Second, 1)
	if r := results[0]; r.err == nil || !r.rollbackPending {
		t.Fatalf("readable, unattributed runtime: err=%v rollbackPending=%v, want the rollback", r.err, r.rollbackPending)
	}
}

// unattributedWorkerStart is a pending create of "worker" that reaches provider
// Start (item.cfg.Command is set).
func unattributedWorkerStart() preparedStart {
	return preparedStart{
		candidate: startCandidate{
			info: sessionpkg.Info{ID: "gc-1", SessionNameMetadata: "worker", InstanceToken: "tok-1", Generation: "2", PendingCreateClaim: true},
			tp:   TemplateParams{Command: "true", SessionName: "worker", TemplateName: "worker"},
		},
		cfg: runtime.Config{Command: "true"},
	}
}

// TestExecutePreparedStartWave_TimedOutStartWithUnknownAttributionDefers pins
// the order of runPreparedStartCandidate's outcome arms: a timed-out start is
// handed to the rollback too, so the unknown-attribution deferral must win over
// the deadline arm.
func TestExecutePreparedStartWave_TimedOutStartWithUnknownAttributionDefers(t *testing.T) {
	sp := runtime.NewFake()
	sp.StartErrors["worker"] = fmt.Errorf("waiting for readiness: %w", context.DeadlineExceeded)
	failIdentityReads(sp, "worker", "GC_SESSION_ID", "GC_INSTANCE_TOKEN")

	results := executePreparedStartWaveForCity(context.Background(), []preparedStart{unattributedWorkerStart()}, t.TempDir(), sp, nil, nil, time.Nanosecond, 1)
	if r := results[0]; r.err != nil || r.rollbackPending || r.outcome != TraceOutcomeDeferred {
		t.Fatalf("result err=%v rollbackPending=%v outcome=%q, want a deferral", r.err, r.rollbackPending, r.outcome)
	}
}

// TestCommitStartResult_UnattributedStartFailureAccruesStartupHealth shows the
// deferral spares only the runtime: a provider Start that failed still accrues
// the startup-health episode and quarantines the name after
// defaultMaxWakeAttempts, and the deferral logs the start error with the failed
// identity reads.
func TestCommitStartResult_UnattributedStartFailureAccruesStartupHealth(t *testing.T) {
	city := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 8, 15, 0, 0, 1, 0, time.UTC)}
	sp := runtime.NewFake()
	sp.StartErrors["worker"] = errors.New("launcher exited")
	failIdentityReads(sp, "worker", "GC_SESSION_ID", "GC_INSTANCE_TOKEN")
	row, err := store.Create(beads.Bead{Title: "worker", Type: sessionpkg.BeadType})
	if err != nil {
		t.Fatal(err)
	}
	item := unattributedWorkerStart()
	item.candidate.info.ID = row.ID
	var stderr strings.Builder

	for i := 0; i < defaultMaxWakeAttempts; i++ {
		result := executePreparedStartWaveForCity(context.Background(), []preparedStart{item}, city, sp, nil, nil, 10*time.Second, 1)[0]
		if result.outcome != TraceOutcomeDeferred {
			t.Fatalf("attempt %d: outcome = %q, want deferred", i+1, result.outcome)
		}
		commitStartResultTraced(result, sessionFrontDoor(store), clk, events.Discard, 0, io.Discard, &stderr, nil)
		clk.Advance(time.Minute)
	}
	episode, err := sessionFrontDoor(store).LoadStartupHealthEpisode("worker")
	if err != nil {
		t.Fatal(err)
	}
	if episode.ConsecutiveCount != defaultMaxWakeAttempts || episode.QuarantinedUntil.IsZero() {
		t.Fatalf("episode count=%d quarantined_until=%v, want %d failures and a quarantine", episode.ConsecutiveCount, episode.QuarantinedUntil, defaultMaxWakeAttempts)
	}
	if !strings.Contains(stderr.String(), "launcher exited (attribution_unknown: GC_SESSION_ID: apiserver unreachable; GC_INSTANCE_TOKEN: apiserver unreachable)") {
		t.Fatalf("stderr = %q, want the deferred start error with the failed identity reads", stderr.String())
	}

	// A collision with an existing session is not a failed start.
	collided := item
	collided.candidate.info.SessionNameMetadata = "other"
	collided.candidate.tp.SessionName = "other"
	sp.StartErrors["other"] = fmt.Errorf("%w: session %q", runtime.ErrSessionExists, "other")
	failIdentityReads(sp, "other", "GC_SESSION_ID", "GC_INSTANCE_TOKEN")
	result := executePreparedStartWaveForCity(context.Background(), []preparedStart{collided}, city, sp, nil, nil, 10*time.Second, 1)[0]
	if result.outcome != TraceOutcomeDeferred {
		t.Fatalf("collision outcome = %q, want deferred", result.outcome)
	}
	commitStartResultTraced(result, sessionFrontDoor(store), clk, events.Discard, 0, io.Discard, io.Discard, nil)
	if n := startupHealthCount(t, store, "other"); n != 0 {
		t.Fatalf("collision startup-health count = %d, want 0", n)
	}
}

// TestExecutePreparedStartWave_LiveUnattributableRuntimeDefers covers the warm
// path: a live runtime under the name whose identity cannot be read is neither
// reused as ours nor handed to the rollback as an ErrSessionExists failure.
func TestExecutePreparedStartWave_LiveUnattributableRuntimeDefers(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	failIdentityReads(sp, "worker", "GC_SESSION_ID", "GC_INSTANCE_TOKEN")
	item := preparedStart{candidate: startCandidate{
		info: sessionpkg.Info{ID: "gc-1", SessionNameMetadata: "worker", InstanceToken: "tok-1", PendingCreateClaim: true},
		tp:   TemplateParams{Command: "true", SessionName: "worker", TemplateName: "worker"},
	}}

	results := executePreparedStartWaveForCity(context.Background(), []preparedStart{item}, t.TempDir(), sp, nil, nil, 10*time.Second, 1)
	if r := results[0]; r.err != nil || r.rollbackPending || r.outcome != TraceOutcomeDeferred {
		t.Fatalf("result err=%v rollbackPending=%v outcome=%q, want a deferral", r.err, r.rollbackPending, r.outcome)
	}
	if !sp.IsRunning("worker") {
		t.Fatal("live runtime was stopped although its identity could not be read")
	}
}

// TestPendingCreateRuntimeClearedForRollback_UnknownBeadScopedRuntimeKept
// covers the drift rollback: a bead-scoped box whose identity cannot be read
// is not stopped, and with it still listed the rollback is refused.
func TestPendingCreateRuntimeClearedForRollback_UnknownBeadScopedRuntimeKept(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 0, 1, 0, time.UTC)
	sp := runtime.NewFake()
	info := createBeadScopedPoolRowWithBox(t, beads.NewMemStore(), sp, now)
	failIdentityReads(sp, info.SessionNameMetadata, "GC_INSTANCE_TOKEN")

	if pendingCreateRuntimeClearedForRollback(successfulPoolStartResult(info, now), sp, io.Discard) {
		t.Fatal("rollback cleared although the live box could not be attributed")
	}
	if !sp.IsRunning(info.SessionNameMetadata) {
		t.Fatalf("box %q was stopped although its identity could not be read", info.SessionNameMetadata)
	}
}

// newAlivePendingCreateEnv seeds a desired pending create whose runtime is up
// but was started outside the reconciler. Its lease is 30 minutes old, so
// anything past the alive rollback branch treats it as stuck.
func newAlivePendingCreateEnv(t *testing.T) (*reconcilerTestEnv, beads.Bead) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	leasedAt := env.clk.Now().Add(-30 * time.Minute)
	env.setSessionMetadata(&session, map[string]string{
		"state":                     "creating",
		"pending_create_claim":      "true",
		"pending_create_started_at": pendingCreateStartedAtNow(leasedAt),
		"last_woke_at":              leasedAt.UTC().Format(time.RFC3339),
	})
	return env, session
}

// reconcileTraced is env.reconcile with a trace cycle for "worker".
func reconcileTraced(env *reconcilerTestEnv, sessions []beads.Bead, trace *sessionReconcilerTraceCycle) {
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeadsTraced(
		context.Background(), env.city, sessions, env.desiredState, cfgNames, env.cfg, env.sp,
		env.store, nil, nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, trace,
		env.startOptions...,
	)
}

func TestReconcileSessionBeads_AlivePendingCreateRollbackDefersOnUnknownAttribution(t *testing.T) {
	env, session := newAlivePendingCreateEnv(t)
	failIdentityReads(env.sp, "worker", "GC_SESSION_ID", "GC_INSTANCE_TOKEN")

	for tick := 1; tick <= 2; tick++ {
		trace := newPoolDesiredStateTestTrace("worker")
		reconcileTraced(env, []beads.Bead{session}, trace)
		got, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "closed" || got.Metadata["pending_create_claim"] != "true" {
			t.Fatalf("tick %d: status=%q claim=%q, want the pending create kept while its runtime is unattributable", tick, got.Status, got.Metadata["pending_create_claim"])
		}
		if !env.sp.IsRunning("worker") {
			t.Fatalf("tick %d: unattributable runtime was stopped", tick)
		}
		deferred := 0
		for _, r := range trace.records {
			if r.SiteCode == TraceSiteReconcilerPendingCreate && r.ReasonCode == TraceReasonPendingCreateRollback &&
				r.OutcomeCode == TraceOutcomeDeferred && r.Fields["attribution"] == "unknown" {
				deferred++
			}
		}
		if deferred != 1 {
			t.Fatalf("tick %d: deferred pending-create rollback records = %d, want 1; records=%+v", tick, deferred, trace.records)
		}
	}
	if n := strings.Count(env.stderr.String(), "attribution_unknown"); n != 1 {
		t.Fatalf("attribution_unknown lines = %d, want 1 (transition only); stderr=%s", n, env.stderr.String())
	}

	delete(env.sp.GetMetaErrors, "worker")
	env.reconcile([]beads.Bead{session})
	if got, _ := env.store.Get(session.ID); got.Status != "closed" {
		t.Fatalf("readable, unattributed runtime: status=%q, want rolled back", got.Status)
	}
}

// TestReconcileSessionBeads_GonePendingCreateRollsBackDespiteUnreadableIdentity
// shows the deferral is bounded: once liveness reports the runtime gone, the
// lease-expired rollback applies whatever the identity reads say.
func TestReconcileSessionBeads_GonePendingCreateRollsBackDespiteUnreadableIdentity(t *testing.T) {
	env, session := newAlivePendingCreateEnv(t)
	failIdentityReads(env.sp, "worker", "GC_SESSION_ID", "GC_INSTANCE_TOKEN")
	env.reconcile([]beads.Bead{session})
	if got, _ := env.store.Get(session.ID); got.Status == "closed" {
		t.Fatal("fixture: alive unattributable pending create rolled back")
	}

	if err := env.sp.Stop("worker"); err != nil {
		t.Fatal(err)
	}
	session, _ = env.store.Get(session.ID)
	env.reconcile([]beads.Bead{session})
	if got, _ := env.store.Get(session.ID); got.Status != "closed" {
		t.Fatalf("gone runtime: status=%q, want the lease-expired rollback", got.Status)
	}
}
