package nudgequeue

import (
	"fmt"
	"testing"
	"time"
)

func startupTestTurn(id, launch, sessionID string, created time.Time) Item {
	return Item{
		ID: id, Agent: "alpha", SessionID: sessionID, Source: SourceSubmit, Message: "begin",
		Reference: StartupTurnReference(launch), CreatedAt: created, DeliverAfter: created, ExpiresAt: created.Add(time.Hour),
	}
}

func TestStartupTurnReferenceCodec(t *testing.T) {
	if ref := StartupTurnReference(""); ref != nil {
		t.Fatalf("StartupTurnReference(\"\") = %+v, want nil: no launch, no binding", ref)
	}
	ref := StartupTurnReference("abc123")
	if ref == nil || ref.Kind != ReferenceKindStartupTurn || ref.ID != "abc123" {
		t.Fatalf("StartupTurnReference = %+v", ref)
	}
	if id, ok := StartupTurnLaunch(Item{Reference: ref}); !ok || id != "abc123" {
		t.Fatalf("StartupTurnLaunch = %q, %v; want abc123, true", id, ok)
	}
	for name, item := range map[string]Item{
		"no reference":            {},
		"another kind":            {Reference: &Reference{Kind: "bead", ID: "abc123"}},
		"the G7-era kind":         {Reference: &Reference{Kind: "startup_nudge", ID: "alpha"}},
		"a startup kind, no id":   {Reference: &Reference{Kind: ReferenceKindStartupTurn}},
		"a plain deferred submit": {Source: SourceSubmit},
	} {
		if _, ok := StartupTurnLaunch(item); ok || IsStartupTurn(item) {
			t.Errorf("%s reads as a startup turn", name)
		}
	}
}

func TestJudgeStartupTurnsVerdicts(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	const session = "s-1"
	turn := func(id, launch string, created time.Time) Item {
		item := startupTestTurn(id, launch, session, created)
		item.DeliverAfter = now
		return item
	}
	seen := func(ids ...string) map[string]bool {
		m := map[string]bool{}
		for _, id := range ids {
			m[id] = true
		}
		return m
	}
	asked := func(id string, ids ...string) LiveLaunch {
		return LiveLaunch{Asked: true, ID: id, Known: true, Seen: seen(ids...)}
	}
	notDue := turn("t-later", "L1", now)
	notDue.DeliverAfter = now.Add(time.Second)
	dueAtEdge := turn("t-edge", "L1", now)
	expired := turn("t-expired", "L1", now.Add(-2*time.Hour))
	expired.DeliverAfter = now.Add(-2 * time.Hour)
	foreign := startupTestTurn("t-foreign", "L1", "s-2", now)
	unfenced := startupTestTurn("t-unfenced", "L1", "", now)
	reminder := Item{ID: "n-1", Agent: "alpha", SessionID: session, Source: "session", CreatedAt: now, DeliverAfter: now}

	cases := []struct {
		name              string
		pending, inFlight []Item
		launch            LiveLaunch
		want              string
	}{
		{"no turn", []Item{reminder}, nil, LiveLaunch{}, "members=[] needs=false hold=false claim= stale=[] superseded=[]"},
		{"another session's turn is not judged", []Item{foreign}, nil, asked("L2", "t-foreign"), "members=[] needs=false hold=false claim= stale=[] superseded=[]"},
		{"an expired turn is not judged", []Item{expired}, nil, LiveLaunch{}, "members=[] needs=false hold=false claim= stale=[] superseded=[]"},
		{"a turn not yet due holds without asking", []Item{notDue}, nil, LiveLaunch{}, "members=[t-later] needs=false hold=true claim= stale=[] superseded=[]"},
		{"a turn in flight holds without asking", nil, []Item{turn("t-1", "L1", now)}, LiveLaunch{}, "members=[t-1] needs=false hold=true claim= stale=[] superseded=[]"},
		{"a turn due at the edge needs the launch", []Item{dueAtEdge}, nil, LiveLaunch{}, "members=[t-edge] needs=true hold=true claim= stale=[] superseded=[]"},
		{"an unfenced turn is the session's", []Item{unfenced}, nil, asked("L1", "t-unfenced"), "members=[t-unfenced] needs=false hold=true claim=t-unfenced stale=[] superseded=[]"},
		{"the live launch's turn is claimed", []Item{turn("t-1", "L1", now)}, nil, asked("L1", "t-1"), "members=[t-1] needs=false hold=true claim=t-1 stale=[] superseded=[]"},
		{"an unknown launch holds", []Item{turn("t-1", "L1", now)}, nil, LiveLaunch{Asked: true, Seen: seen("t-1")}, "members=[t-1] needs=false hold=true claim= stale=[] superseded=[]"},
		{"a turn first seen after the ask is held", []Item{turn("t-1", "L1", now), turn("t-2", "L2", now.Add(time.Second))}, nil, asked("L1", "t-1"), "members=[t-1 t-2] needs=false hold=true claim= stale=[] superseded=[]"},
		{"another launch's turn is stale, pending or in flight", []Item{turn("t-2", "L2", now.Add(time.Second)), turn("t-0", "L0", now.Add(-time.Second))}, []Item{turn("t-1", "L1", now)}, asked("L2", "t-0", "t-1", "t-2"), "members=[t-2 t-0 t-1] needs=false hold=true claim=t-2 stale=[t-0 t-1] superseded=[]"},
		{"only stale turns release the session", []Item{turn("t-1", "L1", now), reminder}, nil, asked("L2", "t-1"), "members=[t-1] needs=false hold=false claim= stale=[t-1] superseded=[]"},
		{"a stale turn not yet due is still dropped once one is due", []Item{notDue, turn("t-2", "L2", now.Add(time.Second))}, nil, asked("L2", "t-later", "t-2"), "members=[t-later t-2] needs=false hold=true claim=t-2 stale=[t-later] superseded=[]"},
		{"the newest turn of one launch wins", []Item{turn("t-1", "L1", now), turn("t-2", "L1", now.Add(time.Second))}, nil, asked("L1", "t-1", "t-2"), "members=[t-1 t-2] needs=false hold=true claim=t-2 stale=[] superseded=[t-1]"},
		{"a live turn in flight holds, an older pending one is superseded", []Item{turn("t-1", "L1", now)}, []Item{turn("t-2", "L1", now.Add(time.Second))}, asked("L1", "t-1", "t-2"), "members=[t-1 t-2] needs=false hold=true claim= stale=[] superseded=[t-1]"},
		{"a newer pending turn is not claimed past an older one in flight", []Item{turn("t-2", "L1", now.Add(time.Second))}, []Item{turn("t-1", "L1", now)}, asked("L1", "t-1", "t-2"), "members=[t-2 t-1] needs=false hold=true claim= stale=[] superseded=[]"},
	}
	for _, c := range cases {
		j := JudgeStartupTurns(session, c.pending, c.inFlight, now, c.launch)
		got := fmt.Sprintf("members=%v needs=%v hold=%v claim=%s stale=%v superseded=%v", j.Members, j.NeedsLaunch, j.Hold, j.Claim, j.Stale, j.Superseded)
		if got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
		if j.Drops() != (len(j.Stale)+len(j.Superseded) > 0) {
			t.Errorf("%s: Drops = %v", c.name, j.Drops())
		}
	}
}

