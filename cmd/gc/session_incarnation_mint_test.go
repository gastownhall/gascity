package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The incarnation mint (I-MINT, E1) as the controllers read it: v2's
// identity comparator and A7's fence see an operator-started runtime as the
// row's current incarnation, and legacy wakes a row an operator start
// advanced as it wakes any PreWake-advanced row.

// TestOperatorStartedRuntimeReadsCurrentInV2 (M8): a runtime an operator's
// Attach started carries the generation and token the Manager minted, so v2
// reads it Current (compareIdentity; ownRuntime) with no rekey, A7's fence
// digest (@gc_fence) is the row's, and the incarnation it replaced now reads
// as an older one of the same row.
func TestOperatorStartedRuntimeReadsCurrentInV2(t *testing.T) {
	m, _ := stampedMem(t, gate.Require)
	id := fieldRow(t, m, "state", "asleep", "provider", "claude")
	city := t.TempDir()
	sp := runtime.NewFake()
	mgr := session.NewManagerWithOptions(m, sp, session.WithCityPath(city))
	if err := mgr.Attach(context.Background(), testActorIn(t, session.ActorOperator, city), id, "claude", runtime.Config{}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	row := fieldInfo(t, m, id)
	env := sp.LastStartConfig("s-row").Env
	rt := runtimeIdentity{Known: true, SessionID: env["GC_SESSION_ID"], Epoch: env[runtime.RuntimeEpochEnv], Token: env[runtime.InstanceTokenEnv]}
	if v := compareIdentity(row, rt); v != identityCurrent || !ownRuntime(v, row, rt) {
		t.Fatalf("operator-started runtime %+v reads %s against row (generation %s), want current", rt, v, row.Generation)
	}
	if row.Generation != "4" || row.InstanceToken == "tok-3" || rt.Epoch != "4" {
		t.Fatalf("row generation %s token %q, runtime epoch %s; want the operator's mint (4, new token)", row.Generation, row.InstanceToken, rt.Epoch)
	}
	want, ok := session.InputFenceFor(row)
	got, gotOK := runtime.InputFenceFromEnv(env)
	if !ok || !gotOK || got.Digest() != want.Digest() {
		t.Fatalf("runtime fence %s, want the row's %s", got, want)
	}
	previous := runtimeIdentity{Known: true, SessionID: id, Epoch: "3", Token: "tok-3"}
	if v := compareIdentity(row, previous); v != identityStaleSelf {
		t.Fatalf("the replaced incarnation reads %s, want stale-self (an older incarnation)", v)
	}
}

// TestLegacyWakesAnOperatorMintedRowLikeAnyPreWakedRow (M9, legacy
// rollback): the legacy start over a row an operator's start advanced (E1)
// behaves as over a row its own PreWake advanced: it wakes it once, its
// PreWake mints the next generation and token, its Start runs under them,
// and its commit lands and releases the lease.
func TestLegacyWakesAnOperatorMintedRowLikeAnyPreWakedRow(t *testing.T) {
	for _, advancer := range []string{"legacy PreWake", "operator start"} {
		t.Run(advancer, func(t *testing.T) {
			store := beads.NewMemStore()
			f := newLeaseStartFixture(t, store)
			id := f.cand.info.ID
			if advancer == "legacy PreWake" {
				if got := f.run(); got != 1 {
					t.Fatalf("first wake = %d, want 1; log %q", got, f.log.String())
				}
			} else {
				mgr := session.NewManagerWithOptions(store, f.sp.Fake, session.WithCityPath(f.city))
				if err := mgr.Start(context.Background(), testActorIn(t, session.ActorOperator, f.city), id, "worker", runtime.Config{}); err != nil {
					t.Fatalf("operator Start: %v", err)
				}
			}
			advanced := mustGetBead(t, store, id).Metadata
			if advanced["generation"] != "2" || advanced["instance_token"] == "tok-1" {
				t.Fatalf("advanced row = %v, want generation 2 and a new token", advanced)
			}
			// The runtime dies; the row is woken again.
			if err := f.sp.Fake.Stop("worker"); err != nil {
				t.Fatal(err)
			}
			if err := store.SetMetadataBatch(id, creatingMeta(map[string]string{"pending_create_claim": "true"})); err != nil {
				t.Fatal(err)
			}
			f.cand.info = fieldInfo(t, store, id)
			starts := len(f.sp.Calls)
			if got := f.run(); got != 1 {
				t.Fatalf("wake over the advanced row = %d, want 1; log %q", got, f.log.String())
			}
			meta := mustGetBead(t, store, id).Metadata
			env := f.sp.LastStartConfig("worker").Env
			if meta["generation"] != "3" || meta["instance_token"] == advanced["instance_token"] ||
				env[runtime.RuntimeEpochEnv] != "3" || env[runtime.InstanceTokenEnv] != meta["instance_token"] {
				t.Fatalf("row %v, Start env %v; want legacy's PreWake at generation 3 started under", meta, env)
			}
			if meta["state"] == string(session.StateCreating) || meta[session.RuntimeLeaseHolderKey] != "" || len(f.sp.Calls) == starts {
				t.Fatalf("row %v after the wake, want committed and the lease released", meta)
			}
			if last := f.sp.starts[len(f.sp.starts)-1]; last == "" {
				t.Fatal("the wake's Start ran without the lease")
			}
		})
	}
}

// TestLegacyPreWakeMintsFromTheFreshRow: the legacy PreWake is the row's
// mint. It lands generation g+1 of the row it re-reads with a fresh token, a
// pending continuation reset consumed (the epoch bumped once, the marker
// cleared), and returns the incarnation and the batch the row now records;
// a row another start minted since the tick read it refuses
// (errPreWakeSuperseded) and writes nothing.
func TestLegacyPreWakeMintsFromTheFreshRow(t *testing.T) {
	store := beads.NewMemStore()
	f := newLeaseStartFixture(t, store)
	id := f.cand.info.ID
	if err := store.SetMetadataBatch(id, map[string]string{"continuation_reset_pending": "true", "continuation_epoch": "4"}); err != nil {
		t.Fatal(err)
	}
	front := sessionFrontDoor(store)
	clk := &clock.Fake{Time: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	info := fieldInfo(t, store, id)
	inc, fold, err := preWakeCommitUnder(info, front, clk, nil)
	if err != nil {
		t.Fatalf("PreWake: %v", err)
	}
	meta := mustGetBead(t, store, id).Metadata
	if meta["generation"] != "2" || meta["instance_token"] == "tok-1" || meta["continuation_epoch"] != "5" || meta["continuation_reset_pending"] != "" {
		t.Fatalf("row after PreWake = %v, want generation 2, a new token, epoch 5 and the reset consumed", meta)
	}
	if inc.Generation != 2 || inc.ContinuationEpoch != 5 || fold["instance_token"] != meta["instance_token"] || fold["continuation_epoch"] != "5" {
		t.Fatalf("incarnation %v, fold %v; want the row's", inc, fold)
	}
	if got := info.ApplyPatch(fold); got.Generation != "2" || got.InstanceToken != meta["instance_token"] {
		t.Fatalf("folded snapshot generation %s token %q, want the row's", got.Generation, got.InstanceToken)
	}
	// A tick snapshot from before that mint is superseded.
	if _, _, err := preWakeCommitUnder(info, front, clk, nil); !errors.Is(err, errPreWakeSuperseded) {
		t.Fatalf("PreWake over a row minted since = %v, want errPreWakeSuperseded", err)
	}
	if after := mustGetBead(t, store, id).Metadata; after["generation"] != "2" || after["instance_token"] != meta["instance_token"] {
		t.Fatalf("row after the refused PreWake = %v, want it untouched", after)
	}
}

// TestLegacyPreWakeDecidesTheResetOnTheRowItMints: the legacy PreWake
// decides a continuation reset on the row its mint reads, as it decides the
// rest of its patch, not on the tick's snapshot: a writer in another process
// does not take the in-process mutation lock the PreWake holds. A fresh-
// restart marker recorded after the snapshot is consumed whole (the epoch
// bumped once, the marker cleared, the conversation reset) in the row, the
// incarnation, the fold the start is built from and the wake trace alike. A
// marker the snapshot read but the row no longer records bumps and resets
// nothing.
func TestLegacyPreWakeDecidesTheResetOnTheRowItMints(t *testing.T) {
	for _, tc := range []struct {
		name               string
		atSnapshot, atMint string // continuation_reset_pending
		wantEpoch          string
		wantReset          bool
	}{
		{name: "recorded after the snapshot", atMint: "true", wantEpoch: "5", wantReset: true},
		{name: "cleared after the snapshot", atSnapshot: "true", wantEpoch: "4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_TMUX_TRACE", "1")
			var trace bytes.Buffer
			prevWriter, prevFlags, prevPrefix := log.Writer(), log.Flags(), log.Prefix()
			log.SetOutput(&trace)
			log.SetFlags(0)
			log.SetPrefix("")
			t.Cleanup(func() {
				log.SetOutput(prevWriter)
				log.SetFlags(prevFlags)
				log.SetPrefix(prevPrefix)
			})
			store := beads.NewMemStore()
			f := newLeaseStartFixture(t, store)
			id := f.cand.info.ID
			if err := store.SetMetadataBatch(id, map[string]string{
				"state": "asleep", "pending_create_claim": "", "continuation_epoch": "4",
				"session_key": "conv-old", "started_config_hash": "hash-old",
				"continuation_reset_pending": tc.atSnapshot,
			}); err != nil {
				t.Fatal(err)
			}
			info := fieldInfo(t, store, id)
			// The marker moves between the tick's snapshot and the mint's read.
			if err := store.SetMetadataBatch(id, map[string]string{"continuation_reset_pending": tc.atMint}); err != nil {
				t.Fatal(err)
			}
			clk := &clock.Fake{Time: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
			inc, fold, err := preWakeCommitUnder(info, sessionFrontDoor(store), clk, nil)
			if err != nil {
				t.Fatalf("PreWake: %v", err)
			}
			meta := mustGetBead(t, store, id).Metadata
			if meta["continuation_epoch"] != tc.wantEpoch || meta["continuation_reset_pending"] != "" || (meta["session_key"] == "") != tc.wantReset {
				t.Fatalf("row after PreWake = %v, want epoch %s, the marker cleared, the conversation reset %v", meta, tc.wantEpoch, tc.wantReset)
			}
			folded := info.ApplyPatch(fold)
			if strconv.FormatInt(inc.ContinuationEpoch, 10) != tc.wantEpoch || folded.ContinuationEpoch != tc.wantEpoch ||
				folded.ContinuationResetPending != "" || folded.SessionKey != meta["session_key"] {
				t.Fatalf("incarnation %v, folded snapshot epoch %s marker %q key %q; want the row's %v",
					inc, folded.ContinuationEpoch, folded.ContinuationResetPending, folded.SessionKey, meta)
			}
			got := trace.String()
			if traced := strings.Contains(got, "[WAKE-TRACE]"); traced != tc.wantReset || (traced && !strings.Contains(got, "session_key")) {
				t.Fatalf("wake trace %q, want a conversation-reset line naming session_key %v", got, tc.wantReset)
			}
		})
	}
}
