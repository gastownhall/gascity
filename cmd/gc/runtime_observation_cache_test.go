package main

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
)

// obsTestMaxAge is the staleness bound the cache tests run at: twice a 15s
// patrol interval, as the lane configures it.
const obsTestMaxAge = 30 * time.Second

var obsTestEpoch = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func newTestObservationCache() (*ObservationCache, *clock.Fake) {
	clk := &clock.Fake{Time: obsTestEpoch}
	return NewObservationCache(clk, obsTestMaxAge, "epoch-test"), clk
}

// obsPass builds a pass finished at the fake clock's current time.
func obsPass(clk *clock.Fake, seq, providerGen uint64, backends ...BackendPass) InventoryPass {
	return InventoryPass{
		Epoch:       "epoch-test",
		Seq:         seq,
		ProviderGen: providerGen,
		StartedAt:   clk.Now(),
		FinishedAt:  clk.Now(),
		Backends:    backends,
	}
}

func completeBackend(label string, names ...string) BackendPass {
	return BackendPass{Label: label, Outcome: OutcomeComplete, Attested: true, Names: names}
}

func unattestedBackend(label string, names ...string) BackendPass {
	return BackendPass{Label: label, Outcome: OutcomeUnattested, Names: names}
}

// partialSingle is a single provider's partial listing.
func partialSingle(names ...string) BackendPass {
	return BackendPass{
		Outcome: OutcomePartial, Attested: true, Names: names,
		Err: &runtime.PartialListError{Err: errors.New("one backend unanswered")},
	}
}

// failedSingle is a single provider's failed listing.
func failedSingle() BackendPass {
	return BackendPass{Outcome: OutcomeFailed, Attested: true, Err: errors.New("list-sessions timed out")}
}

func liveAttrs(incarnation string) InventoryAttrs {
	return InventoryAttrs{Incarnation: incarnation, DeadKnown: true, AttachedKnown: true}
}

func wantFact(t *testing.T, what string, got RuntimeFact, value ObsFact, reason string) {
	t.Helper()
	if got.Value != value || got.Reason != reason {
		t.Fatalf("%s = %v (reason %q), want %v (reason %q)", what, got.Value, got.Reason, value, reason)
	}
}

func getObs(t *testing.T, c *ObservationCache, name string) RuntimeObservation {
	t.Helper()
	obs, ok := c.Get(name)
	if !ok {
		t.Fatalf("Get(%q) found nothing", name)
	}
	return obs
}

// Kills: stale facts treated as current (C2.9). A fact older than maxAge must
// read Unknown through both Get and Snapshot.Fact.
func TestObservationCache_StaleFactReadsUnknown(t *testing.T) {
	c, clk := newTestObservationCache()
	c.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "gc-a")), nil)

	wantFact(t, "fresh Listed", getObs(t, c, "gc-a").Listed, ObsYes, "")
	clk.Advance(obsTestMaxAge)
	wantFact(t, "Listed at exactly maxAge", getObs(t, c, "gc-a").Listed, ObsYes, "")

	clk.Advance(time.Second)
	wantFact(t, "stale Listed via Get", getObs(t, c, "gc-a").Listed, ObsUnknown, obsReasonStale)
	wantFact(t, "stale Listed via Snapshot.Fact", c.Snapshot().Fact("gc-a", FactListed, clk.Now(), obsTestMaxAge), ObsUnknown, obsReasonStale)
}

