package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// createReplacementPendingCreateBead mints a new bead sharing the harness's
// current session name, simulating a reconciler-materialized replacement
// after the prior bead closed as failed-create (ga-o04bfr.1.1 requires the
// episode to accrue across distinct replacement bead IDs).
//
// This cannot go through session.Manager.CreateSession: h.sessionName is
// always the auto-derived, "s-"-prefixed name from the original
// createSessionIntent() call, and ValidateExplicitName permanently rejects
// that reserved prefix for a caller-supplied ExplicitName (by design, so
// external callers can't spoof an auto-generated identity). Passing the
// name via ExtraMeta instead doesn't work either: createBeadOnly's
// auto-name-derivation unconditionally overwrites "session_name" whenever
// ExplicitName is empty. So the replacement bead is built directly against
// the store, mirroring the exact metadata shape createBeadOnly produces for
// an auto-named bead-only create (see internal/session/manager.go), with
// session_name forced to the reused value.
func (h *sessionChaosHarness) createReplacementPendingCreateBead() {
	h.t.Helper()
	now := h.env.clk.Now().UTC()
	created, err := h.env.store.Create(beads.Bead{
		Title: "Chaos worker",
		Type:  sessionpkg.BeadType,
		Labels: []string{
			sessionpkg.LabelSession,
			"template:" + h.template,
		},
		Metadata: map[string]string{
			"template":                  h.template,
			"state":                     string(sessionpkg.StateStartPending),
			"provider":                  "fake",
			"work_dir":                  "",
			"command":                   h.command,
			"resume_flag":               "",
			"resume_style":              "",
			"resume_command":            "",
			"session_id_flag":           "",
			"generation":                fmt.Sprintf("%d", sessionpkg.DefaultGeneration),
			"continuation_epoch":        fmt.Sprintf("%d", sessionpkg.DefaultContinuationEpoch),
			"instance_token":            sessionpkg.NewInstanceToken(),
			"pending_create_claim":      "true",
			"pending_create_started_at": now.Format(time.RFC3339),
			"session_origin":            "ephemeral",
			"session_name":              h.sessionName,
		},
	})
	if err != nil {
		h.t.Fatalf("creating replacement pending-create bead: %v", err)
	}
	h.sessionID = created.ID
	h.setDesired(true)
}

func TestPendingCreateFailuresAccrueStartupHealthEpisodeAcrossReplacementBeads(t *testing.T) {
	h := newSessionChaosHarness(t, 20260830001)
	h.createSessionIntent()
	h.assertCreatingIntent()
	sessionName := h.sessionName

	h.env.sp.StartErrors[sessionName] = errors.New("provider start failure")
	for i := 0; i < defaultMaxWakeAttempts; i++ {
		if i > 0 {
			h.createReplacementPendingCreateBead()
		}
		if tick := runDesiredPendingCreateTicks(t, h, 30); tick == -1 {
			t.Fatalf("failure %d: pending-create claim never released within 30 ticks", i+1)
		}
	}
	delete(h.env.sp.StartErrors, sessionName)

	is := sessionpkg.NewStore(beads.SessionStore{Store: h.env.store})
	episode, err := is.LoadStartupHealthEpisode(sessionName)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if episode.ConsecutiveCount != defaultMaxWakeAttempts {
		t.Errorf("ConsecutiveCount = %d, want %d", episode.ConsecutiveCount, defaultMaxWakeAttempts)
	}
	if episode.QuarantinedUntil.IsZero() {
		t.Error("QuarantinedUntil is zero, want set after reaching the failure threshold")
	}

	bead := h.mustBead()
	if got := bead.Metadata["wake_attempts"]; got != "" {
		t.Errorf("wake_attempts = %q, want empty (pending-create lane must not touch the wake-failure lane)", got)
	}
	if got := bead.Metadata["churn_count"]; got != "" {
		t.Errorf("churn_count = %q, want empty (pending-create lane must not touch the churn lane)", got)
	}

	restarted, err := is.LoadStartupHealthEpisode(sessionName)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode (simulated restart re-read): %v", err)
	}
	if restarted != episode {
		t.Errorf("episode after simulated restart = %+v, want unchanged %+v", restarted, episode)
	}
}

