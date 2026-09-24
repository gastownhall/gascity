package main

import (
	"context"
	"errors"
	"io"
	"sort"
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

// poolChurnIdentity is the pool instance identity used by the churn tests: a
// concrete slot on an expanding pool, exactly as poolDesiredRequestIdentity
// hands it to the create path.
func poolChurnIdentity() poolSessionCreateIdentity {
	return poolSessionCreateIdentity{AgentName: "gastown/bd.dog-1", Slot: 1}
}

const poolChurnTemplate = "gastown/bd.dog"

// failPoolStartLeavingBox models one ga-vcjr9 attempt after the pool bead
// exists: the provider provisions the box under the bead's runtime name, the
// start is then judged failed, and commitStartFailure rolls the pending create
// back. The box is left behind on purpose — a provider whose own start-failure
// cleanup did not run (or could not) is exactly what leaked pods on cherry.
func failPoolStartLeavingBox(t *testing.T, store beads.Store, sp *runtime.Fake, info sessionpkg.Info, now time.Time) {
	t.Helper()
	failPoolStartLeavingBoxWith(t, store, sp, info, now, errors.New("start op failed after provisioning"))
}

// failPoolStartLeavingBoxWith is failPoolStartLeavingBox with a caller-chosen
// start error, so a test can drive a specific commitStartFailure arm.
func failPoolStartLeavingBoxWith(t *testing.T, store beads.Store, sp *runtime.Fake, info sessionpkg.Info, now time.Time, startErr error) {
	t.Helper()
	name := strings.TrimSpace(info.SessionNameMetadata)
	if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("provisioning box %q: %v", name, err)
	}
	result := startResult{
		prepared: preparedStart{candidate: startCandidate{
			info: info,
			tp:   TemplateParams{SessionName: name, TemplateName: poolChurnTemplate, Command: "true"},
		}},
		err:             startErr,
		outcome:         TraceOutcomeProviderError,
		started:         now,
		finished:        now,
		rollbackPending: true,
		provider:        sp,
	}
	commitStartFailure(result, sessionFrontDoor(store), &clock.Fake{Time: now}, events.Discard, 0, io.Discard, nil)
}

func liveRuntimeCount(t *testing.T, sp *runtime.Fake) int {
	t.Helper()
	running, err := sp.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	return len(running)
}

// TestPoolSessionCreate_FailedCreatesLeaveAtMostOneLiveRuntime is the ga-vcjr9
// regression pin for bead-scoped names. On cherry a pool whose start kept
// failing minted bd__dog-<beadID> per attempt and never tore the previous box
// down: 602 pods for desired=1. Runtime names are bead-scoped again, so every
// attempt IS a new name; the invariant that must hold instead is that the
// failed attempt's box is gone before its row closes and a successor is minted.
func TestPoolSessionCreate_FailedCreatesLeaveAtMostOneLiveRuntime(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	identity := poolChurnIdentity()

	names := map[string]struct{}{}
	for attempt := 1; attempt <= 5; attempt++ {
		now := time.Date(2026, 8, 15, 0, 0, attempt, 0, time.UTC)
		open, err := loadSessionBeads(store)
		if err != nil {
			t.Fatalf("attempt %d: loadSessionBeads: %v", attempt, err)
		}
		info, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, newSessionBeadSnapshot(open), now, identity, "")
		if err != nil {
			t.Fatalf("attempt %d: createPoolSessionBeadWithAlias: %v", attempt, err)
		}
		names[info.SessionNameMetadata] = struct{}{}
		failPoolStartLeavingBox(t, store, sp, info, now)
		if n := liveRuntimeCount(t, sp); n > 1 {
			t.Fatalf("attempt %d: %d live runtimes for a desired=1 slot; want <= 1 (ga-vcjr9)", attempt, n)
		}
		if got, _ := store.Get(info.ID); got.Status != "closed" {
			t.Fatalf("attempt %d: failed row %s status %q, want closed after a confirmed teardown", attempt, info.ID, got.Status)
		}
	}
	if len(names) != 5 {
		t.Fatalf("5 attempts minted %d names %v; want 5 bead-scoped names (control: this test must exercise per-generation names)", len(names), sortedNameSet(names))
	}
	if n := liveRuntimeCount(t, sp); n != 0 {
		t.Fatalf("%d live runtimes after 5 rolled-back attempts, want 0", n)
	}
}

