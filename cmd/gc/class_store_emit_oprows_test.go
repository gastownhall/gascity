package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/eventfact"
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

// doorbellHarness is a controller cache over leaf with a registered lane host
// that runs no lane, and a CLI emitter over the same leaf whose rings reach
// the controller's doorbell handler when answer says so (n counts the rings
// from 1).
func doorbellHarness(t *testing.T, leaf beads.Store, answer func(n int) bool) (*opjournal.Store, *events.Fake, string) {
	t.Helper()
	ep := events.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cache, ok := wrapWithCachingStore(context.Background(), leaf, ep, true).(*beads.CachingStore)
	if !ok {
		t.Fatal("no cache")
	}
	f := newLaneFixtureOver(t, cache)
	cs := &controllerState{cityBeadStore: cache, cfg: f.cfg, sp: f.sp, cityPath: f.dir, beadsQuiescent: new(atomic.Bool)}
	h := newConversationLaneHost(cs, nil)
	h.state, h.reason = laneHostStateOff, "test: no lane runs"
	registerLaneHost(ctx, f.dir, h)
	n := 0
	recordJournalRings(t, func(cityPath, conv string, ids []string) {
		n++
		if answer(n) {
			laneSocketReply(cityPath, conv+":"+strings.Join(ids, ","))
		}
	})
	cli := &emittingClassStore{Store: leaf, cityPath: f.dir}
	st, err := opjournal.NewStore(beads.SessionStore{Store: cli}, clock.Real{}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	return st, ep, f.id
}

// eventsOf is the payloads of ep's events of type typ for subject.
func eventsOf(ep *events.Fake, typ, subject string) []json.RawMessage {
	var out []json.RawMessage
	for _, e := range ep.Events {
		if e.Type == typ && (subject == "" || e.Subject == subject) {
			out = append(out, e.Payload)
		}
	}
	return out
}

// A CLI op whose first ring the controller missed (a busy controller, a
// skipped leg) and that is then closed out of process is still announced:
// the doorbell's refresh announces the close of a row its cache never held,
// since the ring is the evidence the row changed just now.
func TestCLIJournalRowCloseAfterALostRingIsAnnounced(t *testing.T) {
	st, ep, conv := doorbellHarness(t, newLaneTestStore(), func(n int) bool { return n > 1 })
	op, _, err := st.Propose(opjournal.Request{
		Conversation: conv, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLegacyCLI,
		ClientKey: "m1", Producer: opjournal.Producer{Principal: "operator", Actor: "operator"}, Payload: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := eventsOf(ep, events.OpChanged, op.ID); len(got) != 0 {
		t.Fatalf("the lost ring announced %d op.changed", len(got))
	}
	if _, err := st.Close(op.ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded, Result: opjournal.Result{Via: "legacy.delivered"}}, "cli"); err != nil {
		t.Fatal(err)
	}
	got := eventsOf(ep, events.OpChanged, op.ID)
	if len(got) != 1 {
		t.Fatalf("op.changed = %d events, want the close once (events %v)", len(got), journalEventLog(ep))
	}
	var p api.OpChangedPayload
	if err := json.Unmarshal(got[0], &p); err != nil {
		t.Fatal(err)
	}
	if p.Op.Status != "closed" {
		t.Fatalf("op.changed status = %q, want closed", p.Op.Status)
	}
}

// A fact a CLI records is announced as fact.recorded through the doorbell,
// on an engine that creates the row closed (SQLite) as on one that creates
// it open and then closes it.
func TestCLIFactIsAnnouncedThroughTheDoorbell(t *testing.T) {
	for _, tc := range []struct {
		name string
		leaf func(t *testing.T) beads.Store
	}{
		{"sqlite", func(t *testing.T) beads.Store {
			leaf, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gc"))
			if err != nil {
				t.Fatal(err)
			}
			return leaf
		}},
		{"memstore", func(*testing.T) beads.Store { return newLaneTestStore() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ep, conv := doorbellHarness(t, tc.leaf(t), func(int) bool { return true })
			fact := opjournal.Fact{Kind: opjournal.FactTurnStarted, Conversation: conv, Generation: 2, Turn: "turn-1", Basis: opjournal.FactBasisRuntime, At: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)}
			if _, err := st.RecordFact(fact); err != nil {
				t.Fatalf("RecordFact: %v", err)
			}
			if got := eventsOf(ep, events.FactRecorded, ""); len(got) != 1 {
				t.Fatalf("fact.recorded = %d events, want one (events %v)", len(got), journalEventLog(ep))
			}
		})
	}
}

// One emission batch rings each conversation once, naming each of its rows
// once however often the batch wrote it.
func TestCLIEmitterRingsEachRowOncePerBatch(t *testing.T) {
	cityPath := t.TempDir()
	rings := recordJournalRings(t, nil)
	cli := &emittingClassStore{Store: &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}, cityPath: cityPath}
	row := func(id, conv string) classStoreEmission {
		return classStoreEmission{eventType: events.BeadUpdated, bead: beads.Bead{ID: id, Type: opjournal.BeadType, Labels: []string{opjournal.ConversationLabel(conv)}}}
	}
	cli.emit(row("gc-a", "gc-1"), row("gc-b", "gc-2"), row("gc-a", "gc-1"), row("gc-c", "gc-1"), row("gc-b", "gc-2"))
	if got := fmt.Sprint(rings()); got != "[gc-1:gc-a,gc-c gc-2:gc-b]" {
		t.Fatalf("rings = %s", got)
	}
	if j := readCityJournal(t, cityPath); len(j) != 0 {
		t.Fatalf("the CLI journaled %+v", j)
	}
}