// Kills: a death concluded from a partial or failed list (R14). A name the
// partial pass did not return keeps its value and its ObservedAt.
func TestObservationCache_PartialInventoryDrawsNoAbsence(t *testing.T) {
	c, clk := newTestObservationCache()
	c.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "gc-a", "gc-b")), nil)
	firstSeen := clk.Now()

	clk.Advance(5 * time.Second)
	c.PublishInventory(obsPass(clk, 2, 1, partialSingle("gc-a")), nil)
	clk.Advance(5 * time.Second)
	c.PublishInventory(obsPass(clk, 3, 1, failedSingle()), nil)

	b := getObs(t, c, "gc-b")
	wantFact(t, "gc-b Listed after partial and failed passes", b.Listed, ObsYes, obsReasonPartialList)
	if !b.Listed.ObservedAt.Equal(firstSeen) {
		t.Fatalf("gc-b Listed.ObservedAt = %v, want the last sighting %v (it must age out, not refresh)", b.Listed.ObservedAt, firstSeen)
	}
}

// Kills: last-written-wins. A probe write older than the stored fact loses.
func TestObservationCache_OlderObservationNeverOverwrites(t *testing.T) {
	c, clk := newTestObservationCache()
	c.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "gc-a")), nil)
	newer := clk.Now().Add(2 * time.Second)
	older := clk.Now().Add(time.Second)

	c.Note("gc-a", FactAttached, ObsYes, newer, SourceProbe, "")
	c.Note("gc-a", FactAttached, ObsNo, older, SourceProbe, "")

	clk.Advance(3 * time.Second)
	got := getObs(t, c, "gc-a").Attached
	wantFact(t, "Attached after an older write", got, ObsYes, "")
	if !got.ObservedAt.Equal(newer) || got.Source != SourceProbe {
		t.Fatalf("Attached = %+v, want the newer probe observation at %v", got, newer)
	}
}

// Kills: spurious enqueues on refresh. Gen moves, and Changed fires, only when
// some fact value flips.
func TestObservationCache_GenAdvancesOnlyOnFlip(t *testing.T) {
	c, clk := newTestObservationCache()
	if flips := c.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "gc-a")), map[string]InventoryAttrs{"gc-a": liveAttrs("i1")}); flips == 0 {
		t.Fatal("first pass reported no flips, want the new name's facts")
	}
	gen := c.Snapshot().Gen
	<-c.Changed()

	clk.Advance(15 * time.Second)
	if flips := c.PublishInventory(obsPass(clk, 2, 1, completeBackend("", "gc-a")), map[string]InventoryAttrs{"gc-a": liveAttrs("i1")}); flips != 0 {
		t.Fatalf("an identical refresh reported %d flips, want 0", flips)
	}
	if got := c.Snapshot().Gen; got != gen {
		t.Fatalf("Gen after a refresh = %d, want %d", got, gen)
	}
	select {
	case <-c.Changed():
		t.Fatal("Changed fired on a refresh")
	default:
	}

	clk.Advance(15 * time.Second)
	dead := liveAttrs("i1")
	dead.AllPanesDead = true
	if flips := c.PublishInventory(obsPass(clk, 3, 1, completeBackend("", "gc-a")), map[string]InventoryAttrs{"gc-a": dead}); flips == 0 {
		t.Fatal("a live-to-corpse pass reported no flips")
	}
	if got := c.Snapshot().Gen; got != gen+1 {
		t.Fatalf("Gen after a flip = %d, want %d", got, gen+1)
	}
	select {
	case <-c.Changed():
	default:
		t.Fatal("Changed did not fire on a flip")
	}
}

// Kills: No for an unobservable fact (k8s attach). A backend that cannot
// observe attach reports Unknown "unsupported", and absence never turns it No.
func TestObservationCache_UnsupportedFactStaysUnknown(t *testing.T) {
	c, clk := newTestObservationCache()
	unsupported := map[string]InventoryAttrs{"gc-pod": {}}
	c.PublishInventory(obsPass(clk, 1, 1, completeBackend("remote", "gc-pod")), unsupported)
	wantFact(t, "k8s Attached", getObs(t, c, "gc-pod").Attached, ObsUnknown, obsReasonUnsupported)
	wantFact(t, "k8s Running", getObs(t, c, "gc-pod").Running, ObsUnknown, obsReasonUnsupported)

	clk.Advance(15 * time.Second)
	c.PublishInventory(obsPass(clk, 2, 1, completeBackend("remote")), nil)
	obs := getObs(t, c, "gc-pod")
	wantFact(t, "absent pod Listed", obs.Listed, ObsNo, "")
	if obs.Attached.Value == ObsNo {
		t.Fatal("an absence turned an unobservable Attached into No")
	}
}

