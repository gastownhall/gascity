package main

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// These tests pin one invariant: gc status never states a fact about an agent
// it did not successfully observe. A runtime probe that times out has learned
// nothing, so a running count of zero taken from it is not a measurement and
// must not be published as one — not in the text table, and not in the JSON
// that dashboards and health checks read.
//
// The failure this guards against is expensive rather than cosmetic. A waking
// operator or agent that reads "0/15 agents running" against a queue of open
// work will restart the agents and re-sling the work, which duplicates builds
// that are already mid-review.

// partialFromInjectedProbeTimeout drives the real bounded status provider with
// a probe slow enough to blow its own budget, then reads the partial flag back
// through statusProviderPartial — the same call the snapshot collector makes.
// The degraded path is reproduced rather than asserted into existence, so these
// tests cannot pass against a build where the timeout stopped marking status
// partial at all.
func partialFromInjectedProbeTimeout(t *testing.T) bool {
	t.Helper()
	origWarn := statusProviderTimeoutWarning
	t.Cleanup(func() { statusProviderTimeoutWarning = origWarn })
	statusProviderTimeoutWarning = func() {}

	base := newStatusProbeProvider()
	// The agent is running. Only the probe fails, which is the whole point:
	// the observation is missing, not the agent.
	base.running.Store(true)
	base.delay.Store(int64(100 * time.Millisecond))
	wrapped := newBoundedStatusProvider(base, 10*time.Millisecond)

	if wrapped.IsRunning("local-core.builder-1") {
		t.Fatal("IsRunning returned true, want the timeout fallback — the probe was supposed to blow its budget")
	}
	return statusProviderPartial(wrapped)
}

// fleetSnapshot builds a city of total agents of which running were observed
// running, matching the shape of the incident report: fifteen agent slots, a
// probe that answered for none of them.
func fleetSnapshot(running, total int, partial bool, partialErrors []string) cityStatusSnapshot {
	rows := make([]cityStatusAgentRow, 0, total)
	for i := range total {
		name := fmt.Sprintf("local-core.builder-%d", i+1)
		rows = append(rows, cityStatusAgentRow{
			Agent:       StatusAgentJSON{Name: name, QualifiedName: name, Running: i < running},
			SessionName: name,
		})
	}
	snapshot := cityStatusSnapshot{
		CityName:      "testcity",
		CityPath:      "/tmp/testcity",
		Agents:        rows,
		Partial:       partial,
		PartialErrors: partialErrors,
	}
	// The controller is up. In the incident the factory was working normally
	// throughout; only the probe failed. Leaving it down would add a
	// controller_not_running signal that has nothing to do with these tests.
	snapshot.Controller.Running = true
	snapshot.Summary.TotalAgents = total
	snapshot.Summary.RunningAgents = running
	return snapshot
}

// agentRows returns only the rows of the Agents block. Assertions about agent
// state have to be scoped to it: the Controller line spends the same word
// ("Controller: stopped") on an entirely different subject, so an unscoped
// search for "stopped" both fails the honesty check spuriously and lets the
// positive control pass without a single agent row in it.
func agentRows(t *testing.T, out string) []string {
	t.Helper()
	var rows []string
	inBlock := false
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "Agents:" {
			inBlock = true
			continue
		}
		if !inBlock {
			continue
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		rows = append(rows, line)
	}
	if len(rows) == 0 {
		t.Fatalf("no agent rows found in output, so any assertion over them would be vacuous:\n%s", out)
	}
	return rows
}

func renderFleet(t *testing.T, snapshot cityStatusSnapshot) string {
	t.Helper()
	var stdout bytes.Buffer
	renderCityStatusText(snapshot, newFakeDrainOps(), &stdout)
	return stdout.String()
}

var zeroOfNRunning = regexp.MustCompile(`(?m)^0/\d+ agents running$`)

