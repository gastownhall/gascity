package storebinding_test

// The wrapper stack's exit evidence: the UNCHANGED class corpus, through the
// production wrappers.
//
// The class corpus is an exported package precisely so later work can bind
// production wrappers to it without editing a single assertion. This file does
// that. Nothing in internal/storebinding/storebindingtest is
// touched: the same suites that prove the bare adapters conform are handed
// front doors that have the cache, the event emission and the maintenance
// discipline in front of them. If any wrapper changes observable behavior —
// a stale cached bead, a re-wrapped sentinel error, a lost capability — the
// corpus fails, and that is the entire design.
//
// It runs over BOTH substitution legs, because a wrapper defect that only shows
// up over a real engine (a mutation the cache did not fence, a transaction
// rollback the cache resurrected) is invisible over an in-memory reference:
//
//   - the canonical Beads adapters over an in-memory store, and
//   - the canonical Beads adapters over a real SQLite engine.
//
// It is an EXTERNAL test package for the same reason its neighbor is: the
// corpus imports storebinding, so an in-package test importing it would close a
// cycle.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storebinding/storebindingtest"
)

// wrappedBindingName is the binding name every wrapper in this file reports.
const wrappedBindingName storebinding.BindingName = "wrapped"

// wrapAllClasses puts the production stack in front of every class front door
// of one already-projected binding. Cache reads are ON: a wrapper stack tested
// with its cache disabled proves nothing about the cache.
func wrapAllClasses(tb storebindingtest.TB, adapters storebinding.BeadsAdapters, capability storebinding.ClassCapability, observer storebinding.ClassObserver) storebinding.BeadsAdapters {
	tb.Helper()
	wrapping := storebinding.ClassWrapping{
		Binding:    wrappedBindingName,
		Capability: capability,
		Observer:   observer,
		CacheReads: true,
	}
	graph, err := storebinding.WrapGraph(adapters.Graph, wrapping)
	if err != nil {
		tb.Fatalf("wrapping the Graph front door: %v", err)
	}
	sessions, err := storebinding.WrapSessions(adapters.Sessions, wrapping)
	if err != nil {
		tb.Fatalf("wrapping the Sessions front door: %v", err)
	}
	ordersStore, err := storebinding.WrapOrders(adapters.Orders, wrapping)
	if err != nil {
		tb.Fatalf("wrapping the Orders front door: %v", err)
	}
	nudges, err := storebinding.WrapNudges(adapters.Nudges, wrapping)
	if err != nil {
		tb.Fatalf("wrapping the Nudges front doors: %v", err)
	}
	messaging, err := storebinding.WrapMessaging(adapters.Messaging, wrapping)
	if err != nil {
		tb.Fatalf("wrapping the Messaging front doors: %v", err)
	}
	adapters.Graph = graph
	adapters.Sessions = sessions
	adapters.Orders = ordersStore
	adapters.Nudges = nudges
	adapters.Messaging = messaging
	return adapters
}

// wrappedLeg is one substitution leg: a name, a fresh binding factory, and the
// capabilities that binding honestly declares.
type wrappedLeg struct {
	name       string
	capability storebinding.ClassCapability
	open       func(storebindingtest.TB) storebinding.BeadsAdapters
}

func wrappedLegs() []wrappedLeg {
	return []wrappedLeg{
		{
			name:       "beads-over-memory",
			capability: storebindingtest.ReferenceCapability,
			open:       storebindingtest.ReferenceAdapters,
		},
		{
			name:       "beads-over-sqlite",
			capability: beadsOverSQLiteCapability,
			open:       openBeadsOverSQLite,
		},
	}
}

// wrapped returns the wrapped front doors of a fresh binding on one leg.
func (l wrappedLeg) wrapped(tb storebindingtest.TB, observer storebinding.ClassObserver) storebinding.BeadsAdapters {
	tb.Helper()
	return wrapAllClasses(tb, l.open(tb), l.capability, observer)
}