// TestPoolSessionCreate_TeardownFailureHoldsTheSlot is the fail-closed half: a
// teardown that cannot be confirmed must keep the failed row OPEN, and an open
// unconfirmed row must refuse a successor for the same identity. Otherwise a
// sick backend (start AND stop failing) would leak one box per tick again.
func TestPoolSessionCreate_TeardownFailureHoldsTheSlot(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	identity := poolChurnIdentity()
	now := time.Date(2026, 8, 15, 0, 0, 1, 0, time.UTC)

	first, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, newSessionBeadSnapshot(nil), now, identity, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sp.StopErrors = map[string]error{first.SessionNameMetadata: errors.New("apiserver unreachable")}
	failPoolStartLeavingBox(t, store, sp, first, now)

	if got, _ := store.Get(first.ID); got.Status == "closed" {
		t.Fatalf("row %s closed although its runtime teardown failed; its box is now unaddressable", first.ID)
	}
	for tick := 0; tick < 3; tick++ {
		open, err := loadSessionBeads(store)
		if err != nil {
			t.Fatalf("loadSessionBeads: %v", err)
		}
		if _, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, newSessionBeadSnapshot(open), now, identity, ""); !errors.Is(err, errPoolSessionNameUnavailable) {
			t.Fatalf("tick %d: successor create error = %v, want errPoolSessionNameUnavailable while the unconfirmed row is open", tick, err)
		}
	}
	if n := liveRuntimeCount(t, sp); n != 1 {
		t.Fatalf("%d live runtimes, want exactly the one held box", n)
	}

	// The backend recovers: the level-triggered rollback retries the teardown,
	// the row closes, and only then may the slot mint a successor.
	delete(sp.StopErrors, first.SessionNameMetadata)
	if !releaseBeadScopedPoolRuntime(first, sp, io.Discard) {
		t.Fatal("teardown still refused after the backend recovered")
	}
	rollbackPendingCreate(first, sessionFrontDoor(store), now, io.Discard)
	open, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	if _, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, newSessionBeadSnapshot(open), now, identity, ""); err != nil {
		t.Fatalf("successor after confirmed teardown: %v", err)
	}
	if n := liveRuntimeCount(t, sp); n != 0 {
		t.Fatalf("%d live runtimes after recovery, want 0", n)
	}
}

// TestPoolSessionCreate_TerminalProviderErrorTearsDownBeforeRollback covers the
// terminal-provider-error arm of commitStartFailure (model_not_found and
// friends), which rolls a pool create back through its own branch. It must
// honor the same teardown-before-close rule as the generic rollback arm: hold
// the row open while Stop fails, close it (with the box gone) once Stop works.
func TestPoolSessionCreate_TerminalProviderErrorTearsDownBeforeRollback(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	identity := poolChurnIdentity()
	now := time.Date(2026, 8, 15, 0, 0, 1, 0, time.UTC)
	terminalErr := errors.New("provider error: model_not_found")
	if runtime.ProviderTerminalErrorReason(terminalErr.Error()) == "" {
		t.Fatal("fixture precondition: the start error must hit the terminal-provider-error arm")
	}

	first, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, newSessionBeadSnapshot(nil), now, identity, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sp.StopErrors = map[string]error{first.SessionNameMetadata: errors.New("apiserver unreachable")}
	failPoolStartLeavingBoxWith(t, store, sp, first, now, terminalErr)
	got, err := store.Get(first.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("row %s closed on a terminal provider error although its runtime teardown failed", first.ID)
	}
	if got.Metadata["pending_create_claim"] != boolMetadata(true) || got.Metadata["state"] != first.MetadataState {
		t.Fatalf("held row claim/state = %q/%q, want it left pending (%q) so the lease-expired rollback retries the teardown",
			got.Metadata["pending_create_claim"], got.Metadata["state"], first.MetadataState)
	}
	if n := liveRuntimeCount(t, sp); n != 1 {
		t.Fatalf("%d live runtimes, want exactly the one held box", n)
	}
	open, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	if _, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, newSessionBeadSnapshot(open), now, identity, ""); !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("successor create error = %v, want errPoolSessionNameUnavailable while the held row is open", err)
	}

	// Control: with a working Stop the same arm tears the box down and closes.
	second, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, newSessionBeadSnapshot(nil), now,
		poolSessionCreateIdentity{AgentName: "gastown/bd.dog-2", Slot: 2}, "")
	if err != nil {
		t.Fatalf("create slot 2: %v", err)
	}
	failPoolStartLeavingBoxWith(t, store, sp, second, now, terminalErr)
	got, err = store.Get(second.ID)
	if err != nil {
		t.Fatalf("Get slot 2: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("row %s status %q after a confirmed teardown, want closed", second.ID, got.Status)
	}
	if got.Metadata["state"] != string(sessionpkg.StateFailedCreate) {
		t.Fatalf("row %s state %q after rollback, want failed-create", second.ID, got.Metadata["state"])
	}
	if got.Metadata[sessionProviderTerminalErrorMetadataKey] != "model_not_found" {
		t.Fatalf("row %s terminal error %q, want model_not_found", second.ID, got.Metadata[sessionProviderTerminalErrorMetadataKey])
	}
	if sp.IsRunning(second.SessionNameMetadata) {
		t.Fatalf("box %q survived the terminal-provider-error rollback", second.SessionNameMetadata)
	}
}

