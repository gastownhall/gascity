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

// dupNamedCity is the fixture config every D-DUP test shares: one backing agent
// and one configured named session, so a stored configured_named_identity of
// "mayor" resolves to a spec and its canonical runtime name.
func dupNamedCity() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "reviewer", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{
			{Name: "mayor", Template: "reviewer", Mode: "on_demand"},
		},
	}
}

// createDupNamedRow seeds one open configured-named session row for identity
// "mayor". generation and sessionName are what the winner rule discriminates on.
func createDupNamedRow(t *testing.T, store beads.Store, title, sessionName, generation string) beads.Bead {
	t.Helper()
	bead, err := store.Create(beads.Bead{
		Title:  title,
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"template":                   "reviewer",
			"generation":                 generation,
			"state":                      "active",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("create named session row %q: %v", title, err)
	}
	return bead
}

// dupReconcileRows projects the named rows out of the store the way the tick
// does, so the sweep and the handler see the same feed.
func dupReconcileRows(t *testing.T, store beads.Store, ids ...string) []sessionpkg.ReconcileSession {
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

// dupSweepInput builds the minimum sweep input that reaches D-DUP over the
// given rows, with admit as the routing seam's enqueue hook.
func dupSweepInput(
	t *testing.T,
	cfg *config.City,
	provider runtime.Provider,
	store beads.Store,
	now time.Time,
	admit func(string, sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error),
	ids ...string,
) detectorSweepInput {
	t.Helper()
	rows := dupReconcileRows(t, store, ids...)
	return detectorSweepInput{
		CityPath: "test-city",
		CityName: "test-city",
		Cfg:      cfg,
		Provider: provider,
		Rows:     rows,
		Desired:  map[string]TemplateParams{},
		Clock:    &clock.Fake{Time: now},
		Trigger:  "patrol",
		Admit:    admit,
	}
}

func dupHandlerParams(cfg *config.City, provider runtime.Provider, store beads.Store) exactSessionStartParams {
	statusWriter, _, statusWriterErr := beads.ResolveConditionalWriter(store)
	return exactSessionStartParams{
		Generation: 1, CityPath: "test-city", CityName: "test-city",
		Config: cfg, Provider: provider, Store: store,
		StatusWriter: statusWriter, StatusWriterError: statusWriterErr,
		Recorder: events.Discard, RolloutMode: rollout.Require,
		Stderr: io.Discard,
	}
}

// TestExactDuplicateNamedLoserRetiredOnceByKey is WD.13's primary RED: two open
// continuity-eligible rows share the named identity "mayor", the sweep raises a
// D-DUP condition for the loser only, hands that exact key to the session-start
// controller, and the handler retires exactly that row — archived, work/wait/
// nudge re-pointed to the winner — while the winner row is untouched. It is the
// keyed re-point of
// TestRetireDuplicateConfiguredNamedSessionBeads_DoesNotStopWinnerSharingSessionName
// (session_beads_test.go:1849).
func TestExactDuplicateNamedLoserRetiredOnceByKey(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cfg := dupNamedCity()
	sessionName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "mayor")
	if err := sp.Start(t.Context(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("start shared runtime %s: %v", sessionName, err)
	}
	loser := createDupNamedRow(t, store, "mayor old", sessionName, "1")
	winner := createDupNamedRow(t, store, "mayor", sessionName, "2")
	work, err := store.Create(beads.Bead{Title: "owned work", Type: "task", Status: "open", Assignee: loser.ID})
	if err != nil {
		t.Fatalf("create loser-owned work: %v", err)
	}
	wait, err := store.Create(beads.Bead{
		Title:  "loser wait",
		Type:   sessionpkg.WaitBeadType,
		Labels: []string{sessionpkg.WaitBeadLabel, "session:" + loser.ID},
		Metadata: map[string]string{
			"session_id": loser.ID,
			"state":      "open",
			"nudge_id":   "nudge-loser",
		},
	})
	if err != nil {
		t.Fatalf("create loser wait: %v", err)
	}

	now := time.Now().UTC()
	admitter := &recordingDetectorAdmitter{}
	in := dupSweepInput(t, cfg, sp, store, now, admitter.admit, loser.ID, winner.ID)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)

	dups := make([]detectorCondition, 0, 1)
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyDup {
			dups = append(dups, cond)
		}
	}
	if len(dups) != 1 {
		t.Fatalf("D-DUP conditions = %d (%#v), want exactly one loser condition", len(dups), dups)
	}
	if dups[0].SessionID != loser.ID {
		t.Fatalf("D-DUP condition key = %q, want the loser %q", dups[0].SessionID, loser.ID)
	}
	if dups[0].Fields["winner_id"] != winner.ID {
		t.Fatalf("D-DUP winner_id = %v, want %q", dups[0].Fields["winner_id"], winner.ID)
	}
	if len(admitter.keys) != 1 || admitter.keys[0] != loser.ID {
		t.Fatalf("sweep enqueued %v, want exactly the loser key %q", admitter.keys, loser.ID)
	}
	if admitter.sources[0] != sessionStartAdmissionDuplicateNamed {
		t.Fatalf("admission source = %q, want %q", admitter.sources[0], sessionStartAdmissionDuplicateNamed)
	}

	params := dupHandlerParams(cfg, sp, store)
	info, response, err := getAuthoritativeSessionStartPersistedRecord(store, loser.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	owner, err := reconcileExactSessionDuplicateNamedRetire(
		sessionStartAdmission{SessionID: loser.ID, Source: sessionStartAdmissionDuplicateNamed},
		params, info, response, clock.Real{},
	)
	if err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
	}

	if !sp.IsRunning(sessionName) {
		t.Fatal("shared runtime was stopped while the winner still owns it")
	}
	retired, err := store.Get(loser.ID)
	if err != nil {
		t.Fatalf("read retired loser: %v", err)
	}
	if retired.Metadata["state"] != "archived" {
		t.Fatalf("loser state = %q, want archived", retired.Metadata["state"])
	}
	if retired.Status != "open" {
		t.Fatalf("loser status = %q, want open non-terminal history", retired.Status)
	}
	survivor, err := store.Get(winner.ID)
	if err != nil {
		t.Fatalf("read winner: %v", err)
	}
	if survivor.Metadata["state"] != "active" || survivor.Metadata["session_name"] != sessionName {
		t.Fatalf("winner row mutated by the loser's retire: %#v", survivor.Metadata)
	}
	updatedWork, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("read loser-owned work: %v", err)
	}
	if updatedWork.Assignee != winner.ID {
		t.Fatalf("work assignee = %q, want winner %q", updatedWork.Assignee, winner.ID)
	}
	updatedWait, err := store.Get(wait.ID)
	if err != nil {
		t.Fatalf("read loser wait: %v", err)
	}
	if updatedWait.Metadata["session_id"] != winner.ID {
		t.Fatalf("wait session_id = %q, want winner %q", updatedWait.Metadata["session_id"], winner.ID)
	}
	nudges, err := sessionpkg.NewStore(beads.SessionStore{Store: store}).WaitNudgeIDs(winner.ID)
	if err != nil {
		t.Fatalf("WaitNudgeIDs(winner): %v", err)
	}
	if len(nudges) != 1 || nudges[0] != "nudge-loser" {
		t.Fatalf("winner wait nudges = %#v, want [nudge-loser]", nudges)
	}
}