// TestWrappedClassFrontsRunTheUnchangedCorpus is the slice's exit evidence.
func TestWrappedClassFrontsRunTheUnchangedCorpus(t *testing.T) {
	for _, leg := range wrappedLegs() {
		t.Run(leg.name, func(t *testing.T) {
			t.Run("graph", func(t *testing.T) {
				storebindingtest.RunGraphStoreTests(storebindingtest.Wrap(t), storebindingtest.GraphSuite{
					NewStore: func(tb storebindingtest.TB) storebinding.GraphStore {
						return leg.wrapped(tb, nil).Graph
					},
					Capability: leg.capability,
				})
			})
			t.Run("sessions", func(t *testing.T) {
				storebindingtest.RunSessionsStoreTests(storebindingtest.Wrap(t), storebindingtest.SessionsSuite{
					NewStore: func(tb storebindingtest.TB) storebinding.SessionsStore {
						return leg.wrapped(tb, nil).Sessions
					},
					Capability: leg.capability,
				})
			})
			t.Run("orders", func(t *testing.T) {
				storebindingtest.RunOrdersStoreTests(storebindingtest.Wrap(t), storebindingtest.OrdersSuite{
					NewStore: func(tb storebindingtest.TB) storebinding.OrdersStore {
						return leg.wrapped(tb, nil).Orders
					},
					Capability: leg.capability,
				})
			})
			t.Run("nudges", func(t *testing.T) {
				storebindingtest.RunNudgeFrontDoorTests(storebindingtest.Wrap(t), storebindingtest.NudgesSuite{
					NewFrontDoors: func(tb storebindingtest.TB) storebinding.NudgeFrontDoors {
						return leg.wrapped(tb, nil).Nudges
					},
					Capability: leg.capability,
				})
			})
			t.Run("messaging", func(t *testing.T) {
				storebindingtest.RunMessagingFrontDoorTests(storebindingtest.Wrap(t), storebindingtest.MessagingSuite{
					NewFrontDoors: func(tb storebindingtest.TB) storebinding.MessagingFrontDoors {
						return leg.wrapped(tb, nil).Messaging
					},
					Capability: leg.capability,
				})
			})
			t.Run("mail-provider", func(t *testing.T) {
				storebindingtest.RunMailProviderTests(t, storebindingtest.MessagingSuite{
					NewFrontDoors: func(tb storebindingtest.TB) storebinding.MessagingFrontDoors {
						return leg.wrapped(tb, nil).Messaging
					},
					Capability: leg.capability,
				})
			})
		})
	}
}

// recordingObserver collects the event stream a wrapper emits.
type recordingObserver struct {
	events []storebinding.ClassEvent
}

func (r *recordingObserver) ObserveClassEvent(event storebinding.ClassEvent) {
	r.events = append(r.events, event)
}

func (r *recordingObserver) stream() []string {
	out := make([]string, len(r.events))
	for index, event := range r.events {
		out[index] = event.String()
	}
	return out
}

// classScript is one deterministic sequence of contract calls. It touches every
// wrapped class and deliberately includes a failing call and a losing
// compare-and-swap, because "the happy path emits the same events" is the easy
// half of parity.
func classScript(tb storebindingtest.TB, adapters storebinding.BeadsAdapters) {
	tb.Helper()
	graph := adapters.Graph
	created, err := graph.Create(beads.Bead{Title: "parity", Type: "task", Status: "open"})
	if err != nil {
		tb.Fatalf("Create: %v", err)
	}
	if _, err := graph.Get(created.ID); err != nil {
		tb.Fatalf("Get: %v", err)
	}
	// Second Get is the cache hit; a leg that does not report one has a cache
	// that is not running.
	if _, err := graph.Get(created.ID); err != nil {
		tb.Fatalf("cached Get: %v", err)
	}
	if _, err := graph.Get("gcg-does-not-exist"); err == nil {
		tb.Fatalf("Get of an absent bead unexpectedly succeeded")
	}
	// A compare-and-swap that wins and then one that loses. The CAS is on
	// metadata rather than on the assignee because the in-memory reference
	// declares no two-argument claim, and a parity script that ran a
	// capability-guarded operation would compare two different scripts.
	if swapped, err := graph.CompareAndSetMetadataKey(created.ID, "owner", "", "worker-a"); err != nil || !swapped {
		tb.Fatalf("CompareAndSetMetadataKey = (%v, %v), want the swap won with no error", swapped, err)
	}
	if swapped, err := graph.CompareAndSetMetadataKey(created.ID, "owner", "", "worker-b"); err != nil || swapped {
		tb.Fatalf("second CompareAndSetMetadataKey = (%v, %v), want a lost race with no error", swapped, err)
	}
	if err := graph.Close(created.ID); err != nil {
		tb.Fatalf("Close: %v", err)
	}
	if err := graph.Ping(); err != nil {
		tb.Fatalf("Ping: %v", err)
	}

	if _, err := adapters.Orders.CreateRun("parity-order", orders.RunOpts{}); err != nil {
		tb.Fatalf("CreateRun: %v", err)
	}
	if _, err := adapters.Orders.Get("gco-does-not-exist"); err == nil {
		tb.Fatalf("Get of an absent order run unexpectedly succeeded")
	}

	if _, err := adapters.Messaging.Mail.Send("parity-from", "parity-to", "subject", "body"); err != nil {
		tb.Fatalf("Send: %v", err)
	}
	if _, err := adapters.Messaging.Mail.Get("gcm-does-not-exist"); err == nil {
		tb.Fatalf("Get of an absent message unexpectedly succeeded")
	}
	if _, err := adapters.Nudges.Queue.Snapshot(); err != nil {
		tb.Fatalf("Snapshot: %v", err)
	}

	sessionsScript(tb, adapters.Sessions)
	nudgesScript(tb, adapters.Nudges)
	extmsgScript(tb, adapters.Messaging)
}

