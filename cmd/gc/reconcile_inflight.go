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
// It sits beside the intent ledger until C1b deletes the ledger and the
// grants; C2b2 and C4b wire it into the planner.

// inflightHardBound is how long after its settlement an ambiguous create may
// stay out of the census before it clears, with an alert (P5). A running
// effect never reaches it: its executor deadline settles it first (P3).
const inflightHardBound = 3 * time.Minute

type inflightKind uint8

const (
	inflightCreate inflightKind = iota + 1
	inflightStart
	inflightWrite
	inflightProbeEffect
	inflightStop
	inflightClose
	inflightRollback
	inflightZombie
)

// inflightEntry is one running effect, or one ambiguous create.
type inflightEntry struct {
	Kind inflightKind
	// Key is the row a non-create effect acts on.
	Key rowKey
	// Token, Identity and Leg are a create's: the instance token the planner
	// minted at submit, its identity (createIdentity.key), and the sessions
	// leg, for the hard bound's alert.
	Token    string
	Identity string
	Leg      string
	// Ambiguous marks a settled create whose row may exist; SettledAt is when.
	Ambiguous bool
	SettledAt time.Time
}

// inflightCensus is what the map reads of the census one pass counts: the
// instance tokens its rows carry, on any leg.
type inflightCensus struct {
	Tokens map[string]bool
}

// clearRecord is one ambiguous create cleared by its token in the census, or
// by the hard bound, which the planner alerts on, naming the identity and
// leg (P5).
type clearRecord struct {
	Entry     inflightEntry
	HardBound bool
}

type inflightMap struct {
	running map[rowKey]inflightEntry
	creates map[string]inflightEntry // running and ambiguous creates, by token
}

var _ plannerInflight = (*inflightMap)(nil)

func newInflightMap() *inflightMap {
	return &inflightMap{running: make(map[rowKey]inflightEntry), creates: make(map[string]inflightEntry)}
}

// add records e at submit. It refuses a create without its token, any other
// kind without its row, and a token or row that already has an entry.
func (m *inflightMap) add(e inflightEntry) bool {
	switch {
	case e.Kind == inflightCreate:
		if e.Token == "" || m.creates[e.Token].Kind != 0 {
			return false
		}
		m.creates[e.Token] = e
	case e.Kind > inflightCreate && e.Kind <= inflightZombie:
		if e.Key.ID == "" || m.running[e.Key].Kind != 0 {
			return false
		}
		m.running[e.Key] = e
	default:
		return false
	}
	return true
}

// settle clears s's running entry, except that an ambiguous create stays
// until its token shows in the census or the hard bound passes. A settlement
// for no running entry is ignored: every effect settles once (P3).
func (m *inflightMap) settle(s settlement) {
	if s.Token == "" {
		delete(m.running, s.Key)
		return
	}
	e, ok := m.creates[s.Token]
	switch {
	case !ok || e.Ambiguous:
	case s.Ambiguous:
		e.Ambiguous, e.SettledAt = true, s.At
		m.creates[s.Token] = e
	default:
		delete(m.creates, s.Token)
	}
}

// clearVisible clears every ambiguous create whose token c shows, and every
// one the hard bound passed at now, in token order. A running effect stays.
func (m *inflightMap) clearVisible(c inflightCensus, now time.Time) []clearRecord {
	var out []clearRecord
	for _, e := range m.view().Entries {
		if !e.Ambiguous {
			continue
		}
		visible := c.Tokens[e.Token]
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

// settlement is the create effect's report as the in-flight map applies it.
// Only a create whose write may have landed is ambiguous; a named reopen
// keeps the row's own token, which the census could never show, and its row
// exists whether or not the reopen landed, so it clears at settlement like a
// landed create.
func (s createSettlement) settlement() settlement {
	return settlement{Kind: "create", Token: s.Token, Ambiguous: s.Ambiguous && s.RetargetRowID == "", At: s.At, Err: s.Err}
}
