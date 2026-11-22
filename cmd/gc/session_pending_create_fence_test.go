package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

func pendingCreateFenceFixture(t *testing.T, store beads.Store) startResult {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Type: sessionBeadType,
		Metadata: creatingMeta(map[string]string{
			"session_name": "worker", "session_name_explicit": "true",
			"pending_create_claim": "true", "generation": "2", "instance_token": "incarnation-2",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return startResult{prepared: preparedStart{candidate: startCandidate{
		info: sessiontest.SeedBead(t, b),
		tp:   TemplateParams{SessionName: "worker", TemplateName: "worker"},
	}}}
}

// An obsolete rollback must leave every durable field and its local fold alone.
func TestRollbackPendingCreateRejectsStaleSnapshot(t *testing.T) {
	for _, change := range []string{"completed", "generation", "token", "active-pending", "awake-claim-cleared"} {
		t.Run(change, func(t *testing.T) {
			store := beads.NewMemStore()
			result := pendingCreateFenceFixture(t, store)
			info := result.prepared.candidate.info
			clk := &clock.Fake{Time: time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC)}
			if change == "completed" {
				if !commitStartResult(result, sessionFrontDoor(store), clk, events.Discard, 0, io.Discard, io.Discard) {
					t.Fatal("start did not commit")
				}
			} else {
				patch := map[string]string{"generation": "3"}
				switch change {
				case "token":
					patch = map[string]string{"instance_token": "incarnation-3"}
				case "active-pending":
					patch = map[string]string{"state": "active"}
				case "awake-claim-cleared":
					patch = map[string]string{"state": "awake", "pending_create_claim": ""}
				}
				if err := store.SetMetadataBatch(info.ID, patch); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := store.Get(info.ID)
			for _, rollback := range []func(sessionpkg.Info, *sessionpkg.Store, time.Time, io.Writer) map[string]string{
				rollbackPendingCreate, rollbackPendingCreateClearingClaim,
			} {
				if fold := rollback(info, sessionFrontDoor(store), clk.Now(), io.Discard); fold != nil {
					t.Error("stale rollback returned a mutation fold")
				}
			}
			after, _ := store.Get(info.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("stale rollback mutated session: status=%s state=%s", after.Status, after.Metadata["state"])
			}
		})
	}
}

func TestStartCompletionRejectsRolledBackSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	result := pendingCreateFenceFixture(t, store)
	clk := &clock.Fake{Time: time.Now()}
	if rollbackPendingCreate(result.prepared.candidate.info, sessionFrontDoor(store), clk.Now(), io.Discard) == nil {
		t.Fatal("pending creation was not rolled back")
	}
	before, _ := store.Get(result.prepared.candidate.info.ID)
	if commitStartResult(result, sessionFrontDoor(store), clk, events.Discard, 0, io.Discard, io.Discard) {
		t.Fatal("stale completion reported success")
	}
	after, _ := store.Get(before.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("stale completion mutated rolled-back session")
	}
}

func TestFailedStartRollsBackAfterHealToAwake(t *testing.T) {
	store := beads.NewMemStore()
	result := pendingCreateFenceFixture(t, store)
	info := result.prepared.candidate.info
	clk := &clock.Fake{Time: time.Now()}
	patch, err := healStateWithRollbackInfo(info, true, true, sessionFrontDoor(store), clk, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.ApplyPatch(patch); got.MetadataState != "awake" || !got.PendingCreateClaim {
		t.Fatalf("heal must leave an awake pending creation, got state=%s claim=%v", got.MetadataState, got.PendingCreateClaim)
	}
	result.err = errors.New("startup handshake failed")
	result.rollbackPending = true
	if commitStartResult(result, sessionFrontDoor(store), clk, events.Discard, 0, io.Discard, io.Discard) {
		t.Fatal("failed start committed")
	}
	got, err := store.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" || got.Metadata["state"] != "failed-create" || got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("failed start left status=%s state=%s claim=%s", got.Status, got.Metadata["state"], got.Metadata["pending_create_claim"])
	}
}

type finalStartCommitProvider struct {
	*runtime.Fake
	afterRefresh func()
	sessionID    string
	stops        int
}

func (p *finalStartCommitProvider) RemoveMeta(name, key string) error {
	// Drain-ack clearing is after refreshAsyncStartResult and before the final
	// atomic commit. Execute the competing transition at this exact boundary.
	if p.afterRefresh != nil {
		fn := p.afterRefresh
		p.afterRefresh = nil
		fn()
	}
	return p.Fake.RemoveMeta(name, key)
}

func (p *finalStartCommitProvider) Stop(name string) error {
	p.stops++
	// Cleanup must run outside the mutation lock, or this acquisition deadlocks.
	return sessionpkg.WithSessionMutationLock(p.sessionID, func() error { return p.Fake.Stop(name) })
}

type finalStartCommitErrorStore struct{ beads.Store }

func (s finalStartCommitErrorStore) SetMetadataBatch(id string, patch map[string]string) error {
	if patch["state_reason"] == "creation_complete" {
		return errors.New("injected completion write failure")
	}
	return s.Store.SetMetadataBatch(id, patch)
}

func TestAsyncStartFinalCommitStaleCleanup(t *testing.T) {
	for _, scenario := range []string{"rollback", "foreign-runtime", "newer-incarnation", "persistence-error"} {
		t.Run(scenario, func(t *testing.T) {
			store := finalStartCommitErrorStore{Store: beads.NewMemStore()}
			result := pendingCreateFenceFixture(t, store)
			info := result.prepared.candidate.info
			clk := &clock.Fake{Time: time.Now()}
			sp := &finalStartCommitProvider{Fake: runtime.NewFake(), sessionID: info.ID}
			if err := sp.Start(t.Context(), "worker", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			setIdentity := func(id, token, generation string) {
				for key, value := range map[string]string{"GC_SESSION_ID": id, "GC_INSTANCE_TOKEN": token, "GC_RUNTIME_EPOCH": generation} {
					if err := sp.SetMeta("worker", key, value); err != nil {
						t.Fatal(err)
					}
				}
			}
			setIdentity(info.ID, info.InstanceToken, info.Generation)
			boundaryReached := false
			sp.afterRefresh = func() {
				boundaryReached = true
				if scenario == "persistence-error" {
					return
				}
				if rollbackPendingCreate(info, sessionFrontDoor(store), clk.Now(), io.Discard) == nil {
					t.Fatal("rollback at final commit boundary did not apply")
				}
				switch scenario {
				case "foreign-runtime":
					setIdentity("foreign-owner", "foreign-token", "9")
				case "newer-incarnation":
					setIdentity(info.ID, "newer-token", "3")
				}
			}
			rec := events.NewFake()
			if commitAsyncStartResultWithContext(t.Context(), result, sp, store, clk, rec, 0, io.Discard, io.Discard, nil) {
				t.Fatal("rejected completion reported success")
			}
			if recorded, err := rec.List(events.Filter{}); err != nil || len(recorded) != 0 {
				t.Fatalf("rejected completion emitted events: %v err=%v", recorded, err)
			}
			if !boundaryReached {
				t.Fatal("did not reach refresh/commit boundary")
			}
			wantStops := 0
			if scenario == "rollback" {
				wantStops = 1
			}
			if sp.stops != wantStops || sp.IsRunning("worker") != (wantStops == 0) {
				t.Fatalf("stops=%d running=%v, want stops=%d", sp.stops, sp.IsRunning("worker"), wantStops)
			}
			got, err := store.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "persistence-error" && (got.Status != "closed" || got.Metadata["state"] != "failed-create") {
				t.Fatalf("stale completion overwrote rollback: status=%s state=%s", got.Status, got.Metadata["state"])
			}
		})
	}
}

func TestSingletonPendingCreateRetryPreservesOccupiedRuntimeWithoutBeadChurn(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(1)}}}
	now := time.Now()
	bp := newAgentBuildParams("test-city", "/city", cfg, sp, now, store, io.Discard)
	bp.sessionBeads = newSessionBeadSnapshot(nil)
	name := poolIdentitySessionName("worker", "worker")
	if err := sp.Start(t.Context(), name, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta(name, "GC_SESSION_ID", "retired-owner"); err != nil {
		t.Fatal(err)
	}
	create := func() (sessionpkg.Info, error) {
		bp.sessionBeads = newSessionBeadSnapshot(nil)
		return createPoolSessionBeadWithGuardedAliasUsingLock(poolCreateViewOf(bp), &cfg.Agents[0], "worker", "worker", 0, nil,
			func(_ string, _ []string, fn func() error) error { return fn() })
	}
	for range 5 {
		info, err := create()
		if !errors.Is(err, errPoolSessionNameUnavailable) {
			t.Errorf("occupied singleton create: err=%v, want name unavailable", err)
		}
		if info.ID != "" {
			// Model the old retry loop, which invalidates each new pending bead.
			closeFailedCreateBead(sessionFrontDoor(store), info, now, io.Discard)
		}
	}
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil || len(all) != 0 {
		t.Fatalf("occupied singleton minted %d beads: %v", len(all), err)
	}
	if owner, err := sp.GetMeta(name, "GC_SESSION_ID"); err != nil || owner != "retired-owner" || !sp.IsRunning(name) {
		t.Fatal("retry changed the occupying runtime")
	}
	if err := sp.Stop(name); err != nil {
		t.Fatal(err)
	}
	if info, err := create(); err != nil || info.ID == "" {
		t.Fatalf("vacated singleton did not recover: %v", err)
	}
}

