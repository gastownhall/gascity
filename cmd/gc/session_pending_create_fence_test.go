package main

import (
	"errors"
	"io"
	"reflect"
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
		return createPoolSessionBeadWithGuardedAliasUsingLock(bp, &cfg.Agents[0], "worker", "worker", 0, nil,
			func(_ string, _ []string, fn func() error) error { return fn() })
	}
	for range 5 {
		info, err := create()
		if !errors.Is(err, errPoolSessionNameUnavailable) {
			t.Errorf("occupied singleton create: err=%v, want name unavailable", err)
		}
		if info.ID != "" {
			// Model the old retry loop, which invalidates each new pending bead.
			closeFailedCreateBead(sessionFrontDoor(store), info.ID, now, io.Discard)
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
