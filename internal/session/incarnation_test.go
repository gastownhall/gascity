package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The incarnation mint (I-MINT; E1): every Manager start mints generation
// g+1 and a fresh token by CAS under the runtime lease before it starts, the
// runtime's identity env is built from the minted value alone, and the start
// confirms only while the row still records it.

var mintT0 = time.Date(2099, 10, 9, 12, 0, 0, 0, time.UTC)

// mintUnder mints session id's next incarnation under lease, as the legacy
// PreWake does before its runtime-only start.
func mintUnder(t testing.TB, m *Manager, lease *RuntimeLease, id string) Incarnation {
	t.Helper()
	info, err := m.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	inc, err := m.PersistedStore().MintIncarnation(context.Background(), lease, MintRequest{Expect: info, By: ActorController, Now: time.Now()})
	if err != nil {
		t.Fatalf("minting %s: %v", id, err)
	}
	return inc
}

// startRuntimeOnly is the legacy start's runtime-only bring-up of id on m:
// the controller under the lease it holds across the start
// (runtimeOnlyActor), running the incarnation its PreWake minted under it.
func startRuntimeOnly(t testing.TB, m *Manager, id, cmd string, hints runtime.Config) error {
	t.Helper()
	by := runtimeOnlyActor(t, m, id)
	return m.StartRuntimeOnly(context.Background(), by, id, mintUnder(t, m, by.Lease, id), cmd, hints)
}

// mintRowMeta is an asleep row at generation 3, token tok-3, continuation
// epoch 2, plus meta.
func mintRowMeta(meta ...string) map[string]string {
	out := map[string]string{
		"state": string(StateAsleep), "session_name": "s-mint", "template": "worker", "provider": "claude",
		"work_dir": "/tmp", "generation": "3", "instance_token": "tok-3", "continuation_epoch": "2",
	}
	for i := 0; i+1 < len(meta); i += 2 {
		out[meta[i]] = meta[i+1]
	}
	return out
}

// mintRow creates mintRowMeta(meta...)'s row in store.
func mintRow(t *testing.T, store beads.Store, meta ...string) beads.Bead {
	t.Helper()
	b, err := store.Create(sessionBeadFixture("s-mint", "open", mintRowMeta(meta...)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func rowMeta(t *testing.T, store beads.Store, id string) map[string]string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Metadata
}

func infoOf(t *testing.T, store beads.Store, id string) Info {
	t.Helper()
	info, err := NewStore(beads.SessionStore{Store: store}).Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// mintReq is a controller's mint of expect at mintT0.
func mintReq(expect Info) MintRequest {
	return MintRequest{Expect: expect, By: ActorController, Now: mintT0}
}

// TestMintIncarnationAdvancesTheRow: a mint lands g+1, a fresh token,
// last_woke_at and the carried continuation epoch on the row in one write,
// and the Incarnation it returns is that row's: its env and fence are the
// row's (InputFenceFor), and a fresh context bumps the epoch and consumes
// the reset the row records.
func TestMintIncarnationAdvancesTheRow(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("fresh=%v", fresh), func(t *testing.T) {
			backing := openLeaseStore(t, t.TempDir())
			b := mintRow(t, backing, "continuation_reset_pending", "true")
			rec := &rowWriteRecorder{Store: backing}
			req := mintReq(infoOf(t, backing, b.ID))
			req.FreshContext = fresh
			inc, err := NewStore(beads.SessionStore{Store: rec}).MintIncarnation(context.Background(), nil, req)
			if err != nil {
				t.Fatalf("MintIncarnation: %v", err)
			}
			wantEpoch, wantPending := "2", "true"
			if fresh {
				wantEpoch, wantPending = "3", ""
			}
			meta := rowMeta(t, backing, b.ID)
			if meta["generation"] != "4" || meta["instance_token"] == "tok-3" || meta["instance_token"] == "" ||
				meta["continuation_epoch"] != wantEpoch || meta["continuation_reset_pending"] != wantPending ||
				meta["last_woke_at"] != mintT0.Format(time.RFC3339) {
				t.Fatalf("row after the mint = %v", meta)
			}
			if len(rec.ifMatch) != 1 {
				t.Fatalf("writes = %v, want one conditional write", rec.writes)
			}
			if !inc.Valid() || inc.ID != b.ID || inc.Generation != 4 || inc.By != ActorController || !inc.recordedBy(meta) {
				t.Fatalf("incarnation %v does not name the row %v", inc, meta)
			}
			env := inc.Env()
			if env[runtime.RuntimeEpochEnv] != "4" || env[runtime.InstanceTokenEnv] != meta["instance_token"] ||
				env["BEADS_HOLDER_TOKEN"] != meta["instance_token"] || env[runtime.ContinuationEpochEnv] != wantEpoch {
				t.Fatalf("env %v does not name the row %v", env, meta)
			}
			fence, ok := InputFenceFor(infoOf(t, backing, b.ID))
			if !ok || fence.Digest() != inc.Fence().Digest() {
				t.Fatalf("fence digest %s, want the row's %s (A7's @gc_fence)", inc.Fence().Digest(), fence.Digest())
			}
		})
	}
}

// TestMintIncarnationRetriesFromTheFreshRead (M2): an unrelated write that
// lands between the mint's read and its write loses it the fence; the retry
// re-reads and lands g+1 of that read, keeping the unrelated write.
func TestMintIncarnationRetriesFromTheFreshRead(t *testing.T) {
	backing := openLeaseStore(t, t.TempDir())
	b := mintRow(t, backing)
	expect := infoOf(t, backing, b.ID)
	store := &racingStore{rowWriteRecorder: &rowWriteRecorder{Store: backing}}
	store.race = func() {
		if err := backing.SetMetadataBatch(b.ID, map[string]string{"synced_at": "racing"}); err != nil {
			t.Error(err)
		}
	}
	inc, err := NewStore(beads.SessionStore{Store: store}).MintIncarnation(context.Background(), nil, mintReq(expect))
	if err != nil {
		t.Fatalf("MintIncarnation: %v", err)
	}
	meta := rowMeta(t, backing, b.ID)
	if len(store.ifMatch) != 2 || meta["generation"] != "4" || inc.Generation != 4 || meta["synced_at"] != "racing" || !inc.recordedBy(meta) {
		t.Fatalf("writes %v, row %v, incarnation %v; want a lost fence, then generation 4 over the racing write", store.writes, meta, inc)
	}
}

// TestMintIncarnationRefusesARowThatMoved: a mint whose row moved since the
// caller read it writes nothing, whether the move is already there at its
// first read or lands between that read and its write: lifecycle facts,
// another start's generation or token (LifecycleInput has neither, so
// without them a retry would mint a stale g+1), a lease taken over, a
// closed row.
func TestMintIncarnationRefusesARowThatMoved(t *testing.T) {
	city := t.TempDir()
	for _, tc := range []struct {
		name string
		move map[string]string
		want error
	}{
		{"lifecycle fact", map[string]string{"state": string(StateSuspended)}, ErrMintSuperseded},
		{"hold", map[string]string{"held_until": mintT0.Add(time.Hour).Format(time.RFC3339)}, ErrMintSuperseded},
		{"another start's generation", map[string]string{"generation": "4"}, ErrMintSuperseded},
		{"another start's token", map[string]string{"instance_token": "tok-other"}, ErrMintSuperseded},
		{"lease taken over", map[string]string{RuntimeLeaseHolderKey: "host-b/7/n", RuntimeLeaseEpochKey: "9"}, ErrMintSuperseded},
		{"closed", nil, ErrMintClosed},
	} {
		for _, racing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/racing=%v", tc.name, racing), func(t *testing.T) {
				backing := openLeaseStore(t, t.TempDir())
				b := mintRow(t, backing, "session_name", "s-lease")
				front := NewStore(beads.SessionStore{Store: backing})
				lease, err := TryRuntimeLease(front, RuntimeLeaseRequest{City: city, Name: "s-lease", ID: b.ID, TTL: leaseTTL, now: func() time.Time { return mintT0 }})
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Release()
				expect := infoOf(t, backing, b.ID)
				move := func() {
					if tc.move == nil {
						if err := backing.Close(b.ID); err != nil {
							t.Error(err)
						}
						return
					}
					if err := backing.SetMetadataBatch(b.ID, tc.move); err != nil {
						t.Error(err)
					}
				}
				store := &racingStore{rowWriteRecorder: &rowWriteRecorder{Store: backing}}
				if racing {
					store.race = move
				} else {
					move()
				}
				before := rowMeta(t, backing, b.ID)
				_, err = NewStore(beads.SessionStore{Store: store}).MintIncarnation(context.Background(), lease, mintReq(expect))
				if !errors.Is(err, tc.want) {
					t.Fatalf("MintIncarnation = %v, want %v", err, tc.want)
				}
				after := rowMeta(t, backing, b.ID)
				if racing {
					before = after // the move itself is the only write
				}
				if after["generation"] != before["generation"] || after["instance_token"] != before["instance_token"] || after["last_woke_at"] != "" {
					t.Fatalf("row after the refused mint = %v, want it untouched", after)
				}
			})
		}
	}
}