// TestLegacyStartCommitRefusesASuspendDuringTheStart (mc-z1v39, CONTRACT S1
// v5.6, §12.2 row 65; J16 B): a `gc session suspend` that lands while a
// legacy start runs sticks. The async commit refuses on the user hold, even
// over a row the heal pass projected awake (the lease verdict alone would
// commit), stops the start's runtime, releases its in-flight lease and
// records no session.woke; the hold stays. Without a suspend the same start
// commits. Kills: the commit's hold check dropped, and a refusal that leaves
// the runtime up.
func TestLegacyStartCommitRefusesASuspendDuringTheStart(t *testing.T) {
	for _, c := range []struct {
		name            string
		suspend, healed bool
	}{
		{name: "no suspend"},
		{name: "suspended", suspend: true},
		{name: "suspended, healed awake", suspend: true, healed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			const name = "worker-held"
			store := beads.NewMemStore()
			clk := &clock.Fake{Time: time.Date(2026, 10, 13, 11, 31, 0, 0, time.UTC)}
			session, err := store.Create(beads.Bead{
				Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel},
				Metadata: creatingMeta(map[string]string{
					"session_name": name, "template": "worker", "generation": "3",
					"instance_token": "tok-3", "last_woke_at": clk.Now().Format(time.RFC3339),
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			for key, value := range map[string]string{"GC_SESSION_ID": session.ID, "GC_INSTANCE_TOKEN": "tok-3", "GC_RUNTIME_EPOCH": "3"} {
				if err := sp.SetMeta(name, key, value); err != nil {
					t.Fatal(err)
				}
			}
			result := startResult{
				prepared: preparedStart{
					candidate: startCandidate{info: sessiontest.SeedBead(t, session), tp: TemplateParams{SessionName: name, TemplateName: "worker"}},
					coreHash:  "core-v1",
				},
				outcome: "success", started: clk.Now(), finished: clk.Now(),
			}
			if c.suspend {
				if _, err := sessionFrontDoor(store).OperatorSuspend(session.ID, clk.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if c.healed {
				if err := store.SetMetadataBatch(session.ID, map[string]string{"state": "awake"}); err != nil {
					t.Fatal(err)
				}
			}
			rec := events.NewFake()
			var stderr bytes.Buffer
			committed := commitAsyncStartResultWithContext(context.Background(), result, sp, store, clk, rec, 0, io.Discard, &stderr, nil)
			got, err := store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			woke := slices.ContainsFunc(rec.Events, func(e events.Event) bool { return e.Type == events.SessionWoke })
			switch {
			case !c.suspend && (!committed || !sp.IsRunning(name) || got.Metadata["started_config_hash"] != "core-v1" || !woke):
				t.Fatalf("committed %v, running %v, row %v, woke %v; want the start committed; stderr=%s", committed, sp.IsRunning(name), got.Metadata, woke, stderr.String())
			case c.suspend && (committed || sp.IsRunning(name) || got.Metadata["started_config_hash"] != "" || woke):
				t.Fatalf("committed %v, running %v, row %v, woke %v; want nothing committed and the runtime stopped; stderr=%s", committed, sp.IsRunning(name), got.Metadata, woke, stderr.String())
			case c.suspend && got.Metadata["sleep_intent"] != "user-hold":
				t.Fatalf("row %v; want the operator's hold kept", got.Metadata)
			case c.healed && got.Metadata["last_woke_at"] != "":
				t.Fatalf("row %v; want the start's in-flight lease released", got.Metadata)
			}
		})
	}
}

// TestRecoverRunningPendingCreateRefusesASuspend (mc-z1v39, CONTRACT S1
// v5.6, §12.2 row 65): recoverRunningPendingCreate is a start's commit too. A
// pending create whose own runtime is alive after a `gc session suspend` (a
// controller that died between the spawn and the commit), whatever state the
// heal pass projects, is never committed over the hold: its runtime is
// stopped, and over the next ticks the row stays held and down. Without a
// suspend the same row is healed and keeps running. Kills: recovery's hold
// check dropped, and a refusal that leaves the runtime up.
func TestRecoverRunningPendingCreateRefusesASuspend(t *testing.T) {
	for _, c := range []struct {
		state   string
		suspend bool
	}{
		{"creating", false},
		{"creating", true},
		{"awake", true},
		{"active", true},
		{"suspended", true},
	} {
		t.Run(fmt.Sprintf("%s suspend=%t", c.state, c.suspend), func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			env.desiredState["worker"] = TemplateParams{Command: "c", SessionName: "worker", TemplateName: "worker"}
			session := env.createSessionBead("worker", "worker")
			env.setSessionMetadata(&session, map[string]string{
				"state": c.state, "pending_create_claim": "true",
				"last_woke_at": env.clk.Now().Add(-time.Hour).UTC().Format(time.RFC3339), // past the in-flight window
			})
			if c.suspend {
				if _, err := sessionFrontDoor(env.store).OperatorSuspend(session.ID, env.clk.Now()); err != nil {
					t.Fatal(err)
				}
				if err := env.store.SetMetadata(session.ID, "state", c.state); err != nil { // the state the heal pass left
					t.Fatal(err)
				}
			}
			if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "c"}); err != nil {
				t.Fatal(err)
			}
			for key, value := range map[string]string{"GC_SESSION_ID": session.ID, "GC_INSTANCE_TOKEN": session.Metadata["instance_token"]} {
				if err := env.sp.SetMeta("worker", key, value); err != nil {
					t.Fatal(err)
				}
			}
			for range 4 {
				b, err := env.store.Get(session.ID)
				if err != nil {
					t.Fatal(err)
				}
				env.reconcile([]beads.Bead{b})
				env.clk.Advance(30 * time.Second)
			}
			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			committed, alive := got.Metadata["started_config_hash"] != "", env.sp.IsRunning("worker")
			switch {
			case !c.suspend && (!committed || !alive):
				t.Fatalf("no suspend: committed=%t alive=%t, want the heal and a running seat (%v)", committed, alive, got.Metadata)
			case c.suspend && (committed || alive || got.Metadata["sleep_intent"] != "user-hold" || got.Metadata["state"] == "awake" || got.Metadata["state"] == "active"):
				t.Fatalf("suspended: committed=%t alive=%t meta=%v, want no commit, the runtime down and the hold kept", committed, alive, got.Metadata)
			}
		})
	}
}