// TestPoolSessionCreate_CanonicalSingletonNameIsTemplateBeadID pins the
// runtime-name contract the fresh-init Tier C test checks
// (fresh_install_spawn_test.go: "claude-*"): a canonical singleton pool (max=1,
// no namepool, no tmux_alias) runs as <template>-<beadID>, so the bead's
// metadata can be looked up from the runtime name.
func TestPoolSessionCreate_CanonicalSingletonNameIsTemplateBeadID(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "claude",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}
	if !cfg.Agents[0].UsesCanonicalSingletonPoolIdentity() {
		t.Fatal("fixture precondition: claude must be a canonical singleton pool")
	}
	store := beads.NewMemStore()
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, io.Discard)
	bp.sessionBeads = newSessionBeadSnapshot(nil)
	_, qualifiedInstance, slot := poolDesiredRequestIdentity(&cfg.Agents[0], 1)
	info, err := createPoolSessionBeadWithGuardedAlias(bp, &cfg.Agents[0], cfg.Agents[0].QualifiedName(), qualifiedInstance, slot, nil)
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithGuardedAlias: %v", err)
	}
	if want := "claude-" + info.ID; info.SessionNameMetadata != want {
		t.Fatalf("canonical singleton session_name = %q, want %q", info.SessionNameMetadata, want)
	}
	if info.Alias != "claude" || info.AgentName != "claude" {
		t.Fatalf("alias/agent_name = %q/%q, want the canonical identity \"claude\" (mail/claim identity unchanged)", info.Alias, info.AgentName)
	}
}

// TestPoolSessionCreate_DistinctSlotsGetDistinctNames is the control for the
// test above: a fix that collapsed every pool session onto one name would pass
// the churn test and destroy the pool. Distinct slots must stay distinct.
func TestPoolSessionCreate_DistinctSlotsGetDistinctNames(t *testing.T) {
	store := beads.NewMemStore()

	names := map[string]struct{}{}
	for slot := 1; slot <= 3; slot++ {
		identity := poolSessionCreateIdentity{
			AgentName: "gastown/bd.dog-" + string(rune('0'+slot)),
			Slot:      slot,
		}
		info, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, nil, time.Now().UTC(), identity, "")
		if err != nil {
			t.Fatalf("slot %d: createPoolSessionBeadWithAlias: %v", slot, err)
		}
		names[strings.TrimSpace(info.SessionNameMetadata)] = struct{}{}
	}
	if len(names) != 3 {
		t.Fatalf("3 distinct pool slots produced %d distinct session names %v; want 3", len(names), sortedNameSet(names))
	}
}