// TestTimedOutProbeNeverRendersAbsenceAsFact is the acceptance case from the
// incident: a probe that reached no agent at all must not print a table of
// stopped agents footed by a definitive count.
func TestTimedOutProbeNeverRendersAbsenceAsFact(t *testing.T) {
	partial := partialFromInjectedProbeTimeout(t)
	if !partial {
		t.Fatal("an injected probe timeout did not mark the status partial; the rest of this test would assert nothing")
	}

	const probeReason = "runtime status probe incomplete; non-running agent rows are unknown"
	out := renderFleet(t, fleetSnapshot(0, 15, partial, []string{probeReason}))

	// (a) No agent row may claim an agent is stopped.
	rows := agentRows(t, out)
	if len(rows) != 15 {
		t.Fatalf("scanned %d agent rows, want all 15:\n%s", len(rows), out)
	}
	for _, row := range rows {
		if strings.Contains(row, "stopped") {
			t.Fatalf("agent row states an unobserved agent is stopped: %q\nfull output:\n%s", row, out)
		}
	}

	// (b) No definitive count of zero.
	if zeroOfNRunning.MatchString(out) {
		t.Fatalf("output carries a definitive zero count from a probe that observed nothing:\n%s", out)
	}

	// (c) The degradation is named on stdout, where the count is — not only
	// on stderr, which scrolls above a table that looks complete.
	summary := "0 running, 15 unknown of 15 agents"
	if !strings.Contains(out, summary) {
		t.Fatalf("output does not report the unknown agents where the count belongs:\n%s", out)
	}
	if !strings.Contains(out, probeReason) {
		t.Fatalf("output does not name why the count is not a measurement:\n%s", out)
	}
	summaryAt := strings.Index(out, summary)
	if reasonAt := strings.Index(out, probeReason); reasonAt < summaryAt {
		t.Fatalf("the reason for the degradation must sit with the count, not above the table:\n%s", out)
	}
}

// TestHealthyProbeStillReportsStoppedAgents is the positive control the
// incident report asked for. Without it every assertion above could be
// satisfied by a renderer broken into printing nothing.
func TestHealthyProbeStillReportsStoppedAgents(t *testing.T) {
	out := renderFleet(t, fleetSnapshot(1, 3, false, nil))

	stopped := 0
	for _, row := range agentRows(t, out) {
		if strings.Contains(row, "stopped") {
			stopped++
		}
		if strings.Contains(row, "unknown") {
			t.Fatalf("nothing is unknown when the probe answered: %q\nfull output:\n%s", row, out)
		}
	}
	if stopped != 2 {
		t.Fatalf("agent rows reading stopped = %d, want the 2 genuinely stopped agents:\n%s", stopped, out)
	}
	if !strings.Contains(out, "1/3 agents running") {
		t.Fatalf("a healthy probe must still print a real count:\n%s", out)
	}
	if strings.Contains(out, "partial status") {
		t.Fatalf("a healthy probe must not caveat its own count:\n%s", out)
	}

	// A city that really is down still says so.
	down := renderFleet(t, fleetSnapshot(0, 15, false, nil))
	if !zeroOfNRunning.MatchString(down) {
		t.Fatalf("an observed-empty city must still report 0/15 agents running:\n%s", down)
	}
}

// TestPartialStatusDoesNotSignalNoAgentsRunning covers the machine-readable
// half, which is the surface a dashboard consumes. The text renderer stopped
// folding unknown into stopped; the JSON health block kept deriving
// no_agents_running from the same zero.
func TestPartialStatusDoesNotSignalNoAgentsRunning(t *testing.T) {
	partial := partialFromInjectedProbeTimeout(t)
	if !partial {
		t.Fatal("an injected probe timeout did not mark the status partial")
	}
	snapshot := fleetSnapshot(0, 15, partial, []string{"runtime status probe incomplete"})

	status := cityStatusJSONFromSnapshot(snapshot, snapshot.Summary)

	for _, signal := range status.Health.Signals {
		if signal == "no_agents_running" {
			t.Fatalf("health signals assert absence from a probe that observed nothing: %v", status.Health.Signals)
		}
	}
	if !slices.Contains(status.Health.Signals, "agent_state_unknown") {
		t.Fatalf("health signals = %v, want agent_state_unknown so a consumer can tell unknown from empty", status.Health.Signals)
	}
	if !status.Partial {
		t.Fatal("status.partial = false, want true so the payload is self-describing")
	}
	if status.Summary.UnknownAgents != 15 {
		t.Fatalf("summary.unknown_agents = %d, want 15", status.Summary.UnknownAgents)
	}
	if !status.Health.Degraded {
		t.Fatal("health.degraded = false, want true — an unobservable fleet is a degraded read")
	}
}