// TestDetectorDuplicateNamedWinnerRuleIsGenerationThenCanonicalThenCreatedAt
// asserts the ordering directly so it cannot drift: the detector must name the
// same survivor the retire logic itself would, on each rung of
// namedSessionWinsCanonicalRepairInfo, and the sweep's pinned iteration order
// (session name, then bead ID) must not change the answer.
func TestDetectorDuplicateNamedWinnerRuleIsGenerationThenCanonicalThenCreatedAt(t *testing.T) {
	canonical := config.NamedSessionRuntimeName("test-city", config.Workspace{Name: "test-city"}, "mayor")
	base := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	row := func(id, sessionName, generation string, createdAt time.Time) sessionpkg.Info {
		return sessionpkg.Info{
			ID:                      id,
			SessionNameMetadata:     sessionName,
			Generation:              generation,
			CreatedAt:               createdAt,
			ConfiguredNamedSession:  true,
			ConfiguredNamedIdentity: "mayor",
		}
	}
	for _, tc := range []struct {
		name string
		rows []sessionpkg.Info
		want string
	}{
		{
			name: "generation wins first",
			rows: []sessionpkg.Info{
				row("a-newest", canonical, "1", base.Add(time.Hour)),
				row("b-older", "stale-runtime", "9", base),
			},
			want: "b-older",
		},
		{
			name: "canonical session name breaks equal generations",
			rows: []sessionpkg.Info{
				row("a-noncanonical", "stale-runtime", "3", base.Add(time.Hour)),
				row("b-canonical", canonical, "3", base),
			},
			want: "b-canonical",
		},
		{
			name: "created-at breaks equal generation and canonicality",
			rows: []sessionpkg.Info{
				row("a-old", canonical, "3", base),
				row("b-new", canonical, "3", base.Add(time.Hour)),
			},
			want: "b-new",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forward := detectorDuplicateWinner(tc.rows, canonical)
			reversed := detectorDuplicateWinner([]sessionpkg.Info{tc.rows[1], tc.rows[0]}, canonical)
			if forward.ID != tc.want || reversed.ID != tc.want {
				t.Fatalf("winner = %q (forward) / %q (reversed), want %q from both orders", forward.ID, reversed.ID, tc.want)
			}
			if !namedSessionWinsCanonicalRepairInfo(forward, otherThan(tc.rows, forward.ID), canonical) {
				t.Fatalf("detector winner %q does not beat its sibling under the retire logic's own rule", forward.ID)
			}
		})
	}
}