// TestMintIncarnationRefusesAReleasedOrForeignLease: a mint under a lease
// already released, or held on another row, writes nothing.
func TestMintIncarnationRefusesAReleasedOrForeignLease(t *testing.T) {
	backing := openLeaseStore(t, t.TempDir())
	b := mintRow(t, backing, "session_name", "s-lease")
	front := NewStore(beads.SessionStore{Store: backing})
	lease, err := TryRuntimeLease(front, RuntimeLeaseRequest{City: t.TempDir(), Name: "s-lease", ID: b.ID, TTL: leaseTTL, now: func() time.Time { return mintT0 }})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if _, err := front.MintIncarnation(context.Background(), lease, mintReq(infoOf(t, backing, b.ID))); !errors.Is(err, ErrRuntimeLeaseLost) {
		t.Fatalf("mint under a released lease = %v, want ErrRuntimeLeaseLost", err)
	}
	foreign := &RuntimeLease{id: "other-row"}
	if _, err := front.MintIncarnation(context.Background(), foreign, mintReq(infoOf(t, backing, b.ID))); err == nil || !strings.Contains(err.Error(), "other-row") {
		t.Fatalf("mint under another row's lease = %v, want refused", err)
	}
	if meta := rowMeta(t, backing, b.ID); meta["generation"] != "3" {
		t.Fatalf("row = %v, want generation 3 left", meta)
	}
}

// TestMintIncarnationWithoutConditionalWrites (M6): a require-mode store
// that cannot fence, or one that refuses a conditional write at call time,
// refuses the mint (ErrMintNoCAS) and writes nothing; a store with
// conditional writes off (a legacy city) warns once and writes blind.
func TestMintIncarnationWithoutConditionalWrites(t *testing.T) {
	t.Run("require without CAS", func(t *testing.T) {
		backing := openLeaseStore(t, t.TempDir())
		b := mintRow(t, backing)
		_, err := NewStore(beads.SessionStore{Store: noCASStore{backing}}).MintIncarnation(context.Background(), nil, mintReq(infoOf(t, backing, b.ID)))
		if !errors.Is(err, ErrMintNoCAS) || rowMeta(t, backing, b.ID)["generation"] != "3" {
			t.Fatalf("mint = %v, row %v; want ErrMintNoCAS and no write", err, rowMeta(t, backing, b.ID))
		}
	})
	t.Run("unsupported at call time", func(t *testing.T) {
		backing := openLeaseStore(t, t.TempDir())
		b := mintRow(t, backing)
		store := unsupportedCASStore{&rowWriteRecorder{Store: backing}}
		_, err := NewStore(beads.SessionStore{Store: store}).MintIncarnation(context.Background(), nil, mintReq(infoOf(t, backing, b.ID)))
		if !errors.Is(err, ErrMintNoCAS) || rowMeta(t, backing, b.ID)["generation"] != "3" {
			t.Fatalf("mint = %v, row %v; want ErrMintNoCAS and no write", err, rowMeta(t, backing, b.ID))
		}
	})
	t.Run("off writes blind and warns once", func(t *testing.T) {
		var buf bytes.Buffer
		prev := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(prev)
		prevWarned := mintNoCASWarned.Swap(false)
		defer mintNoCASWarned.Store(prevWarned)
		store := beads.NewMemStore()
		b := mintRow(t, store)
		front := NewStore(beads.SessionStore{Store: store})
		for want := 4; want <= 5; want++ {
			inc, err := front.MintIncarnation(context.Background(), nil, mintReq(infoOf(t, store, b.ID)))
			if err != nil || inc.Generation != int64(want) || rowMeta(t, store, b.ID)["generation"] != fmt.Sprint(want) {
				t.Fatalf("off-mode mint = %v, %v; want generation %d written", inc, err, want)
			}
		}
		if n := strings.Count(buf.String(), "session mint: the session store has no conditional writes"); n != 1 {
			t.Fatalf("warned %d times, want once: %q", n, buf.String())
		}
	})
}

// unsupportedCASStore refuses every conditional write as unsupported.
type unsupportedCASStore struct{ *rowWriteRecorder }

func (s unsupportedCASStore) UpdateIfMatch(string, int64, beads.UpdateOpts) error {
	return beads.ErrConditionalWriteUnsupported
}

