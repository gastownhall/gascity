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
// every census leg, read in memory each pass. It is index-only: an exact leg
// (the SQLite binding, whose cache is semantically exact) is read from its
// CachingStore through the session front door, and every other leg (bd,
// native Dolt, Postgres) from the backstop lane's last recording of legacy's
// own live read. A pass never reads a non-exact leg's store.
//
// A leg whose read fails is served whole from its last good rows while they
// are within the leg's bound; past it the leg is stale (its rows Keep). A
// stale leg, or one with nothing to serve but a partial read's rows, leaves
// the census incomplete (no fresh create anywhere, POOL-047). A hard failure
// of the sessions leg with nothing to serve fails the pass: an error is not an
// empty city.
//
// Rows are keyed by (leg, bead ID), never by session name, so rows that share
// a name get one entry each (F8). The fold keeps legacy's first-leg-wins rule
// by bead ID with the sessions binding first; a later copy of the same ID is a
// duplicate (C2.11).
//
// Unwired in this slice: P3-7 reads it once per allocator pass, P3-5a decides
// over it, and the ledger (P3-4) clears entries against its unfolded rows.

// censusLastGoodBound is how long an exact leg may be served from its last
// good rows before it is partial (P3 spec §4.2 rule 4, cacheLagBound).
const censusLastGoodBound = 60 * time.Second

// censusLegState is how one leg's rows reached this pass.
type censusLegState uint8

const (
	// legMissing: the read failed and there is no last good to serve. The
	// census is incomplete; the leg holds no rows, or only a partial read's.
	legMissing censusLegState = iota
	// legRead: read this pass (an exact leg) or recorded by the backstop lane
	// within its bound (a non-exact leg).
	legRead
	// legLastGood: this pass's read failed; the leg is served whole from its
	// last good rows, within the bound.
	legLastGood
	// legStale: served from rows older than the bound. The leg is partial and
	// the census incomplete.
	legStale
)

func (s censusLegState) String() string {
	switch s {
	case legRead:
		return "read"
	case legLastGood:
		return "last-good"
	case legStale:
		return "stale"
	default:
		return "missing"
	}
}

// censusLeg is one leg's status in one census.
type censusLeg struct {
	Ref    string // rowKey.Leg
	Exact  bool
	State  censusLegState
	ReadAt time.Time // when the served rows were read; zero when missing
	Err    error     // this pass's read error, if any
}

// complete reports whether the leg's rows are a whole read within its bound,
// so a row missing from it proves the row closed.
func (l censusLeg) complete() bool { return l.State == legRead || l.State == legLastGood }

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

// censusRecording is the backstop lane's last recording of one non-exact
// session census leg (P3-2): the session front door's live ListAll of the
// leg, when it was read, and the error the read returned. Rows are what
// legacy's census keeps: all of a clean read, what a partial read returned,
// nothing of a hard failure.
type censusRecording struct {
	Rows []session.Info
	At   time.Time
	Err  error
}

// censusLegFeed is the census's seam to P3-2's demand reads, which own leg
// classification and the backstop lane. Both functions are required.
type censusLegFeed struct {
	// exact reports whether store's cache is semantically exact (its backing
	// declares beads.CachedReadExact).
	exact func(store beads.Store) bool
	// recorded returns the backstop lane's last recording of a non-exact leg,
	// keyed by store, or false when the lane has none. It must not read the
	// store.
	recorded func(store beads.Store) (censusRecording, bool)
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
	// leases holds every lease-holding copy on every leg by encoded identity,
	// in leg order then bead ID.
	leases map[string][]rowKey
}

// censusLegRows is one leg's last good read.
type censusLegRows struct {
	infos []session.Info
	at    time.Time
}

// censusReader reads the census pass after pass. It keeps each leg's last
// good rows, so it is owned by one goroutine (the allocator lane).
type censusReader struct {
	feed censusLegFeed
	// recordingMaxAge bounds a backstop recording's age (2 × patrol, P3 spec
	// §4.3); an older recording serves its leg as stale.
	recordingMaxAge time.Duration
	lastGood        map[string]censusLegRows
}

func newCensusReader(feed censusLegFeed, recordingMaxAge time.Duration) *censusReader {
	return &censusReader{feed: feed, recordingMaxAge: recordingMaxAge, lastGood: make(map[string]censusLegRows)}
}