// TestObservedEmptyCityStillSignalsNoAgentsRunning is the positive control for
// the JSON half: the fix must not have bought its honesty by never reporting a
// genuinely dead city.
func TestObservedEmptyCityStillSignalsNoAgentsRunning(t *testing.T) {
	snapshot := fleetSnapshot(0, 15, false, nil)

	status := cityStatusJSONFromSnapshot(snapshot, snapshot.Summary)

	if !slices.Contains(status.Health.Signals, "no_agents_running") {
		t.Fatalf("health signals = %v, want no_agents_running when the probe observed an empty city", status.Health.Signals)
	}
	if slices.Contains(status.Health.Signals, "agent_state_unknown") {
		t.Fatalf("health signals = %v, nothing is unknown when the probe answered", status.Health.Signals)
	}
	if status.Summary.UnknownAgents != 0 {
		t.Fatalf("summary.unknown_agents = %d, want 0 outside partial status", status.Summary.UnknownAgents)
	}
}

// TestPartialStatusWithEveryAgentObservedRunning pins the boundary: partial is
// not by itself a reason to call anything unknown. When the probe answered for
// every agent, the counts are measurements and read exactly as before.
func TestPartialStatusWithEveryAgentObservedRunning(t *testing.T) {
	snapshot := fleetSnapshot(3, 3, true, nil)

	status := cityStatusJSONFromSnapshot(snapshot, snapshot.Summary)

	if len(status.Health.Signals) != 0 {
		t.Fatalf("health signals = %v, want none when every agent was observed running", status.Health.Signals)
	}
	if status.Summary.UnknownAgents != 0 {
		t.Fatalf("summary.unknown_agents = %d, want 0 when nothing is unknown", status.Summary.UnknownAgents)
	}
}

// TestPublishedCountsAndUnknownAgentsShareOneSource pins unknown_agents to the
// same summary the payload publishes as running_agents and total_agents. Both
// production callers pass snapshot.Summary, so this is a fence rather than a
// live bug: an unknown count that disagrees with the counts printed beside it
// would reintroduce the self-contradicting output this change exists to remove.
func TestPublishedCountsAndUnknownAgentsShareOneSource(t *testing.T) {
	snapshot := fleetSnapshot(0, 15, true, []string{"runtime status probe incomplete"})
	published := StatusSummaryJSON{TotalAgents: 4, RunningAgents: 1}

	status := cityStatusJSONFromSnapshot(snapshot, published)

	if status.Summary.UnknownAgents != 3 {
		t.Fatalf("summary.unknown_agents = %d, want 3 — the payload publishes 1 running of 4", status.Summary.UnknownAgents)
	}
	if gap := status.Summary.TotalAgents - status.Summary.RunningAgents; gap != status.Summary.UnknownAgents {
		t.Fatalf("unknown_agents = %d contradicts the counts published beside it (%d total, %d running)",
			status.Summary.UnknownAgents, status.Summary.TotalAgents, status.Summary.RunningAgents)
	}
}

func TestUnknownAgentCount(t *testing.T) {
	tests := []struct {
		name    string
		running int
		total   int
		partial bool
		want    int
	}{
		{name: "healthy probe knows every state", running: 1, total: 18, partial: false, want: 0},
		{name: "timed-out probe knows none of them", running: 0, total: 15, partial: true, want: 15},
		{name: "timed-out probe answered for some", running: 1, total: 18, partial: true, want: 17},
		{name: "partial but everything answered", running: 3, total: 3, partial: true, want: 0},
		{name: "no agents configured", running: 0, total: 0, partial: true, want: 0},
		// running above total cannot happen from the collector, but a
		// negative unknown count would be worse than a clamped one.
		{name: "running above total clamps rather than going negative", running: 4, total: 3, partial: true, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := unknownAgentCount(tc.running, tc.total, tc.partial); got != tc.want {
				t.Fatalf("unknownAgentCount(%d, %d, %v) = %d, want %d", tc.running, tc.total, tc.partial, got, tc.want)
			}
		})
	}
}