func otherThan(rows []sessionpkg.Info, id string) sessionpkg.Info {
	for _, r := range rows {
		if r.ID != id {
			return r
		}
	}
	return sessionpkg.Info{}
}

// TestDetectorSoleNamedRowNeverEnqueuesDuplicate is the third AC negative: one
// row for a named identity is never a duplicate, so the sweep raises nothing and
// the handler's guard refuses the key even if some other admission carries it
// into the seam.
func TestDetectorSoleNamedRowNeverEnqueuesDuplicate(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cfg := dupNamedCity()
	sessionName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "mayor")
	sole := createDupNamedRow(t, store, "mayor", sessionName, "1")

	admitter := &recordingDetectorAdmitter{}
	in := dupSweepInput(t, cfg, sp, store, time.Now().UTC(), admitter.admit, sole.ID)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyDup {
			t.Fatalf("sole named row raised a duplicate condition: %#v", cond)
		}
	}
	if len(admitter.keys) != 0 {
		t.Fatalf("sole named row enqueued %v; want zero enqueues", admitter.keys)
	}

	params := dupHandlerParams(cfg, sp, store)
	info, response, err := getAuthoritativeSessionStartPersistedRecord(store, sole.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	if exactSessionDuplicateNamedCandidate(params, info, response) {
		t.Fatal("seam guard claimed a sole named row as a duplicate loser")
	}
	before, err := store.Get(sole.ID)
	if err != nil {
		t.Fatal(err)
	}
	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(),
		sessionStartAdmission{SessionID: sole.ID, Source: sessionStartAdmissionDuplicateNamed},
		params, info, response, clock.Real{},
	)
	if handled {
		t.Fatalf("seam claimed a sole named row (owner=%v err=%v)", owner, err)
	}
	after, err := store.Get(sole.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("sole named row mutated by a refused duplicate handler: revision %d -> %d", before.Revision, after.Revision)
	}
}