// read takes one census over legs, which sessionCensusStoreCandidates
// resolved with the sessions leg first. It errors when there are no legs, or
// when the sessions leg failed hard with nothing to serve.
func (r *censusReader) read(now time.Time, cfg *config.City, legs []classStoreCandidate) (*sessionCensus, error) {
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
		leg, infos := r.readLeg(now, source)
		if i == 0 && leg.State == legMissing && !beads.IsPartialResult(leg.Err) {
			return nil, fmt.Errorf("session census sessions leg %q: %w", leg.Ref, leg.Err)
		}
		c.Legs = append(c.Legs, leg)
		for _, info := range infos {
			id := strings.TrimSpace(info.ID)
			if id == "" {
				continue
			}
			k := rowKey{Leg: leg.Ref, ID: id}
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
				canonicalLeg[id] = leg.Ref
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

// readLeg reads one leg and applies the last-good rule.
func (r *censusReader) readLeg(now time.Time, source classStoreCandidate) (censusLeg, []session.Info) {
	leg := censusLeg{Ref: source.ref, Exact: r.feed.exact(source.store)}
	bound := censusLastGoodBound
	var infos []session.Info
	var at time.Time
	if leg.Exact {
		infos, leg.Err = sessionFrontDoor(source.store).ListAll(session.ListAllOptions{})
		at = now
	} else {
		bound = r.recordingMaxAge
		rec, ok := r.feed.recorded(source.store)
		switch {
		case !ok:
			leg.Err = errors.New("no backstop recording")
		default:
			leg.Err = rec.Err
			infos = rec.Rows
			at = rec.At
		}
	}
	if leg.Err == nil {
		r.lastGood[leg.Ref] = censusLegRows{infos: infos, at: at}
		leg.State, leg.ReadAt = legRead, at
	} else if good, ok := r.lastGood[leg.Ref]; ok {
		infos = good.infos
		leg.State, leg.ReadAt = legLastGood, good.at
	} else {
		// A partial read's rows still occupy their slots and names (legacy's
		// fold keeps them); the leg stays incomplete.
		if !beads.IsPartialResult(leg.Err) {
			infos = nil
		}
		return leg, infos
	}
	if now.Sub(leg.ReadAt) > bound {
		leg.State = legStale
	}
	return leg, infos
}

// index orders the canonical rows and builds the per-pass lookups.
func (c *sessionCensus) index() {
	order := make(map[string]int, len(c.Legs))
	for i, l := range c.Legs {
		order[l.Ref] = i
	}
	byLegThenID := func(keys []rowKey) {
		sort.Slice(keys, func(i, j int) bool {
			a, b := keys[i], keys[j]
			if order[a.Leg] != order[b.Leg] {
				return order[a.Leg] < order[b.Leg]
			}
			return a.ID < b.ID
		})
	}
	byLegThenID(c.canonical)
	c.byName = make(map[string][]rowKey)
	for _, k := range c.canonical {
		if name := strings.TrimSpace(c.Rows[k].Info.SessionName); name != "" {
			c.byName[name] = append(c.byName[name], k)
		}
	}
	// The identity lease is checked against every copy on every leg, as the
	// create effect checks it under its locks (freshPoolAvailabilityInfos):
	// a copy whose identity fields differ from the canonical row's still
	// holds its spelling. Copies with the same fields are one holder.
	all := make([]rowKey, 0, len(c.Rows))
	for k := range c.Rows {
		all = append(all, k)
	}
	byLegThenID(all)
	c.leases = make(map[string][]rowKey)
	seen := make(map[[4]string]bool, len(all))
	for _, k := range all {
		info := c.Rows[k].Info
		copyKey := [4]string{k.ID, strings.TrimSpace(info.SessionNameMetadata), strings.TrimSpace(info.Alias), strings.TrimSpace(info.AgentName)}
		if seen[copyKey] {
			continue
		}
		seen[copyKey] = true
		if lease, ok := poolIdentityLeaseOf(info); ok {
			c.leases[lease] = append(c.leases[lease], k)
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

// CompleteLegs names the legs whose rows are a whole read within their bound
// (ledgerCensus.Legs): only there does a missing row prove a close.
func (c *sessionCensus) CompleteLegs() map[string]bool {
	out := make(map[string]bool, len(c.Legs))
	for _, l := range c.Legs {
		if l.complete() {
			out[l.Ref] = true
		}
	}
	return out
}

// Incomplete reports whether some leg has no whole read within its bound to
// serve (missing or stale): no fresh create may be planned anywhere from that
// view (POOL-047), as legacy blocks every create on a partial census (owner
// decision 2026-10-03 at P3-3 review).
func (c *sessionCensus) Incomplete() bool {
	for _, l := range c.Legs {
		if !l.complete() {
			return true
		}
	}
	return false
}

// StaleLegs names the legs served past their bound: their rows Keep.
func (c *sessionCensus) StaleLegs() map[string]bool {
	var out map[string]bool
	for _, l := range c.Legs {
		if l.State == legStale {
			if out == nil {
				out = make(map[string]bool)
			}
			out[l.Ref] = true
		}
	}
	return out
}

// RowsNamed returns the canonical rows whose runtime session name is name.
func (c *sessionCensus) RowsNamed(name string) []rowKey {
	return c.byName[strings.TrimSpace(name)]
}

// IdentityLeaseHolder returns an open row copy, on any leg, that holds
// template's pool identity agentName (ensurePoolIdentityNotHeldByOpenRow's
// snapshot leg): a fresh create for that identity is doomed, so the planner
// plans none, and spends no token or fenced re-census on it (F8). The create
// effect re-checks live under the identifier locks. agentName is the identity
// after template defaulting, and only bead-scoped identities hold a lease
// (P3-5a obligations).
func (c *sessionCensus) IdentityLeaseHolder(cfg *config.City, template, agentName string) (rowKey, bool) {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return rowKey{}, false
	}
	for _, k := range c.leases[poolIdentitySessionName(agentName, template)] {
		if poolIdentityLeaseTemplateMatches(c.Rows[k].Info, cfg, template) {
			return k, true
		}
	}
	return rowKey{}, false
}

// UnknownStates counts canonical rows in states main does not know, by
// template, so the scale of the enterprise-state migration (OQ-4) is visible
// before the cutover.
func (c *sessionCensus) UnknownStates(cfg *config.City) map[string]int {
	var out map[string]int
	for _, k := range c.canonical {
		row := c.Rows[k]
		if !row.UnknownState {
			continue
		}
		template := normalizedSessionTemplateInfo(row.Info, cfg)
		if template == "" {
			template = row.Info.Template
		}
		if out == nil {
			out = make(map[string]int)
		}
		out[template]++
	}
	return out
}

// Ledger is the census as the intent ledger reads it (P3-4): every row on
// every leg with its config-only endpoint (endpointKeyForAgent), and the legs
// whose read is complete.
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
	return ledgerCensus{Rows: rows, Legs: c.CompleteLegs()}
}