// sessionsScript is the Sessions leg of the parity script. The class is here
// for the same reason the others are: an event stream that covers five of six
// wrapped classes proves parity for five of six.
func sessionsScript(tb storebindingtest.TB, sessions storebinding.SessionsStore) {
	tb.Helper()
	id, err := sessions.CreateSession(session.CreateSpec{Title: "parity", AgentName: "parity"})
	if err != nil {
		tb.Fatalf("CreateSession: %v", err)
	}
	if _, err := sessions.Get(id); err != nil {
		tb.Fatalf("Get: %v", err)
	}
	if _, err := sessions.Get("gcs-does-not-exist"); err == nil {
		tb.Fatalf("Get of an absent session unexpectedly succeeded")
	}
	if err := sessions.ApplyPatch(id, session.MetadataPatch{"parity": "yes"}); err != nil {
		tb.Fatalf("ApplyPatch: %v", err)
	}
	if err := sessions.SetState(id, session.StateActive, "parity"); err != nil {
		tb.Fatalf("SetState: %v", err)
	}
	// The close race, both ways: exactly one caller wins and the loser is
	// healthy. A provider that reported the loss as an error would separate
	// the streams here.
	at := time.Unix(1750000000, 0).UTC()
	if won, err := sessions.Close(id, "parity", at); err != nil || !won {
		tb.Fatalf("Close = (%v, %v), want the race won with no error", won, err)
	}
	if won, err := sessions.Close(id, "parity", at); err != nil || won {
		tb.Fatalf("second Close = (%v, %v), want the race lost with no error", won, err)
	}
}

// nudgesScript is the Nudges leg: the queue and its shadow projection are two
// contracts of one class, and both are wrapped, so both are scripted.
func nudgesScript(tb storebindingtest.TB, nudges storebinding.NudgeFrontDoors) {
	tb.Helper()
	at := time.Unix(1750000000, 0).UTC()
	item := nudgequeue.Item{
		ID:           "parity-nudge",
		Agent:        storebindingtest.ConformanceNudgeAgent,
		Source:       "parity",
		Message:      "check your hook",
		CreatedAt:    at,
		DeliverAfter: at,
		ExpiresAt:    at.Add(24 * time.Hour),
	}
	if err := nudges.Queue.Enqueue(item); err != nil {
		tb.Fatalf("Enqueue: %v", err)
	}
	claimed, err := nudges.Queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{storebindingtest.ConformanceNudgeAgent}}, at)
	if err != nil {
		tb.Fatalf("ClaimDue: %v", err)
	}
	if len(claimed) != 1 {
		tb.Fatalf("ClaimDue claimed %d items, want exactly the one enqueued", len(claimed))
	}
	if err := nudges.Queue.Ack([]string{item.ID}, "", "delivered", "parity"); err != nil {
		tb.Fatalf("Ack: %v", err)
	}
	// A claim with nothing due: an empty batch is a legitimate outcome, not a
	// failure, and it is the outcome a wrapper is most likely to misclassify.
	if _, err := nudges.Queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{storebindingtest.ConformanceNudgeAgent}}, at); err != nil {
		tb.Fatalf("ClaimDue with nothing due: %v", err)
	}

	shadow := item
	shadow.ID = "parity-shadow"
	if _, _, err := nudges.Shadows.Save(shadow); err != nil {
		tb.Fatalf("Save: %v", err)
	}
	if err := nudges.Shadows.Terminalize(shadow, "delivered", "parity", "", at); err != nil {
		tb.Fatalf("Terminalize: %v", err)
	}
	// The retention sweep is maintenance, and it takes the SHADOW BEAD id that
	// Save minted — not the durable nudge id and not the agent.
	stale := item
	stale.ID = "parity-stale"
	beadID, _, err := nudges.Shadows.Save(stale)
	if err != nil {
		tb.Fatalf("Save of the sweep target: %v", err)
	}
	if err := nudges.Shadows.SweepStale(beadID, "parity sweep", at); err != nil {
		tb.Fatalf("SweepStale: %v", err)
	}
}

