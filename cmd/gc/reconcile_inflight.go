package main

import (
	"sort"
	"time"
)

// The planner's in-flight map (CONTRACT v5 P5; START-CREATE A5): the effects
// the planner submitted and has not drained a settlement for, plus the
// creates whose write call errored after the row may have landed. The planner
// records an entry before it submits the effect, so the next pass counts it,
// and proposes nothing for a row with a running effect. Nothing else outlives
// settlement: the census reads the CachingStore the effects write through, so
// a landed write shows on the next pass. No lock: only the planner goroutine
// adds, settles or clears (P1), and effects report by settlement.
//
// The planner owns it; admit (C1b-2) counts its entries, and C4b submits the
// effects that settle them.

// inflightHardBound is how long after its settlement an ambiguous create may
// stay out of the census before it clears, with an alert (P5). A running
// effect never reaches it: its executor deadline settles it first (P3).
const inflightHardBound = 3 * time.Minute

// inflightCreate is the kind of a create's entry and settlement. Every other
// kind is opaque to the map: it names a row effect, and C1b's per-kind table
// gives it meaning.
const inflightCreate = "create"

// inflightEntry is one running effect, or one ambiguous create.
type inflightEntry struct {
	Kind string
	// Seq is the submit's sequence number, which add assigns and the effect's
	// settlement echoes, so a duplicate or stale settlement never clears the
	// row's next effect.
	Seq uint64
	// Key is the row a non-create effect acts on.
	Key rowKey
	// Endpoint is the effect's endpoint, for admission's gates and counts.
	Endpoint endpointKey
	// The fields below are a create's. Token is the instance token the planner
	// minted at submit; Identity is createIdentity.key; Leg is the sessions leg,
	// for the hard bound's alert. Template, QualifiedInstance, Slot,
	// WorkBeadID and SessionName are its planning stand-in (C1b's
	// inFlightStandIns).
	Token             string
	Identity          string
	Leg               string
	Template          string
	QualifiedInstance string
	Slot              int
	WorkBeadID        string
	SessionName       string
	// Ambiguous marks a settled create whose row may exist; SettledAt is when.
	Ambiguous bool
	SettledAt time.Time
}

// inflightCensus is what the map reads of the census one pass counts: the
// instance tokens its rows carry, on any leg, and the configured named
// identities its open canonical rows hold, by createIdentity.key.
type inflightCensus struct {
	Tokens map[string]bool
	Named  map[string]bool
}

// clearRecord is one ambiguous create cleared by its token in the census, or
// by the hard bound, which the planner alerts on, naming the identity and
// leg (P5).
type clearRecord struct {
	Entry     inflightEntry
	HardBound bool
}

type inflightMap struct {
	seq     uint64
	running map[rowKey]inflightEntry
	creates map[string]inflightEntry // running and ambiguous creates, by token
}

var _ plannerInflight = (*inflightMap)(nil)

func newInflightMap() *inflightMap {
	return &inflightMap{running: make(map[rowKey]inflightEntry), creates: make(map[string]inflightEntry)}
}

// add records e at submit and returns its sequence number, which the
// effect's settlement must carry; 0 means refused. It tells creates from row
// effects by kind and refuses no kind: only a create without its token, a row
// effect without its row, and a token or row that already has an entry.
func (m *inflightMap) add(e inflightEntry) uint64 {
	if e.Kind == inflightCreate {
		if _, held := m.creates[e.Token]; e.Token == "" || held {
			return 0
		}
	} else if _, held := m.running[e.Key]; e.Key.ID == "" || held {
		return 0
	}
	m.seq++
	e.Seq = m.seq
	if e.Kind == inflightCreate {
		m.creates[e.Token] = e
	} else {
		m.running[e.Key] = e
	}
	return e.Seq
}