func TestQuarantinedStartupHealthBlocksProviderStartUntilExpiry(t *testing.T) {
	h := newSessionChaosHarness(t, 20260830002)
	h.createSessionIntent()
	h.assertCreatingIntent()
	sessionName := h.sessionName

	h.env.sp.StartErrors[sessionName] = errors.New("provider start failure")
	for i := 0; i < defaultMaxWakeAttempts; i++ {
		if i > 0 {
			h.createReplacementPendingCreateBead()
		}
		if tick := runDesiredPendingCreateTicks(t, h, 30); tick == -1 {
			t.Fatalf("failure %d: pending-create claim never released within 30 ticks", i+1)
		}
	}

	is := sessionpkg.NewStore(beads.SessionStore{Store: h.env.store})
	episode, err := is.LoadStartupHealthEpisode(sessionName)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if episode.QuarantinedUntil.IsZero() {
		t.Fatal("episode not quarantined after reaching the failure threshold; cannot test quarantine gating")
	}

	// The 5th failure's bead already rolled back to closed/failed-create (like
	// every prior one), so no open bead remains for this session name.
	// reconcileTick (unlike production's syncSessionBeads) never materializes a
	// replacement on its own — simulate the one production would create for a
	// still-desired name, exactly as the loop above does for failures 2-5, so
	// there is a live candidate to exercise the quarantine gate against.
	h.createReplacementPendingCreateBead()
	delete(h.env.sp.StartErrors, sessionName)
	startsBefore := h.countRuntimeCalls("Start")
	for i := 0; i < 5; i++ {
		h.reconcileTick()
	}
	if got := h.countRuntimeCalls("Start"); got != startsBefore {
		t.Fatalf("Start called %d more time(s) before quarantine expiry; want 0 (quarantine must block retry)", got-startsBefore)
	}

	h.env.clk.Advance(episode.QuarantinedUntil.Add(time.Second).Sub(h.env.clk.Now()))
	h.reconcileTick()
	if got := h.countRuntimeCalls("Start"); got <= startsBefore {
		t.Fatalf("Start not attempted after quarantine expiry (calls before=%d after=%d)", startsBefore, got)
	}
}

func TestQuarantinedStartupHealthMirrorsCountAndKindOntoVisibleSessionRow(t *testing.T) {
	h := newSessionChaosHarness(t, 20260830005)
	h.createSessionIntent()
	h.assertCreatingIntent()
	sessionName := h.sessionName

	h.env.sp.StartErrors[sessionName] = errors.New("provider start failure")
	for i := 0; i < defaultMaxWakeAttempts; i++ {
		if i > 0 {
			h.createReplacementPendingCreateBead()
		}
		if tick := runDesiredPendingCreateTicks(t, h, 30); tick == -1 {
			t.Fatalf("failure %d: pending-create claim never released within 30 ticks", i+1)
		}
	}

	is := sessionpkg.NewStore(beads.SessionStore{Store: h.env.store})
	episode, err := is.LoadStartupHealthEpisode(sessionName)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if episode.QuarantinedUntil.IsZero() {
		t.Fatal("episode not quarantined after reaching the failure threshold; cannot test the mirror")
	}
	if episode.Kind != sessionpkg.FailureKindOther {
		t.Fatalf("test precondition: episode.Kind = %q, want %q (a plain injected error classifies as \"other\", not \"timeout\")", episode.Kind, sessionpkg.FailureKindOther)
	}

	// The 5th failure's bead already rolled back to closed/failed-create (like
	// every prior one), so no open bead remains for this session name. Create
	// a live candidate for the quarantine gate to evaluate and mirror onto,
	// exactly as the expiry test above does for the same reason.
	h.createReplacementPendingCreateBead()
	delete(h.env.sp.StartErrors, sessionName)
	h.reconcileTick()

	bead := h.mustBead()
	wantCount := strconv.Itoa(episode.ConsecutiveCount)
	if got := bead.Metadata[startupHealthActiveCountMetadataKey]; got != wantCount {
		t.Errorf("%s = %q, want %q (mirrored episode ConsecutiveCount)", startupHealthActiveCountMetadataKey, got, wantCount)
	}
	if got := bead.Metadata[startupHealthActiveKindMetadataKey]; got != string(episode.Kind) {
		t.Errorf("%s = %q, want %q (mirrored episode Kind)", startupHealthActiveKindMetadataKey, got, string(episode.Kind))
	}
}