func TestStartupBarrierHoldsBehindATurnNotYetClaimable(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	var barrier StartupBarrier
	notDue := startupTestTurn("t-later", "L1", "s-1", now)
	notDue.DeliverAfter = now.Add(time.Second)
	barrier.Add(notDue, false, now)
	leased := startupTestTurn("t-leased", "L1", "s-3", now)
	leased.Agent, leased.LeaseUntil = "gamma", now.Add(time.Minute)
	barrier.Add(leased, true, now)
	due := startupTestTurn("t-due", "L1", "s-2", now)
	due.Agent = "beta"
	barrier.Add(due, false, now)
	lapsed := startupTestTurn("t-lapsed", "L1", "s-4", now)
	lapsed.Agent, lapsed.LeaseUntil = "delta", now
	barrier.Add(lapsed, true, now)
	barrier.Add(Item{ID: "n-plain", Agent: "epsilon", SessionID: "s-5"}, false, now)
	cases := []struct {
		name string
		item Item
		want bool
	}{
		{"the turn itself", notDue, false},
		{"an item fenced to the session", Item{ID: "a", Agent: "other", SessionID: "s-1"}, true},
		{"an item on the turn's key", Item{ID: "b", Agent: "alpha"}, true},
		{"an item behind a leased turn", Item{ID: "c", Agent: "gamma"}, true},
		{"an item behind a due turn", Item{ID: "d", Agent: "beta", SessionID: "s-2"}, false},
		{"an item behind a lapsed lease", Item{ID: "e", Agent: "delta", SessionID: "s-4"}, false},
		{"an item behind a plain item", Item{ID: "f", Agent: "epsilon", SessionID: "s-5"}, false},
		{"an item with no session and no key", Item{ID: "g"}, false},
	}
	for _, c := range cases {
		if got := barrier.Holds(c.item); got != c.want {
			t.Errorf("%s: Holds = %v, want %v", c.name, got, c.want)
		}
	}
	if (StartupBarrier{}).Holds(Item{ID: "h", Agent: "alpha", SessionID: "s-1"}) {
		t.Errorf("an empty barrier holds an item")
	}
}
