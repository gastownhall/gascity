package main

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

const rowLeg = "city:test-city"

func rowAt(d time.Duration) string { return gatherNow.Add(d).Format(time.RFC3339) }

// rowWorld is a World and an allocation over rows on one leg at gatherNow,
// with every row's entry reading alive and its mislabelled flag as gather
// sets it.
func rowWorld(t *testing.T, rows ...beads.Bead) (*World, *allocDecision) {
	t.Helper()
	c := readCensus(t, gatherNow, censusLegs(rowLeg, censusStore(rows...)))
	w := &World{Now: gatherNow, Census: c, Mislabelled: make(map[rowKey]bool)}
	a := &allocDecision{Snapshot: &selectionSnapshot{Entries: make(map[rowKey]*selectionEntry)}}
	for _, row := range c.Canonical() {
		if row.Info.Template == "" && row.Info.SessionNameMetadata == "" {
			w.Mislabelled[row.Key] = true
		}
		a.Snapshot.Entries[row.Key] = &selectionEntry{Key: row.Key, Liveness: livenessAlive}
	}
	return w, a
}

func rowKeyOf(id string) rowKey { return rowKey{Leg: rowLeg, ID: id} }

// killPending is a `gc session kill` fence stamped 10s before gatherNow.
var killPending = []string{"state", "asleep", "state_reason", session.KillPendingReason, "sleep_reason", "killed", "slept_at", rowAt(-10 * time.Second)}

// Kills reordered arms, an arm dropped, and a deadline lost. Each row meets
// two arms' conditions; the earlier arm in CONTRACT v5 §4 must win. Legacy's
// order differs where v5 says so (decideSession runs its unknown-state arm
// before the timer heals as well, but v5 puts the rekey and the stop request
// above it, and metadata above the baseline; those arms land with C4c2,
// C6b2, C7d and C7c, which extend this test).
func TestDecideRowArmOrderMatchesLegacy(t *testing.T) {
	var names []string
	last := 0
	for _, arm := range rowArms {
		names = append(names, arm.name)
		n, err := strconv.Atoi(arm.name[1:])
		if err != nil || n <= last {
			t.Fatalf("arm %s out of CONTRACT v5 §4 order after A%d", arm.name, last)
		}
		last = n
	}
	if want := []string{"A1", "A2", "A5", "A6", "A9", "A19"}; !slices.Equal(names, want) {
		t.Fatalf("rowArms = %v, want %v", names, want)
	}

	expiredHold := []string{"held_until", rowAt(-time.Minute)}
	cases := []struct {
		name     string
		meta     []string
		unknown  bool // the entry's liveness reads unknown
		unranked bool
		noRow    bool
		wantKind string
		want     string
		wantNext time.Time
	}{
		{name: "A1 no row", noRow: true, want: decideNoRow},
		{name: "A1 mislabelled before A2 kill fence", meta: append([]string{"template", "", "session_name", ""}, killPending...), want: decideMislabelled},
		{name: "A2 kill fence before A6 heals", meta: append(append([]string{}, killPending...), expiredHold...), want: decideKillFence, wantNext: gatherNow.Add(session.KillPendingGrace - 10*time.Second)},
		{name: "A2 kill fence before A9 liveness", meta: killPending, unknown: true, want: decideKillFence, wantNext: gatherNow.Add(session.KillPendingGrace - 10*time.Second)},
		{name: "A5 unknown state before A6 heals", meta: append([]string{"state", "hibernating"}, expiredHold...), want: decideUnknownState},
		{name: "stop-pending is not A5's", meta: []string{"state", "draining", "state_reason", "drain-ack-stop-pending"}, want: decideNoAction},
		{name: "A6 heals before A9 liveness", meta: expiredHold, unknown: true, wantKind: intentRowHeal, want: decideTimerHeal},
		{name: "A6 heals before A9's desire gate", meta: expiredHold, unranked: true, wantKind: intentRowHeal, want: decideTimerHeal},
		{name: "A6 running timer falls through to A9", meta: []string{"held_until", rowAt(time.Minute)}, unknown: true, want: decideLivenessUnknown, wantNext: gatherNow.Add(time.Minute + time.Second)},
		{name: "A9 desire gate", unranked: true, want: decideUnranked},
		{name: "no arm", want: decideNoAction},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := append([]string{"template", "worker", "session_name", "s-gc-1", "state", "asleep", "generation", "3"}, tc.meta...)
			var rows []beads.Bead
			if !tc.noRow {
				rows = append(rows, sessionRow("gc-1", meta...))
			}
			w, a := rowWorld(t, rows...)
			k := rowKeyOf("gc-1")
			if e := a.Snapshot.Entries[k]; e != nil && tc.unknown {
				e.Liveness = livenessUnknown
			}
			if tc.unranked {
				delete(a.Snapshot.Entries, k)
			}
			it, next := decideRow(w, a, k)
			if it.Kind != tc.wantKind || it.Reason != tc.want || it.Key != k {
				t.Fatalf("decideRow = (%q, %q, %v), want (%q, %q, %v)", it.Kind, it.Reason, it.Key, tc.wantKind, tc.want, k)
			}
			if !next.Equal(tc.wantNext) {
				t.Fatalf("next = %v, want %v", next, tc.wantNext)
			}
			if it.Kind != "" && it.Basis != (rowBasis{Incarnation: 3}) {
				t.Fatalf("basis = %+v, want the census row's incarnation 3", it.Basis)
			}
		})
	}
}

