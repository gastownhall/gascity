package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/session"
)

// The transaction's runtime lease (B-a4; ARCH-RESTRUCTURE R2, L1a's
// guidance for PR B).

// leased is a spec that takes the row's lease record, with sections.
func leased(sections ...section) effectSpec {
	return effectSpec{needs: needs{Lease: true}, sections: sections}
}

// Kills the record taken by an effect that should hold the flock alone, or
// not taken by one that should: a needs.Lease effect's sections read the row
// carrying its record, with an epoch, and a name-lock-only effect's read
// none; the record is released at the end, and the flock freed.
func TestTxTakesTheRowLeaseRecordOnlyWhenItNeedsIt(t *testing.T) {
	for _, c := range []struct {
		name   string
		spec   func(section) effectSpec
		record bool
	}{
		{"needs.Lease", func(s section) effectSpec { return leased(s) }, true},
		{"the name lock alone", func(s section) effectSpec { return effectSpec{needs: needs{NameLock: true}, sections: []section{s}} }, false},
	} {
		k := newTxKit(t)
		var holder, epoch string
		s := k.run(context.Background(), c.spec(section{Decide: func(v txView) txStep {
			holder, epoch = v.Meta[session.RuntimeLeaseHolderKey], v.Meta[session.RuntimeLeaseEpochKey]
			return mark(txKeyA)(v)
		}}))
		if s.Outcome != settledLanded || (holder != "") != c.record || c.record && epoch != "1" {
			t.Errorf("%s: settlement %+v, the section read holder %q epoch %q; want a record %t", c.name, s, holder, epoch, c.record)
		}
		if got := k.meta(session.RuntimeLeaseHolderKey); got != "" {
			t.Errorf("%s: the record outlived the effect: holder %q", c.name, got)
		}
		if nameHeld(t, k.p.World.CityPath, "s-gc-1") {
			t.Errorf("%s: the name's flock outlived the effect", c.name)
		}
	}
}