// TestExactDuplicateNamedRetireRefusesWhenRuntimeStopFails is the AC's first
// negative, ported from
// TestRetireDuplicateConfiguredNamedSessionBeads_StopFailureKeepsRuntimeOwner
// (session_beads_test.go:1976): a loser holding its own runtime whose stop fails
// is a refusal — nothing archived, nothing re-pointed, zero partial mutation.
func TestExactDuplicateNamedRetireRefusesWhenRuntimeStopFails(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cfg := dupNamedCity()
	winnerSessionName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "mayor")
	loserSessionName := "old-mayor-runtime"
	if err := sp.Start(t.Context(), loserSessionName, runtime.Config{}); err != nil {
		t.Fatalf("start loser runtime: %v", err)
	}
	sp.StopErrors[loserSessionName] = errors.New("stop failed")
	loser := createDupNamedRow(t, store, "mayor old", loserSessionName, "1")
	winner := createDupNamedRow(t, store, "mayor", winnerSessionName, "2")
	work, err := store.Create(beads.Bead{Title: "owned work", Type: "task", Status: "open", Assignee: loser.ID})
	if err != nil {
		t.Fatalf("create loser-owned work: %v", err)
	}

	params := dupHandlerParams(cfg, sp, store)
	info, response, err := getAuthoritativeSessionStartPersistedRecord(store, loser.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	if !exactSessionDuplicateNamedCandidate(params, info, response) {
		t.Fatal("seam guard did not recognize the live duplicate loser")
	}
	owner, err := reconcileExactSessionDuplicateNamedRetire(
		sessionStartAdmission{SessionID: loser.ID, Source: sessionStartAdmissionDuplicateNamed},
		params, info, response, clock.Real{},
	)
	if err == nil {
		t.Fatal("handler reported success after the loser's runtime stop failed")
	}
	if owner != exactSessionStartKeyedOwner {
		t.Fatalf("refusal owner = %v, want keyed ownership", owner)
	}
	if !sp.IsRunning(loserSessionName) {
		t.Fatalf("loser runtime %q unexpectedly stopped", loserSessionName)
	}
	stored, err := store.Get(loser.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata["state"] == "archived" || stored.Metadata["session_name"] != loserSessionName {
		t.Fatalf("loser mutated despite the stop failure: %#v", stored.Metadata)
	}
	updatedWork, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedWork.Assignee != loser.ID {
		t.Fatalf("work assignee = %q, want unchanged loser %q", updatedWork.Assignee, loser.ID)
	}
	if _, err := store.Get(winner.ID); err != nil {
		t.Fatalf("read winner: %v", err)
	}
}

// TestExactWakeAdmissionHealsExpiredLifecycleTimers is the expired-timer arm of
// the slice: an elapsed hold or quarantine is cleared by the wake handler at
// admission, on the authoritative row it re-reads anyway. No detector family
// exists for it, so this is the only keyed heal path.
func TestExactWakeAdmissionHealsExpiredLifecycleTimers(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "expired hold", key: "held_until"},
		{name: "expired quarantine", key: "quarantined_until"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
			bead := env.createSessionBead("worker", "worker")
			env.markSessionActive(&bead)
			elapsed := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
			env.setSessionMetadata(&bead, map[string]string{tc.key: elapsed})

			params := dupHandlerParams(env.cfg, env.sp, env.store)
			info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
			if err != nil {
				t.Fatalf("authoritative read: %v", err)
			}
			healed, healedResponse, applied := healExactSessionAdmissionTimers(params, info, response, clock.Real{})
			if !applied {
				t.Fatal("admission heal reported no effect for an elapsed timer")
			}
			if healedResponse.Revision == 0 || healedResponse.Revision == response.Revision {
				t.Fatalf("post-heal revision = %d, want a fresh revision (pre-heal %d)", healedResponse.Revision, response.Revision)
			}
			if healed.HeldUntil != "" || healed.QuarantinedUntil != "" {
				t.Fatalf("healed row still carries a timer: held=%q quarantined=%q", healed.HeldUntil, healed.QuarantinedUntil)
			}
			stored, err := env.store.Get(bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Metadata[tc.key] != "" {
				t.Fatalf("durable %s = %q, want cleared at admission", tc.key, stored.Metadata[tc.key])
			}
		})
	}
}

