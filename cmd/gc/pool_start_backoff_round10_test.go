package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// The cases codex round 10 found (evidence 07-codex-r10.md), pinned on the
// restructured shape: no owed-reset marker, no sweep — the clear of the work
// bead's record runs BEFORE the batch that confirms the start, on the one
// commit path, and a clear that fails fails the commit; the bead a start
// ran for is its session's trigger, pinned while the start is in flight.

// TestConfirmedStartClearsTheRecordBeforeItConfirms: through the real
// reconciler, a start whose work-record clear is refused by the work store
// is NOT confirmed — the session bead keeps its pending-create claim and no
// creation_complete, the runtime keeps running, the record keeps its count.
// The next tick's pending-create recovery clears the record and only then
// confirms. Nothing about the clear lives in process memory or on a session
// key: a fresh policy (a restarted controller) over the same store finishes
// it.
func TestConfirmedStartClearsTheRecordBeforeItConfirms(t *testing.T) {
	h := newStartBackoffHarness(t, 5)
	if starts := h.advance(15*time.Second, errPreStartFailure); starts != 2 {
		t.Fatalf("starts = %d, want 2\nstderr:\n%s", starts, h.env.stderr.String())
	}
	if got := readWorkStartFailureState(h.reload().Metadata).Failures; got != 2 {
		t.Fatalf("record = %d, want 2", got)
	}
	h.env.clk.Time = h.env.clk.Time.Add(time.Minute)
	// The work store refuses the clear the confirming start owes.
	realWriter, _ := beads.ConditionalWriterFor(h.env.store)
	h.policy.resolveWriter = func(beads.Store) (beads.ConditionalWriter, error) {
		return round9FailingWriter{ConditionalWriter: realWriter, err: errors.New("work store: write timed out")}, nil
	}
	if !h.tick(nil) {
		t.Fatal("the successful start must be planned")
	}
	if got := readWorkStartFailureState(h.reload().Metadata).Failures; got != 2 {
		t.Fatalf("the refused clear must leave the record at 2, got %d", got)
	}
	open := round10OpenSessionInfos(t, h.env.store)
	if len(open) != 1 {
		t.Fatalf("one open session, got %d", len(open))
	}
	pending := open[0]
	if !pending.PendingCreateClaim || strings.TrimSpace(pending.CreationCompleteAt) != "" {
		t.Fatalf("a start whose clear failed must NOT be confirmed: %+v\nstderr:\n%s", pending, h.env.stderr.String())
	}
	if !strings.Contains(h.env.stderr.String(), "work_record_clear_failed") {
		t.Fatalf("the refused clear is said as the commit's outcome:\n%s", h.env.stderr.String())
	}
	name := pending.SessionName
	if !h.env.sp.IsRunning(name) {
		t.Fatalf("the runtime keeps running for %q while the commit waits\nstderr:\n%s", name, h.env.stderr.String())
	}
	// The live runtime answers for the bead it was started for (the tmux
	// adapter reports GC_SESSION_ID / GC_INSTANCE_TOKEN from the session's
	// environment; the fake's metadata is seeded the same way).
	for key, value := range map[string]string{"GC_SESSION_ID": pending.ID, "GC_INSTANCE_TOKEN": pending.InstanceToken} {
		if err := h.env.sp.SetMeta(name, key, value); err != nil {
			t.Fatal(err)
		}
	}
	// A restarted controller: a fresh policy, the store intact. The tick sees
	// an alive runtime under a pending-create claim and recovers it — the
	// clear first, then the confirmation.
	h.installPolicy()
	row, err := h.env.store.Get(pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.env.reconcileWithPoolDesired([]beads.Bead{row}, map[string]int{backoffHarnessTemplate: 1})
	if got := h.reload(); len(workStartFailureClearPatch(got.Metadata)) != 0 {
		t.Fatalf("recovery must clear the record before it confirms: %v\nstderr:\n%s", got.Metadata, h.env.stderr.String())
	}
	healed := round10OpenSessionInfos(t, h.env.store)
	if len(healed) != 1 || healed[0].PendingCreateClaim || strings.TrimSpace(healed[0].CreationCompleteAt) == "" {
		t.Fatalf("recovery must confirm the start once the clear landed: %+v\nstderr:\n%s", healed, h.env.stderr.String())
	}
	if !strings.Contains(h.env.stderr.String(), "start-failure record cleared") {
		t.Fatalf("the clear is logged:\n%s", h.env.stderr.String())
	}
	if len(h.mails) != 0 {
		t.Fatalf("no park, no mail: %d", len(h.mails))
	}
}

func round10OpenSessionInfos(t *testing.T, store beads.Store) []session.Info {
	t.Helper()
	rows, err := store.List(beads.ListQuery{Type: sessionBeadType})
	if err != nil {
		t.Fatal(err)
	}
	var open []beads.Bead
	for _, b := range rows {
		if b.Status != "closed" {
			open = append(open, b)
		}
	}
	return sessionInfosFromBeads(open)
}

// TestRecoverRunningPendingCreate_KeepsTheClaimWhenTheClearFails: the
// recovery path clears the trigger bead's record BEFORE it heals the claim;
// a clear the work store refuses leaves the claim in place (the next tick
// tries again), and the same recovery over a working store clears and heals.
func TestRecoverRunningPendingCreate_KeepsTheClaimWhenTheClearFails(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "helper", MaxActiveSessions: intPtr(1)}}}
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{Title: "helper work", Type: "task", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey:          "helper",
		beadmeta.StartFailuresMetadataKey:     "4",
		beadmeta.StartFailedAtMetadataKey:     "2026-09-12T02:00:00Z",
		beadmeta.StartFailureMetadataKey:      "boom",
		beadmeta.StartBackoffUntilMetadataKey: "2026-09-12T02:01:20Z",
	}})
	if err != nil {
		t.Fatal(err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":                    "sky",
			"pending_create_claim":            "true",
			"state":                           "active",
			"state_reason":                    "creation_complete",
			beadmeta.TriggerBeadIDMetadataKey: work.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tp := TemplateParams{SessionName: "sky", TemplateName: "helper"}
	clk := &clock.Fake{Time: time.Date(2026, 3, 18, 12, 0, 1, 0, time.UTC)}
	var stderr bytes.Buffer
	realWriter, _ := beads.ConditionalWriterFor(store)
	policy := &workStartFailurePolicy{
		workStore:         store,
		templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
		canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
		limitFor:          func(string) int { return 5 },
		resolveWriter: func(beads.Store) (beads.ConditionalWriter, error) {
			return round9FailingWriter{ConditionalWriter: realWriter, err: errors.New("work store: write timed out")}, nil
		},
		stderr: &stderr,
	}
	if ok, _ := recoverRunningPendingCreate(sessiontest.SeedBead(t, bead), tp, cfg, store, clk, nil, policy); ok {
		t.Fatal("a refused clear must not heal the claim")
	}
	row, _ := store.Get(bead.ID)
	if row.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("the claim must stay for the next tick, got %q", row.Metadata["pending_create_claim"])
	}
	if got, _ := store.Get(work.ID); readWorkStartFailureState(got.Metadata).Failures != 4 {
		t.Fatalf("the refused clear leaves the record: %v", got.Metadata)
	}
	// The store is back: the same recovery clears, then heals.
	policy.resolveWriter = nil
	if ok, _ := recoverRunningPendingCreate(sessiontest.SeedBead(t, row), tp, cfg, store, clk, nil, policy); !ok {
		t.Fatalf("recovery over a working store must heal\nstderr:\n%s", stderr.String())
	}
	if got, _ := store.Get(work.ID); len(workStartFailureClearPatch(got.Metadata)) != 0 {
		t.Fatalf("the record must be cleared: %v", got.Metadata)
	}
	if healed, _ := store.Get(bead.ID); healed.Metadata["pending_create_claim"] != "" {
		t.Fatalf("the claim must be healed once the clear landed: %q", healed.Metadata["pending_create_claim"])
	}
}