// settle applies s to the running entry it names by kind (a create by its
// token, any other effect by its row) and sequence number. The entry clears,
// except that an ambiguous create stays until its token shows in the census
// or the hard bound passes. A settlement for no running entry, or for an
// earlier submit, is ignored: every effect settles once (P3).
func (m *inflightMap) settle(s settlement) {
	if s.Kind != inflightCreate {
		if e, ok := m.running[s.Key]; ok && e.Seq == s.Seq {
			delete(m.running, s.Key)
		}
		return
	}
	e, ok := m.creates[s.Token]
	switch {
	case !ok || e.Seq != s.Seq || e.Ambiguous:
	case s.Outcome == settledAmbiguous:
		e.Ambiguous, e.SettledAt = true, s.At
		m.creates[s.Token] = e
	default:
		delete(m.creates, s.Token)
	}
}

// clearVisible clears every ambiguous create whose token c shows, every named
// one whose identity c shows an open canonical row for, and every one the
// hard bound passed at now, in token order. A named identity has one open
// row (the flock, C11), so its row stands for the create whatever token it
// carries: a reopen keeps the row's own, AdoptLive re-stamps it, and a
// create abandoned before its write leaves the row it found. A running
// effect stays.
func (m *inflightMap) clearVisible(c inflightCensus, now time.Time) []clearRecord {
	var out []clearRecord
	for _, e := range m.view().Entries {
		if !e.Ambiguous {
			continue
		}
		visible := c.Tokens[e.Token] || c.Named[e.Identity]
		if !visible && now.Sub(e.SettledAt) < inflightHardBound {
			continue
		}
		delete(m.creates, e.Token)
		out = append(out, clearRecord{Entry: e, HardBound: !visible})
	}
	return out
}

// inflightView is an immutable copy of the map for one pass: the running
// effects by row, then the creates by token.
type inflightView struct {
	Entries []inflightEntry
}

func (m *inflightMap) view() inflightView {
	out := make([]inflightEntry, 0, len(m.running)+len(m.creates))
	for _, e := range m.running {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Key, out[j].Key
		return a.Leg < b.Leg || (a.Leg == b.Leg && a.ID < b.ID)
	})
	creates := make([]inflightEntry, 0, len(m.creates))
	for _, e := range m.creates {
		creates = append(creates, e)
	}
	sort.Slice(creates, func(i, j int) bool { return creates[i].Token < creates[j].Token })
	return inflightView{Entries: append(out, creates...)}
}

// uncensusedCreates counts the running and ambiguous creates whose token no
// census row carries: the creates the city in-flight count takes from the
// map, since a census row that carries the token counts as itself (P4).
func (v inflightView) uncensusedCreates(c inflightCensus) int {
	n := 0
	for _, e := range v.Entries {
		if e.Kind == inflightCreate && !c.Tokens[e.Token] {
			n++
		}
	}
	return n
}

// settlement is the create effect's report as the planner applies it,
// carrying everything its in-flight entry and its backoff records need. Only
// a create whose write may have landed is ambiguous. A named reopen keeps
// the row's own token, which the census could never show, and its row
// exists whether or not the reopen landed, so it clears at settlement like a
// landed create, and backs nothing off. That relies on the CachingStore's
// dirty-row refresh: the next census and the next create's live read under
// the identity flock see the reopened row, so no second create or reopen
// follows. A landed create resets its identity's record, and one refused at
// a stage backs it off with the stage as its cause under its ConfigRev
// (AM-N8); a failure past the stages (failed worktree evidence) throttles
// only the work item.
func (s createSettlement) settlement() settlement {
	out := settlement{Kind: inflightCreate, Seq: s.Seq, Token: s.Token, Outcome: settledFailed, Cause: s.Stage, Facts: effectFacts{Work: s.Work}, Err: s.Err, At: s.At}
	switch {
	case s.Landed:
		out.Outcome = settledLanded
	case s.Ambiguous && s.RetargetRowID == "":
		out.Outcome = settledAmbiguous
	case s.Stage != "":
		out.Outcome = settledRefused
	}
	if s.Landed || s.Stage != "" {
		out.BackoffKey, out.Fingerprint = createBackoffKey(s.Identity), s.ConfigRev
	}
	return out
}