// TestPoolSessionCreate_LiveHolderKeepsItsNameAndBlocksTheSlot is the MANDATORY
// reverse control. While an open, unconfirmed session bead holds the pool
// identity's lease, a fresh create must fail closed — it must NOT mint a
// bead-scoped sibling next to it, because a second generation beside an
// unconfirmed one is the ga-vcjr9 leak — and must not disturb the holder.
func TestPoolSessionCreate_LiveHolderKeepsItsNameAndBlocksTheSlot(t *testing.T) {
	store := beads.NewMemStore()
	identity := poolChurnIdentity()

	live, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, nil, time.Now().UTC(), identity, "")
	if err != nil {
		t.Fatalf("live createPoolSessionBeadWithAlias: %v", err)
	}
	liveName := strings.TrimSpace(live.SessionNameMetadata)

	second, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, nil, time.Now().UTC(), identity, "")
	if err == nil {
		t.Fatalf("second create returned session_name %q while %s is live; want a typed unavailable error", second.SessionNameMetadata, live.ID)
	}
	if !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("second create error = %v, want errPoolSessionNameUnavailable", err)
	}

	stored, getErr := store.Get(live.ID)
	if getErr != nil {
		t.Fatalf("store.Get(%s): %v", live.ID, getErr)
	}
	if got := strings.TrimSpace(stored.Metadata["session_name"]); got != liveName {
		t.Fatalf("live session_name = %q, want %q (a blocked create must not disturb the live holder)", got, liveName)
	}
	if stored.Status == "closed" {
		t.Fatalf("live session bead %s was closed by a blocked create", live.ID)
	}

	all, listErr := store.ListByLabel(sessionBeadLabel, 0)
	if listErr != nil {
		t.Fatalf("ListByLabel(%q): %v", sessionBeadLabel, listErr)
	}
	for _, b := range all {
		if b.ID == live.ID {
			continue
		}
		t.Fatalf("blocked create left session bead %s (session_name %q) behind; no suffixed sibling may be minted", b.ID, b.Metadata["session_name"])
	}
}

// TestPoolSessionCreate_ConfiguredNamedSessionOwnerReusesItsOwnName closes the
// selfOwner=="" gap. A pool that materializes a configured named session's
// identity must be allowed to claim that identity's reserved runtime name;
// otherwise the config reservation rejects it on every tick and — before the
// fix — the rejection was what minted a fresh suffixed name each time.
func TestPoolSessionCreate_ConfiguredNamedSessionOwnerReusesItsOwnName(t *testing.T) {
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		NamedSessions: []config.NamedSession{{Name: "crew", Template: "worker"}},
	}
	reserved := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "crew")

	store := beads.NewMemStore()
	info, err := createPoolSessionBeadWithAlias(store, "worker", cfg, newSessionBeadSnapshot(nil), time.Now().UTC(),
		poolSessionCreateIdentity{AgentName: "crew"}, reserved)
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithAlias: %v", err)
	}
	if got := strings.TrimSpace(info.SessionNameMetadata); got != reserved {
		t.Fatalf("session_name = %q, want the reserved runtime name %q for its own configured owner", got, reserved)
	}
}

// TestPoolSessionCreate_ForeignConfiguredReservationStillBlocks is the control
// for the test above: passing the owner through must not turn the config
// reservation off for a pool that does NOT own the reserved name.
func TestPoolSessionCreate_ForeignConfiguredReservationStillBlocks(t *testing.T) {
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		NamedSessions: []config.NamedSession{{Name: "crew", Template: "worker"}},
	}
	reserved := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "crew")

	store := beads.NewMemStore()
	_, err := createPoolSessionBeadWithAlias(store, "worker", cfg, newSessionBeadSnapshot(nil), time.Now().UTC(),
		poolSessionCreateIdentity{AgentName: "squatter"}, reserved)
	if err == nil {
		t.Fatal("createPoolSessionBeadWithAlias claimed a name reserved for a different configured named session")
	}
	if !errors.Is(err, errPoolSessionNameUnavailable) {
		t.Fatalf("error = %v, want errPoolSessionNameUnavailable", err)
	}
}

