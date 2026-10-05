package main

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/worktree"
)

// createRefusal is a Backoff view holding one create record for identity
// (createIdentity.key or allocPlan.identity), live until until.
func createRefusal(identity, cause string, until time.Time) map[string]backoffRecord {
	return map[string]backoffRecord{createBackoffKey(identity): {Consecutive: 1, Until: until, Cause: cause}}
}

// workRefusal is a Backoff view refusing spec's evidence for a minute past
// allocNow.
func workRefusal(spec worktree.Spec) map[string]backoffRecord {
	return map[string]backoffRecord{workBackoffKey(spec.BeadID): {
		Consecutive: 1, Until: allocNow.Add(time.Minute), Cause: createStageWorktree, Fingerprint: specFingerprint(spec),
	}}
}

var backoffSpec = worktree.Spec{BeadID: "w-1", StoreRef: "city", Path: "/wt/w-1", Branch: "b"}

// backoffKinds is one key of each kind with the fingerprint its writer uses.
var backoffKinds = []struct {
	name, key, fingerprint string
}{
	{"row", rowBackoffKey(rowKey{Leg: "sessions", ID: "gc-a"}), ""},
	{"create", createBackoffKey(createIdentity{Template: "worker", QualifiedInstance: "worker-1", Slot: 1}.key()), "rev-1"},
	{"named", createBackoffKey(createIdentity{Template: "chat", QualifiedInstance: "chat", Named: true}.key()), "rev-1"},
	{"work", workBackoffKey("w-1"), specFingerprint(backoffSpec)},
}

// Kills: the four refusal kinds drifting apart (C3): linear or uncapped
// backoff for any kind; a count that never resets on success or on a new
// fingerprint (a config change for create and named keys, new evidence for
// work keys); a requested Until shortened by the backoff.
func TestBackoffTableIdenticalScheduleForAllKinds(t *testing.T) {
	for _, k := range backoffKinds {
		t.Run(k.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := newBackoffTable()
				refuse := func(fingerprint string, until time.Time) backoffRecord {
					t.Helper()
					b.Refuse(k.key, time.Now(), until, "cause", fingerprint)
					r, ok := b.Snapshot()[k.key]
					if !ok {
						t.Fatalf("no record for %s", k.key)
					}
					return r
				}
				// Each refusal comes after the last one expired: consecutive.
				for i, want := range []time.Duration{10, 20, 40, 80, 160, 300, 300, 300} {
					r := refuse(k.fingerprint, time.Time{})
					if got := time.Until(r.Until); got != want*time.Second || r.Consecutive != i+1 {
						t.Fatalf("refusal %d: backoff %v consecutive %d, want %v and %d", i+1, got, r.Consecutive, want*time.Second, i+1)
					}
					advance(time.Until(r.Until))
				}
				b.Succeed(k.key)
				if r := refuse(k.fingerprint, time.Time{}); time.Until(r.Until) != backoffBase || r.Consecutive != 1 {
					t.Fatalf("after success: %+v, want a fresh 10s", r)
				}
				advance(backoffBase)
				if r := refuse(k.fingerprint+"-changed", time.Time{}); time.Until(r.Until) != backoffBase || r.Consecutive != 1 {
					t.Fatalf("after a fingerprint change: %+v, want a fresh 10s", r)
				}
				advance(backoffBase)
				if r := refuse(k.fingerprint+"-changed", time.Now().Add(time.Minute)); time.Until(r.Until) != time.Minute || r.Consecutive != 2 {
					t.Fatalf("requested floor lost: %+v, want 1m and consecutive 2", r)
				}
			})
		})
	}
}

// Kills: a held grant's session key, or a create re-planned before its
// record shows, escalating its own backoff within one refusal; a later
// request shortening a live record; a cause not kept verbatim (F3 reads
// "fence" and "fence-read" apart).
func TestBackoffRefusalWhileLiveDoesNotEscalate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBackoffTable()
		k := backoffKinds[1].key
		for i := 0; i < 4; i++ {
			b.Refuse(k, time.Now(), time.Time{}, createStageFenceRead, "rev-1")
			if r := b.Snapshot()[k]; r.Consecutive != 1 || !r.Until.Equal(time.Now().Add(backoffBase)) || r.Cause != createStageFenceRead {
				t.Fatalf("refusal %d: %+v, want consecutive 1 until now+10s, cause fence-read", i+1, r)
			}
			advance(100 * time.Millisecond)
		}
		b.Refuse(k, time.Now(), time.Now().Add(time.Minute), createStageFence, "rev-1")
		b.Refuse(k, time.Now(), time.Time{}, createStageFence, "rev-1")
		if r := b.Snapshot()[k]; time.Until(r.Until) != time.Minute || r.Consecutive != 1 || r.Cause != createStageFence {
			t.Fatalf("re-refusal shortened or escalated: %+v, want until now+1m, consecutive 1, cause fence", r)
		}
	})
}

// Kills: a new fingerprint inheriting the old one's live Until, so new
// worktree evidence for a bead stays refused for the old evidence's backoff
// (up to 5m).
func TestBackoffNewFingerprintWhileLiveStartsAFreshBackoff(t *testing.T) {
	now := time.Unix(1_000, 0)
	b := newBackoffTable()
	k := workBackoffKey("w-1")
	old := worktree.Spec{BeadID: "w-1", Path: "/wt", Generation: "g1"}
	next := old
	next.Generation = "g2"
	for i := 0; i < 6; i++ { // escalate g1 to the 5m cap
		b.Refuse(k, now, time.Time{}, createStageWorktree, specFingerprint(old))
		now = b.Snapshot()[k].Until
	}
	now = now.Add(-time.Second) // g1's record still live
	b.Refuse(k, now, time.Time{}, createStageWorktree, specFingerprint(old))
	b.Refuse(k, now, time.Time{}, createStageWorktree, specFingerprint(next))
	if r := b.Snapshot()[k]; r.Consecutive != 1 || !r.Until.Equal(now.Add(backoffBase)) {
		t.Fatalf("new evidence while the old record is live = %+v, want consecutive 1 until now+10s", r)
	}
}