// Kills: an ownerless runtime hidden (POOL-052). A runtime whose clean
// attribution read has no GC_SESSION_ID is still visible by name.
func TestObservationCache_OwnerlessRuntimeVisibleByName(t *testing.T) {
	c, clk := newTestObservationCache()
	attrs := liveAttrs("i1")
	attrs.OwnerState = OwnerNone
	owned := liveAttrs("i2")
	owned.OwnerState, owned.OwnerID, owned.InstanceToken = OwnerSession, "gc-42", "tok"
	c.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "gc-orphan", "gc-owned")),
		map[string]InventoryAttrs{"gc-orphan": attrs, "gc-owned": owned})

	orphan := getObs(t, c, "gc-orphan")
	if orphan.OwnerState != OwnerNone || orphan.Owner.SessionID != "" {
		t.Fatalf("orphan owner = (%v, %+v), want OwnerNone with no key", orphan.OwnerState, orphan.Owner)
	}
	wantFact(t, "orphan Listed", orphan.Listed, ObsYes, "")
	if _, ok := c.Snapshot().ByName["gc-orphan"]; !ok {
		t.Fatal("the snapshot hides the ownerless runtime")
	}
	got := getObs(t, c, "gc-owned")
	if got.OwnerState != OwnerSession || got.Owner.SessionID != "gc-42" || got.Owner.SessionName != "gc-owned" || got.InstanceToken != "tok" {
		t.Fatalf("owned observation = %+v, want session gc-42 with its token", got)
	}
}

// Kills: owner fields read as last written (Appendix C gap 1). Get serves
// Incarnation, Owner and InstanceToken only while Listed reads Yes and the
// pass that listed the name also enriched it: not after a pass that listed
// it without enrichment, not once stale, and not after a provider swap.
func TestObservationCache_OwnerGatedOnFreshPrimedListedIncarnation(t *testing.T) {
	owned := func(incarnation string) map[string]InventoryAttrs {
		a := liveAttrs(incarnation)
		a.OwnerState, a.OwnerID, a.InstanceToken = OwnerSession, "gc-42", "tok"
		return map[string]InventoryAttrs{"gc-a": a}
	}
	wantOwner := func(t *testing.T, c *ObservationCache, what string, trusted bool) {
		t.Helper()
		got := getObs(t, c, "gc-a")
		has := got.OwnerState == OwnerSession && got.Owner.SessionID == "gc-42" && got.InstanceToken == "tok" && got.Incarnation == "i1"
		cleared := got.OwnerState == OwnerUnknown && got.Owner.SessionID == "" && got.InstanceToken == "" && got.Incarnation == ""
		if (trusted && !has) || (!trusted && !cleared) {
			t.Fatalf("%s: owner = (%v, %q, token %q, incarnation %q), trusted=%v", what, got.OwnerState, got.Owner.SessionID, got.InstanceToken, got.Incarnation, trusted)
		}
	}

	t.Run("listed without enrichment", func(t *testing.T) {
		c, clk := newTestObservationCache()
		c.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "gc-a")), owned("i1"))
		wantOwner(t, c, "enriched pass", true)
		clk.Advance(15 * time.Second)
		c.PublishInventory(obsPass(clk, 2, 1, completeBackend("", "gc-a")), nil)
		wantOwner(t, c, "unenriched pass", false)
		if c.Snapshot().ByName["gc-a"].Owner.SessionID != "gc-42" {
			t.Fatal("the stored owner should stay underneath; only reads are gated")
		}
	})
	t.Run("stale", func(t *testing.T) {
		c, clk := newTestObservationCache()
		c.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "gc-a")), owned("i1"))
		clk.Advance(obsTestMaxAge + time.Second)
		wantOwner(t, c, "stale", false)
	})
	t.Run("provider swapped", func(t *testing.T) {
		c, clk := newTestObservationCache()
		c.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "gc-a")), owned("i1"))
		clk.Advance(15 * time.Second)
		c.PublishInventory(obsPass(clk, 2, 2, partialSingle("gc-a")), owned("i1"))
		wantOwner(t, c, "unprimed after the swap", false)
	})
}

