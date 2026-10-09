package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The allocator's session census (I2, P3 spec §4.2): every open session row on
// every census leg, read in memory each pass through the leg's CachingStore,
// as legacy's census reads it (collectOpenSessionInfos with live=false), but
// at beads.FederatedReadTier on every leg, so a relocated sessions binding
// serves its wisp-tier rows too (CONTRACT v5 AL1).
//
// A hard read error on the sessions leg fails the pass: an error is not an
// empty city. A partial read keeps the rows it returned and makes the pass
// partial (causeStoreQueryPartial), so nothing shrinks. Every other leg keeps
// the rows it returned: none on a hard error. No create rests on this read
// alone: a pool create's locked live re-census reads every leg and fails
// closed on any partial one; a named create's locked read covers the sessions
// store only, which fails closed on a partial read, since named rows live only
// there (C11 refuses duplicates elsewhere at boot).
//
// Rows are keyed by (leg, bead ID), never by session name, so rows that share
// a name get one entry each (F8). The fold keeps legacy's first-leg-wins rule
// by bead ID with the sessions binding first; a later copy of the same ID is a
// duplicate (C2.11).
//
// Unwired in this slice: the planner reads it once per pass, the decide
// plans over it, and admission counts bring-up rows from its projection.

// censusLeg is one leg's read in one census.
type censusLeg struct {
	Ref string // rowKey.Leg
	Err error  // the read's error, if any
}

// censusRow is one open session row on one leg.
type censusRow struct {
	Key  rowKey
	Info session.Info
	// DuplicateOf is the leg of the canonical copy when an earlier leg holds
	// the same bead ID (C2.11); empty on the canonical row.
	DuplicateOf string

	Incarnation   int64 // the row's generation; 0 when unparseable
	InstanceToken string
	// PendingCreate: the row holds pending_create_claim and is not committed
	// (active or awake). Admission counts it in flight; no lease window
	// applies (v5 P4, SC A5). A row committed mid-start at cutover can still
	// hold the claim and would otherwise count forever; a reopened named row
	// (stopped, with the claim) still counts.
	PendingCreate bool
	// UnknownState is a state main does not know, other than drain-ack
	// stop-pending (F9, SESS-044). The row still occupies its slot.
	UnknownState bool
	// StopKeys are the row's raw stop-request keys, which only activeStop
	// reads (v5 D1).
	StopKeys rawStopKeys
}

// sessionCensus is one pass's census. It is immutable once read.
type sessionCensus struct {
	At   time.Time
	Legs []censusLeg // census order: the sessions leg first
	// Rows is the unfolded census: every open row on every leg, duplicates
	// included, so a write is visible whichever leg it landed on.
	Rows map[rowKey]censusRow

	canonical []rowKey            // first-leg-wins rows, in leg order then bead ID
	byName    map[string][]rowKey // canonical rows by runtime session name
}

// readSessionCensus takes one census over legs, which
// sessionCensusStoreCandidates resolved with the sessions leg first. It
// errors when there are no legs, or when the sessions leg failed hard.
func readSessionCensus(now time.Time, legs []classStoreCandidate) (*sessionCensus, error) {
	if len(legs) == 0 {
		return nil, errors.New("session census: no legs")
	}
	c := &sessionCensus{At: now, Rows: make(map[rowKey]censusRow)}
	canonicalLeg := make(map[string]string)
	for i, source := range legs {
		listed, err := sessionFrontDoor(source.store).ListAllWithResponses(session.ListAllOptions{TierMode: beads.FederatedReadTier})
		if i == 0 && err != nil && !beads.IsPartialResult(err) {
			return nil, fmt.Errorf("session census sessions leg %q: %w", source.ref, err)
		}
		c.Legs = append(c.Legs, censusLeg{Ref: source.ref, Err: err})
		for _, l := range listed {
			id := strings.TrimSpace(l.Info.ID)
			if id == "" {
				continue
			}
			k := rowKey{Leg: source.ref, ID: id}
			row := newCensusRow(k, l.Info)
			row.StopKeys = readStopKeys(l.Response.Metadata)
			if first, dup := canonicalLeg[id]; dup {
				// One effect, one row: only the canonical copy counts in flight.
				row.DuplicateOf, row.PendingCreate = first, false
			} else {
				canonicalLeg[id] = source.ref
				c.canonical = append(c.canonical, k)
			}
			c.Rows[k] = row
		}
	}
	c.index()
	return c, nil
}

