package main

import (
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/worktree"
)

// The allocator's backoff table (I13; CONTRACT C5.11 as amended 2026-10-04
// per SIMPLIFICATION-CHECKPOINT C3): every refusal not to retry at pass rate,
// under one schedule. It is not part of the ledger and blocks no effect. A
// writer records a refusal before it settles the entry it refused, and a pass
// reads the ledger before the table, so no pass sees one without the other.
//
// Unwired in this slice: P3-7 prunes it at each pass start and hands the pass
// a Snapshot; P4.1's session keys record row refusals.

const (
	// backoffBase and backoffMax bound a key's backoff (C5.11).
	backoffBase = 10 * time.Second
	backoffMax  = 5 * time.Minute
)

// vetoBackoff is C5.11's backoff for the nth consecutive refusal: 10s
// doubling, capped at 5m.
func vetoBackoff(n int) time.Duration {
	if n > 6 { // 10s × 2^5 = 320s already exceeds the cap
		return backoffMax
	}
	return min(backoffBase<<(max(n, 1)-1), backoffMax)
}

// Backoff keys (C5.11): "row:<leg>/<id>" for a session key's refusal;
// "create:<template>/<instance>" or "named:<identity>" for a create that
// failed without writing (AM-N8), fingerprinted by its ConfigRev; and
// "work:<bead>" for worktree evidence that failed verification (C6.5(a)),
// fingerprinted by the evidence.

func rowBackoffKey(k rowKey) string { return "row:" + k.Leg + "/" + k.ID }

// createBackoffKey is a create identity's backoff key, from
// createIdentity.key or allocPlan.identity.
func createBackoffKey(identity string) string {
	if strings.HasPrefix(identity, "named:") {
		return identity
	}
	return "create:" + identity
}

func workBackoffKey(beadID string) string { return "work:" + beadID }

// specFingerprint is a work record's fingerprint: new evidence for the same
// bead (another generation, path or branch) is verified afresh.
func specFingerprint(spec worktree.Spec) string { return fmt.Sprintf("%#v", spec) }

// backoffRecord is one key's refusal state. Cause is the refusal's cause,
// kept verbatim: the planner advances a pool request to its next slot only
// on createStageFence (F3).
type backoffRecord struct {
	Consecutive int
	Until       time.Time
	Cause       string
	Fingerprint string
}

// live reports whether r still refuses its key at now.
func (r backoffRecord) live(now time.Time) bool { return r.Until.After(now) }

type backoffTable struct {
	mu   sync.Mutex
	recs map[string]backoffRecord
}

func newBackoffTable() *backoffTable {
	return &backoffTable{recs: make(map[string]backoffRecord)}
}

// Refuse records a refusal of k at now, live until the later of until and
// k's backoff. A refusal while k's record is live is the same refusal seen
// again: it keeps the count and only extends Until. A refusal under another
// fingerprint starts the count afresh.
func (t *backoffTable) Refuse(k string, now, until time.Time, cause, fingerprint string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.recs[k]
	if r.Fingerprint != fingerprint {
		r = backoffRecord{Fingerprint: fingerprint}
	}
	if !r.live(now) {
		r.Consecutive++
	}
	if b := now.Add(vetoBackoff(r.Consecutive)); b.After(until) {
		until = b
	}
	if until.After(r.Until) {
		r.Until = until
	}
	r.Cause = cause
	t.recs[k] = r
}

// Succeed drops k's record: k succeeded (a start issued, a create committed,
// a worktree verified), so its next refusal backs off from the start.
func (t *backoffTable) Succeed(k string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.recs, k)
}

// Prune drops every record whose key left config or demand, so the table
// stays bounded: a create record reserved under a ConfigRev other than
// configRev (config is fixed per revision, so this also drops an identity
// config no longer holds), a work record for a bead not in demand, and a row
// record whose row c shows closed (gone from a leg c holds complete). Bead
// IDs hold no "/", so a row key splits at its last one.
func (t *backoffTable) Prune(configRev string, c ledgerCensus, demand map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, r := range t.recs {
		var drop bool
		switch kind, rest, _ := strings.Cut(k, ":"); kind {
		case "row":
			i := strings.LastIndex(rest, "/")
			leg := rest[:max(i, 0)]
			_, open := c.Rows[rowKey{Leg: leg, ID: rest[i+1:]}]
			drop = !open && c.Legs[leg]
		case "work":
			drop = !demand[rest]
		default: // create and named
			drop = r.Fingerprint != configRev
		}
		if drop {
			delete(t.recs, k)
		}
	}
}

// Snapshot returns a copy of every record: the pass's Backoff view.
func (t *backoffTable) Snapshot() map[string]backoffRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	return maps.Clone(t.recs)
}