// TestConfirmIncarnationOnlyWhileTheRowRecordsIt (M5): a confirm lands while
// the row records its incarnation; after a newer mint it writes nothing and
// reports false; an incarnation no mint made is refused.
func TestConfirmIncarnationOnlyWhileTheRowRecordsIt(t *testing.T) {
	backing := openLeaseStore(t, t.TempDir())
	b := mintRow(t, backing)
	front := NewStore(beads.SessionStore{Store: backing})
	first, err := front.MintIncarnation(context.Background(), nil, mintReq(infoOf(t, backing, b.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if landed, err := front.ConfirmIncarnation(first, MetadataPatch{"state": string(StateActive)}); err != nil || !landed {
		t.Fatalf("confirm of the current incarnation = %v, %v; want landed", landed, err)
	}
	newer, err := front.MintIncarnation(context.Background(), nil, mintReq(infoOf(t, backing, b.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if landed, err := front.ConfirmIncarnation(first, MetadataPatch{"state_reason": "stale"}); err != nil || landed {
		t.Fatalf("confirm after a newer mint = %v, %v; want refused", landed, err)
	}
	if meta := rowMeta(t, backing, b.ID); meta["state_reason"] == "stale" || !newer.recordedBy(meta) {
		t.Fatalf("row = %v, want the newer incarnation and no stale confirm", meta)
	}
	if _, err := front.ConfirmIncarnation(Incarnation{ID: b.ID, Generation: 5}, MetadataPatch{"state": "x"}); !errors.Is(err, ErrIncarnationNotMinted) {
		t.Fatalf("confirm of an unminted incarnation = %v, want ErrIncarnationNotMinted", err)
	}
}

// TestIncarnationNeverPrintsItsToken: no formatting verb reaches the token.
func TestIncarnationNeverPrintsItsToken(t *testing.T) {
	inc := createdIncarnation("gc-1", "secret-token-value", ActorOperator, mintT0)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if got := fmt.Sprintf(verb, inc); strings.Contains(got, "secret-token-value") {
			t.Errorf("%s printed the token: %s", verb, got)
		}
	}
	if !strings.Contains(inc.String(), inc.Fence().Digest()) {
		t.Errorf("String %q does not show the fence digest", inc.String())
	}
}

// startSeen is what a provider Start saw: the row at the call and the
// launch's identity env.
type startSeen struct {
	row map[string]string
	env map[string]string
}

// mintSeeingProvider records, at each Start, the row as the store holds it
// and the launch's env; failFirst Starts die during startup instead.
type mintSeeingProvider struct {
	*runtime.Fake
	store     beads.Store
	id        string
	failFirst int
	hook      func()
	mu        sync.Mutex
	seen      []startSeen
}

func (p *mintSeeingProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	b, err := p.store.Get(p.id)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.seen = append(p.seen, startSeen{row: b.Metadata, env: cfg.Env})
	fail := len(p.seen) <= p.failFirst
	p.mu.Unlock()
	if fail {
		return fmt.Errorf("%w: session %q", runtime.ErrSessionDiedDuringStartup, name)
	}
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	if p.hook != nil {
		p.hook()
	}
	return nil
}

// assertStartRanMinted fails unless s's launch ran under the row's
// incarnation as of the call, a new one after prev's generation.
func assertStartRanMinted(t *testing.T, s startSeen, prevGen int) {
	t.Helper()
	row, env := s.row, s.env
	if row["generation"] != fmt.Sprint(prevGen+1) {
		t.Fatalf("row at Start = %v, want generation %d minted before it", row, prevGen+1)
	}
	for k, rk := range map[string]string{
		runtime.RuntimeEpochEnv: "generation", runtime.InstanceTokenEnv: "instance_token",
		"BEADS_HOLDER_TOKEN": "instance_token", runtime.ContinuationEpochEnv: "continuation_epoch",
	} {
		if env[k] == "" || env[k] != row[rk] {
			t.Fatalf("Start env %s = %q, want the row's %s %q", k, env[k], rk, row[rk])
		}
	}
}

// TestManagerStartMintsBeforeStart (M1): an operator's Attach or Start of a
// stopped session advances the row's generation and token before the
// provider Start, which runs under them alone, and confirms that
// incarnation; a pending continuation reset is consumed in the same mint.
func TestManagerStartMintsBeforeStart(t *testing.T) {
	for _, verb := range []string{"attach", "start", "start with a reset pending"} {
		t.Run(verb, func(t *testing.T) {
			store := openLeaseStore(t, t.TempDir())
			var meta []string
			if strings.Contains(verb, "reset") {
				meta = []string{"continuation_reset_pending", "true"}
			}
			b := mintRow(t, store, meta...)
			sp := &mintSeeingProvider{Fake: runtime.NewFake(), store: store, id: b.ID}
			mgr := newTestManager(t, store, sp)
			by := testActor(mgr, ActorOperator)
			var err error
			if verb == "attach" {
				err = mgr.Attach(context.Background(), by, b.ID, "claude", runtime.Config{})
			} else {
				err = mgr.Start(context.Background(), by, b.ID, "claude", runtime.Config{})
			}
			if err != nil {
				t.Fatalf("%s: %v", verb, err)
			}
			if len(sp.seen) != 1 {
				t.Fatalf("starts = %d, want 1", len(sp.seen))
			}
			assertStartRanMinted(t, sp.seen[0], 3)
			if tok := sp.seen[0].row["instance_token"]; tok == "tok-3" {
				t.Fatalf("Start ran under the previous incarnation's token %q", tok)
			}
			wantEpoch := "2"
			if meta != nil {
				wantEpoch = "3"
			}
			after := rowMeta(t, store, b.ID)
			if after["generation"] != "4" || after["instance_token"] != sp.seen[0].row["instance_token"] ||
				after["continuation_epoch"] != wantEpoch || after["continuation_reset_pending"] != "" || after["state"] != string(StateActive) {
				t.Fatalf("row after the start = %v, want generation 4, its token, epoch %s, active", after, wantEpoch)
			}
		})
	}
}

// TestManagerStartConfirmRefusedAfterANewerMint (M5): a start whose row
// records another incarnation by the time it confirms writes no confirm,
// reports ErrStateSync wrapping ErrMintSuperseded, and leaves the runtime to
// its newer owner rather than stop it by name.
func TestManagerStartConfirmRefusedAfterANewerMint(t *testing.T) {
	store := openLeaseStore(t, t.TempDir())
	b := mintRow(t, store)
	sp := &mintSeeingProvider{Fake: runtime.NewFake(), store: store, id: b.ID}
	sp.hook = func() {
		if err := store.SetMetadataBatch(b.ID, map[string]string{"generation": "9", "instance_token": "tok-newer"}); err != nil {
			t.Error(err)
		}
	}
	mgr := newTestManager(t, store, sp)
	err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude", runtime.Config{})
	if !errors.Is(err, ErrStateSync) || !errors.Is(err, ErrMintSuperseded) {
		t.Fatalf("Start = %v, want ErrStateSync wrapping ErrMintSuperseded", err)
	}
	after := rowMeta(t, store, b.ID)
	if after["state"] == string(StateActive) || after["instance_token"] != "tok-newer" {
		t.Fatalf("row = %v, want the newer incarnation unconfirmed by the stale start", after)
	}
	if sp.CountCalls("Stop", "s-mint") != 0 {
		t.Fatal("the refused confirm stopped the runtime by name")
	}
}

// TestStaleKeyRetryMintsAgain (M4): a resume that dies during startup and
// relaunches fresh mints again for the relaunch: two Starts, two
// generations and tokens, each the row's at its call, and the row ends
// recording the relaunch's. The relaunch keeps the epoch and leaves the
// reset the stale-key clear recorded to the next start. The legacy
// runtime-only start relaunches under the incarnation its caller's PreWake
// minted, which that caller commits against.
func TestStaleKeyRetryMintsAgain(t *testing.T) {
	for _, runtimeOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("runtimeOnly=%v", runtimeOnly), func(t *testing.T) {
			store := openLeaseStore(t, t.TempDir())
			b := mintRow(t, store, "session_key", "k-stale", "resume_flag", "--resume")
			sp := &mintSeeingProvider{Fake: runtime.NewFake(), store: store, id: b.ID, failFirst: 1}
			mgr := newTestManager(t, store, sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
			var err error
			if runtimeOnly {
				err = startRuntimeOnly(t, mgr, b.ID, "claude --resume k-stale", runtime.Config{}) // its PreWake mints 4
			} else {
				err = mgr.Start(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude --resume k-stale", runtime.Config{})
			}
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			if len(sp.seen) != 2 {
				t.Fatalf("starts = %d, want the failed resume and one fresh relaunch", len(sp.seen))
			}
			first, second := sp.seen[0], sp.seen[1]
			assertStartRanMinted(t, first, 3)
			after := rowMeta(t, store, b.ID)
			if after["continuation_reset_pending"] != "true" || first.env[runtime.ContinuationEpochEnv] != second.env[runtime.ContinuationEpochEnv] {
				t.Fatalf("row %v, relaunch env %v after %v; want the same epoch and the reset left pending", after, second.env, first.env)
			}
			if runtimeOnly {
				assertStartRanMinted(t, second, 3)
				if second.env[runtime.InstanceTokenEnv] != first.env[runtime.InstanceTokenEnv] || after["generation"] != "4" {
					t.Fatalf("legacy relaunch env %v after %v, row %v; want its caller's incarnation kept", second.env, first.env, after)
				}
				return
			}
			assertStartRanMinted(t, second, 4)
			if first.env[runtime.InstanceTokenEnv] == second.env[runtime.InstanceTokenEnv] {
				t.Fatalf("relaunch env %v after %v, want a new token", second.env, first.env)
			}
			if after["generation"] != "5" || after["instance_token"] != second.env[runtime.InstanceTokenEnv] {
				t.Fatalf("row = %v, want the relaunch's incarnation", after)
			}
		})
	}
}

// TestOperatorStartConvergesOnAStartThatWonTheLease (M3): a start that held
// the runtime lease between an operator's read and the operator's own
// acquire (here the controller's: a PreWake mint under the lease, then its
// Start) is serialized by the lease. When its runtime is up the operator
// converges on it without minting or starting: one process, and the row and
// the runtime name one generation. When it died, the operator mints the next
// generation and starts: two Starts, two generations, each the row's.
func TestOperatorStartConvergesOnAStartThatWonTheLease(t *testing.T) {
	for _, died := range []bool{false, true} {
		t.Run(fmt.Sprintf("died=%v", died), func(t *testing.T) {
			store := openLeaseStore(t, t.TempDir())
			b := mintRow(t, store)
			sp := &mintSeeingProvider{Fake: runtime.NewFake(), store: store, id: b.ID}
			gate := &isRunningHook{Provider: sp, at: 2}
			mgr := newTestManager(t, store, gate)
			city, _ := mgr.CityDir()
			gate.once = func() {
				// The controller's start, between the operator's read and its lease.
				lease, err := TryRuntimeLease(mgr.PersistedStore(), RuntimeLeaseRequest{City: city.Path(), Name: "s-mint", ID: b.ID, TTL: leaseTTL})
				if err != nil {
					t.Error(err)
					return
				}
				defer lease.Release()
				inc := mintUnder(t, mgr, lease, b.ID)
				if err := sp.Start(context.Background(), "s-mint", runtime.Config{Env: inc.Env()}); err != nil {
					t.Error(err)
				}
				if died {
					_ = sp.Stop("s-mint")
				}
			}
			if err := mgr.Attach(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude", runtime.Config{}); err != nil {
				t.Fatalf("Attach: %v", err)
			}
			if gate.calls < gate.at || len(sp.seen) == 0 || sp.seen[0].env[runtime.RuntimeEpochEnv] != "4" {
				t.Fatalf("the controller's start did not run inside the operator's attach (IsRunning calls %d)", gate.calls)
			}
			after := rowMeta(t, store, b.ID)
			last := sp.seen[len(sp.seen)-1]
			if !died {
				if len(sp.seen) != 1 || after["generation"] != "4" || after["instance_token"] != last.env[runtime.InstanceTokenEnv] {
					t.Fatalf("starts %d, row %v; want the controller's one start (generation 4) converged on", len(sp.seen), after)
				}
				return
			}
			if len(sp.seen) != 2 || sp.seen[0].env[runtime.RuntimeEpochEnv] != "4" {
				t.Fatalf("starts = %d, want the controller's (4) and the operator's", len(sp.seen))
			}
			assertStartRanMinted(t, last, 4)
			if after["generation"] != "5" || after["instance_token"] != last.env[runtime.InstanceTokenEnv] {
				t.Fatalf("row = %v, want the operator's generation 5", after)
			}
		})
	}
}

// isRunningHook runs once at the at-th IsRunning, then answers from its
// provider as it stood before once ran: the read a start decides on, of a
// runtime another start brings up right after. An Attach reads IsRunning
// first for the row's transport (transportForBead), then for its decision.
type isRunningHook struct {
	runtime.Provider
	at, calls int
	once      func()
}

func (p *isRunningHook) IsRunning(name string) bool {
	p.calls++
	running := p.Provider.IsRunning(name)
	if p.calls == p.at {
		p.once()
	}
	return running
}

// TestCreateStartedTakesTheRuntimeLease (M7): CreateSession's first start
// runs under the runtime lease on the new row's name: a name whose flock is
// held refuses busy before any provider call and rolls the row back, and a
// start that ran confirms its created incarnation and releases the lease.
func TestCreateStartedTakesTheRuntimeLease(t *testing.T) {
	t.Run("held flock is busy", func(t *testing.T) {
		store := openLeaseStore(t, t.TempDir())
		sp := runtime.NewFake()
		mgr := newTestManager(t, store, sp)
		city, _ := mgr.CityDir()
		held, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: city.Path(), Name: "named-worker"})
		if err != nil {
			t.Fatal(err)
		}
		defer held.Release()
		_, err = mgr.CreateSession(context.Background(), CreateOptions{ExplicitName: "named-worker", Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude"})
		if !errors.Is(err, ErrRuntimeLeaseBusy) || !errors.Is(err, ErrSessionStarting) {
			t.Fatalf("CreateSession under a held flock = %v, want busy (ErrSessionStarting)", err)
		}
		if sp.CountCalls("Start", "named-worker") != 0 {
			t.Fatal("the busy create started the runtime")
		}
		all, err := store.List(beads.ListQuery{Type: BeadType, IncludeClosed: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range all {
			if row.Status != "closed" {
				t.Fatalf("row %s left %s, want the busy create rolled back", row.ID, row.Status)
			}
		}
	})
	t.Run("started under the lease", func(t *testing.T) {
		store := openLeaseStore(t, t.TempDir())
		sp := &createLeaseProvider{Fake: runtime.NewFake(), store: store}
		mgr := newTestManager(t, store, sp)
		info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude"})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if len(sp.holders) != 1 || sp.holders[0] == "" || !sp.watched[0] {
			t.Fatalf("lease holders seen at Start = %q (watched %v), want one held, watched lease", sp.holders, sp.watched)
		}
		meta := rowMeta(t, store, info.ID)
		if meta[RuntimeLeaseHolderKey] != "" || meta[RuntimeLeaseEpochKey] != "1" || meta["state"] != string(StateActive) || meta["generation"] != "1" {
			t.Fatalf("row after the create = %v, want generation 1 confirmed active and epoch 1 released", meta)
		}
		env := sp.LastStartConfig(info.SessionName).Env
		if env[runtime.RuntimeEpochEnv] != "1" || env[runtime.InstanceTokenEnv] != meta["instance_token"] {
			t.Fatalf("Start env %v, want the created incarnation %v", env, meta)
		}
	})
	t.Run("confirm refused after a newer mint", func(t *testing.T) {
		store := openLeaseStore(t, t.TempDir())
		sp := &createLeaseProvider{Fake: runtime.NewFake(), store: store, remint: true}
		mgr := newTestManager(t, store, sp)
		_, err := mgr.CreateSession(context.Background(), CreateOptions{ExplicitName: "named-worker", Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude"})
		if !errors.Is(err, ErrMintSuperseded) || !errors.Is(err, ErrStateSync) {
			t.Fatalf("CreateSession over a re-minted row = %v, want ErrStateSync wrapping ErrMintSuperseded", err)
		}
		// The runtime is the newer incarnation's owner's: never stopped by
		// name, and its row never closed under it.
		if !sp.IsRunning("named-worker") || sp.CountCalls("Stop", "named-worker") != 0 {
			t.Fatal("the refused confirm stopped the runtime by name")
		}
		all, err := store.List(beads.ListQuery{Type: BeadType, IncludeClosed: true})
		if err != nil || len(all) != 1 || all[0].Status == "closed" {
			t.Fatalf("rows %v (%v), want the created row left open", all, err)
		}
	})
}

// createLeaseProvider records, at each Start, the launched row's lease
// holder and whether the Start's ctx ends at the lease's SafeUntil; remint
// moves the row's token right after the Start.
type createLeaseProvider struct {
	*runtime.Fake
	store   beads.Store
	remint  bool
	holders []string
	watched []bool
}

func (p *createLeaseProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	id := cfg.Env["GC_SESSION_ID"]
	b, _ := p.store.Get(id)
	p.holders = append(p.holders, b.Metadata[RuntimeLeaseHolderKey])
	_, deadline := ctx.Deadline()
	p.watched = append(p.watched, deadline)
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	if p.remint {
		return p.store.SetMetadataBatch(id, map[string]string{"instance_token": "tok-newer"})
	}
	return nil
}

// TestRuntimeOnlyStartRunsTheCallersIncarnation (S6): the legacy
// runtime-only start runs only under the incarnation its caller minted, and
// only while the row still records it; it mints no token of its own.
func TestRuntimeOnlyStartRunsTheCallersIncarnation(t *testing.T) {
	store := openLeaseStore(t, t.TempDir())
	b := mintRow(t, store)
	sp := &mintSeeingProvider{Fake: runtime.NewFake(), store: store, id: b.ID}
	mgr := newTestManager(t, store, sp)
	by := runtimeOnlyActor(t, mgr, b.ID)
	for name, inc := range map[string]Incarnation{
		"unminted":       {ID: b.ID, Generation: 4, ContinuationEpoch: 1},
		"another row's":  createdIncarnation("other", "tok-o", ActorController, mintT0),
		"the zero value": {},
	} {
		if err := mgr.StartRuntimeOnly(context.Background(), by, b.ID, inc, "claude", runtime.Config{}); !errors.Is(err, ErrIncarnationNotMinted) {
			t.Errorf("%s: StartRuntimeOnly = %v, want ErrIncarnationNotMinted", name, err)
		}
	}
	stale := mintUnder(t, mgr, by.Lease, b.ID)
	mintUnder(t, mgr, by.Lease, b.ID) // a newer one since
	if err := mgr.StartRuntimeOnly(context.Background(), by, b.ID, stale, "claude", runtime.Config{}); !errors.Is(err, ErrMintSuperseded) {
		t.Fatalf("StartRuntimeOnly under a superseded incarnation = %v, want ErrMintSuperseded", err)
	}
	if len(sp.seen) != 0 {
		t.Fatalf("a refused runtime-only start reached the provider: %d starts", len(sp.seen))
	}
	current := mintUnder(t, mgr, by.Lease, b.ID)
	if err := mgr.StartRuntimeOnly(context.Background(), by, b.ID, current, "claude", runtime.Config{}); err != nil {
		t.Fatalf("StartRuntimeOnly: %v", err)
	}
	if len(sp.seen) != 1 || sp.seen[0].env[runtime.RuntimeEpochEnv] != fmt.Sprint(current.Generation) || !current.recordedBy(sp.seen[0].row) ||
		sp.seen[0].env[runtime.InstanceTokenEnv] != sp.seen[0].row["instance_token"] {
		t.Fatalf("Start saw %+v, want the caller's incarnation %v", sp.seen, current)
	}
}

// TestStartNeverMintsOverALiveRuntime: under its lease a start reads the
// runtime fresh, and a runtime that is up with its agent alive is never
// minted over: the row and the only live runtime keep naming one
// incarnation, and nothing starts.
//   - A managed suspend (`gc session suspend`) leaves the runtime to the
//     controller's drain; an operator's attach or submit before it lands
//     resumes the suspended row on that runtime.
//   - A provider whose cached IsRunning reads a runtime gone that another
//     start brought up before this one read the row (the tmux state cache's
//     TTL, its degraded mode).
func TestStartNeverMintsOverALiveRuntime(t *testing.T) {
	for _, verb := range []string{"attach", "submit"} {
		t.Run("managed suspend then "+verb, func(t *testing.T) {
			store := openLeaseStore(t, t.TempDir())
			sp := runtime.NewFake()
			mgr := newTestManager(t, store, sp)
			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatal(err)
			}
			runEnv := sp.LastStartConfig(info.SessionName).Env
			if _, err := mgr.PersistedStore().OperatorSuspend(info.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
			by := testActor(mgr, ActorOperator)
			if verb == "attach" {
				err = mgr.Attach(context.Background(), by, info.ID, "claude", runtime.Config{})
			} else {
				_, err = mgr.Submit(context.Background(), by, info.ID, "hi", "claude", runtime.Config{}, SubmitIntentDefault)
			}
			if err != nil {
				t.Fatalf("%s: %v", verb, err)
			}
			after := rowMeta(t, store, info.ID)
			if after["generation"] != runEnv[runtime.RuntimeEpochEnv] || after["instance_token"] != runEnv[runtime.InstanceTokenEnv] {
				t.Fatalf("row gen=%s tok=%s, but the only live runtime runs gen=%s tok=%s", after["generation"], after["instance_token"], runEnv[runtime.RuntimeEpochEnv], runEnv[runtime.InstanceTokenEnv])
			}
			if n := sp.CountCalls("Start", info.SessionName); n != 1 {
				t.Fatalf("starts = %d, want the create's only", n)
			}
			if after["state"] != string(StateActive) || after["sleep_intent"] != "" {
				t.Fatalf("row = %v, want the operator's resume converged: active, the hold consumed", after)
			}
		})
	}
	t.Run("stale provider cache", func(t *testing.T) {
		store := openLeaseStore(t, t.TempDir())
		b := mintRow(t, store)
		sp := &staleCacheProvider{Fake: runtime.NewFake(), stale: true}
		mgr := newTestManager(t, store, sp)
		city, _ := mgr.CityDir()
		// The controller's start, wholly under its lease, before the operator reads.
		lease, err := TryRuntimeLease(mgr.PersistedStore(), RuntimeLeaseRequest{City: city.Path(), Name: "s-mint", ID: b.ID, TTL: leaseTTL})
		if err != nil {
			t.Fatal(err)
		}
		inc := mintUnder(t, mgr, lease, b.ID)
		if err := sp.Fake.Start(context.Background(), "s-mint", runtime.Config{Env: inc.Env()}); err != nil {
			t.Fatal(err)
		}
		lease.Release()
		if err := mgr.Attach(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude", runtime.Config{}); err != nil {
			t.Fatalf("Attach: %v", err)
		}
		if after := rowMeta(t, store, b.ID); after["generation"] != "4" || after["instance_token"] != inc.token || sp.CountCalls("Start", "s-mint") != 1 {
			t.Fatalf("row %v, starts %d; want the controller's live generation 4 kept and no second start", after, sp.CountCalls("Start", "s-mint"))
		}
	})
}

// staleCacheProvider answers IsRunning from a snapshot taken before another
// start, until its own Start invalidates it, as the tmux provider's state
// cache does; its fresh read (ObserveLivenessSince, as tmux's
// FreshLivenessObserver) goes past it.
type staleCacheProvider struct {
	*runtime.Fake
	stale bool
}

func (p *staleCacheProvider) IsRunning(name string) bool {
	if p.stale {
		return false
	}
	return p.Fake.IsRunning(name)
}

func (p *staleCacheProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.stale = false
	return p.Fake.Start(ctx, name, cfg)
}

func (p *staleCacheProvider) ObserveLivenessSince(name string, _ []string, _ time.Time) (runtime.Liveness, error) {
	running := p.Fake.IsRunning(name)
	return runtime.Liveness{Running: running, Alive: running}, nil
}

// recyclingProvider's Start replaces a pane whose agent died (a zombie), as
// the tmux provider's zombie recycle does, instead of reporting it exists.
type recyclingProvider struct{ *runtime.Fake }

func (p *recyclingProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.Zombies[name] {
		_ = p.Stop(name)
		delete(p.Zombies, name)
	}
	return p.Fake.Start(ctx, name, cfg)
}

// TestStartReplacesAZombieUnderANewIncarnation: resuming a suspended row
// whose pane is up with its agent dead (a zombie) is a start, not a
// converge: the start mints and its Start replaces the pane, so the row and
// the new process name the new incarnation. A start that still finds the
// name taken (a provider that does not recycle) converges and withdraws its
// mint. (A row not suspended keeps ensureRunning's: an operator attaches to
// a crashed pane to read it.)
func TestStartReplacesAZombieUnderANewIncarnation(t *testing.T) {
	for _, recycles := range []bool{true, false} {
		t.Run(fmt.Sprintf("recycles=%v", recycles), func(t *testing.T) {
			store := openLeaseStore(t, t.TempDir())
			b := mintRow(t, store, "state", string(StateSuspended))
			fake := runtime.NewFake()
			if err := fake.Start(context.Background(), "s-mint", runtime.Config{Env: map[string]string{runtime.RuntimeEpochEnv: "3", runtime.InstanceTokenEnv: "tok-3"}}); err != nil {
				t.Fatal(err)
			}
			fake.Zombies["s-mint"] = true
			var sp runtime.Provider = fake
			if recycles {
				sp = &recyclingProvider{Fake: fake}
			}
			mgr := newTestManager(t, store, sp)
			hints := runtime.Config{ProcessNames: []string{"claude"}}
			if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude", hints); err != nil {
				t.Fatalf("Start: %v", err)
			}
			after := rowMeta(t, store, b.ID)
			env := fake.LastStartConfig("s-mint").Env
			if !recycles {
				if after["generation"] != "3" || after["instance_token"] != "tok-3" || after["last_woke_at"] != "" {
					t.Fatalf("row %v, want the converged start's mint withdrawn", after)
				}
				return
			}
			if after["generation"] != "4" || env[runtime.RuntimeEpochEnv] != "4" || env[runtime.InstanceTokenEnv] != after["instance_token"] {
				t.Fatalf("row %v, recycled pane env %v; want both on the new generation 4", after, env)
			}
		})
	}
}

// TestFailedStartWithdrawsItsMint: a start that launches nothing (its Start
// fails, its stale-key relaunch fails) leaves the row as it found it: the
// generation, token, continuation epoch, last_woke_at and a consumed reset
// marker are written back while the row still records the mint.
func TestFailedStartWithdrawsItsMint(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta []string
		sp   func(*runtime.Fake, beads.Store, string) runtime.Provider
	}{
		{"start error", []string{"continuation_reset_pending", "true", "last_woke_at", "2099-01-01T00:00:00Z"}, func(f *runtime.Fake, _ beads.Store, _ string) runtime.Provider {
			return &startOverrideProvider{Fake: f, startErr: errors.New("out of memory")}
		}},
		{"stale-key relaunch fails", []string{"session_key", "k-stale", "resume_flag", "--resume"}, func(f *runtime.Fake, store beads.Store, id string) runtime.Provider {
			return &mintSeeingProvider{Fake: f, store: store, id: id, failFirst: 2}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openLeaseStore(t, t.TempDir())
			b := mintRow(t, store, tc.meta...)
			before := rowMeta(t, store, b.ID)
			mgr := newTestManager(t, store, tc.sp(runtime.NewFake(), store, b.ID), WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
			if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude --resume k-stale", runtime.Config{}); err == nil {
				t.Fatal("Start succeeded, want its failure")
			}
			after := rowMeta(t, store, b.ID)
			for _, k := range []string{"generation", "instance_token", "continuation_epoch", "last_woke_at"} {
				if after[k] != before[k] {
					t.Errorf("%s = %q after the failed start, want %q", k, after[k], before[k])
				}
			}
			if tc.name == "start error" && after["continuation_reset_pending"] != "true" {
				t.Errorf("continuation_reset_pending = %q, want the consumed reset restored", after["continuation_reset_pending"])
			}
		})
	}
}

// TestUnmintOnlyWhileTheRowRecordsTheMint: a withdrawal writes nothing over
// a row that moved on, and an empty confirm still reads whether the row
// records its incarnation.
func TestUnmintOnlyWhileTheRowRecordsTheMint(t *testing.T) {
	backing := openLeaseStore(t, t.TempDir())
	b := mintRow(t, backing)
	front := NewStore(beads.SessionStore{Store: backing})
	first, err := front.MintIncarnation(context.Background(), nil, mintReq(infoOf(t, backing, b.ID)))
	if err != nil {
		t.Fatal(err)
	}
	newer, err := front.MintIncarnation(context.Background(), nil, mintReq(infoOf(t, backing, b.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if landed, err := front.unmintIncarnation(first); err != nil || landed {
		t.Fatalf("withdrawing a superseded mint = %v, %v; want refused", landed, err)
	}
	if meta := rowMeta(t, backing, b.ID); !newer.recordedBy(meta) {
		t.Fatalf("row %v, want the newer mint kept", meta)
	}
	if landed, err := front.ConfirmIncarnation(first, nil); err != nil || landed {
		t.Fatalf("empty confirm of a superseded mint = %v, %v; want false", landed, err)
	}
	if landed, err := front.ConfirmIncarnation(newer, nil); err != nil || !landed {
		t.Fatalf("empty confirm of the current mint = %v, %v; want true", landed, err)
	}
	if landed, err := front.unmintIncarnation(newer); err != nil || !landed {
		t.Fatalf("withdrawing the current mint = %v, %v; want landed", landed, err)
	}
	if meta := rowMeta(t, backing, b.ID); !first.recordedBy(meta) {
		t.Fatalf("row %v, want the mint before it back", meta)
	}
}

// TestMintRefusesALapsedLease: a lease past its SafeUntil on its own clock
// mints nothing, though no other holder took it yet.
func TestMintRefusesALapsedLease(t *testing.T) {
	backing := openLeaseStore(t, t.TempDir())
	b := mintRow(t, backing, "session_name", "s-lease")
	front := NewStore(beads.SessionStore{Store: backing})
	now := mintT0
	lease, err := TryRuntimeLease(front, RuntimeLeaseRequest{City: t.TempDir(), Name: "s-lease", ID: b.ID, TTL: leaseTTL, now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	now = lease.SafeUntil()
	_, err = front.MintIncarnation(context.Background(), lease, mintReq(infoOf(t, backing, b.ID)))
	if !errors.Is(err, ErrMintSuperseded) || !errors.Is(err, ErrRuntimeLeaseLost) {
		t.Fatalf("mint under a lapsed lease = %v, want ErrMintSuperseded wrapping ErrRuntimeLeaseLost", err)
	}
	if meta := rowMeta(t, backing, b.ID); meta["generation"] != "3" {
		t.Fatalf("row %v, want generation 3 left", meta)
	}
}

// TestRuntimeOnlyStartConsumesALateReset: a continuation reset recorded
// after the caller's PreWake (its prepared start's stale-resume clear) is
// consumed by the legacy runtime-only start: the env publishes the bumped
// epoch, which the row records, under the caller's generation and token.
func TestRuntimeOnlyStartConsumesALateReset(t *testing.T) {
	store := openLeaseStore(t, t.TempDir())
	b := mintRow(t, store)
	sp := &mintSeeingProvider{Fake: runtime.NewFake(), store: store, id: b.ID}
	mgr := newTestManager(t, store, sp)
	by := runtimeOnlyActor(t, mgr, b.ID)
	inc := mintUnder(t, mgr, by.Lease, b.ID)
	if err := store.SetMetadataBatch(b.ID, map[string]string{"continuation_reset_pending": "true"}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.StartRuntimeOnly(context.Background(), by, b.ID, inc, "claude", runtime.Config{}); err != nil {
		t.Fatalf("StartRuntimeOnly: %v", err)
	}
	env, meta := sp.seen[0].env, rowMeta(t, store, b.ID)
	want := fmt.Sprint(inc.ContinuationEpoch + 1)
	if env[runtime.ContinuationEpochEnv] != want || meta["continuation_epoch"] != want || meta["continuation_reset_pending"] != "" ||
		env[runtime.InstanceTokenEnv] != inc.token || !inc.recordedBy(meta) {
		t.Fatalf("env %v, row %v; want epoch %s under the caller's incarnation", env, meta, want)
	}
}

// TestRecordedIncarnationForTestStaysInTests: production code never builds
// an incarnation it did not mint. The test-only constructor appears in no
// non-test Go file of the repository (so in no package that imports this
// one, directly or not) but its definition.
func TestRecordedIncarnationForTestStaysInTests(t *testing.T) {
	root := repoRoot(t)
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules" || strings.HasPrefix(name, "bazel-")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		n := strings.Count(string(src), "RecordedIncarnationForTest(")
		rel, _ := filepath.Rel(root, path)
		if filepath.ToSlash(rel) == "internal/session/incarnation.go" {
			n-- // its definition
		}
		if n > 0 {
			t.Errorf("%s calls RecordedIncarnationForTest outside a test", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 500 {
		t.Fatalf("scanned %d Go files; the walk missed the repository", scanned)
	}
}

// launchThenFailProvider scripts each Start: "die" fails during startup
// with nothing launched; "launch-fail" launches the session under the
// start's env, then fails (a readiness wait that timed out) with a cleanup
// that could not kill it, as tmux's best-effort cleanupFailedStart can;
// anything else starts. Unscripted Starts start. Its GetMeta reads a running
// session's environment, as tmux's show-environment does.
type launchThenFailProvider struct {
	*runtime.Fake
	script []string
	n      int
}

func (p *launchThenFailProvider) GetMeta(name, key string) (string, error) {
	if p.IsRunning(name) {
		if v, ok := p.LastStartConfig(name).Env[key]; ok {
			return v, nil
		}
	}
	return p.Fake.GetMeta(name, key)
}

func (p *launchThenFailProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	step := ""
	if p.n < len(p.script) {
		step = p.script[p.n]
	}
	p.n++
	switch step {
	case "die":
		p.Calls = append(p.Calls, runtime.Call{Method: "Start", Name: name, Config: cfg})
		return fmt.Errorf("%w: session %q", runtime.ErrSessionDiedDuringStartup, name)
	case "launch-fail":
		if err := p.Fake.Start(ctx, name, cfg); err != nil {
			return err
		}
		return errors.New("waiting for the agent: readiness timed out")
	}
	return p.Fake.Start(ctx, name, cfg)
}

// TestFailedStartKeepsAMintItsProcessCarries: a Start that failed after its
// launch, whose process survived, keeps its mint: the row and the surviving
// runtime name one incarnation, and the next start converges on it instead
// of starting a second process. So does a read of the runtime's token that
// cannot answer. A stale-key relaunch follows the same rule.
func TestFailedStartKeepsAMintItsProcessCarries(t *testing.T) {
	t.Run("launched then failed", func(t *testing.T) {
		store := openLeaseStore(t, t.TempDir())
		b := mintRow(t, store)
		sp := &launchThenFailProvider{Fake: runtime.NewFake(), script: []string{"launch-fail"}}
		mgr := newTestManager(t, store, sp)
		by := testActor(mgr, ActorOperator)
		if err := mgr.Start(context.Background(), by, b.ID, "claude", runtime.Config{}); err == nil {
			t.Fatal("Start succeeded, want its failure")
		}
		env, row := sp.LastStartConfig("s-mint").Env, rowMeta(t, store, b.ID)
		if row["generation"] != "4" || row["instance_token"] != env[runtime.InstanceTokenEnv] {
			t.Fatalf("row gen=%s tok=%s, but the surviving runtime runs gen=%s tok=%s", row["generation"], row["instance_token"], env[runtime.RuntimeEpochEnv], env[runtime.InstanceTokenEnv])
		}
		if err := mgr.Start(context.Background(), by, b.ID, "claude", runtime.Config{}); err != nil {
			t.Fatalf("second Start: %v", err)
		}
		if n, after := sp.CountCalls("Start", "s-mint"), rowMeta(t, store, b.ID); n != 1 || after["instance_token"] != env[runtime.InstanceTokenEnv] {
			t.Fatalf("starts %d, row %v; want the second start converged on the survivor", n, after)
		}
	})
	t.Run("token unreadable", func(t *testing.T) {
		store := openLeaseStore(t, t.TempDir())
		b := mintRow(t, store)
		fake := runtime.NewFake()
		fake.GetMetaErrors = map[string]map[string]error{"s-mint": {runtime.InstanceTokenEnv: errors.New("tmux: server busy")}}
		sp := &startOverrideProvider{Fake: fake, startErr: errors.New("out of memory")}
		mgr := newTestManager(t, store, sp)
		if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude", runtime.Config{}); err == nil {
			t.Fatal("Start succeeded, want its failure")
		}
		if row := rowMeta(t, store, b.ID); row["generation"] != "4" {
			t.Fatalf("row %v, want the mint kept when the runtime's token cannot be read", row)
		}
	})
	t.Run("stale-key relaunch launched then failed", func(t *testing.T) {
		store := openLeaseStore(t, t.TempDir())
		b := mintRow(t, store, "session_key", "k-stale", "resume_flag", "--resume")
		sp := &launchThenFailProvider{Fake: runtime.NewFake(), script: []string{"die", "launch-fail"}}
		mgr := newTestManager(t, store, sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
		if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude --resume k-stale", runtime.Config{}); err == nil {
			t.Fatal("Start succeeded, want the relaunch's failure")
		}
		env, row := sp.LastStartConfig("s-mint").Env, rowMeta(t, store, b.ID)
		if row["generation"] != "5" || env[runtime.RuntimeEpochEnv] != "5" || row["instance_token"] != env[runtime.InstanceTokenEnv] {
			t.Fatalf("row gen=%s tok=%s, but the surviving relaunch runs gen=%s tok=%s", row["generation"], row["instance_token"], env[runtime.RuntimeEpochEnv], env[runtime.InstanceTokenEnv])
		}
	})
}

// freshReadUnavailable is a provider whose fresh read cannot answer (a tmux
// list-panes that timed out) while its Start works.
type freshReadUnavailable struct{ *runtime.Fake }

func (p *freshReadUnavailable) ObserveLivenessSince(string, []string, time.Time) (runtime.Liveness, error) {
	return runtime.Liveness{}, fmt.Errorf("%w: observing tmux session: state cache cannot vouch for it", runtime.ErrRuntimeUnavailable)
}

// TestStartRefusesWhenTheFreshReadCannotAnswer: under its lease a start
// whose fresh read of the runtime fails refuses (fail closed): no mint, no
// Start, runtime.ErrRuntimeUnavailable reported once.
func TestStartRefusesWhenTheFreshReadCannotAnswer(t *testing.T) {
	store := openLeaseStore(t, t.TempDir())
	b := mintRow(t, store)
	sp := &freshReadUnavailable{Fake: runtime.NewFake()}
	mgr := newTestManager(t, store, sp)
	err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude", runtime.Config{})
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || strings.Count(err.Error(), "runtime unavailable") != 1 {
		t.Fatalf("Start = %v, want ErrRuntimeUnavailable, said once", err)
	}
	if n, row := sp.CountCalls("Start", "s-mint"), rowMeta(t, store, b.ID); n != 0 || row["generation"] != "3" || row["instance_token"] != "tok-3" {
		t.Fatalf("starts %d, row %v; want nothing started or minted", n, row)
	}
}

// freshReadMisses reads the runtime not running though it is up: it came up
// past the read, or the provider's read lags.
type freshReadMisses struct{ *runtime.Fake }

func (p *freshReadMisses) ObserveLivenessSince(string, []string, time.Time) (runtime.Liveness, error) {
	return runtime.Liveness{}, nil
}

// failWriteBackStore fails the conditional write that would put the row
// back on tok-3: a converged start's withdrawal.
type failWriteBackStore struct {
	*rowWriteRecorder
	failed int
}

func (s *failWriteBackStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if opts.Metadata["instance_token"] == "tok-3" {
		s.failed++
		return errors.New("database is locked")
	}
	return s.rowWriteRecorder.UpdateIfMatch(id, rev, opts)
}

// TestConvergedStartWithdrawsItsMint: a start whose Start finds the name
// taken past its fresh read converges on that runtime: its mint is
// withdrawn and folded off the row it decides on, so the operator's hold is
// consumed against the row as it was. A withdrawal that does not land is
// ErrStateSync, never a success over a row that names no live process.
func TestConvergedStartWithdrawsItsMint(t *testing.T) {
	held := []string{
		"state", string(StateSuspended), "held_until", time.Now().Add(testIndefiniteHold).UTC().Format(time.RFC3339),
		"sleep_intent", string(SleepReasonUserHold), "suspended_at", "2026-10-09T11:00:00Z",
	}
	for _, failWithdraw := range []bool{false, true} {
		t.Run(fmt.Sprintf("withdrawal fails=%v", failWithdraw), func(t *testing.T) {
			backing := openLeaseStore(t, t.TempDir())
			b := mintRow(t, backing, held...)
			store := backing
			fail := &failWriteBackStore{rowWriteRecorder: &rowWriteRecorder{Store: backing}}
			if failWithdraw {
				store = fail
			}
			sp := &freshReadMisses{Fake: runtime.NewFake()}
			if err := sp.Start(context.Background(), "s-mint", runtime.Config{Env: map[string]string{runtime.RuntimeEpochEnv: "3", runtime.InstanceTokenEnv: "tok-3"}}); err != nil {
				t.Fatal(err)
			}
			mgr := newTestManager(t, store, sp)
			err := mgr.Attach(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude", runtime.Config{})
			row := rowMeta(t, backing, b.ID)
			if failWithdraw {
				if !errors.Is(err, ErrStateSync) || fail.failed == 0 || row["state"] == string(StateActive) {
					t.Fatalf("Attach = %v (withdrawal failures %d), row %v; want ErrStateSync and no confirm", err, fail.failed, row)
				}
				return
			}
			if err != nil {
				t.Fatalf("Attach: %v", err)
			}
			if row["generation"] != "3" || row["instance_token"] != "tok-3" || row["last_woke_at"] != "" || row["sleep_intent"] != "" || row["state"] != string(StateActive) {
				t.Fatalf("row %v, want the mint withdrawn and the hold consumed on generation 3", row)
			}
		})
	}
}

// TestStartFoundLiveUnderTheLeaseConvergesAsMainDid: a row not suspended
// whose runtime another start brought up between the operator's read and
// its lease converges as a Start that found the runtime up always did: the
// runtime's binding is recorded and the operator's hold consumed.
func TestStartFoundLiveUnderTheLeaseConvergesAsMainDid(t *testing.T) {
	store := openLeaseStore(t, t.TempDir())
	b := mintRow(t, store, "held_until", time.Now().Add(testIndefiniteHold).UTC().Format(time.RFC3339),
		"sleep_intent", string(SleepReasonUserHold), "suspended_at", "2026-10-09T11:00:00Z")
	fake := runtime.NewFake()
	gate := &isRunningHook{Provider: fake, at: 2}
	gate.once = func() {
		if err := fake.Start(context.Background(), "s-mint", runtime.Config{Env: map[string]string{runtime.RuntimeEpochEnv: "3", runtime.InstanceTokenEnv: "tok-3"}}); err != nil {
			t.Error(err)
		}
	}
	mgr := newTestManager(t, store, boundProvider{Provider: gate, binding: runtime.SessionBinding{ThreadID: "thread-9", ProjectID: "project-9"}})
	if err := mgr.Attach(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude", runtime.Config{}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	row := rowMeta(t, store, b.ID)
	if gate.calls < gate.at || fake.CountCalls("Start", "s-mint") != 1 || row["generation"] != "3" {
		t.Fatalf("IsRunning calls %d, starts %d, row %v; want the other start's runtime converged on, nothing minted", gate.calls, fake.CountCalls("Start", "s-mint"), row)
	}
	if row[MetadataRuntimeThreadID] != "thread-9" || row["sleep_intent"] != "" || row["state"] != string(StateActive) {
		t.Fatalf("row %v, want its binding recorded and the hold consumed", row)
	}
}

// TestStaleKeyWaitEndingAfterTheLaunchKeepsTheMint: a start whose context
// ends during the stale-key wait after its Start succeeded keeps its mint:
// the process it launched carries it.
func TestStaleKeyWaitEndingAfterTheLaunchKeepsTheMint(t *testing.T) {
	store := openLeaseStore(t, t.TempDir())
	b := mintRow(t, store, "session_key", "k-1", "resume_flag", "--resume")
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp, WithStaleKeyDetectionWaiter(func(context.Context, string) error { return context.Canceled }))
	if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude --resume k-1", runtime.Config{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v, want the wait's context.Canceled", err)
	}
	env, row := sp.LastStartConfig("s-mint").Env, rowMeta(t, store, b.ID)
	if row["generation"] != "4" || row["instance_token"] != env[runtime.InstanceTokenEnv] {
		t.Fatalf("row %v, launched env %v; want the launched process's mint kept", row, env)
	}
}

// retryOrphanProvider's first Start dies during startup with nothing
// launched; from its second scan on, the process table holds a root of the
// session it cannot terminate, so the stale-key relaunch refuses.
type retryOrphanProvider struct {
	*runtime.Fake
	city  string
	scans int
}

func (p *retryOrphanProvider) Start(_ context.Context, name string, cfg runtime.Config) error {
	p.Calls = append(p.Calls, runtime.Call{Method: "Start", Name: name, Config: cfg})
	return fmt.Errorf("%w: session %q", runtime.ErrSessionDiedDuringStartup, name)
}

func (p *retryOrphanProvider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	p.scans++
	if p.scans < 2 {
		return nil, nil
	}
	return []runtime.LiveRuntime{{PID: 2147480000, SessionID: id, City: p.city}}, nil
}

func (p *retryOrphanProvider) TerminateRuntime(runtime.LiveRuntime) error {
	return errors.New("operation not permitted")
}

// TestRelaunchRefusedByAnOrphanMintsNothing: the stale-key relaunch mints
// only after its orphan kill, so a relaunch the kill refuses mints nothing,
// and the failed start's own mint is withdrawn: the row is as it was.
func TestRelaunchRefusedByAnOrphanMintsNothing(t *testing.T) {
	store := openLeaseStore(t, t.TempDir())
	b := mintRow(t, store, "session_key", "k-stale", "resume_flag", "--resume")
	city := t.TempDir()
	sp := &retryOrphanProvider{Fake: runtime.NewFake(), city: city}
	mgr := newTestManager(t, store, sp, WithCityPath(city))
	err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude --resume k-stale", runtime.Config{})
	if err == nil || !strings.Contains(err.Error(), "orphan cleanup") || sp.scans < 2 {
		t.Fatalf("Start = %v (scans %d), want the relaunch's orphan refusal", err, sp.scans)
	}
	if row := rowMeta(t, store, b.ID); row["generation"] != "3" || row["instance_token"] != "tok-3" || row["last_woke_at"] != "" {
		t.Fatalf("row %v, want nothing minted left behind", row)
	}
}

// TestStartWithNothingToConfirmStillChecksItsMint: a start on a row whose
// confirm has nothing to write (already active, no claim, no stop request,
// no binding) still refuses when the row records a newer incarnation.
func TestStartWithNothingToConfirmStillChecksItsMint(t *testing.T) {
	store := openLeaseStore(t, t.TempDir())
	b := mintRow(t, store, "state", string(StateActive))
	sp := &mintSeeingProvider{Fake: runtime.NewFake(), store: store, id: b.ID}
	sp.hook = func() {
		if err := store.SetMetadataBatch(b.ID, map[string]string{"generation": "9", "instance_token": "tok-newer"}); err != nil {
			t.Error(err)
		}
	}
	mgr := newTestManager(t, store, sp)
	if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude", runtime.Config{}); !errors.Is(err, ErrMintSuperseded) {
		t.Fatalf("Start = %v, want ErrMintSuperseded", err)
	}
}

// TestIncarnationFencesReadTheWholeIdentity: an empty confirm on a closed
// row is refused, a withdrawal over a row whose token moved at the same
// generation writes nothing, and a mint's identity keys win over its
// request's own patch.
func TestIncarnationFencesReadTheWholeIdentity(t *testing.T) {
	backing := openLeaseStore(t, t.TempDir())
	b := mintRow(t, backing)
	front := NewStore(beads.SessionStore{Store: backing})
	req := mintReq(infoOf(t, backing, b.ID))
	req.Patch = func(Info, Incarnation) MetadataPatch {
		return MetadataPatch{"generation": "99", "instance_token": "tok-patch", "synced_at": "patched"}
	}
	inc, err := front.MintIncarnation(context.Background(), nil, req)
	if err != nil {
		t.Fatal(err)
	}
	if meta := rowMeta(t, backing, b.ID); meta["generation"] != "4" || !inc.recordedBy(meta) || meta["synced_at"] != "patched" {
		t.Fatalf("row %v, want the mint's identity over its patch's, the patch's other keys kept", meta)
	}
	if err := backing.SetMetadataBatch(b.ID, map[string]string{"instance_token": "tok-other"}); err != nil {
		t.Fatal(err)
	}
	if landed, err := front.unmintIncarnation(inc); err != nil || landed {
		t.Fatalf("withdrawal over a moved token = %v, %v; want refused", landed, err)
	}
	if meta := rowMeta(t, backing, b.ID); meta["instance_token"] != "tok-other" || meta["generation"] != "4" {
		t.Fatalf("row %v, want the moved token kept", meta)
	}
	closed, err := backing.Create(sessionBeadFixture("s-closed", "open", mintRowMeta("session_name", "s-closed")))
	if err != nil {
		t.Fatal(err)
	}
	cinc, err := front.MintIncarnation(context.Background(), nil, mintReq(infoOf(t, backing, closed.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.Close(closed.ID); err != nil {
		t.Fatal(err)
	}
	if landed, err := front.ConfirmIncarnation(cinc, nil); err != nil || landed {
		t.Fatalf("empty confirm on a closed row = %v, %v; want false", landed, err)
	}
}