func TestStartupHealthEpisodeClearsOnFirstSuccessfulStart(t *testing.T) {
	h := newSessionChaosHarness(t, 20260830003)
	h.createSessionIntent()
	h.assertCreatingIntent()
	sessionName := h.sessionName

	h.env.sp.StartErrors[sessionName] = errors.New("provider start failure")
	const belowThreshold = defaultMaxWakeAttempts - 2
	for i := 0; i < belowThreshold; i++ {
		if i > 0 {
			h.createReplacementPendingCreateBead()
		}
		if tick := runDesiredPendingCreateTicks(t, h, 30); tick == -1 {
			t.Fatalf("failure %d: pending-create claim never released within 30 ticks", i+1)
		}
	}

	is := sessionpkg.NewStore(beads.SessionStore{Store: h.env.store})
	before, err := is.LoadStartupHealthEpisode(sessionName)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if before.ConsecutiveCount != belowThreshold {
		t.Fatalf("ConsecutiveCount = %d, want %d before the successful start", before.ConsecutiveCount, belowThreshold)
	}

	h.createReplacementPendingCreateBead()
	delete(h.env.sp.StartErrors, sessionName)
	started := false
	for i := 0; i < 10; i++ {
		h.reconcileTick()
		if h.countRuntimeCalls("Start") > 0 {
			started = true
			break
		}
	}
	if !started {
		t.Fatal("Start was never attempted after clearing the injected error")
	}

	after, err := is.LoadStartupHealthEpisode(sessionName)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode after successful start: %v", err)
	}
	if after.ConsecutiveCount != 0 {
		t.Errorf("ConsecutiveCount after successful start = %d, want 0 (episode must clear on recovery)", after.ConsecutiveCount)
	}
}

func TestQuarantinedStartupHealthMirrorClearsOnSuccessfulRecovery(t *testing.T) {
	h := newSessionChaosHarness(t, 20260830006)
	h.createSessionIntent()
	h.assertCreatingIntent()
	sessionName := h.sessionName

	h.env.sp.StartErrors[sessionName] = errors.New("provider start failure")
	for i := 0; i < defaultMaxWakeAttempts; i++ {
		if i > 0 {
			h.createReplacementPendingCreateBead()
		}
		if tick := runDesiredPendingCreateTicks(t, h, 30); tick == -1 {
			t.Fatalf("failure %d: pending-create claim never released within 30 ticks", i+1)
		}
	}

	is := sessionpkg.NewStore(beads.SessionStore{Store: h.env.store})
	episode, err := is.LoadStartupHealthEpisode(sessionName)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if episode.QuarantinedUntil.IsZero() {
		t.Fatal("episode not quarantined after reaching the failure threshold; cannot test the mirror clear")
	}

	// A live candidate for the gate to mirror onto while still quarantined,
	// exactly as the mirror-write test above does for the same reason.
	h.createReplacementPendingCreateBead()
	delete(h.env.sp.StartErrors, sessionName)
	h.reconcileTick()
	if got := h.mustBead().Metadata[startupHealthActiveCountMetadataKey]; got == "" {
		t.Fatalf("test precondition: %s empty before quarantine expiry, want it mirrored first", startupHealthActiveCountMetadataKey)
	}

	h.env.clk.Advance(episode.QuarantinedUntil.Add(time.Second).Sub(h.env.clk.Now()))
	started := false
	for i := 0; i < 10; i++ {
		h.reconcileTick()
		if h.countRuntimeCalls("Start") > 0 {
			started = true
			break
		}
	}
	if !started {
		t.Fatal("Start was never attempted after quarantine expiry")
	}

	after := h.mustBead()
	if got := after.Metadata[startupHealthActiveCountMetadataKey]; got != "0" {
		t.Errorf("%s = %q after successful recovery, want \"0\" (mirror must clear alongside the episode)", startupHealthActiveCountMetadataKey, got)
	}
	if got := after.Metadata[startupHealthActiveKindMetadataKey]; got != "" {
		t.Errorf("%s = %q after successful recovery, want empty (mirror must clear alongside the episode)", startupHealthActiveKindMetadataKey, got)
	}
}