// TestDetectorRaisesNoDuplicateForExpiredTimerRow is the AC's second negative:
// an expired hold timer produces NO D-DUP detection — the sweep has no timer
// family at all — so the only heal path is the wake handler's admission clear.
func TestDetectorRaisesNoDuplicateForExpiredTimerRow(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cfg := dupNamedCity()
	sessionName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "mayor")
	held := createDupNamedRow(t, store, "mayor", sessionName, "1")
	if err := sessionFrontDoor(store).ApplyPatch(held.ID, sessionpkg.MetadataPatch{
		"held_until": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed expired hold: %v", err)
	}

	admitter := &recordingDetectorAdmitter{}
	in := dupSweepInput(t, cfg, sp, store, time.Now().UTC(), admitter.admit, held.ID)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyDup {
			t.Fatalf("expired-timer row raised a D-DUP condition: %#v", cond)
		}
	}
	if len(admitter.keys) != 0 {
		t.Fatalf("expired-timer row enqueued %v; want zero enqueues", admitter.keys)
	}

	params := dupHandlerParams(cfg, sp, store)
	info, response, err := getAuthoritativeSessionStartPersistedRecord(store, held.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	healed, _, applied := healExactSessionAdmissionTimers(params, info, response, clock.Real{})
	if !applied || healed.HeldUntil != "" {
		t.Fatalf("wake-handler admission did not clear the expired hold: applied=%v held=%q", applied, healed.HeldUntil)
	}
}

// TestLegacyDuplicateRetireYieldsToKeyedOwnedRow is the coexistence-doctrine RED
// for the destructive arm. Both writers read the same duplicate set on the same
// tick, so an acting D-DUP beside a non-yielding legacy stops the loser's
// runtime twice and races two re-points at the same work beads.
func TestLegacyDuplicateRetireYieldsToKeyedOwnedRow(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cfg := dupNamedCity()
	sessionName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "mayor")
	loser := createDupNamedRow(t, store, "mayor old", sessionName, "1")
	winner := createDupNamedRow(t, store, "mayor", sessionName, "2")
	work, err := store.Create(beads.Bead{Title: "owned work", Type: "task", Status: "open", Assignee: loser.ID})
	if err != nil {
		t.Fatalf("create loser-owned work: %v", err)
	}

	rows := dupReconcileRows(t, store, loser.ID, winner.ID)
	retireDuplicateConfiguredNamedSessionRows(
		"", store, nil, sp, cfg, "test-city", rows,
		func(info sessionpkg.Info) bool { return info.ID == loser.ID },
		time.Now().UTC(), io.Discard,
	)

	stored, err := store.Get(loser.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata["state"] == "archived" {
		t.Fatal("legacy Phase-0b retired a loser the keyed D-DUP handler owns")
	}
	updatedWork, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedWork.Assignee != loser.ID {
		t.Fatalf("legacy re-pointed work for a keyed-owned loser: assignee = %q", updatedWork.Assignee)
	}
}

// TestLegacyExpiredTimerHealSelfYieldsAfterKeyedAdmissionHeal records this
// slice's other ownership-semantics decision. The expired-timer heal takes NO
// ownership fence, unlike the duplicate retire: it is a convergent, idempotent,
// provider-free clear of an already-elapsed timestamp, so two writers cannot
// disagree and there is no destructive effect to fence. It self-yields instead —
// once the keyed admission heal has cleared the timer, legacy's Phase-0a fold
// finds nothing to clear and performs zero writes.
func TestLegacyExpiredTimerHealSelfYieldsAfterKeyedAdmissionHeal(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	bead := env.createSessionBead("worker", "worker")
	env.markSessionActive(&bead)
	env.setSessionMetadata(&bead, map[string]string{
		"held_until": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
	})

	params := dupHandlerParams(env.cfg, env.sp, env.store)
	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	healed, _, applied := healExactSessionAdmissionTimers(params, info, response, clock.Real{})
	if !applied {
		t.Fatal("keyed admission heal reported no effect for an elapsed hold")
	}

	before, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacy := healExpiredTimersInfo(healed, sessionFrontDoor(env.store), clock.Real{})
	after, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("legacy Phase-0a heal wrote over a row the keyed admission already healed: revision %d -> %d", before.Revision, after.Revision)
	}
	if legacy.HeldUntil != "" {
		t.Fatalf("legacy fold reintroduced a hold: %q", legacy.HeldUntil)
	}
}
