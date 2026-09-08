package herdr

import (
	"testing"
	"time"
)

// activityTestProvider wires a Provider at the fake server's socket with the
// activity knobs shrunk so poll-driven assertions settle in milliseconds, and
// guarantees the tracker goroutines stop with the test.
func activityTestProvider(t *testing.T, sock string) *Provider {
	t.Helper()
	restore := []struct {
		p *time.Duration
		v time.Duration
	}{
		{&activityPollInterval, activityPollInterval},
		{&activityEventDebounce, activityEventDebounce},
		{&activityPollTimeout, activityPollTimeout},
	}
	t.Cleanup(func() {
		for _, r := range restore {
			*r.p = r.v
		}
	})
	activityPollInterval = 25 * time.Millisecond
	activityEventDebounce = 5 * time.Millisecond
	activityPollTimeout = 2 * time.Second

	p := eventTestProvider(t, sock)
	t.Cleanup(p.act.stop)
	return p
}

// lastActivity fetches GetLastActivity and fails the test on error (the
// contract is a nil error always).
func lastActivity(t *testing.T, p *Provider, name string) time.Time {
	t.Helper()
	got, err := p.GetLastActivity(name)
	if err != nil {
		t.Fatalf("GetLastActivity(%q): %v", name, err)
	}
	return got
}