// Kills a mislabelled row acted on (CONTRACT v5 A1, AL1): a bead labeled
// gc:session with no template and no session name proposes nothing, not
// even its expired timer's heal.
func TestMislabelledRowIsNone(t *testing.T) {
	w, a := rowWorld(t, sessionRow("gc-1", "state", "asleep", "held_until", rowAt(-time.Minute)))
	it, _ := decideRow(w, a, rowKeyOf("gc-1"))
	if it.Kind != "" || it.Reason != decideMislabelled {
		t.Fatalf("decideRow = (%q, %q), want None (mislabelled)", it.Kind, it.Reason)
	}
}

// Kills D2's fresh-legs column mapped wrong: idle, no-wake-reason and
// idle-respawn begin and signal as the probing -fresh kinds; every other
// reason as a plain row write.
func TestDrainKindFollowsD2FreshLegs(t *testing.T) {
	for _, tc := range []struct {
		reason        string
		begin, signal string
	}{
		{"idle", intentDrainBeginFresh, intentSignalFresh},
		{"no-wake-reason", intentDrainBeginFresh, intentSignalFresh},
		{"idle-respawn", intentDrainBeginFresh, intentSignalFresh},
		{"orphaned", intentDrainBegin, intentSignal},
		{"suspended", intentDrainBegin, intentSignal},
		{"config-drift", intentDrainBegin, intentSignal},
		{"execution-stalled", intentDrainBegin, intentSignal},
	} {
		if got := drainKind(tc.reason, false); got != tc.begin {
			t.Errorf("drainKind(%q, begin) = %q, want %q", tc.reason, got, tc.begin)
		}
		if got := drainKind(tc.reason, true); got != tc.signal {
			t.Errorf("drainKind(%q, signal) = %q, want %q", tc.reason, got, tc.signal)
		}
	}
}

// Kills a row counted on one endpoint and gated or proposed on another:
// the census's bring-up row, the allocation's entry, gather's breaker
// readings and a create intent for the row's template all name the same
// endpoint, a row whose template resolves only through its stored common
// name included.
func TestRowEndpointIsOneHelper(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Provider: "codex"},
		Agents: []config.Agent{
			{Name: "worker", Provider: "claude", MaxActiveSessions: intPtr(3)},
			{Name: "builder", Upstream: "u1", MaxActiveSessions: intPtr(2)},
			{Name: "plain", MaxActiveSessions: intPtr(2)},
		},
	}
	rows := []beads.Bead{
		poolRow("gc-1", "worker", 1, "asleep"),
		poolRow("gc-2", "builder", 1, "asleep"),
		poolRow("gc-3", "plain", 1, "asleep"),
		sessionRow("gc-4", "common_name", "worker", "session_name", "s-gc-4", "state", "asleep"),
	}
	c := readCensus(t, gatherNow, censusLegs(rowLeg, censusStore(rows...)))
	d, err := decideAllocation(allocInputs{Now: gatherNow, Cfg: cfg, Census: c})
	if err != nil {
		t.Fatal(err)
	}
	gates := gatherGates(nil, cfg, c.Canonical())
	want := map[string]endpointKey{"gc-1": "provider:claude", "gc-2": "upstream:u1", "gc-3": "provider:codex", "gc-4": "provider:claude"}
	for _, r := range c.BringUp(cfg) {
		if r.Endpoint != want[r.Key.ID] {
			t.Errorf("%s bring-up endpoint = %q, want %q", r.Key.ID, r.Endpoint, want[r.Key.ID])
		}
		if e := d.Snapshot.Entries[r.Key]; e == nil || e.Endpoint != r.Endpoint {
			t.Errorf("%s allocation entry endpoint = %+v, want %q", r.Key.ID, e, r.Endpoint)
		}
		if _, ok := gates[r.Endpoint]; !ok {
			t.Errorf("%s endpoint %q has no breaker reading", r.Key.ID, r.Endpoint)
		}
		info := c.Rows[r.Key].Info
		if info.Template == "" {
			continue
		}
		if got := createIntent(cfg, "", allocPlan{Kind: createPool, Template: info.Template}).Endpoint; got != r.Endpoint {
			t.Errorf("create intent for %s's template endpoint = %q, want its row's %q", r.Key.ID, got, r.Endpoint)
		}
	}
}
