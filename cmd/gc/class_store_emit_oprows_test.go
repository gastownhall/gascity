package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// recordJournalRings swaps the CLI emitter's controller doorbell for one
// that records each ring and then runs answer, when set.
func recordJournalRings(t *testing.T, answer func(cityPath, conv string, ids []string)) func() []string {
	t.Helper()
	var (
		mu    sync.Mutex
		rings []string
	)
	prev := classStoreJournalRing
	classStoreJournalRing = func(cityPath, conv string, ids []string) {
		mu.Lock()
		rings = append(rings, conv+":"+strings.Join(ids, ","))
		mu.Unlock()
		if answer != nil {
			answer(cityPath, conv, ids)
		}
	}
	t.Cleanup(func() { classStoreJournalRing = prev })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), rings...)
	}
}

// DES-O1 D18 for a one-shot CLI: its writes to journal rows (an op row, the
// runtime row) journal no bead.* event, and so no message text, and ring the
// controller's doorbell with the row instead. Every other bead still
// journals its bead.* events.
func TestCLIEmitterRingsForJournalRowsInsteadOfJournaling(t *testing.T) {
	cityPath := t.TempDir()
	rings := recordJournalRings(t, nil)
	cli := &emittingClassStore{Store: &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}, cityPath: cityPath}
	st, err := opjournal.NewStore(beads.SessionStore{Store: cli}, clock.Real{}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := st.Propose(opjournal.Request{
		Conversation: "gc-1", Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane,
		ClientKey: "m1", Producer: opjournal.Producer{Principal: "t3:env", Actor: "user"}, Payload: "secret text",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Transition(op.ID, opjournal.StatusSubmitted, opjournal.StatusAccepted, "lane"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Cancel(op.ID, "t3:env", ""); err != nil {
		t.Fatal(err)
	}
	rt, err := opjournal.EncodeRuntimeRow("gc", opjournal.RuntimeRow{Conversation: "gc-1", Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Create(rt); err != nil {
		t.Fatal(err)
	}
	task, err := cli.Create(beads.Bead{Title: "task"})
	if err != nil {
		t.Fatal(err)
	}
	journal := readCityJournal(t, cityPath)
	for _, e := range journal {
		if e.Subject != task.ID {
			t.Errorf("the CLI journaled %s %s for a journal row", e.Type, e.Subject)
		}
		if strings.Contains(string(e.Payload), "secret text") {
			t.Errorf("the CLI journaled the message text in %s", e.Type)
		}
	}
	if len(journal) != 1 || journal[0].Type != events.BeadCreated {
		t.Fatalf("journal = %+v, want the task's bead.created alone", journal)
	}
	got := rings()
	if len(got) < 4 {
		t.Fatalf("rings = %v, want one per journal-row write", got)
	}
	for _, r := range got[:3] {
		if r != "gc-1:"+op.ID {
			t.Fatalf("rings = %v, want gc-1:%s for each op write", got, op.ID)
		}
	}
	if !strings.HasPrefix(got[3], "gc-1:") || got[3] == "gc-1:"+op.ID {
		t.Fatalf("runtime row ring = %q", got[3])
	}
}

// The whole path: a one-shot CLI's writes to a lane op ring the
// controller's doorbell, whose refresh through the controller's cache makes
// the bridge record op.changed for each status, ending closed, and no
// bead.* for the row, whether or not a lane runs.
func TestCLIJournalRowWritesReachOpChangedThroughTheDoorbell(t *testing.T) {
	leaf := newLaneTestStore()
	ep := events.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Served from its synchronous prime, with no background refresh to race.
	cache, ok := wrapWithCachingStore(context.Background(), leaf, ep, true).(*beads.CachingStore)
	if !ok {
		t.Fatal("no cache")
	}
	f := newLaneFixtureOver(t, cache)
	cs := &controllerState{cityBeadStore: cache, cfg: f.cfg, sp: f.sp, cityPath: f.dir, beadsQuiescent: new(atomic.Bool)}
	h := newConversationLaneHost(cs, nil)
	h.state, h.reason = laneHostStateOff, "test: no lane runs"
	registerLaneHost(ctx, f.dir, h)
	var replies []string
	recordJournalRings(t, func(cityPath, conv string, ids []string) {
		replies = append(replies, laneSocketReply(cityPath, conv+":"+strings.Join(ids, ",")))
	})

	cli := &emittingClassStore{Store: leaf, cityPath: f.dir}
	st, err := opjournal.NewStore(beads.SessionStore{Store: cli}, clock.Real{}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := st.Propose(opjournal.Request{
		Conversation: f.id, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane,
		ClientKey: "m1", Producer: opjournal.Producer{Principal: "t3:env", Actor: "user"}, Payload: "secret text",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Transition(op.ID, opjournal.StatusSubmitted, opjournal.StatusAccepted, "lane"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Cancel(op.ID, "t3:env", ""); err != nil {
		t.Fatal(err)
	}
	var statuses []string
	for _, e := range ep.Events {
		if e.Subject != op.ID {
			continue
		}
		if e.Type != events.OpChanged {
			t.Fatalf("the op row recorded %s (events %v)", e.Type, journalEventLog(ep))
		}
		if strings.Contains(string(e.Payload), "secret text") {
			t.Fatalf("op.changed carries the text: %s", e.Payload)
		}
		var p api.OpChangedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, p.Op.Status)
	}
	if strings.Join(statuses, ",") != "submitted,accepted,closed" {
		t.Fatalf("op.changed statuses = %v (replies %v, events %v)", statuses, replies, journalEventLog(ep))
	}
	if len(readCityJournal(t, f.dir)) != 0 {
		t.Fatalf("the CLI journaled %+v", readCityJournal(t, f.dir))
	}
}