// waitActivity polls GetLastActivity until cond holds or the deadline passes.
func waitActivity(t *testing.T, p *Provider, name string, timeout time.Duration, cond func(time.Time) bool) time.Time {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := lastActivity(t, p, name)
		if cond(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("GetLastActivity(%q) never satisfied condition; last value %v", name, got)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestActivityCapabilities(t *testing.T) {
	f, sock := newFakeHerdrServer(t)
	_ = f
	p := activityTestProvider(t, sock)
	caps := p.Capabilities()
	if !caps.CanReportActivity {
		t.Fatal("CanReportActivity must be true: GetLastActivity now returns meaningful results")
	}
	if !caps.NeedsClaimBackstop {
		t.Fatal("NeedsClaimBackstop must be true: reporting activity must not deactivate the stalled-claim nudge backstop")
	}
}

func TestActivityUnknownSessionReturnsZero(t *testing.T) {
	f, sock := newFakeHerdrServer(t)
	f.setAgents() // empty list
	p := activityTestProvider(t, sock)
	if got := lastActivity(t, p, "ghost"); !got.IsZero() {
		t.Fatalf("unknown session: want zero time, got %v", got)
	}
}

// The very first GetLastActivity call seeds synchronously: a session that is
// already working must read as active immediately, with no cold-zero window
// (a cold zero would let a pending config-drift reset fire on the first tick
// after a supervisor restart).
func TestActivityColdSeedIsSynchronous(t *testing.T) {
	f, sock := newFakeHerdrServer(t)
	f.setAgents(agentInfo{Name: "w", PaneID: "%1", AgentStatus: "working", Revision: 1})
	p := activityTestProvider(t, sock)
	got := lastActivity(t, p, "w")
	if got.IsZero() {
		t.Fatal("first call returned zero: cold seed must be synchronous")
	}
	if age := time.Since(got); age > time.Second {
		t.Fatalf("first call returned stale time (age %v); want ~now", age)
	}
}

// A session sitting at working emits no transitions, so it must read as
// continuously active — otherwise long legitimate work looks stale and the
// progress-stall/idle nets would recycle a live agent (the #312/#468 churn
// class).
func TestActivityWorkingIsContinuouslyActive(t *testing.T) {
	f, sock := newFakeHerdrServer(t)
	f.setAgents(agentInfo{Name: "w", PaneID: "%1", AgentStatus: "working", Revision: 1})
	p := activityTestProvider(t, sock)

	first := lastActivity(t, p, "w")
	time.Sleep(20 * time.Millisecond)
	second := lastActivity(t, p, "w")
	if !second.After(first) {
		t.Fatalf("working session must read as continuously active: second %v not after first %v", second, first)
	}
	if age := time.Since(second); age > time.Second {
		t.Fatalf("working session activity age %v; want ~now", age)
	}
}

// When a session leaves working, the stamp freezes at the observed transition
// and ages from there — that frozen stamp is what quiescence and idle-timeout
// checks measure against.
func TestActivityIdleStampFreezesAtTransition(t *testing.T) {
	f, sock := newFakeHerdrServer(t)
	f.setAgents(agentInfo{Name: "a", PaneID: "%1", AgentStatus: "working", Revision: 1})
	p := activityTestProvider(t, sock)
	lastActivity(t, p, "a") // seed while working

	f.setAgents(agentInfo{Name: "a", PaneID: "%1", AgentStatus: "idle", Revision: 1})
	// Wait for a poll to observe the transition: the value stops tracking now.
	frozen := waitActivity(t, p, "a", 2*time.Second, func(got time.Time) bool {
		return !got.IsZero() && time.Since(got) > 10*time.Millisecond
	})
	time.Sleep(30 * time.Millisecond)
	if again := lastActivity(t, p, "a"); !again.Equal(frozen) {
		t.Fatalf("idle stamp must freeze at the observed transition: %v then %v", frozen, again)
	}
}

// herdr replays a backlog of recent events to every new subscription, so a
// replayed agent-status frame must never re-stamp activity. Stamps come only
// from polls observing a state CHANGE; events merely accelerate the next poll.
func TestActivityReplayedStatusEventsDoNotRefreshStamp(t *testing.T) {
	f, sock := newFakeHerdrServer(t)
	f.setAgents(agentInfo{Name: "a", PaneID: "%1", AgentStatus: "idle", Revision: 1})
	p := activityTestProvider(t, sock)
	seeded := lastActivity(t, p, "a") // first observation stamps once
	if seeded.IsZero() {
		t.Fatal("seed poll must stamp first observation")
	}

	// The tracker's self-subscription reaches the fake; replay the same
	// (already-current) status at it repeatedly.
	stream := <-f.streams
	deadline := time.Now().Add(60 * time.Millisecond)
	for time.Now().Before(deadline) {
		stream.push(t, `{"event":"pane.agent_status_changed","data":{"pane_id":"%1","agent_status":"idle"}}`)
		time.Sleep(5 * time.Millisecond)
	}

	if got := lastActivity(t, p, "a"); !got.Equal(seeded) {
		t.Fatalf("replayed status events must not refresh the stamp: seeded %v, now %v", seeded, got)
	}
}

// For sessions herdr cannot classify (agent_status "unknown" — e.g. bare
// shells), the output revision counter is the only activity signal: a changed
// revision stamps, a stable one ages.
func TestActivityUnknownStatusStampsOnRevisionChange(t *testing.T) {
	f, sock := newFakeHerdrServer(t)
	f.setAgents(agentInfo{Name: "sh", PaneID: "%1", AgentStatus: "unknown", Revision: 7})
	p := activityTestProvider(t, sock)
	seeded := lastActivity(t, p, "sh")

	time.Sleep(20 * time.Millisecond)
	f.setAgents(agentInfo{Name: "sh", PaneID: "%1", AgentStatus: "unknown", Revision: 8})
	bumped := waitActivity(t, p, "sh", 2*time.Second, func(got time.Time) bool {
		return got.After(seeded)
	})

	// Stable revision from here: the stamp must age, not refresh.
	time.Sleep(30 * time.Millisecond)
	if got := lastActivity(t, p, "sh"); !got.Equal(bumped) {
		t.Fatalf("stable revision must not re-stamp: %v then %v", bumped, got)
	}
}

// For sessions herdr HAS classified, status is authoritative and revision is
// ignored: an idle TUI that redraws (spinners, status lines) must not read as
// active, or idle detection would never fire for it.
func TestActivityDetectedIdleRevisionTickDoesNotStamp(t *testing.T) {
	f, sock := newFakeHerdrServer(t)
	f.setAgents(agentInfo{Name: "a", PaneID: "%1", AgentStatus: "idle", Revision: 1})
	p := activityTestProvider(t, sock)
	seeded := lastActivity(t, p, "a")

	for rev := uint64(2); rev < 6; rev++ {
		f.setAgents(agentInfo{Name: "a", PaneID: "%1", AgentStatus: "idle", Revision: rev})
		time.Sleep(15 * time.Millisecond)
	}
	if got := lastActivity(t, p, "a"); !got.Equal(seeded) {
		t.Fatalf("detected-idle revision ticks must not stamp activity: seeded %v, now %v", seeded, got)
	}
}

// A transient poll failure keeps the last known state: an outage must not
// fabricate idleness (wiping a working session would let a drift reset or
// stall recycle kill a live agent) and must not fabricate absence.
func TestActivityPollFailureKeepsLastState(t *testing.T) {
	f, sock := newFakeHerdrServer(t)
	f.setAgents(agentInfo{Name: "w", PaneID: "%1", AgentStatus: "working", Revision: 1})
	p := activityTestProvider(t, sock)
	lastActivity(t, p, "w") // seed

	_ = f.ln.Close() // server gone: every subsequent poll fails

	time.Sleep(60 * time.Millisecond) // several failed poll intervals
	got := lastActivity(t, p, "w")
	if got.IsZero() {
		t.Fatal("poll failure wiped the entry; last known state must be kept")
	}
	if age := time.Since(got); age > time.Second {
		t.Fatalf("working session must stay continuously active across an outage; age %v", age)
	}
}

// An agent that disappears from agent.list is gone: its entry drops and
// GetLastActivity returns zero (unknown), so consumers fall back to their
// no-op paths rather than acting on a stale stamp.
func TestActivityRemovedAgentReturnsZero(t *testing.T) {
	f, sock := newFakeHerdrServer(t)
	f.setAgents(agentInfo{Name: "a", PaneID: "%1", AgentStatus: "idle", Revision: 1})
	p := activityTestProvider(t, sock)
	if got := lastActivity(t, p, "a"); got.IsZero() {
		t.Fatal("seed must observe the agent")
	}

	f.setAgents() // agent gone
	waitActivity(t, p, "a", 2*time.Second, func(got time.Time) bool {
		return got.IsZero()
	})
}

// Stopping the tracker releases its goroutines (subscription stream included).
// Production never stops it — the tracker lives as long as the provider — but
// the release path keeps tests hermetic and bounds a future teardown hook.
func TestActivityTrackerStopReleasesGoroutines(t *testing.T) {
	f, sock := newFakeHerdrServer(t)
	f.setAgents(agentInfo{Name: "a", PaneID: "%1", AgentStatus: "idle", Revision: 1})

	before := goroutineCount()
	p := eventTestProvider(t, sock)
	shrinkActivityKnobs(t)
	lastActivity(t, p, "a") // starts tracker + stream
	p.act.stop()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if goroutineCount() <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines leaked after stop: before=%d now=%d", before, goroutineCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// shrinkActivityKnobs shrinks the poll cadence for one test and restores it.
func shrinkActivityKnobs(t *testing.T) {
	t.Helper()
	pi, ed := activityPollInterval, activityEventDebounce
	t.Cleanup(func() { activityPollInterval, activityEventDebounce = pi, ed })
	activityPollInterval = 25 * time.Millisecond
	activityEventDebounce = 5 * time.Millisecond
}