// TestRefreshConfiguredNamedStartCandidateCarriesTheTrigger: the refresh
// resolves the holder's template afresh and REPLACES its params; the
// trigger the build put on them (a mirror of the bead's persisted trigger)
// is not a template property and rides through.
func TestRefreshConfiguredNamedStartCandidateCarriesTheTrigger(t *testing.T) {
	resetSkillCatalogCache()
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"),
		[]byte("[pack]\nname = \"named-refresh-trigger-test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city", Provider: "claude"},
		Session:       config.SessionConfig{Provider: "tmux"},
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Providers:     map[string]config.ProviderSpec{"claude": {Command: "true", PromptMode: "none", SupportsACP: boolPtr(true)}},
		Agents:        []config.Agent{{Name: "mayor", Scope: "city", Provider: "claude"}},
		NamedSessions: []config.NamedSession{{Template: "mayor", Mode: "always"}},
	}
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               "mayor",
			"session_name_explicit":      boolMetadata(true),
			"template":                   "mayor",
			"agent_name":                 "mayor",
			"state":                      string(session.StateCreating),
			"pending_create_claim":       "true",
			namedSessionMetadataKey:      boolMetadata(true),
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "always",
			"continuation_epoch":         "1",
			"generation":                 "1",
			"instance_token":             session.NewInstanceToken(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := TemplateParams{
		TemplateName:        "mayor",
		SessionName:         "mayor",
		InstanceName:        "mayor",
		Command:             "true",
		WorkDir:             cityPath,
		TriggerBeadID:       "gp-work",
		TriggerBeadStoreRef: "rig:a",
	}
	refreshed := refreshConfiguredNamedStartCandidate(
		startCandidate{info: sessiontest.SeedBead(t, bead), tp: stale},
		cityPath, cfg.Workspace.Name, cfg, runtime.NewFake(), store,
		&clock.Fake{Time: time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)}, ioDiscard{},
	)
	if refreshed.tp.ConfiguredNamedIdentity != "mayor" {
		t.Fatalf("control: the refresh must have replaced the params (ConfiguredNamedIdentity = %q)", refreshed.tp.ConfiguredNamedIdentity)
	}
	if refreshed.tp.TriggerBeadID != "gp-work" || refreshed.tp.TriggerBeadStoreRef != "rig:a" {
		t.Fatalf("the refreshed params must carry the build's trigger, got %q/%q", refreshed.tp.TriggerBeadID, refreshed.tp.TriggerBeadStoreRef)
	}
}

