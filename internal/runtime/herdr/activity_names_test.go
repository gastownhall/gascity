package herdr

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// gc session names that herdrAgentName rewrites: a mixed-case rig name and a
// name over herdr's 32-char limit (hashed). The tracker observes herdr's
// registry, which only ever holds the mapped forms, while every consumer asks
// by the gc session name.
var rewrittenSessionNames = []string{
	"Indigo--anthony",
	"GunnInternships--gastown__refinery",
}

func TestActivityRewrittenNamesAreRewritten(t *testing.T) {
	for _, name := range rewrittenSessionNames {
		if herdrAgentName(name) == name {
			t.Fatalf("fixture %q is not rewritten by herdrAgentName; the tests below would not exercise the mapping", name)
		}
	}
}

// GetLastActivity is asked by gc session name and must find the entry herdr
// registered under the mapped name. A zero here blinds idle timeout, idle
// nudge, and every other activity consumer for the session forever.
func TestActivityGetLastActivityResolvesRewrittenNames(t *testing.T) {
	for _, name := range rewrittenSessionNames {
		t.Run(name, func(t *testing.T) {
			f, sock := newFakeHerdrServer(t)
			f.setAgents(agentInfo{Name: herdrAgentName(name), PaneID: "%1", AgentStatus: "idle", Revision: 1})
			p := activityTestProvider(t, sock)
			if got := lastActivity(t, p, name); got.IsZero() {
				t.Fatalf("GetLastActivity(%q) = zero; want the stamp tracked under %q", name, herdrAgentName(name))
			}
		})
	}
}

// The first-call seed retries until the requested session is observed. It
// must look for the mapped registry name: keyed by the raw gc name it would
// never match, burn its whole retry budget, and stall the first caller.
func TestActivitySeedRetryKeyedByRegistryName(t *testing.T) {
	name := "Indigo--anthony"
	f, sock := newFakeHerdrServer(t)
	f.setAgents() // herdr's agent detection lags pane creation
	p := activityTestProvider(t, sock)

	budget, interval := activitySeedRetryBudget, activitySeedRetryInterval
	t.Cleanup(func() { activitySeedRetryBudget, activitySeedRetryInterval = budget, interval })
	activitySeedRetryBudget = 10 * time.Second
	activitySeedRetryInterval = 10 * time.Millisecond

	// The agent registers between the first seed poll and its retry.
	f.mu.Lock()
	f.onList = func(call int) {
		if call == 2 {
			f.setAgents(agentInfo{Name: herdrAgentName(name), PaneID: "%1", AgentStatus: "working", Revision: 1})
		}
	}
	f.mu.Unlock()

	start := time.Now()
	got := lastActivity(t, p, name)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("seed took %v: it never recognized %q as the requested session", elapsed, herdrAgentName(name))
	}
	if got.IsZero() {
		t.Fatal("seed returned zero for a session that appeared within the retry budget")
	}
}

// herdrAgentName is many-to-one: gc names differing only in case (or in runes
// the mapping folds to '-') share one herdr registry name, and herdr's
// agent_name_taken guarantees at most one agent holds it. Activity is keyed by
// that registry name, so colliding gc names read the same entry — the one
// agent that actually exists. This pins that documented behavior rather than
// inventing disambiguation machinery for a pathological naming clash.
func TestActivityCollidingGCNamesShareRegistryEntry(t *testing.T) {
	f, sock := newFakeHerdrServer(t)
	f.setAgents(agentInfo{Name: "indigo--x", PaneID: "%1", AgentStatus: "working", Revision: 1})
	p := activityTestProvider(t, sock)
	for _, name := range []string{"Indigo--x", "indigo--x", "INDIGO--X"} {
		if got := lastActivity(t, p, name); got.IsZero() {
			t.Errorf("GetLastActivity(%q) = zero; colliding names share the registry entry %q", name, "indigo--x")
		}
	}
}

// bindSessionMeta records the sidecar binding a successful Start leaves
// behind (exact gc name + pane), which is what boundSessionNames enumerates.
func bindSessionMeta(t *testing.T, p *Provider, name, pane string) {
	t.Helper()
	if err := p.SetMeta(name, metaBoundName, name); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta(name, metaBoundPane, pane); err != nil {
		t.Fatal(err)
	}
}