// extmsgScript is the external-messaging leg. Mail is only one of the five
// Messaging front doors; the other four are wrapped independently and would
// otherwise never appear in a parity stream at all.
func extmsgScript(tb storebindingtest.TB, messaging storebinding.MessagingFrontDoors) {
	tb.Helper()
	ctx := context.Background()
	at := time.Unix(1750000000, 0).UTC()
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "parity", Provider: "parity", AccountID: "parity-account"}
	conversation := extmsg.ConversationRef{
		ScopeID:        "parity-scope",
		Provider:       "parity",
		AccountID:      "parity-account",
		ConversationID: "parity-conversation",
		Kind:           extmsg.ConversationDM,
	}
	if _, err := messaging.Bindings.Bind(ctx, caller, extmsg.BindInput{
		Conversation: conversation,
		SessionID:    "parity-session",
		Now:          at,
	}); err != nil {
		tb.Fatalf("Bind: %v", err)
	}
	if err := messaging.DeliveryContexts.Record(ctx, caller, extmsg.DeliveryContextRecord{
		SessionID:         "parity-session",
		Conversation:      conversation,
		BindingGeneration: 1,
		LastPublishedAt:   at,
		LastMessageID:     "parity-message",
	}); err != nil {
		tb.Fatalf("Record: %v", err)
	}
	if _, err := messaging.Groups.EnsureGroup(ctx, caller, extmsg.EnsureGroupInput{
		RootConversation: conversation,
		Mode:             extmsg.GroupModeLauncher,
		DefaultHandle:    "parity",
	}); err != nil {
		tb.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := messaging.Transcripts.Append(ctx, extmsg.AppendTranscriptInput{
		Caller:            caller,
		Conversation:      conversation,
		Kind:              extmsg.TranscriptMessageInbound,
		Provenance:        extmsg.TranscriptProvenanceLive,
		ProviderMessageID: "parity-provider-message",
		Actor:             extmsg.ExternalActor{ID: "parity-actor", DisplayName: "Parity"},
		Text:              "parity",
		CreatedAt:         at,
	}); err != nil {
		tb.Fatalf("Append: %v", err)
	}
	if _, err := messaging.Bindings.Unbind(ctx, caller, extmsg.UnbindInput{
		Conversation: &conversation,
		SessionID:    "parity-session",
		Now:          at,
	}); err != nil {
		tb.Fatalf("Unbind: %v", err)
	}
}