// Kills: facts served before a backend's first complete pass (C4.5 item 4).
func TestObservationCache_UnprimedIsUnknown(t *testing.T) {
	c, clk := newTestObservationCache()
	c.PublishInventory(obsPass(clk, 1, 1, partialSingle("gc-a")), nil)
	snap := c.Snapshot()
	if snap.AllPrimed() {
		t.Fatal("AllPrimed after only a partial pass")
	}
	if snap.ByName["gc-a"].Listed.Value != ObsYes {
		t.Fatalf("stored Listed = %v, want the partial pass's Yes kept underneath", snap.ByName["gc-a"].Listed.Value)
	}
	wantFact(t, "unprimed Listed", getObs(t, c, "gc-a").Listed, ObsUnknown, obsReasonUnprimed)
	wantFact(t, "unprimed Listed via Fact", snap.Fact("gc-a", FactListed, clk.Now(), obsTestMaxAge), ObsUnknown, obsReasonUnprimed)

	clk.Advance(15 * time.Second)
	c.PublishInventory(obsPass(clk, 2, 1, completeBackend("", "gc-a")), nil)
	if !c.Snapshot().AllPrimed() {
		t.Fatal("not AllPrimed after a complete pass")
	}
	wantFact(t, "primed Listed", getObs(t, c, "gc-a").Listed, ObsYes, "")
}

// Kills: absence from an ssh/exec/acp complete-empty. An unattested backend's
// error-free empty listing proves nothing.
func TestObservationCache_UnattestedBackendDrawsNoAbsence(t *testing.T) {
	c, clk := newTestObservationCache()
	c.PublishInventory(obsPass(clk, 1, 1, unattestedBackend("", "gc-ssh")), nil)
	clk.Advance(15 * time.Second)
	c.PublishInventory(obsPass(clk, 2, 1, unattestedBackend("")), nil)

	obs := c.Snapshot().ByName["gc-ssh"]
	if obs.Listed.Value != ObsYes || obs.Listed.Reason != obsReasonUnattested {
		t.Fatalf("gc-ssh Listed = %+v, want Yes kept (reason %q)", obs.Listed, obsReasonUnattested)
	}
	if c.Snapshot().Primed[""] {
		t.Fatal("an unattested backend became primed")
	}
}

// Kills: a complete tmux pass concluding absence for an ACP name. Absence is
// concluded only by a complete pass on the backend that last listed the name.
func TestObservationCache_AbsenceOnlyFromTheBackendThatListedIt(t *testing.T) {
	c, clk := newTestObservationCache()
	c.PublishInventory(obsPass(clk, 1, 1, completeBackend("default", "gc-tmux"), unattestedBackend("acp", "gc-acp")), nil)

	clk.Advance(15 * time.Second)
	c.PublishInventory(obsPass(clk, 2, 1, completeBackend("default"), unattestedBackend("acp")), nil)
	snap := c.Snapshot()
	if got := snap.ByName["gc-acp"].Listed; got.Value != ObsYes {
		t.Fatalf("gc-acp Listed = %+v after a complete tmux pass, want Yes kept", got)
	}
	tmux := snap.ByName["gc-tmux"]
	for what, f := range map[string]RuntimeFact{"Listed": tmux.Listed, "Running": tmux.Running, "ProcessAlive": tmux.ProcessAlive} {
		if f.Value != ObsNo || !f.ObservedAt.Equal(clk.Now()) {
			t.Fatalf("gc-tmux %s = %+v, want No observed at the complete pass", what, f)
		}
	}
	if tmux.Backend != "default" || snap.ByName["gc-acp"].Backend != "acp" {
		t.Fatalf("backends = (%q, %q), want (default, acp)", tmux.Backend, snap.ByName["gc-acp"].Backend)
	}
}