// A journal row that names no conversation (a DES-BX event-fact or
// projector row) written through the CLI emitter journals no bead.* event
// and rings nothing: no lane reads it.
func TestCLIEmitterDropsJournalRowsOfNoConversation(t *testing.T) {
	cityPath := t.TempDir()
	rings := recordJournalRings(t, nil)
	cli := &emittingClassStore{Store: &beads.MemStore{IDPrefix: "gc"}, cityPath: cityPath}
	for _, label := range []string{eventfact.Label, "gc:event-projector"} {
		b, err := cli.Create(beads.Bead{Title: label, Type: opjournal.BeadType, Labels: []string{label}})
		if err != nil {
			t.Fatalf("create %s row: %v", label, err)
		}
		if err := cli.Close(b.ID); err != nil {
			t.Fatalf("close %s row: %v", label, err)
		}
	}
	if j := readCityJournal(t, cityPath); len(j) != 0 {
		t.Fatalf("the CLI journaled %+v", j)
	}
	if got := rings(); len(got) != 0 {
		t.Fatalf("rings = %v, want none", got)
	}
}

// ringJournalRows sends at most laneSocketMaxRows ids per verb, so a write
// of more rows than one verb takes rings in chunks the controller accepts.
func TestRingJournalRowsChunksToTheVerbCap(t *testing.T) {
	cityPath := shortSocketTempDir(t, "gc-ring-")
	var (
		mu    sync.Mutex
		lines []string
	)
	startFakeUnixSocket(t, controllerSocketPath(cityPath), func(line string) string {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
		return "ok\n"
	})
	t.Setenv(hookDeadlineEnv, "")
	ids := make([]string, laneSocketMaxRows+3)
	for i := range ids {
		ids[i] = fmt.Sprintf("gc-%d", i)
	}
	ringJournalRows(cityPath, "gc-1", ids)
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("verbs = %d, want 2: %v", len(lines), lines)
	}
	if want := laneSocketPrefix + "gc-1:" + strings.Join(ids[:laneSocketMaxRows], ","); lines[0] != want {
		t.Fatalf("first verb = %q, want %q", lines[0], want)
	}
	if want := laneSocketPrefix + "gc-1:" + strings.Join(ids[laneSocketMaxRows:], ","); lines[1] != want {
		t.Fatalf("second verb = %q, want %q", lines[1], want)
	}
	for _, line := range lines {
		if got := laneSocketReply(t.TempDir(), strings.TrimPrefix(line, laneSocketPrefix)); got != "no-host" {
			t.Fatalf("laneSocketReply(%q) = %q, want the ids accepted (no-host)", line, got)
		}
	}
}

// The doorbell refuses a verb naming more than laneSocketMaxRows rows, and
// accepts one naming exactly that many.
func TestLaneSocketReplyCapsRowIDs(t *testing.T) {
	ids := make([]string, laneSocketMaxRows+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("gc-%d", i)
	}
	city := t.TempDir()
	if got, want := laneSocketReply(city, "gc-1:"+strings.Join(ids, ",")), fmt.Sprintf("refused %d row ids, max %d", laneSocketMaxRows+1, laneSocketMaxRows); got != want {
		t.Fatalf("over the cap: %q, want %q", got, want)
	}
	if got := laneSocketReply(city, "gc-1:"+strings.Join(ids[:laneSocketMaxRows], ",")); got != "no-host" {
		t.Fatalf("at the cap: %q, want no-host", got)
	}
}