// TestBuildDesiredState_NamedHolderTriggerIsPinnedWhileAStartIsInFlight: a
// holder whose start is in flight (its pending-create claim set) keeps the
// trigger that start ran for, even when the build now has other work for
// it — the failure charge and the pre-confirmation clear (recovery's
// workTriggerFromInfo) name the bead the start ran for, never a bead the
// build rebound meanwhile. Once the start has committed (the claim clear)
// the holder follows its wake request again.
func TestBuildDesiredState_NamedHolderTriggerIsPinnedWhileAStartIsInFlight(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
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
		w, err := store.Create(beads.Bead{Title: title, Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "solo"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Update(w.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &identity}); err != nil {
			t.Fatal(err)
		}
		return w
	}
	a := mkWork("work A: the start in flight ran for it")
	holder, err := store.Create(beads.Bead{
		Title:  "solo holder",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":                              "solo",
			"agent_name":                            identity,
			"alias":                                 identity,
			"session_name":                          "solo-holder",
			"state":                                 string(session.StateCreating),
			"pending_create_claim":                  "true",
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
	trigger := func() string {
		row, err := store.Get(holder.ID)
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
	// A is done and B is the holder's work now — while A's start is still
	// in flight.
	if err := store.Close(a.ID); err != nil {
		t.Fatal(err)
	}
	b := mkWork("work B: eligible demand now")
	var stderr bytes.Buffer
	res := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)
	if tp := desiredFor(res); tp.TriggerBeadID != a.ID || tp.TriggerBeadStoreRef != "city" {
		t.Fatalf("a start in flight pins the params' trigger to the bead it ran for (%s), got %q/%q\nstderr:\n%s", a.ID, tp.TriggerBeadID, tp.TriggerBeadStoreRef, stderr.String())
	}
	if got := trigger(); got != a.ID {
		t.Fatalf("a start in flight pins the holder's trigger to %s, got %q (B = %s)\nstderr:\n%s", a.ID, got, b.ID, stderr.String())
	}
	// The start committed: the claim is clear and the holder is active. The
	// build follows its wake request again — B.
	if err := store.SetMetadataBatch(holder.ID, map[string]string{"pending_create_claim": "", "state": string(session.StateActive)}); err != nil {
		t.Fatal(err)
	}
	res = buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)
	if tp := desiredFor(res); tp.TriggerBeadID != b.ID {
		t.Fatalf("between starts the build's verdict follows the wake request (%s), got %q\nstderr:\n%s", b.ID, tp.TriggerBeadID, stderr.String())
	}
	if got := trigger(); got != b.ID {
		t.Fatalf("between starts the holder is rebound to %s, got %q\nstderr:\n%s", b.ID, got, stderr.String())
	}
	// The pin is the in-flight predicate alone.
	for _, tc := range []struct {
		info session.Info
		want bool
	}{
		{session.Info{PendingCreateClaim: true}, true},
		{session.Info{State: session.StateCreating}, true},
		{session.Info{State: session.StateStartPending}, true},
		{session.Info{State: session.StateActive}, false},
		{session.Info{State: session.StateAsleep}, false},
	} {
		if got := startInFlightInfo(tc.info); got != tc.want {
			t.Fatalf("startInFlightInfo(%+v) = %v, want %v", tc.info, got, tc.want)
		}
	}
}

// TestWriteWorkRecordFencedReadsLiveBehindAStaleCache: with a conditional
// writer the record is computed from the LIVE backing row too. A cache that
// still serves "no record" while the backing holds four failures would
// otherwise compute an empty clear — no write, no fence, the four failures
// kept behind a confirmed start — and would charge a fifth failure as a
// first one. The fence is checked on the backing's revision, which is the
// row read.
func TestWriteWorkRecordFencedReadsLiveBehindAStaleCache(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1), MaxStartFailures: intPtr(5)}}}
	backing := beads.NewMemStore()
	caching := beads.NewCachingStoreForTest(backing, nil)
	work, err := caching.Create(beads.Bead{Title: "cached work", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := caching.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	four := map[string]string{beadmeta.StartFailuresMetadataKey: "4", beadmeta.StartFailedAtMetadataKey: time.Now().UTC().Format(time.RFC3339), beadmeta.StartFailureMetadataKey: "boom"}
	if err := backing.SetMetadataBatch(work.ID, four); err != nil {
		t.Fatal(err)
	}
	wrapped := &beadPolicyStore{Store: caching, cfg: cfg}
	if cached, _ := wrapped.Get(work.ID); readWorkStartFailureState(cached.Metadata).Failures != 0 {
		t.Fatalf("fixture: the cache must still serve the cleared row, got %v", cached.Metadata)
	}
	writer, ok := beads.ConditionalWriterFor(backing)
	if !ok {
		t.Fatal("fixture: the backing store fences")
	}
	var stderr bytes.Buffer
	policy := &workStartFailurePolicy{
		workStore:         wrapped,
		templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
		canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
		limitFor:          func(string) int { return 5 },
		resolveWriter:     func(beads.Store) (beads.ConditionalWriter, error) { return writer, nil }, // fenced
		stderr:            &stderr,
	}
	if !policy.recordStartSuccess(workTrigger{BeadID: work.ID, StoreRef: "city"}, "worker") {
		t.Fatalf("the fenced clear must settle\nstderr:\n%s", stderr.String())
	}
	row, err := backing.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(workStartFailureClearPatch(row.Metadata)) != 0 {
		t.Fatalf("the fenced clear must be computed from the backing and land there: %v\nstderr:\n%s", row.Metadata, stderr.String())
	}
	if !strings.Contains(stderr.String(), "record cleared") {
		t.Fatalf("the clear is reported once it landed:\n%s", stderr.String())
	}
	// The charge side, same stale cache: four on the backing, none cached —
	// the fifth failure parks, it is not a first failure.
	if err := backing.SetMetadataBatch(work.ID, four); err != nil {
		t.Fatal(err)
	}
	if cached, _ := wrapped.Get(work.ID); readWorkStartFailureState(cached.Metadata).Failures != 0 {
		t.Fatalf("fixture: the cache must still serve the cleared row, got %v", cached.Metadata)
	}
	policy.recordStartFailure(workTrigger{BeadID: work.ID, StoreRef: "city"}, "worker", errPreStartFailure, time.Now().UTC())
	row, _ = backing.Get(work.ID)
	if state := readWorkStartFailureState(row.Metadata); !state.Parked() || state.ParkFailures != 5 {
		t.Fatalf("the fenced charge must be computed from the live row (a fifth failure parks), got %+v\nstderr:\n%s", state, stderr.String())
	}
}

// round10ErrStore fails every read.
type round10ErrStore struct {
	beads.Store
	err error
}

func (s round10ErrStore) Get(string) (beads.Bead, error) { return beads.Bead{}, s.err }

// TestFindTriggerBeadExplicitStoreReadErrorIsNotAbsence: a trigger names
// its store; when that store FAILS to answer, the lookup is an error — it
// does not fall through to the sweep of every store, where a migration's
// retained copy would be found and charged (or cleared) in the active
// copy's place. A named store that answers not-found still falls back to
// the sweep, so a re-homed bead is found.
func TestFindTriggerBeadExplicitStoreReadErrorIsNotAbsence(t *testing.T) {
	const id = "gg-rehomed"
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
	retained := beads.NewMemStoreFrom(1, []beads.Bead{{ID: id, Title: "retained copy", Type: "task", Status: "in_progress", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}}}, nil)
	var stderr bytes.Buffer
	policy := &workStartFailurePolicy{
		workStore:         retained,
		rigStores:         map[string]beads.Store{"a": round10ErrStore{Store: beads.NewMemStore(), err: errors.New("rig store unavailable")}},
		templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
		canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
		limitFor:          func(string) int { return 5 },
		stderr:            &stderr,
	}
	if _, _, ok, err := policy.findTriggerBead(id, "rig:a"); ok || err == nil || !strings.Contains(err.Error(), `store "rig:a" named by the trigger`) || !strings.Contains(err.Error(), "rig store unavailable") {
		t.Fatalf("a named store that fails to answer is an error, not absence: ok=%v err=%v", ok, err)
	}
	policy.recordStartFailure(workTrigger{BeadID: id, StoreRef: "rig:a"}, "worker", errPreStartFailure, time.Now().UTC())
	if row, _ := retained.Get(id); readWorkStartFailureState(row.Metadata).Failures != 0 {
		t.Fatalf("the retained copy must not be charged in the active copy's place: %v", row.Metadata)
	}
	if !strings.Contains(stderr.String(), "could not be read") || !strings.Contains(stderr.String(), "rig store unavailable") {
		t.Fatalf("the dropped charge is said:\n%s", stderr.String())
	}
	if policy.recordStartSuccess(workTrigger{BeadID: id, StoreRef: "rig:a"}, "worker") {
		t.Fatal("a clear whose named store failed to answer is not settled")
	}
	// Not-found in the named store: the sweep finds the re-homed bead.
	policy.rigStores = map[string]beads.Store{"a": beads.NewMemStore()}
	policy.recordStartFailure(workTrigger{BeadID: id, StoreRef: "rig:a"}, "worker", errPreStartFailure, time.Now().UTC())
	if row, _ := retained.Get(id); readWorkStartFailureState(row.Metadata).Failures != 1 {
		t.Fatalf("a named store that answers not-found falls back to the sweep: %v", row.Metadata)
	}
	// A nil policy settles everything and charges nothing.
	var none *workStartFailurePolicy
	none.recordStartFailure(workTrigger{BeadID: id}, "worker", errPreStartFailure, time.Now().UTC())
	if !none.recordStartSuccess(workTrigger{BeadID: id}, "worker") {
		t.Fatal("a nil policy has nothing to clear")
	}
}