// Kills: absence concluded by a backend that no longer holds the name. A
// name that moved from acp to tmux belongs to tmux: a complete acp pass does
// not conclude it absent while tmux is ServerAbsent.
func TestObservationCache_NameMovedBetweenBackendsConcludedOnlyByItsNewBackend(t *testing.T) {
	c, clk := newTestObservationCache()
	c.PublishInventory(obsPass(clk, 1, 1, completeBackend("default"), completeBackend("acp", "gc-moved")), nil)
	clk.Advance(15 * time.Second)
	c.PublishInventory(obsPass(clk, 2, 1, completeBackend("default", "gc-moved"), completeBackend("acp")), nil)
	if got := c.Snapshot().ByName["gc-moved"].Backend; got != "default" {
		t.Fatalf("gc-moved backend = %q after tmux listed it, want default", got)
	}

	clk.Advance(15 * time.Second)
	absent := BackendPass{
		Label: "default", Outcome: OutcomePartial, Attested: true, ServerAbsent: true,
		Err: &runtime.PartialListError{Err: errors.New("no tmux server"), ServerAbsent: true},
	}
	c.PublishInventory(obsPass(clk, 3, 1, absent, completeBackend("acp")), nil)
	if got := c.Snapshot().ByName["gc-moved"].Listed; got.Value != ObsYes {
		t.Fatalf("gc-moved Listed = %+v, want Yes kept: only a complete tmux pass may conclude it absent", got)
	}
}

// Kills: a swapped-out backend's names kept Listed forever, and concluded
// absent while a new backend lists partially. After a swap removes the
// backend that listed a name, only a pass on which every backend is complete
// concludes it absent.
func TestObservationCache_SwappedOutBackendConcludedOnlyByAllCompletePass(t *testing.T) {
	c, clk := newTestObservationCache()
	c.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "gc-old")), nil)

	clk.Advance(15 * time.Second)
	acpPartial := BackendPass{Label: "acp", Outcome: OutcomePartial, Attested: true, Err: &runtime.PartialListError{Err: errors.New("one socket")}}
	c.PublishInventory(obsPass(clk, 2, 2, completeBackend("default"), acpPartial), nil)
	if got := c.Snapshot().ByName["gc-old"].Listed; got.Value != ObsYes {
		t.Fatalf("gc-old Listed = %+v while acp lists partially, want Yes kept", got)
	}

	clk.Advance(15 * time.Second)
	c.PublishInventory(obsPass(clk, 3, 2, completeBackend("default"), completeBackend("acp")), nil)
	if got := c.Snapshot().ByName["gc-old"].Listed; got.Value != ObsNo || !got.ObservedAt.Equal(clk.Now()) {
		t.Fatalf("gc-old Listed = %+v after an all-complete pass, want No", got)
	}
}

// Kills: pruning names whose backend failed. A failed listing observes
// nothing, so its names are kept (and age out) however long it fails.
func TestObservationCache_FailedBackendNamesAreNotPruned(t *testing.T) {
	c, clk := newTestObservationCache()
	c.PublishInventory(obsPass(clk, 1, 1, completeBackend("default"), completeBackend("acp", "gc-acp")), nil)
	failedACP := BackendPass{Label: "acp", Outcome: OutcomeFailed, Attested: true, Err: errors.New("acp down")}
	for seq := uint64(2); seq <= 2+observationRetentionPasses; seq++ {
		clk.Advance(15 * time.Second)
		c.PublishInventory(obsPass(clk, seq, 1, completeBackend("default"), failedACP), nil)
	}
	if _, ok := c.Snapshot().ByName["gc-acp"]; !ok {
		t.Fatalf("gc-acp pruned after %d failed acp passes", observationRetentionPasses+1)
	}
}