// TestWrappedClassEventsAreProviderNeutral is the typed-event-parity evidence.
// The same script over two different bindings must produce the SAME event
// stream: same classes, same operations, same kinds, same outcomes, same order.
// A wrapper that leaked anything provider-shaped into an event — a different
// operation name, a retry the other leg does not do, an error classified
// differently — separates the two streams immediately.
func TestWrappedClassEventsAreProviderNeutral(t *testing.T) {
	streams := map[string][]string{}
	for _, leg := range wrappedLegs() {
		observer := &recordingObserver{}
		runner := storebindingtest.Wrap(t)
		classScript(runner, leg.wrapped(runner, observer))
		if len(observer.events) == 0 {
			t.Fatalf("%s emitted no class events; the parity comparison below would be vacuous", leg.name)
		}
		for _, event := range observer.events {
			if err := event.Validate(); err != nil {
				t.Fatalf("%s emitted an event outside the closed contract: %v", leg.name, err)
			}
			if event.Binding != wrappedBindingName {
				t.Fatalf("%s emitted event %s for binding %q, want %q", leg.name, event, event.Binding, wrappedBindingName)
			}
		}
		streams[leg.name] = observer.stream()
	}

	legs := wrappedLegs()
	reference := streams[legs[0].name]
	for _, leg := range legs[1:] {
		got := streams[leg.name]
		if len(got) != len(reference) {
			t.Fatalf("%s emitted %d events, %s emitted %d; the same script must produce the same stream:\n%s\nvs\n%s",
				leg.name, len(got), legs[0].name, len(reference), formatStream(got), formatStream(reference))
		}
		for index := range got {
			if got[index] != reference[index] {
				t.Errorf("event %d: %s emitted %q, %s emitted %q", index, leg.name, got[index], legs[0].name, reference[index])
			}
		}
	}

	// A stream that never reports a cache hit, a lost claim or a failure is a
	// stream that proves parity only for the trivial cases.
	wantPresent := []string{
		"graph/wrapped cache Get hit",
		"graph/wrapped claim CompareAndSetMetadataKey conflict",
		"graph/wrapped read Get failed",
		"graph/wrapped probe Ping ok",
		"orders/wrapped write CreateRun ok",
		"orders/wrapped read Get failed",
		"messaging/wrapped write Send ok",
		"nudges/wrapped read Snapshot ok",
		// One entry per wrapped front door, so a class that stopped emitting
		// cannot hide behind the classes that still do. The five extmsg
		// services and the two nudge fronts are separate wrappers reporting
		// one class each, which is precisely how a silent one goes unnoticed.
		"sessions/wrapped write CreateSession ok",
		"sessions/wrapped read Get failed",
		"sessions/wrapped claim Close ok",
		"sessions/wrapped claim Close conflict",
		"nudges/wrapped write Enqueue ok",
		"nudges/wrapped claim ClaimDue ok",
		"nudges/wrapped write Save ok",
		"nudges/wrapped write Terminalize ok",
		"nudges/wrapped maintenance SweepStale ok",
		"messaging/wrapped write Bind ok",
		"messaging/wrapped write Unbind ok",
		"messaging/wrapped write Record ok",
		"messaging/wrapped write EnsureGroup ok",
		"messaging/wrapped write Append ok",
	}
	for _, want := range wantPresent {
		if !containsString(reference, want) {
			t.Errorf("the parity stream never contains %q; it is:\n%s", want, formatStream(reference))
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func formatStream(stream []string) string {
	out := ""
	for index, value := range stream {
		out += fmt.Sprintf("  %2d %s\n", index, value)
	}
	return out
}

// TestWrappedGraphServesCachedReadsFromMemory proves the cache is real rather
// than decorative: with the store's Get made unusable after the first read, a
// cached lookup still answers, and an uncached one does not.
//
// It reads through a path the wrapper never writes through — the underlying
// beads store — to confirm the cache never invented the value.
func TestWrappedGraphServesCachedReadsFromMemory(t *testing.T) {
	store := beads.NewMemStore()
	adapters, err := storebinding.NewBeadsAdapters(store,
		storebinding.BeadsAdapterIdentity{OpenerID: "wrappers", ComponentID: "cache", PhysicalID: "memory"},
		storebindingtest.NewMemoryNudgeQueue())
	if err != nil {
		t.Fatalf("projecting the reference binding: %v", err)
	}
	counter := &countingGraphStore{GraphStore: adapters.Graph}
	wrapped, err := storebinding.WrapGraph(counter, storebinding.ClassWrapping{
		Binding:    wrappedBindingName,
		Capability: storebindingtest.ReferenceCapability,
		CacheReads: true,
	})
	if err != nil {
		t.Fatalf("wrapping the Graph front door: %v", err)
	}

	created, err := wrapped.Create(beads.Bead{Title: "cached", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		got, err := wrapped.Get(created.ID)
		if err != nil {
			t.Fatalf("Get attempt %d: %v", attempt, err)
		}
		if got.Title != "cached" {
			t.Fatalf("Get attempt %d returned title %q, want %q", attempt, got.Title, "cached")
		}
	}
	if counter.gets != 1 {
		t.Fatalf("the wrapped store performed %d inner Get calls for 3 reads, want exactly 1; the cache is not serving", counter.gets)
	}

	// The persisted value is what the cache claimed, read through the raw store
	// the wrapper never writes through.
	persisted, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("reading the persisted bead directly: %v", err)
	}
	if persisted.Title != "cached" {
		t.Fatalf("the persisted bead has title %q, want %q; the cache answered from something that was never stored", persisted.Title, "cached")
	}

	// A mutation through the wrapper must discard the cached value, and the
	// next read must go back to the store.
	if err := wrapped.SetMetadata(created.ID, "phase", "wrapped"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	refreshed, err := wrapped.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after mutation: %v", err)
	}
	if refreshed.Metadata["phase"] != "wrapped" {
		t.Fatalf("Get after mutation returned metadata %v, want phase=wrapped; the cache served a stale bead", refreshed.Metadata)
	}
	if counter.gets != 2 {
		t.Fatalf("the wrapped store performed %d inner Get calls, want 2; the mutation did not invalidate the cache", counter.gets)
	}
}

// TestWrappedGraphDoesNotShareCachedMemory proves the cache hands out detached
// beads. A caller that mutates what it was given must not be able to poison the
// next reader — the failure mode of every cache that returns its own map.
func TestWrappedGraphDoesNotShareCachedMemory(t *testing.T) {
	adapters := storebindingtest.ReferenceAdapters(storebindingtest.Wrap(t))
	wrapped, err := storebinding.WrapGraph(adapters.Graph, storebinding.ClassWrapping{
		Binding:    wrappedBindingName,
		Capability: storebindingtest.ReferenceCapability,
		CacheReads: true,
	})
	if err != nil {
		t.Fatalf("wrapping the Graph front door: %v", err)
	}
	created, err := wrapped.Create(beads.Bead{
		Title:    "detached",
		Type:     "task",
		Status:   "open",
		Labels:   []string{"storage"},
		Metadata: beads.StringMap{"plan_key": "wrapped"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first, err := wrapped.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(first.Labels) == 0 || first.Metadata == nil {
		t.Fatalf("the fixture lost its labels or metadata (%+v); this test would prove nothing", first)
	}
	first.Labels[0] = "poisoned"
	first.Metadata["plan_key"] = "poisoned"

	second, err := wrapped.Get(created.ID)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if second.Labels[0] != "storage" {
		t.Errorf("the cached bead's label was poisoned by a caller: got %q, want %q", second.Labels[0], "storage")
	}
	if second.Metadata["plan_key"] != "wrapped" {
		t.Errorf("the cached bead's metadata was poisoned by a caller: got %q, want %q", second.Metadata["plan_key"], "wrapped")
	}
}

// countingGraphStore counts inner reads so a test can tell a cache hit from a
// pass-through.
type countingGraphStore struct {
	storebinding.GraphStore
	gets int
}

func (c *countingGraphStore) Get(id string) (beads.Bead, error) {
	c.gets++
	return c.GraphStore.Get(id)
}

// TestWrappedFrontDoorsRefuseAnUnavailableClass pins the construction gate: a
// class the binding declares unavailable, or a missing front door, fails at
// Wrap rather than at the first call in production.
func TestWrappedFrontDoorsRefuseAnUnavailableClass(t *testing.T) {
	adapters := storebindingtest.ReferenceAdapters(storebindingtest.Wrap(t))
	unavailable := storebinding.ClassWrapping{Binding: wrappedBindingName}
	if _, err := storebinding.WrapGraph(adapters.Graph, unavailable); !errors.Is(err, storebinding.ErrMissingCapability) {
		t.Errorf("WrapGraph over an unavailable class = %v, want ErrMissingCapability", err)
	}
	if _, err := storebinding.WrapSessions(adapters.Sessions, unavailable); !errors.Is(err, storebinding.ErrMissingCapability) {
		t.Errorf("WrapSessions over an unavailable class = %v, want ErrMissingCapability", err)
	}
	available := storebinding.ClassWrapping{Binding: wrappedBindingName, Capability: storebindingtest.ReferenceCapability}
	if _, err := storebinding.WrapGraph(nil, available); !errors.Is(err, storebinding.ErrInvalidClassWrapping) {
		t.Errorf("WrapGraph over a nil front door = %v, want ErrInvalidClassWrapping", err)
	}
	if _, err := storebinding.WrapNudges(storebinding.NudgeFrontDoors{}, available); !errors.Is(err, storebinding.ErrInvalidClassWrapping) {
		t.Errorf("WrapNudges over an empty front-door set = %v, want ErrInvalidClassWrapping", err)
	}
	if _, err := storebinding.WrapMessaging(storebinding.MessagingFrontDoors{}, available); !errors.Is(err, storebinding.ErrInvalidClassWrapping) {
		t.Errorf("WrapMessaging over an empty front-door set = %v, want ErrInvalidClassWrapping", err)
	}
	unnamed := storebinding.ClassWrapping{Capability: storebindingtest.ReferenceCapability}
	if _, err := storebinding.WrapGraph(adapters.Graph, unnamed); !errors.Is(err, storebinding.ErrInvalidClassWrapping) {
		t.Errorf("WrapGraph without a binding name = %v, want ErrInvalidClassWrapping", err)
	}
}

// TestWrappedGraphPassesSentinelErrorsThrough proves the wrapper does not
// re-wrap what the binding returned. Callers across the codebase match
// beads.ErrNotFound; a wrapper that decorated it would break them silently, and
// a wrapper that replaced the message would break every log-scraping runbook.
func TestWrappedGraphPassesSentinelErrorsThrough(t *testing.T) {
	adapters := storebindingtest.ReferenceAdapters(storebindingtest.Wrap(t))
	wrapped, err := storebinding.WrapGraph(adapters.Graph, storebinding.ClassWrapping{
		Binding:    wrappedBindingName,
		Capability: storebindingtest.ReferenceCapability,
		CacheReads: true,
	})
	if err != nil {
		t.Fatalf("wrapping the Graph front door: %v", err)
	}
	_, bare := adapters.Graph.Get("gcg-absent")
	_, through := wrapped.Get("gcg-absent")
	if !errors.Is(through, beads.ErrNotFound) {
		t.Fatalf("the wrapped Get returned %v, want an error matching beads.ErrNotFound", through)
	}
	if bare.Error() != through.Error() {
		t.Fatalf("the wrapped Get returned %q, want the binding's own %q verbatim", through.Error(), bare.Error())
	}
}

// TestWrappedGraphNeverFallsBackOnFailure pins the no-fallback rule: a failing
// binding fails once, at the call that failed. No retry, no second store, no
// provider re-resolution.
func TestWrappedGraphNeverFallsBackOnFailure(t *testing.T) {
	failure := errors.New("binding is unavailable")
	failing := &failingGraphStore{
		GraphStore: storebindingtest.ReferenceAdapters(storebindingtest.Wrap(t)).Graph,
		err:        failure,
	}
	wrapped, err := storebinding.WrapGraph(failing, storebinding.ClassWrapping{
		Binding:    wrappedBindingName,
		Capability: storebindingtest.ReferenceCapability,
		CacheReads: true,
	})
	if err != nil {
		t.Fatalf("wrapping the Graph front door: %v", err)
	}
	if _, err := wrapped.Get("gcg-1"); !errors.Is(err, failure) {
		t.Fatalf("Get through a failing binding = %v, want the binding's own error", err)
	}
	if failing.calls != 1 {
		t.Fatalf("a failing Get was attempted %d times, want exactly 1; the wrapper retried or fell back", failing.calls)
	}
	if _, err := wrapped.Create(beads.Bead{Title: "x"}); !errors.Is(err, failure) {
		t.Fatalf("Create through a failing binding = %v, want the binding's own error", err)
	}
	if failing.calls != 2 {
		t.Fatalf("the wrapper made %d inner calls after two operations, want 2", failing.calls)
	}
}

// TestWrappedGraphInvalidatesOnAFailedWrite pins the cache's most dangerous
// assumption. A write that returns an error may still have applied — a partial
// batch, a timeout after commit — so a cache that trusts the error and keeps
// its entry serves the pre-write value forever.
func TestWrappedGraphInvalidatesOnAFailedWrite(t *testing.T) {
	adapters := storebindingtest.ReferenceAdapters(storebindingtest.Wrap(t))
	applying := &applyThenFailGraphStore{GraphStore: adapters.Graph}
	wrapped, err := storebinding.WrapGraph(applying, storebinding.ClassWrapping{
		Binding:    wrappedBindingName,
		Capability: storebindingtest.ReferenceCapability,
		CacheReads: true,
	})
	if err != nil {
		t.Fatalf("wrapping the Graph front door: %v", err)
	}
	created, err := adapters.Graph.Create(beads.Bead{Title: "applied", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := wrapped.Get(created.ID); err != nil {
		t.Fatalf("priming Get: %v", err)
	}
	applying.fail = errors.New("write reported a failure after applying")
	if err := wrapped.SetMetadata(created.ID, "phase", "wrapped"); !errors.Is(err, applying.fail) {
		t.Fatalf("SetMetadata = %v, want the injected failure", err)
	}
	got, err := wrapped.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after the failed write: %v", err)
	}
	if got.Metadata["phase"] != "wrapped" {
		t.Fatalf("Get after a failed-but-applied write returned %v, want phase=wrapped; the cache kept a value the store no longer holds", got.Metadata)
	}
}

// failingGraphStore fails every operation and counts attempts.
type failingGraphStore struct {
	storebinding.GraphStore
	err   error
	calls int
}

func (f *failingGraphStore) Get(string) (beads.Bead, error) {
	f.calls++
	return beads.Bead{}, f.err
}

func (f *failingGraphStore) Create(beads.Bead) (beads.Bead, error) {
	f.calls++
	return beads.Bead{}, f.err
}

// applyThenFailGraphStore applies a metadata write and then reports it failed.
type applyThenFailGraphStore struct {
	storebinding.GraphStore
	fail error
}

func (a *applyThenFailGraphStore) SetMetadata(id, key, value string) error {
	if err := a.GraphStore.SetMetadata(id, key, value); err != nil {
		return err
	}
	if a.fail != nil {
		return a.fail
	}
	return nil
}

// TestWrappedGraphCacheSurvivesConcurrentMutation is the generation-fence
// proof. A read that started before a mutation must not install its result
// afterwards; the class corpus is sequential and cannot see this, so it is
// pinned here and run under -race.
func TestWrappedGraphCacheSurvivesConcurrentMutation(t *testing.T) {
	adapters := storebindingtest.ReferenceAdapters(storebindingtest.Wrap(t))
	wrapped, err := storebinding.WrapGraph(adapters.Graph, storebinding.ClassWrapping{
		Binding:    wrappedBindingName,
		Capability: storebindingtest.ReferenceCapability,
		CacheReads: true,
	})
	if err != nil {
		t.Fatalf("wrapping the Graph front door: %v", err)
	}
	created, err := wrapped.Create(beads.Bead{Title: "racing", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const rounds = 200
	done := make(chan error, 2)
	go func() {
		for round := 0; round < rounds; round++ {
			if err := wrapped.SetMetadata(created.ID, "round", fmt.Sprintf("%d", round)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	go func() {
		for round := 0; round < rounds; round++ {
			if _, err := wrapped.Get(created.ID); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for index := 0; index < 2; index++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent round: %v", err)
		}
	}

	final, err := wrapped.Get(created.ID)
	if err != nil {
		t.Fatalf("final Get: %v", err)
	}
	persisted, err := adapters.Graph.Get(created.ID)
	if err != nil {
		t.Fatalf("reading the persisted bead through the unwrapped front door: %v", err)
	}
	if final.Metadata["round"] != persisted.Metadata["round"] {
		t.Fatalf("after the race the cache serves round=%q while the store holds round=%q", final.Metadata["round"], persisted.Metadata["round"])
	}
}

// TestWrappedOrdersReportsMaintenanceSeparately pins the maintenance signal:
// a retention sweep is not an ordinary write, and an operator watching for
// maintenance failures must be able to find them.
func TestWrappedOrdersReportsMaintenanceSeparately(t *testing.T) {
	observer := &recordingObserver{}
	adapters := storebindingtest.ReferenceAdapters(storebindingtest.Wrap(t))
	wrapped, err := storebinding.WrapOrders(adapters.Orders, storebinding.ClassWrapping{
		Binding:    wrappedBindingName,
		Capability: storebindingtest.ReferenceCapability,
		Observer:   observer,
	})
	if err != nil {
		t.Fatalf("wrapping the Orders front door: %v", err)
	}
	run, err := wrapped.CreateRun("maintenance-order", orders.RunOpts{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := wrapped.CloseRuns(context.Background(), []string{run.ID}, "retention"); err != nil {
		t.Fatalf("CloseRuns: %v", err)
	}
	want := "orders/wrapped maintenance CloseRuns ok"
	if !containsString(observer.stream(), want) {
		t.Fatalf("the observed stream never reports %q; it is:\n%s", want, formatStream(observer.stream()))
	}
}

// TestWrappedSessionsReportsALostCloseAsAConflict pins the one place a wrapper
// has to understand a contract rather than merely forward it: exactly one
// caller wins a session close, and the loser is healthy.
func TestWrappedSessionsReportsALostCloseAsAConflict(t *testing.T) {
	observer := &recordingObserver{}
	adapters := storebindingtest.ReferenceAdapters(storebindingtest.Wrap(t))
	wrapped, err := storebinding.WrapSessions(adapters.Sessions, storebinding.ClassWrapping{
		Binding:    wrappedBindingName,
		Capability: storebindingtest.ReferenceCapability,
		Observer:   observer,
	})
	if err != nil {
		t.Fatalf("wrapping the Sessions front door: %v", err)
	}
	id, err := wrapped.CreateSession(session.CreateSpec{Title: "polly", AgentName: "polly"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	now := time.Now().UTC()
	if won, err := wrapped.Close(id, "done", now); err != nil || !won {
		t.Fatalf("first Close = (%v, %v), want the race won with no error", won, err)
	}
	if won, err := wrapped.Close(id, "done", now); err != nil || won {
		t.Fatalf("second Close = (%v, %v), want the race lost with no error", won, err)
	}
	stream := observer.stream()
	for _, want := range []string{
		"sessions/wrapped claim Close ok",
		"sessions/wrapped claim Close conflict",
	} {
		if !containsString(stream, want) {
			t.Errorf("the observed stream never reports %q; it is:\n%s", want, formatStream(stream))
		}
	}
	if containsString(stream, "sessions/wrapped claim Close failed") {
		t.Errorf("a lost close race was reported as a failure; it is:\n%s", formatStream(stream))
	}
}