// newCensusRow is info's census row at k, as its canonical copy.
func newCensusRow(k rowKey, info session.Info) censusRow {
	row := censusRow{
		Key:           k,
		Info:          info,
		InstanceToken: info.InstanceToken,
		UnknownState:  !isKnownStateInfo(info) && !isDrainAckStopPendingInfo(info),
	}
	row.Incarnation, _ = strconv.ParseInt(strings.TrimSpace(info.Generation), 10, 64)
	state := session.State(strings.TrimSpace(info.MetadataState))
	row.PendingCreate = info.PendingCreateClaim && state != session.StateActive && state != session.StateAwake
	return row
}

// reread is k's census row read again as info, with its persisted metadata
// meta. A nil meta keeps the census's stop keys: a re-decide never drops a
// request it was not shown.
func (c *sessionCensus) reread(k rowKey, info session.Info, meta map[string]string) censusRow {
	r, prev := newCensusRow(k, info), c.Rows[k]
	r.StopKeys = prev.StopKeys
	if meta != nil {
		r.StopKeys = readStopKeys(meta)
	}
	if r.DuplicateOf = prev.DuplicateOf; r.DuplicateOf != "" {
		r.PendingCreate = false
	}
	return r
}

// index orders the canonical rows and builds the per-pass lookups.
func (c *sessionCensus) index() {
	order := make(map[string]int, len(c.Legs))
	for i, l := range c.Legs {
		order[l.Ref] = i
	}
	sort.Slice(c.canonical, func(i, j int) bool {
		a, b := c.canonical[i], c.canonical[j]
		if order[a.Leg] != order[b.Leg] {
			return order[a.Leg] < order[b.Leg]
		}
		return a.ID < b.ID
	})
	c.byName = make(map[string][]rowKey)
	for _, k := range c.canonical {
		if name := strings.TrimSpace(c.Rows[k].Info.SessionName); name != "" {
			c.byName[name] = append(c.byName[name], k)
		}
	}
}

// Canonical returns the folded census, one row per bead ID: the sessions leg
// first, then each leg in order, rows by bead ID within a leg.
func (c *sessionCensus) Canonical() []censusRow {
	out := make([]censusRow, 0, len(c.canonical))
	for _, k := range c.canonical {
		out = append(out, c.Rows[k])
	}
	return out
}

// Partial reports whether some leg's read was partial: its rows are what the
// read returned, so the pass retains (causeStoreQueryPartial).
func (c *sessionCensus) Partial() bool {
	for _, l := range c.Legs {
		if beads.IsPartialResult(l.Err) {
			return true
		}
	}
	return false
}

// RowsNamed returns the canonical rows whose runtime session name is name.
func (c *sessionCensus) RowsNamed(name string) []rowKey {
	return c.byName[strings.TrimSpace(name)]
}

// bringUpRow is one census row as admission counts bring-up rows (v5 P4).
type bringUpRow struct {
	Key           rowKey
	Token         string      // the row's instance token
	Endpoint      endpointKey // config-only key (rowEndpoint)
	PendingCreate bool        // canonical copy only, so one row counts once
}

// BringUp is the census bring-up projection: every row on every leg, in key
// order, with its token, its config-only endpoint and its pending-create
// claim. It has no lease and no read start (S1-8).
func (c *sessionCensus) BringUp(cfg *config.City) []bringUpRow {
	keys := make([]rowKey, 0, len(c.Rows))
	for k := range c.Rows {
		keys = append(keys, k)
	}
	sortRowKeys(keys)
	out := make([]bringUpRow, 0, len(keys))
	for _, k := range keys {
		row := c.Rows[k]
		out = append(out, bringUpRow{Key: k, Token: row.InstanceToken, Endpoint: rowEndpoint(cfg, row.Info), PendingCreate: row.PendingCreate})
	}
	return out
}