// Kills: a tmux corpse reported Running=Yes. Listed and Running are separate
// facts, and a listed name with no enrichment has Running Unknown.
func TestObservationCache_CorpseIsListedNotRunning(t *testing.T) {
	c, clk := newTestObservationCache()
	corpse := liveAttrs("i1")
	corpse.AllPanesDead = true
	c.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "gc-corpse", "gc-live", "gc-bare")),
		map[string]InventoryAttrs{"gc-corpse": corpse, "gc-live": liveAttrs("i2")})

	dead := getObs(t, c, "gc-corpse")
	wantFact(t, "corpse Listed", dead.Listed, ObsYes, "")
	wantFact(t, "corpse Running", dead.Running, ObsNo, "")
	wantFact(t, "corpse ProcessAlive", dead.ProcessAlive, ObsNo, "")
	live := getObs(t, c, "gc-live")
	wantFact(t, "live Running", live.Running, ObsYes, "")
	wantFact(t, "live Attached", live.Attached, ObsNo, "")
	if live.Incarnation != "i2" {
		t.Fatalf("live Incarnation = %q, want i2", live.Incarnation)
	}
	wantFact(t, "unenriched Running", getObs(t, c, "gc-bare").Running, ObsUnknown, "")
}

// Kills: old-provider facts served as primed. A new ProviderGen unprimes every
// backend until its next complete pass.
func TestObservationCache_ProviderSwapUnprimes(t *testing.T) {
	c, clk := newTestObservationCache()
	c.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "gc-a")), nil)
	if !c.Snapshot().AllPrimed() {
		t.Fatal("not primed after a complete pass")
	}

	clk.Advance(15 * time.Second)
	c.PublishInventory(obsPass(clk, 2, 2, partialSingle("gc-a")), nil)
	if c.Snapshot().AllPrimed() {
		t.Fatal("still primed after a provider swap with no complete pass")
	}
	wantFact(t, "Listed after the swap", getObs(t, c, "gc-a").Listed, ObsUnknown, obsReasonProviderSwapped)

	clk.Advance(15 * time.Second)
	c.PublishInventory(obsPass(clk, 3, 2, completeBackend("", "gc-a")), nil)
	if !c.Snapshot().AllPrimed() {
		t.Fatal("not primed after the new provider's complete pass")
	}
}

// Kills: unbounded growth. A name absent for 10 consecutive passes is dropped;
// a listed name never is.
func TestObservationCache_PrunesAbsentNamesAfterRetention(t *testing.T) {
	c, clk := newTestObservationCache()
	c.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "gc-gone", "gc-stays")), nil)
	for seq := uint64(2); seq <= 1+observationRetentionPasses; seq++ {
		clk.Advance(15 * time.Second)
		c.PublishInventory(obsPass(clk, seq, 1, completeBackend("", "gc-stays")), nil)
		_, kept := c.Snapshot().ByName["gc-gone"]
		if absent := seq - 1; absent < observationRetentionPasses && !kept {
			t.Fatalf("gc-gone dropped after %d absent passes, want it kept for %d", absent, observationRetentionPasses)
		} else if absent == observationRetentionPasses && kept {
			t.Fatalf("gc-gone kept after %d absent passes", absent)
		}
	}
	if _, ok := c.Snapshot().ByName["gc-stays"]; !ok {
		t.Fatal("a listed name was pruned")
	}
}

