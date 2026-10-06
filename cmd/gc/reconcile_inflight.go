package main

import (
	"sort"
	"time"
)

// The planner's in-flight map (CONTRACT v4 §5.1-§5.2): the effects the
// planner submitted that the census may not show yet. The planner records an
// entry before it submits the effect (C5.3), so the next pass counts it;
// every entry blocks its row from submit to settlement (C5.5). After
// settlement only creates, starts and reopens stay, until the census shows
// their marker or the hard bound passes (C5.2, C5.4). No lock: only the
// planner goroutine adds, settles or clears (C5.1, C1.11), and effects
// report by settlement.
//
// It sits beside the intent ledger until C1b deletes the ledger and the
// grants; C2b2 and C4b wire it into the planner.

// inflightHardBound is how long after its settlement a tracked entry's
// marker may stay out of the census before the entry clears as unwritten,
// with an alert (C5.4(5), C5.15). A running entry never reaches it: its
// executor deadline settles it first (C1.14).
const inflightHardBound = 3 * time.Minute

type inflightKind uint8

const (
	inflightCreate inflightKind = iota + 1
	inflightStart
	inflightReopen
	inflightWrite
	inflightProbeEffect
	inflightStop
	inflightClose
	inflightRollback
	inflightZombie
)

// tracked reports a kind whose result the census must show before the pass
// counts it from the census alone (C5.2); every other kind clears at
// settlement, whatever its outcome.
func (k inflightKind) tracked() bool {
	return k == inflightCreate || k == inflightStart || k == inflightReopen
}

// capped reports a kind that counts toward the city in-flight cap (C5.2).
func (k inflightKind) capped() bool { return k == inflightCreate || k == inflightStart }

type inflightState uint8

const (
	inflightRunning inflightState = iota + 1
	inflightLanded
	inflightAmbiguous
	inflightFailed
	inflightRefused
	// inflightAlreadyRunning is a start's outcome only: its fresh check found
	// the runtime alive, so it wrote nothing (C8.10).
	inflightAlreadyRunning
)

type inflightEntry struct {
	Kind inflightKind
	// Key is the entry's key, one entry per key: rowBackoffKey of the row an
	// effect acts on, or createBackoffKey of the identity a create makes.
	Key string
	// Leg is the census leg the marker shows on: the row's leg, or the
	// sessions leg for a create.
	Leg         string
	Endpoint    endpointKey
	Template    string
	ConfigRev   string
	SubmittedAt time.Time
	State       inflightState
	Marker      inflightMarker
	// WroteRow says the settled effect may have written a row: it landed, it
	// is ambiguous, or it failed after its write (C5.4(2)).
	WroteRow  bool
	SettledAt time.Time
}

// inflightMarker is the census-visible trace of an effect's write (C5.4(1)).
type inflightMarker struct {
	RowID         string // create: the new row, once known; start, reopen: the row
	InstanceToken string // create: the token the planner minted at submit
	Generation    int64  // start: the generation its PreWake wrote
}

// inflightCensus is what the map reads of the census one pass counts: every
// open row on every leg, and the legs read without error. Only on a clean
// leg does a missing row prove absence.
type inflightCensus struct {
	Rows map[rowKey]inflightRow
	Legs map[string]bool
}

// inflightRow is one census row as the map and the in-flight count read it.
type inflightRow struct {
	Generation    int64
	InstanceToken string
	StartLease    bool // START-043's start-in-flight lease
}

// clearRecord is one entry cleared by its marker, or by the hard bound,
// which the planner alerts on, naming the entry, key and leg (C5.15).
type clearRecord struct {
	Entry     inflightEntry
	HardBound bool
}

type inflightMap struct {
	entries map[string]*inflightEntry
}

var _ plannerInflight = (*inflightMap)(nil)

func newInflightMap() *inflightMap {
	return &inflightMap{entries: make(map[string]*inflightEntry)}
}

// add records e as running at submit (C5.3). It refuses an entry without a
// key or a kind, and a key that already has an entry: the pass proposes
// nothing for a row in flight (C5.5).
func (m *inflightMap) add(e inflightEntry) bool {
	if e.Key == "" || e.Kind < inflightCreate || e.Kind > inflightZombie || m.entries[e.Key] != nil {
		return false
	}
	e.State = inflightRunning
	m.entries[e.Key] = &e
	return true
}