// Kills (N48): one key's refusal moving or sharing another key's record,
// including the same name under another kind.
func TestBackoffKeysAreIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBackoffTable()
		b.Refuse(backoffKinds[0].key, time.Now(), time.Time{}, "fresh-read", "")
		advance(time.Second)
		b.Refuse(workBackoffKey("gc-a"), time.Now(), time.Time{}, createStageWorktree, "spec")
		snap := b.Snapshot()
		if r := snap[backoffKinds[0].key]; time.Until(r.Until) != 9*time.Second || r.Cause != "fresh-read" || r.Consecutive != 1 {
			t.Fatalf("another key's refusal moved the row record: %+v", r)
		}
		if r := snap[workBackoffKey("gc-a")]; time.Until(r.Until) != 10*time.Second || r.Consecutive != 1 {
			t.Fatalf("work record shares the row's backoff: %+v", r)
		}
		snap[backoffKinds[0].key] = backoffRecord{}
		if b.Snapshot()[backoffKinds[0].key].Consecutive != 1 {
			t.Fatal("editing a Snapshot edited the table")
		}
	})
}

// Kills: memory growing with every key ever refused; or a prune that drops
// a record still in force: an open row's, a row's on a leg the census did
// not hold, an in-demand bead's, or a create record under the current
// ConfigRev. A leg name holding "/" still prunes by its own leg.
func TestBackoffPruneBoundsTheTable(t *testing.T) {
	now := time.Unix(1_000, 0)
	b := newBackoffTable()
	open := rowKey{Leg: "sessions", ID: "gc-open"}
	closed := rowKey{Leg: "sessions", ID: "gc-closed"}
	unread := rowKey{Leg: "rig", ID: "gc-r"}
	slashed := rowKey{Leg: "rigs/a", ID: "gc-s"}
	keep := []string{rowBackoffKey(open), rowBackoffKey(unread), workBackoffKey("w-demand"), createBackoffKey("worker/worker-1")}
	drop := []string{rowBackoffKey(closed), rowBackoffKey(slashed), workBackoffKey("w-gone"), createBackoffKey("worker/worker-2")}
	for _, k := range []string{rowBackoffKey(open), rowBackoffKey(closed), rowBackoffKey(unread), rowBackoffKey(slashed), workBackoffKey("w-demand"), workBackoffKey("w-gone")} {
		b.Refuse(k, now, time.Time{}, "c", "")
	}
	b.Refuse(createBackoffKey("worker/worker-1"), now, time.Time{}, createStageFence, "rev-2")
	b.Refuse(createBackoffKey("worker/worker-2"), now, time.Time{}, createStageFence, "rev-1")
	c := ledgerCensusOf(map[rowKey]ledgerRow{open: {}}, "rigs/a")
	b.Prune("rev-2", c, map[string]bool{"w-demand": true})
	snap := b.Snapshot()
	for _, k := range keep {
		if _, ok := snap[k]; !ok {
			t.Errorf("prune dropped %s", k)
		}
	}
	for _, k := range drop {
		if _, ok := snap[k]; ok {
			t.Errorf("prune kept %s", k)
		}
	}
}

// Kills: a named identity's create record outliving the identity in config.
// Config is fixed per revision, so a config without the identity is a new
// ConfigRev, and the prune at the next pass drops the record while a still
// configured identity refused under the new revision keeps its own.
func TestBackoffNamedIdentityPrunedWhenUnconfigured(t *testing.T) {
	now := time.Unix(1_000, 0)
	b := newBackoffTable()
	mayor := createBackoffKey(createIdentity{Template: "mayor", QualifiedInstance: "mayor", Named: true}.key())
	chat := createBackoffKey(createIdentity{Template: "chat", QualifiedInstance: "chat", Named: true}.key())
	if mayor != "named:mayor" {
		t.Fatalf("named key = %q, want named:mayor (C5.11)", mayor)
	}
	b.Refuse(mayor, now, time.Time{}, createStageResolve, "rev-with-mayor")
	b.Prune("rev-with-mayor", ledgerCensus{}, nil)
	if _, ok := b.Snapshot()[mayor]; !ok {
		t.Fatal("prune under the same ConfigRev dropped a configured identity's record")
	}
	b.Refuse(chat, now, time.Time{}, createStageLock, "rev-without-mayor")
	b.Prune("rev-without-mayor", ledgerCensus{}, nil)
	snap := b.Snapshot()
	if _, ok := snap[mayor]; ok {
		t.Fatal("the unconfigured identity's record survived the ConfigRev change")
	}
	if _, ok := snap[chat]; !ok {
		t.Fatal("prune dropped the record refused under the current ConfigRev")
	}
}

// Kills: keys that leave the contract's namespaces (C5.11), or a pool key
// and a named key that collide.
func TestBackoffKeysFollowTheContract(t *testing.T) {
	got := strings.Join([]string{
		rowBackoffKey(rowKey{Leg: "sessions", ID: "gc-1"}),
		createBackoffKey(createIdentity{Template: "worker", QualifiedInstance: "worker-1"}.key()),
		createBackoffKey(createIdentity{Template: "worker", QualifiedInstance: "worker", Named: true}.key()),
		workBackoffKey("w-1"),
	}, " ")
	if want := "row:sessions/gc-1 create:worker/worker-1 named:worker work:w-1"; got != want {
		t.Fatalf("keys = %q, want %q", got, want)
	}
}