// Kills: a map mutated after publish. A snapshot taken before later publishes
// and notes stays exactly as it was, and concurrent readers never race the
// writer (run under -race).
func TestObservationCache_SnapshotImmutableUnderConcurrentPublish(t *testing.T) {
	c := NewObservationCache(clock.Real{}, obsTestMaxAge, "epoch-race")
	publish := func(seq uint64, names ...string) {
		attrs := make(map[string]InventoryAttrs, len(names))
		for _, n := range names {
			a := liveAttrs(fmt.Sprintf("%s-%d", n, seq%3))
			a.AllPanesDead = seq%2 == 0
			attrs[n] = a
		}
		now := time.Now()
		c.PublishInventory(InventoryPass{
			Seq: seq, ProviderGen: 1, StartedAt: now, FinishedAt: now,
			Backends: []BackendPass{completeBackend("", names...)},
		}, attrs)
	}
	publish(1, "gc-a", "gc-b")
	before := c.Snapshot()
	frozen := make(map[string]RuntimeObservation, len(before.ByName))
	for k, v := range before.ByName {
		frozen[k] = v
	}
	frozenPrimed := map[string]bool{}
	for k, v := range before.Primed {
		frozenPrimed[k] = v
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := c.Snapshot()
				for name := range snap.ByName {
					_ = snap.Fact(name, FactRunning, time.Now(), obsTestMaxAge)
					_, _ = c.Get(name)
				}
				_ = snap.AllPrimed()
			}
		}()
	}
	for seq := uint64(2); seq < 200; seq++ {
		names := []string{"gc-a"}
		if seq%3 != 0 {
			names = append(names, "gc-b", fmt.Sprintf("gc-%d", seq%7))
		}
		publish(seq, names...)
		c.Note("gc-a", FactPending, ObsFact(seq%3), time.Now(), SourceProbe, "")
	}
	close(stop)
	wg.Wait()

	if !reflect.DeepEqual(before.ByName, frozen) || !reflect.DeepEqual(before.Primed, frozenPrimed) {
		t.Fatal("a published snapshot changed after later publishes")
	}
}

// Kills: serving a pass older than the caller's bound, or one whose merged
// listing failed, to a legacy consumer instead of its live fallback.
func TestObservationCache_FreshSnapshotRejectsStaleAndFailed(t *testing.T) {
	c, clk := newTestObservationCache()
	if _, ok := c.FreshSnapshot(obsTestMaxAge); ok {
		t.Fatal("FreshSnapshot before any pass")
	}
	pass := obsPass(clk, 1, 1, completeBackend("", "gc-a"))
	pass.MergedNames = []string{"gc-a"}
	c.PublishInventory(pass, nil)
	if got, ok := c.FreshSnapshot(obsTestMaxAge); !ok || got.Inventory.Seq != 1 || !reflect.DeepEqual(got.Inventory.MergedNames, []string{"gc-a"}) {
		t.Fatalf("FreshSnapshot = (%+v, %v), want pass 1", got, ok)
	}
	clk.Advance(obsTestMaxAge + time.Second)
	if _, ok := c.FreshSnapshot(obsTestMaxAge); ok {
		t.Fatal("FreshSnapshot served a pass older than maxAge")
	}

	partial := obsPass(clk, 2, 1, partialSingle("gc-a"))
	partial.MergedNames, partial.MergedErr = []string{"gc-a"}, &runtime.PartialListError{Err: errors.New("acp down")}
	c.PublishInventory(partial, nil)
	if _, ok := c.FreshSnapshot(obsTestMaxAge); !ok {
		t.Fatal("FreshSnapshot rejected a partial pass; legacy consumers use its visible names")
	}
	failed := obsPass(clk, 3, 1, failedSingle())
	failed.MergedErr = errors.New("list-sessions timed out")
	c.PublishInventory(failed, nil)
	if _, ok := c.FreshSnapshot(obsTestMaxAge); ok {
		t.Fatal("FreshSnapshot served a failed pass")
	}
}