// Session events carry the gc session name, not herdr's mapped registry
// name: bound sessions reverse-map through their sidecar binding, and foreign
// agents with no binding keep their registry name (as ListRunning does).
func TestSessionEventStreamAttributesGCSessionName(t *testing.T) {
	mixed, long := rewrittenSessionNames[0], rewrittenSessionNames[1]
	f, sock := newFakeHerdrServer(t)
	f.setAgents(
		agentInfo{Name: herdrAgentName(mixed), PaneID: "w1:p1"},
		agentInfo{Name: herdrAgentName(long), PaneID: "w2:p1"},
		agentInfo{Name: "foreign", PaneID: "w3:p1"},
	)
	p := eventTestProvider(t, sock)
	bindSessionMeta(t, p, mixed, "w1:p1")
	bindSessionMeta(t, p, long, "w2:p1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.SubscribeSessionEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeSessionEvents: %v", err)
	}
	if ev := recvEvent(t, ch, 2*time.Second); ev.Kind != runtime.SessionEventResync {
		t.Fatalf("first event = %v, want resync", ev.Kind)
	}
	stream := recvStream(t, f)

	for _, tc := range []struct{ pane, want string }{
		{"w1:p1", mixed},
		{"w2:p1", long},
		{"w3:p1", "foreign"},
	} {
		stream.push(t, `{"data":{"pane_id":"`+tc.pane+`","type":"pane_exited"},"event":"pane_exited"}`)
		if ev := recvEvent(t, ch, 2*time.Second); ev.Session != tc.want {
			t.Errorf("pane %s => Session %q, want %q", tc.pane, ev.Session, tc.want)
		}
	}
}

// Start writes the sidecar binding only after herdr registered the agent, so
// a cycle can list the agent before its binding exists. The debounced re-list
// must then reverse-map the same way, re-attributing the already-subscribed
// pane to its gc session name.
func TestSessionEventStreamRelistAttributesGCSessionName(t *testing.T) {
	name := rewrittenSessionNames[0]
	f, sock := newFakeHerdrServer(t)
	f.setAgents(agentInfo{Name: herdrAgentName(name), PaneID: "w1:p1"})
	p := eventTestProvider(t, sock)
	// Call 1 is the cycle-start listing; call 2 is the debounced re-list.
	relisted := make(chan struct{})
	f.mu.Lock()
	f.onList = func(call int) {
		if call == 2 {
			close(relisted)
		}
	}
	f.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := p.SubscribeSessionEvents(ctx)
	if err != nil {
		t.Fatalf("SubscribeSessionEvents: %v", err)
	}
	if ev := recvEvent(t, ch, 2*time.Second); ev.Kind != runtime.SessionEventResync {
		t.Fatalf("first event = %v, want resync", ev.Kind)
	}
	stream := recvStream(t, f)

	bindSessionMeta(t, p, name, "w1:p1")
	stream.push(t, `{"data":{"agent":"claude","pane_id":"w1:p1","type":"pane_agent_detected"},"event":"pane_agent_detected"}`)
	recvEvent(t, ch, 2*time.Second) // attributed by the pre-binding listing; arms the re-list

	select {
	case <-relisted:
	case <-time.After(3 * time.Second):
		t.Fatal("pane_agent_detected never triggered the debounced re-list")
	}
	// The re-list request has been answered; the stream applies it before it
	// reads its next frame, so each pushed frame is a round trip through the
	// read loop and attribution flips within a frame or two.
	deadline := time.Now().Add(3 * time.Second)
	for {
		stream.push(t, `{"data":{"pane_id":"w1:p1","type":"pane_exited"},"event":"pane_exited"}`)
		ev := recvEvent(t, ch, 2*time.Second)
		if ev.Session == name {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after re-list: Session %q, want %q", ev.Session, name)
		}
	}
}

// Event attribution has the same many-to-one mapping to contend with: when two
// bound gc names fold to one registry name, the first bound name (sidecar
// directory order) owns it, and unbound registry names pass through.
func TestRegistryToSessionNamesCollisionFirstBoundWins(t *testing.T) {
	long := rewrittenSessionNames[1]
	m := registryToSessionNames([]string{"Indigo--x", "indigo--x", long})
	if got := sessionNameFor(m, "indigo--x"); got != "Indigo--x" {
		t.Errorf("colliding registry name => %q, want first bound %q", got, "Indigo--x")
	}
	if got := sessionNameFor(m, herdrAgentName(long)); got != long {
		t.Errorf("hashed registry name => %q, want %q", got, long)
	}
	if got := sessionNameFor(m, "foreign"); got != "foreign" {
		t.Errorf("unbound registry name => %q, want it unchanged", got)
	}
	if got := sessionNameFor(nil, "foreign"); got != "foreign" {
		t.Errorf("nil map => %q, want it unchanged", got)
	}
}