// failFirstStartupHealthLoadStore forces exactly the first ListByMetadata
// lookup filtered on the given session name's startup-health episode key to
// fail, then delegates every later matching call — including the sibling
// LoadStartupHealthEpisode calls in session_lifecycle_parallel.go's
// start-result commit paths — to the real store. Scoping the failure to only
// the first matching call isolates the assertion to the reconciler's own
// quarantine-gate lookup, which always runs before any commit-path lookup
// within a tick: a matching log line can then only be attributed to that
// call site, not to a sibling that already logs correctly today.
type failFirstStartupHealthLoadStore struct {
	beads.Store
	sessionName string
	err         error
	calls       int
	failed      bool
}

func (s *failFirstStartupHealthLoadStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if filters[sessionpkg.StartupHealthSessionNameMetadataKey] == s.sessionName {
		s.calls++
		if !s.failed {
			s.failed = true
			return nil, s.err
		}
	}
	return s.Store.ListByMetadata(filters, limit, opts...)
}

func TestQuarantineGateLogsOnStartupHealthLoadError(t *testing.T) {
	h := newSessionChaosHarness(t, 20260830004)
	h.createSessionIntent()
	h.assertCreatingIntent()
	sessionName := h.sessionName

	h.env.sp.StartErrors[sessionName] = errors.New("provider start failure")
	for i := 0; i < defaultMaxWakeAttempts; i++ {
		if i > 0 {
			h.createReplacementPendingCreateBead()
		}
		if tick := runDesiredPendingCreateTicks(t, h, 30); tick == -1 {
			t.Fatalf("failure %d: pending-create claim never released within 30 ticks", i+1)
		}
	}

	is := sessionpkg.NewStore(beads.SessionStore{Store: h.env.store})
	episode, err := is.LoadStartupHealthEpisode(sessionName)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if episode.QuarantinedUntil.IsZero() || !h.env.clk.Now().Before(episode.QuarantinedUntil) {
		t.Fatal("episode not actively quarantined after reaching the failure threshold; cannot test the quarantine-gate load path")
	}

	// A live candidate bead for the gate to evaluate, exactly as the
	// expiry test above creates for the same reason.
	h.createReplacementPendingCreateBead()
	delete(h.env.sp.StartErrors, sessionName)

	loadErr := errors.New("injected startup-health store fault")
	failing := &failFirstStartupHealthLoadStore{
		Store:       h.env.store,
		sessionName: sessionName,
		err:         loadErr,
	}
	h.env.store = failing
	h.env.stderr.Reset()

	startsBefore := h.countRuntimeCalls("Start")
	h.reconcileTickWithoutPostInvariants(false)

	if failing.calls < 1 {
		t.Fatal("ListByMetadata for the startup-health episode was never called; test does not exercise the quarantine gate")
	}
	// LoadStartupHealthEpisode wraps the store error before returning it
	// (internal/session/startup_health.go), so check the reconciler's own
	// log prefix and the root-cause error text independently rather than
	// hardcoding that wrapping's exact format.
	stderrText := h.env.stderr.String()
	wantPrefix := fmt.Sprintf("session reconciler: loading startup-health episode for %s:", sessionName)
	if !strings.Contains(stderrText, wantPrefix) {
		t.Fatalf("stderr = %q, want a line starting with %q (quarantine gate must log a LoadStartupHealthEpisode error instead of silently failing open)", stderrText, wantPrefix)
	}
	if !strings.Contains(stderrText, loadErr.Error()) {
		t.Fatalf("stderr = %q, want the injected error %q to appear in the logged line", stderrText, loadErr.Error())
	}
	if got := h.countRuntimeCalls("Start"); got <= startsBefore {
		t.Fatalf("Start not attempted after a startup-health load error (calls before=%d after=%d); fail-open must still let the retry through", startsBefore, got)
	}
}
