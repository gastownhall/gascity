package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The allocator's session census (I2, P3 spec §4.2): every open session row on
// every census leg, read in memory each pass through the leg's CachingStore,
// as legacy's census reads it (collectOpenSessionInfos with live=false).
//
// A hard read error on the sessions leg fails the pass: an error is not an
// empty city. A partial read keeps the rows it returned and makes the pass
// partial (causeStoreQueryPartial), so nothing shrinks. Every other leg keeps
// the rows it returned: none on a hard error. The create effect's locked live
// re-census fails closed on any partial leg, so no plan rests on this read.
//
// Rows are keyed by (leg, bead ID), never by session name, so rows that share
// a name get one entry each (F8). The fold keeps legacy's first-leg-wins rule
// by bead ID with the sessions binding first; a later copy of the same ID is a
// duplicate (C2.11).
//
// Unwired in this slice: P3-7 reads it once per allocator pass, P3-5a decides
// over it, and the ledger (P3-4) clears entries against its unfolded rows.

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

	// The fields below are named after ledgerRow (P3-4), which reads them.
	Incarnation   int64 // the row's generation; 0 when unparseable
	InstanceToken string
	// StartLease is START-043's start-in-flight lease: the row holds its
	// pending-create claim or is creating, and last_woke_at is within
	// startupTimeout + 2s + 5s (pendingCreateStartInFlightInfo).
	StartLease bool
	// PendingCreate is a never-started pending create (no last_woke_at)
	// within its lease (POOL-028, poolSessionWithinPendingCreateLease).
	PendingCreate bool
	// UnknownState is a state main does not know, other than drain-ack
	// stop-pending (F9, SESS-044). The row still occupies its slot.
	UnknownState bool
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
func readSessionCensus(now time.Time, cfg *config.City, legs []classStoreCandidate) (*sessionCensus, error) {
	if len(legs) == 0 {
		return nil, errors.New("session census: no legs")
	}
	c := &sessionCensus{At: now, Rows: make(map[rowKey]censusRow)}
	var startupTimeout time.Duration
	if cfg != nil {
		startupTimeout = cfg.Session.StartupTimeoutDuration()
	}
	clk := &clock.Fake{Time: now}
	canonicalLeg := make(map[string]string)
	for i, source := range legs {
		infos, err := sessionFrontDoor(source.store).ListAll(session.ListAllOptions{})
		if i == 0 && err != nil && !beads.IsPartialResult(err) {
			return nil, fmt.Errorf("session census sessions leg %q: %w", source.ref, err)
		}
		c.Legs = append(c.Legs, censusLeg{Ref: source.ref, Err: err})
		for _, info := range infos {
			id := strings.TrimSpace(info.ID)
			if id == "" {
				continue
			}
			k := rowKey{Leg: source.ref, ID: id}
			row := censusRow{
				Key:           k,
				Info:          info,
				InstanceToken: info.InstanceToken,
				UnknownState:  !isKnownStateInfo(info) && !isDrainAckStopPendingInfo(info),
			}
			row.Incarnation, _ = strconv.ParseInt(strings.TrimSpace(info.Generation), 10, 64)
			if first, dup := canonicalLeg[id]; dup {
				row.DuplicateOf = first
			} else {
				// One effect, one row: only the canonical copy counts in flight.
				canonicalLeg[id] = source.ref
				row.StartLease = pendingCreateStartInFlightInfo(info, clk, startupTimeout)
				row.PendingCreate = strings.TrimSpace(info.LastWokeAt) == "" && poolSessionWithinPendingCreateLease(info, cfg, now)
				c.canonical = append(c.canonical, k)
			}
			c.Rows[k] = row
		}
	}
	c.index()
	return c, nil
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

// Ledger is the census as the intent ledger reads it (P3-4): every row on
// every leg with its config-only endpoint (endpointKeyForAgent), and the legs
// read without error. ReadStarted stays zero (C1a removes it).
func (c *sessionCensus) Ledger(cfg *config.City) ledgerCensus {
	rows := make(map[rowKey]ledgerRow, len(c.Rows))
	for k, row := range c.Rows {
		agent := findAgentByTemplate(cfg, normalizedSessionTemplateInfo(row.Info, cfg))
		rows[k] = ledgerRow{
			Incarnation:   row.Incarnation,
			InstanceToken: row.InstanceToken,
			Endpoint:      endpointKeyForAgent(cfg, agent, row.Info),
			StartLease:    row.StartLease,
			PendingCreate: row.PendingCreate,
		}
	}
	legs := make(map[string]bool, len(c.Legs))
	for _, l := range c.Legs {
		if l.Err == nil {
			legs[l.Ref] = true
		}
	}
	return ledgerCensus{Rows: rows, Legs: legs}
}