func sortedNameSet(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// createBeadScopedPoolRowWithBox mints a bead-scoped pool row and provisions its
// box without any attribution metadata (GC_SESSION_ID / GC_INSTANCE_TOKEN),
// the shape of an exec pack without get-meta or a k8s pod whose tmux is not up.
func createBeadScopedPoolRowWithBox(t *testing.T, store beads.Store, sp *runtime.Fake, now time.Time) sessionpkg.Info {
	t.Helper()
	info, err := createPoolSessionBeadWithAlias(store, poolChurnTemplate, nil, newSessionBeadSnapshot(nil), now, poolChurnIdentity(), "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !infoOwnsPoolSessionName(info) {
		t.Fatalf("fixture precondition: session_name %q is not bead-scoped", info.SessionNameMetadata)
	}
	if err := sp.Start(context.Background(), info.SessionNameMetadata, runtime.Config{}); err != nil {
		t.Fatalf("provisioning box: %v", err)
	}
	return info
}

func successfulPoolStartResult(info sessionpkg.Info, now time.Time) startResult {
	name := strings.TrimSpace(info.SessionNameMetadata)
	return startResult{
		prepared: preparedStart{candidate: startCandidate{
			info: info,
			tp:   TemplateParams{SessionName: name, TemplateName: poolChurnTemplate, Command: "true"},
		}},
		outcome:  "success",
		started:  now,
		finished: now,
	}
}

// TestCommitAsyncStart_CanceledSuccessHoldsBeadScopedPoolRowWhenTeardownFails
// covers the controller-shutdown path: Start succeeded, but the commit context
// was canceled, so the pending create is rolled back. For a bead-scoped pool row
// that rollback must go through the teardown gate like every other close of an
// unconfirmed create: while Stop fails the row stays OPEN with its claim (the
// lease-expired rollback retries the teardown); once Stop works the box is gone
// and the row closes.
func TestCommitAsyncStart_CanceledSuccessHoldsBeadScopedPoolRowWhenTeardownFails(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	now := time.Date(2026, 8, 15, 0, 0, 1, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	info := createBeadScopedPoolRowWithBox(t, store, sp, now)
	name := info.SessionNameMetadata
	sp.StopErrors[name] = errors.New("apiserver unreachable")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if commitAsyncStartResultWithContext(ctx, successfulPoolStartResult(info, now), sp, store, clk, events.Discard, 0, io.Discard, io.Discard, nil) {
		t.Fatal("canceled async success reported committed")
	}
	got, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("row %s closed on a canceled commit although its runtime teardown failed; box %q is now unaddressable", info.ID, name)
	}
	if got.Metadata["pending_create_claim"] != boolMetadata(true) {
		t.Fatalf("held row pending_create_claim = %q, want kept so the lease-expired rollback retries the teardown", got.Metadata["pending_create_claim"])
	}
	if !sp.IsRunning(name) {
		t.Fatalf("fixture: box %q should still be up while Stop fails", name)
	}

	delete(sp.StopErrors, name)
	commitAsyncStartResultWithContext(ctx, successfulPoolStartResult(info, now), sp, store, clk, events.Discard, 0, io.Discard, io.Discard, nil)
	if got, _ := store.Get(info.ID); got.Status != "closed" {
		t.Fatalf("row status after a confirmed teardown = %q, want closed", got.Status)
	}
	if sp.IsRunning(name) {
		t.Fatalf("box %q survived the canceled-commit rollback", name)
	}
}

// TestCommitAsyncStart_StaleResultStopsUnattributedBeadScopedPoolRuntime covers
// the race where the lease-expired rollback closed a bead-scoped pool row while
// its async Start was still running, and the Start then brought the box up. The
// successor runs under a new name, so this box must be stopped by name even
// though its attribution metadata is unreadable. A box that positively carries a
// different instance token (a newer generation of the same bead) is left alone.
func TestCommitAsyncStart_StaleResultStopsUnattributedBeadScopedPoolRuntime(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 0, 1, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	t.Run("unattributed box of a closed row is stopped", func(t *testing.T) {
		store := beads.NewMemStore()
		sp := runtime.NewFake()
		info := createBeadScopedPoolRowWithBox(t, store, sp, now)
		rollbackPendingCreate(info, sessionFrontDoor(store), now, io.Discard)
		if got, _ := store.Get(info.ID); got.Status != "closed" {
			t.Fatalf("fixture: row status = %q, want closed by the rollback", got.Status)
		}
		if commitAsyncStartResultWithContext(context.Background(), successfulPoolStartResult(info, now), sp, store, clk, events.Discard, 0, io.Discard, io.Discard, nil) {
			t.Fatal("stale async start against a closed row reported committed")
		}
		if sp.IsRunning(info.SessionNameMetadata) {
			t.Fatalf("box %q of closed row %s survived; nothing will address it again", info.SessionNameMetadata, info.ID)
		}
	})

	t.Run("box carrying a newer instance token is kept", func(t *testing.T) {
		store := beads.NewMemStore()
		sp := runtime.NewFake()
		info := createBeadScopedPoolRowWithBox(t, store, sp, now)
		if err := sp.SetMeta(info.SessionNameMetadata, "GC_INSTANCE_TOKEN", "newer-generation-token"); err != nil {
			t.Fatalf("SetMeta: %v", err)
		}
		rollbackPendingCreate(info, sessionFrontDoor(store), now, io.Discard)
		commitAsyncStartResultWithContext(context.Background(), successfulPoolStartResult(info, now), sp, store, clk, events.Discard, 0, io.Discard, io.Discard, nil)
		if !sp.IsRunning(info.SessionNameMetadata) {
			t.Fatalf("box %q positively owned by another generation was stopped", info.SessionNameMetadata)
		}
	})
}