// Kills a write premise without the lease (L1a: every section's premise
// adds lease.HoldsMeta): another holder taking the record between the
// sections refuses the next one on the premise, naming the lease.
func TestTxPremiseHoldsTheLease(t *testing.T) {
	k := newTxKit(t)
	k.on(seamAfterWrite, func() { k.outside(session.RuntimeLeaseHolderKey, "other-host", session.RuntimeLeaseEpochKey, "9") })
	s := k.run(context.Background(), leased(section{Decide: mark(txKeyA)}, section{Decide: mark(txKeyB)}))
	if s.Cause != causePremise || !strings.Contains(errString(s.Err), "runtime lease") || k.meta(txKeyB) != "" {
		t.Fatalf("settlement %+v, b=%q; want the second section refused on the lease", s, k.meta(txKeyB))
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Kills a Call off the lease's watch: a Call whose lease another holder
// takes over sees its context end with ErrRuntimeLeaseLost, and its context
// never outlives the lease's SafeUntil.
func TestTxCallRunsOnTheLeaseWatch(t *testing.T) {
	defer func(every time.Duration) { leaseWatchEvery = every }(leaseWatchEvery)
	leaseWatchEvery = 10 * time.Millisecond
	k := newTxKit(t)
	k.on(seamBeforeCall, func() { k.outside(session.RuntimeLeaseHolderKey, "other-host", session.RuntimeLeaseEpochKey, "9") })
	var cause error
	var deadline time.Time
	call := called(callStart, section{Decide: func(txView) txStep { return txStep{} }}, func(ctx context.Context, _ txCaps, _ any) (any, error) {
		deadline, _ = ctx.Deadline()
		select {
		case <-ctx.Done():
			cause = context.Cause(ctx)
		case <-time.After(5 * time.Second):
		}
		return nil, cause
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	k.run(ctx, effectSpec{needs: needs{Lease: true}, sections: []section{call}})
	if !errors.Is(cause, session.ErrRuntimeLeaseLost) {
		t.Fatalf("the Call's context ended with %v, want the lease lost", cause)
	}
	if deadline.IsZero() || time.Until(deadline) > time.Minute {
		t.Fatalf("the Call's deadline %v outlives the effect's", deadline)
	}
}

// Kills a TTL from anything but the effect's own deadline, or an effect
// that outlives its lease: the record's TTL is the effect's remaining
// budget plus the margin, and the effect's context ends by SafeUntil.
func TestTxLeaseTTLComesFromTheEffectsDeadline(t *testing.T) {
	k := newTxKit(t)
	var ttl, expires string
	var deadline time.Time
	call := called(callStart, section{Decide: func(v txView) txStep {
		ttl, expires = v.Meta[session.RuntimeLeaseTTLKey], v.Meta[session.RuntimeLeaseExpiresKey]
		return txStep{}
	}}, func(ctx context.Context, _ txCaps, _ any) (any, error) {
		deadline, _ = ctx.Deadline()
		return nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	k.run(ctx, leased(call))
	secs, _ := strconv.Atoi(ttl)
	want := session.RuntimeLeaseTTL(2 * time.Minute)
	if d := time.Duration(secs)*time.Second - want; d > time.Second || d < -2*time.Second {
		t.Fatalf("record TTL %ss, want the effect's budget plus the margin (%v)", ttl, want)
	}
	exp, err := time.Parse(time.RFC3339, expires)
	if err != nil || deadline.After(exp.Add(-session.RuntimeLeaseSkewAllowance)) {
		t.Fatalf("the effect's deadline %v outlives SafeUntil (expires %q)", deadline, expires)
	}
}

// Kills an effect that outlives its lease when it has no deadline of its
// own: its Calls and its sections' reads end by the lease's SafeUntil.
func TestTxEffectWithoutADeadlineEndsByItsLease(t *testing.T) {
	k := newTxKit(t)
	var deadline time.Time
	var ok bool
	var expires string
	call := called(callStart, section{Decide: func(v txView) txStep { expires = v.Meta[session.RuntimeLeaseExpiresKey]; return txStep{} }}, func(ctx context.Context, _ txCaps, _ any) (any, error) {
		deadline, ok = ctx.Deadline()
		return nil, nil
	})
	k.run(context.Background(), leased(call))
	exp, err := time.Parse(time.RFC3339, expires)
	safe := exp.Add(-session.RuntimeLeaseSkewAllowance)
	if err != nil || !ok || deadline.After(safe.Add(time.Second)) || deadline.Before(safe.Add(-2*time.Second)) {
		t.Fatalf("Call deadline %v (set %t), want the lease's SafeUntil %v (expires %q less the skew)", deadline, ok, safe, expires)
	}
	k = newTxKit(t)
	ok = false
	k.p.seam = func(ctx context.Context, at txSeam, _ intent, _, _ int) error {
		if at == seamAfterRowRead {
			deadline, ok = ctx.Deadline()
			expires = k.meta(session.RuntimeLeaseExpiresKey)
		}
		return nil
	}
	k.run(context.Background(), leased(section{Decide: mark(txKeyA)}))
	exp, err = time.Parse(time.RFC3339, expires)
	safe = exp.Add(-session.RuntimeLeaseSkewAllowance)
	if err != nil || !ok || deadline.After(safe.Add(time.Second)) || deadline.Before(safe.Add(-2*time.Second)) {
		t.Fatalf("section deadline %v (set %t), want the lease's SafeUntil %v", deadline, ok, safe)
	}
}

// Kills a lease read through the leg's cache (L1a: freshBead must read fresh
// through effectTx's wrapper stores): another host's record written behind
// the cache is seen on the lease's first read, so the effect refuses busy
// without ever writing a record at a stale revision; and a row closed behind
// the cache refuses as closed.
func TestTxLeaseReadsTheRowFresh(t *testing.T) {
	m := beads.NewMemStoreFrom(0, []beads.Bead{poolRow("gc-1", "worker", 1, "asleep")}, nil)
	writes := 0
	k := newTxKitOn(t, m, countingCAS{simBacking{m}, &writes})
	k.outside(session.RuntimeLeaseHolderKey, "other-host/1/n", session.RuntimeLeaseEpochKey, "4",
		session.RuntimeLeaseExpiresKey, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), session.RuntimeLeaseTTLKey, "3600",
		session.RuntimeLeaseFlockKey, "other-boot/1/2/token")
	if cached, _ := k.cache.Get("gc-1"); cached.Metadata[session.RuntimeLeaseHolderKey] != "" {
		t.Fatal("the cache saw the outside record: this test needs it stale")
	}
	writes = 0
	s := k.run(context.Background(), leased(section{Decide: func(txView) txStep { return txStep{} }}))
	if s.Cause != causeNameBusy || writes != 0 {
		t.Fatalf("settlement %+v after %d record writes, want busy on the first, fresh read", s, writes)
	}
	var busy *session.RuntimeLeaseBusyError
	if !errors.As(s.Err, &busy) || busy.Holder != "other-host/1/n" || busy.Expires.IsZero() {
		t.Fatalf("busy refusal error %v, want it to name the holder and its expiry", s.Err)
	}

	k = newTxKit(t)
	if err := k.backing.Close("gc-1"); err != nil {
		t.Fatal(err)
	}
	ran := false
	s = k.run(context.Background(), leased(section{Decide: func(txView) txStep { ran = true; return txStep{} }}))
	if s.Cause != causeLeaseRowClosed || ran {
		t.Fatalf("settlement %+v (decided %t), want refused %q", s, ran, causeLeaseRowClosed)
	}
}

// countingCAS is a backing that counts its conditional writes.
type countingCAS struct {
	simBacking
	n *int
}

func (c countingCAS) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	*c.n++
	return c.simBacking.UpdateIfMatch(id, rev, opts)
}

// Kills a lease refusal mapped to the wrong cause: busy, a closed row, a
// store that cannot fence, and a store that fails each refuse with their
// own.
func TestLockRuntimeNameRefusalCauses(t *testing.T) {
	session.ExpectNoCityRefusalsForTest(t) // the relative-city case refuses
	city := t.TempDir()
	row := func(store beads.Store) session.Info {
		b, err := store.Create(sessionRow("lease", "template", "worker", "session_name", "s-lease"))
		if err != nil {
			t.Fatal(err)
		}
		info, err := sessionFrontDoor(store).Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	w := &World{CityPath: city}
	ttl := session.RuntimeLeaseTTL(time.Minute)

	cas, _ := stampedMem(t, gate.Require)
	open := row(cas)
	holdName(t, city, "s-lease")
	var busy *session.RuntimeLeaseBusyError
	if _, _, cause, err := lockRuntimeName(w, open, sessionFrontDoor(cas), ttl); cause != causeNameBusy || !errors.As(err, &busy) || busy.Holder == "" {
		t.Errorf("busy: cause %q, err %v; want %q naming the holder", cause, err, causeNameBusy)
	}
	if _, _, cause, err := lockRuntimeName(&World{CityPath: "relative/city"}, open, nil, 0); cause != causeLeaseNoCity || !errors.Is(err, session.ErrRuntimeLeaseNoCity) {
		t.Errorf("relative city: cause %q, err %v; want %q", cause, err, causeLeaseNoCity)
	}
	renamed := open
	renamed.SessionName = "s-other"
	if _, _, cause, err := lockRuntimeName(&World{CityPath: t.TempDir()}, renamed, sessionFrontDoor(cas), ttl); cause != causeLeaseRenamed || err == nil {
		t.Errorf("renamed: cause %q, err %v; want %q", cause, err, causeLeaseRenamed)
	}
	contended, _ := stampedMem(t, gate.Require)
	if _, _, cause, err := lockRuntimeName(&World{CityPath: t.TempDir()}, row(contended), sessionFrontDoor(losingCAS{contended}), ttl); cause != causeLeaseContended || !errors.Is(err, session.ErrRuntimeLeaseContention) {
		t.Errorf("contended: cause %q, err %v; want %q", cause, err, causeLeaseContended)
	}
	cityFile := filepath.Join(t.TempDir(), "file") // a city path that is a file: no lock dir under it
	if err := os.WriteFile(cityFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, cause, err := lockRuntimeName(&World{CityPath: cityFile}, open, nil, 0); cause != causeLeaseLocalFS || !errors.Is(err, session.ErrRuntimeLeaseLocalFS) {
		t.Errorf("lock dir unusable: cause %q, err %v; want %q", cause, err, causeLeaseLocalFS)
	}

	city2 := t.TempDir()
	w2 := &World{CityPath: city2}
	closedStore, _ := stampedMem(t, gate.Require)
	closed := row(closedStore)
	if err := closedStore.Close(closed.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, cause, _ := lockRuntimeName(w2, closed, sessionFrontDoor(closedStore), ttl); cause != causeLeaseRowClosed {
		t.Errorf("closed row: cause %q, want %q", cause, causeLeaseRowClosed)
	}

	broken := leaseReadFailingStore{Store: closedStore}
	if _, _, cause, _ := lockRuntimeName(w2, closed, sessionFrontDoor(broken), ttl); cause != causeLeaseStore {
		t.Errorf("store down: cause %q, want %q", cause, causeLeaseStore)
	}

	unfenced, _ := stampedMem(t, gate.Require)
	if _, _, cause, _ := lockRuntimeName(w2, row(unfenced), sessionFrontDoor(noCASStore{unfenced}), ttl); cause != causeLeaseNoCAS {
		t.Errorf("no conditional writes: cause %q, want %q", cause, causeLeaseNoCAS)
	}
}

// leaseReadFailingStore is a store whose reads fail.
type leaseReadFailingStore struct{ beads.Store }

func (leaseReadFailingStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("store down")
}

// noCASStore is a store whose conditional write is unsupported.
type noCASStore struct{ *beads.MemStore }

func (noCASStore) UpdateIfMatch(string, int64, beads.UpdateOpts) error {
	return beads.ErrConditionalWriteUnsupported
}

// Kills a kind that creates, destroys or restarts a runtime, or writes the
// incarnation or the commit, without the row's lease record: every kind
// with a Call, a commit section (premiseOwnToken) or a close section takes
// it, and so do the kinds the replays build as such (the coordinator's S2
// table): start, adopt (its commit has the default premise), stop with its
// finalize, close, rollback, zombie and rekey.
func TestEffectSpecsThatChangeARuntimeTakeTheLease(t *testing.T) {
	for kind, spec := range effectSpecs {
		for _, sec := range spec.sections {
			if (sec.call != nil || sec.Premise == premiseOwnToken || sec.Premise == premiseClose) && !spec.needs.Lease {
				t.Errorf("%s: a section with a Call, the commit or a close, without needs.Lease", kind)
			}
		}
	}
	for _, kind := range []string{intentStart, intentAdopt, intentStop, intentClose, intentRollback, intentZombie, intentRekey} {
		if !effectSpecs[kind].needs.Lease {
			t.Errorf("%s: without the row's lease record", kind)
		}
	}
}

// Kills a per-intent needsFor dropping the record: a spec whose needs take
// the lease keeps it whatever needsFor returns.
func TestTxNeedsForCannotDropTheLease(t *testing.T) {
	k := newTxKit(t)
	var holder string
	spec := leased(section{Decide: func(v txView) txStep { holder = v.Meta[session.RuntimeLeaseHolderKey]; return txStep{} }})
	spec.needsFor = func(*World, intent) needs { return needs{NameLock: true} }
	k.run(context.Background(), spec)
	if holder == "" {
		t.Fatal("needsFor dropped the row's lease record")
	}
}

// Kills HoldsMeta on the default premise only (S3): another holder taking
// the record refuses a close section and a commit section too.
func TestTxCloseAndCommitHoldTheLease(t *testing.T) {
	k := newCloseTxKit(t)
	take := func(k *txKit) {
		k.outside(session.RuntimeLeaseHolderKey, "other-host", session.RuntimeLeaseEpochKey, "9")
	}
	k.on(seamAfterWrite, func() { take(k) })
	s := k.run(context.Background(), leased(section{Decide: mark(txKeyA)}, terminal))
	if s.Closed || s.Cause != causePremise || !strings.Contains(errString(s.Err), "runtime lease") {
		t.Fatalf("close after a takeover: settlement %+v, want refused on the lease", s)
	}
	k = newTxKit(t)
	k.on(seamAfterCall, func() { take(k) })
	preWake := called(callStart, section{Decide: func(txView) txStep { return txStep{Write: session.MetadataPatch{"state": "creating"}} }},
		func(context.Context, txCaps, any) (any, error) { return nil, nil })
	commit := section{Premise: premiseOwnToken, Decide: mark(txKeyA)}
	k.on(seamAfterWrite, func() {})
	s = k.run(context.Background(), leased(preWake, commit))
	if s.Cause != causePremise || k.meta(txKeyA) != "" || !strings.Contains(errString(s.Err), "runtime lease") {
		t.Fatalf("commit after a takeover: settlement %+v, a=%q; want refused on the lease", s, k.meta(txKeyA))
	}
}

// Kills a Call that does not carry the lease (S6): a leasing helper called
// from it runs under the effect's lease instead of finding its own flock
// busy.
func TestTxCallCarriesTheLease(t *testing.T) {
	k := newTxKit(t)
	var helperErr error
	call := called(callStart, section{Decide: func(txView) txStep { return txStep{} }}, func(ctx context.Context, c txCaps, _ any) (any, error) {
		if c.by.Kind != session.ActorController || c.by.Lease == nil {
			t.Errorf("the Call's actor = kind %d, lease %v; want the controller carrying the effect's lease", c.by.Kind, c.by.Lease)
		}
		release, err := session.LeaseRuntimeName(ctx, c.by, "s-gc-1")
		if err == nil {
			release()
		}
		helperErr = err
		return nil, nil
	})
	k.run(context.Background(), leased(call))
	if helperErr != nil {
		t.Fatalf("a leasing helper in the Call: %v, want it to borrow the effect's lease", helperErr)
	}
}

// Kills a lease's own end labeled as a shutdown (S7): an effect whose lease
// passed SafeUntil fails lease-expired, and one taken over lease-lost.
func TestTxLeaseEndsAreLabeled(t *testing.T) {
	expired, cancel := context.WithCancelCause(context.Background())
	cancel(session.ErrRuntimeLeaseExpired)
	if s := ended(expired); s.Cause != causeLeaseExpired {
		t.Errorf("expired: cause %q, want %q", s.Cause, causeLeaseExpired)
	}
	lost, cancel2 := context.WithCancelCause(context.Background())
	cancel2(session.ErrRuntimeLeaseLost)
	if s := ended(lost); s.Cause != causeLeaseLost {
		t.Errorf("lost: cause %q, want %q", s.Cause, causeLeaseLost)
	}
}

// Kills a lease record written for an effect already past its deadline: it
// ends before the lease, writing nothing.
func TestTxPastItsDeadlineWritesNoRecord(t *testing.T) {
	k := newTxKit(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	ran := false
	s := k.run(ctx, leased(section{Decide: func(txView) txStep { ran = true; return txStep{} }}))
	if s.Cause != causeDeadline || ran || k.meta(session.RuntimeLeaseEpochKey) != "" {
		t.Fatalf("settlement %+v (decided %t), epoch %q; want a deadline end and no record", s, ran, k.meta(session.RuntimeLeaseEpochKey))
	}
}

// Kills the lease taken after the effect's runtime read (an effect deciding
// a kill on a read the lease does not cover): at the runtime read the row
// already carries the record.
func TestTxLeaseComesBeforeTheRuntimeRead(t *testing.T) {
	k := newTxKit(t)
	sawRecord := false
	k.leaf.during = func() { sawRecord = sawRecord || k.meta(session.RuntimeLeaseHolderKey) != "" }
	spec := effectSpec{needs: needs{Lease: true, Runtime: true}, sections: []section{{Decide: func(txView) txStep { return txStep{} }}}}
	k.run(context.Background(), spec)
	if !sawRecord {
		t.Fatal("the runtime was read before the row's lease record was taken")
	}
}

// Kills a lease read that fails when a newer write races its refresh (S5):
// the lease's row store answers from the backing then, instead of
// ErrRowRefreshFenced.
func TestLeaseRowStoreReadsTheBackingWhenARefreshIsFenced(t *testing.T) {
	m := beads.NewMemStoreFrom(0, []beads.Bead{poolRow("gc-1", "worker", 1, "asleep")}, nil)
	stampedMemStore(t, m)
	var cache *beads.CachingStore
	raced, writes := false, 0
	cache = beads.NewCachingStoreForTest(leaseRacingBacking{simBacking{m}, func() {
		if !raced {
			raced, writes = true, writes+1
			if err := cache.SetMetadataBatch("gc-1", map[string]string{txKeyA: strconv.Itoa(writes)}); err != nil {
				t.Error(err)
			}
		}
	}}, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.RefreshRow("gc-1"); !errors.Is(err, beads.ErrRowRefreshFenced) {
		t.Fatalf("the race did not fence the refresh: %v", err)
	}
	raced = false
	store := leaseRowStore{freshRowStore{blindWriteRefusingStore: blindWriteRefusingStore{inner: cache}, cache: cache}}
	b, err := store.Get("gc-1")
	if err != nil || b.Metadata[txKeyA] != "2" {
		t.Fatalf("Get = %v, %v; want the backing's row, past the racing write", b.Metadata, err)
	}
}

// leaseRacingBacking runs onGet before each Get.
type leaseRacingBacking struct {
	simBacking
	onGet func()
}

func (r leaseRacingBacking) Get(id string) (beads.Bead, error) {
	r.onGet()
	return r.simBacking.Get(id)
}

// losingCAS is a store whose every conditional write loses its fence.
type losingCAS struct{ *beads.MemStore }

func (losingCAS) UpdateIfMatch(id string, rev int64, _ beads.UpdateOpts) error {
	return &beads.PreconditionFailedError{ID: id, Expected: rev}
}