// settle applies s to its running entry. An untracked kind, and an effect
// that wrote nothing, clears at once (C5.2, C5.4(2)). A settlement for no
// running entry is ignored: every effect settles once (C5.6).
func (m *inflightMap) settle(s settlement) {
	e := m.entries[s.Key]
	if e == nil || e.State != inflightRunning || s.Outcome <= inflightRunning {
		return
	}
	e.State, e.WroteRow, e.SettledAt = s.Outcome, s.WroteRow, s.At
	e.Marker.RowID = firstNonEmpty(s.Marker.RowID, e.Marker.RowID)
	e.Marker.InstanceToken = firstNonEmpty(s.Marker.InstanceToken, e.Marker.InstanceToken)
	if s.Marker.Generation != 0 {
		e.Marker.Generation = s.Marker.Generation
	}
	nothingWritten := s.Outcome == inflightRefused || s.Outcome == inflightAlreadyRunning ||
		(s.Outcome == inflightFailed && !s.WroteRow)
	if !e.Kind.tracked() || nothingWritten {
		delete(m.entries, s.Key)
	}
}

// clearVisible clears every settled entry whose marker c shows, and every
// one the hard bound passed at now, in key order. A running entry stays.
func (m *inflightMap) clearVisible(c inflightCensus, now time.Time) []clearRecord {
	var out []clearRecord
	for _, e := range m.view().Entries {
		if e.State == inflightRunning {
			continue
		}
		visible := markerVisible(e, c)
		if !visible && now.Sub(e.SettledAt) < inflightHardBound {
			continue
		}
		delete(m.entries, e.Key)
		out = append(out, clearRecord{Entry: e, HardBound: !visible})
	}
	return out
}

// markerVisible reports whether c shows e's write (C5.4(1)). The marker
// rules depend on the start/create design (simplify/START-CREATE.md); each
// kind keeps its rule in its own function.
func markerVisible(e inflightEntry, c inflightCensus) bool {
	switch e.Kind {
	case inflightCreate:
		return createMarkerVisible(e.Marker, c)
	case inflightStart:
		return startMarkerVisible(e.Leg, e.Marker, c)
	case inflightReopen:
		return reopenMarkerVisible(e.Leg, e.Marker, c)
	}
	return false
}

// createMarkerVisible: the new row ID, or a row that carries the create's
// instance token, on any leg.
func createMarkerVisible(m inflightMarker, c inflightCensus) bool {
	for k, r := range c.Rows {
		if createMarks(m, k, r) {
			return true
		}
	}
	return false
}

func createMarks(m inflightMarker, k rowKey, r inflightRow) bool {
	return (m.RowID != "" && k.ID == m.RowID) || (m.InstanceToken != "" && r.InstanceToken == m.InstanceToken)
}

// startMarkerVisible: the row at the generation its PreWake wrote or later,
// or the row gone from its leg's error-free read. The marker alone clears a
// start (B-2): there is no inventory catch-up rule.
func startMarkerVisible(leg string, m inflightMarker, c inflightCensus) bool {
	r, ok := c.Rows[rowKey{Leg: leg, ID: m.RowID}]
	if !ok {
		return c.Legs[leg]
	}
	return m.Generation > 0 && r.Generation >= m.Generation
}

// reopenMarkerVisible: the row, open on the sessions leg.
func reopenMarkerVisible(leg string, m inflightMarker, c inflightCensus) bool {
	_, ok := c.Rows[rowKey{Leg: leg, ID: m.RowID}]
	return ok
}

// inflightView is an immutable copy of the map for one pass, by key.
type inflightView struct {
	Entries []inflightEntry
}

func (m *inflightMap) view() inflightView {
	out := make([]inflightEntry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return inflightView{Entries: out}
}

// capped returns the creates and starts the view counts toward the city cap,
// and the census rows they stand for, which the pass must not count again
// (C5.13): a start's row, and a create's row by row ID or instance token.
func (v inflightView) capped(c inflightCensus) (n int, represented map[rowKey]bool) {
	represented = make(map[rowKey]bool)
	for _, e := range v.Entries {
		if !e.Kind.capped() {
			continue
		}
		n++
		if e.Kind == inflightStart {
			represented[rowKey{Leg: e.Leg, ID: e.Marker.RowID}] = true
			continue
		}
		for k, r := range c.Rows {
			if createMarks(e.Marker, k, r) {
				represented[k] = true
			}
		}
	}
	return n, represented
}

// settlement is the create effect's report as the in-flight map applies it:
// a landed or ambiguous create may have written its row; any other wrote
// nothing (C5.4(2)). A reopen's marker is its retargeted row.
func (s createSettlement) settlement() settlement {
	out := settlement{
		Key:     createBackoffKey(s.Identity),
		Outcome: inflightFailed,
		Marker:  inflightMarker{RowID: firstNonEmpty(s.RowID, s.RetargetRowID), InstanceToken: s.Token},
		At:      s.At,
	}
	switch {
	case s.Landed:
		out.Outcome, out.WroteRow = inflightLanded, true
	case s.Ambiguous:
		out.Outcome, out.WroteRow = inflightAmbiguous, true
	}
	return out
}
