package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// namepoolAgentForNudgeTest builds the pool shape that reproduces ga-mzq6: a
// pool whose members are named from a namepool file, so live instances are
// addressed as "dir/binding.<namepool-name>" rather than "pool-N". Those
// instance identities are not config entries.
func namepoolAgentForNudgeTest() config.Agent {
	return config.Agent{
		Name:              "polecat",
		Dir:               "gascity",
		BindingName:       "gastown",
		NamepoolNames:     []string{"furiosa", "rictus"},
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(2),
	}
}

// TestDoSlingNudgeNamepoolInstanceReachesRunningSession covers ga-mzq6
// acceptance criterion 1: when a namepool-named pool member is already
// running, the nudge resolves the live instance and is delivered instead of
// failing the config lookup on the instance name.
func TestDoSlingNudgeNamepoolInstanceReachesRunningSession(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	const sessionName = "gastown__polecat-th-dpcg1"
	if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("Start(%q): %v", sessionName, err)
	}
	sp.Calls = nil

	a := namepoolAgentForNudgeTest()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{a},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.CityPath = t.TempDir()
	if _, err := deps.Store.Create(beads.Bead{
		Title:  "gascity/gastown.furiosa",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "gascity/gastown.polecat",
			"session_name": sessionName,
			"pool_slot":    "1",
		},
	}); err != nil {
		t.Fatal(err)
	}
	prev := startNudgePoller
	startNudgePoller = func(_, _, _ string) error { return nil }
	t.Cleanup(func() { startNudgePoller = prev })

	doSlingNudge(&a, deps.CityName, deps.CityPath, cfg, sp, deps.Store, stdout, stderr)

	if strings.Contains(stderr.String(), "not found in config") {
		t.Fatalf("stderr = %q, want no config lookup failure for the live pool instance", stderr.String())
	}
	if strings.Contains(stdout.String(), "No running sessions") || strings.Contains(stderr.String(), "No running sessions") {
		t.Fatalf("stdout=%q stderr=%q, want the live instance nudged, not the zero-instance wake path", stdout.String(), stderr.String())
	}
	var observedLiveSession bool
	for _, call := range sp.Calls {
		if call.Method == "IsRunning" && call.Name == sessionName {
			observedLiveSession = true
			break
		}
	}
	if !observedLiveSession {
		t.Fatalf("runtime calls = %#v, want IsRunning for namepool instance session %q", sp.Calls, sessionName)
	}
	if !strings.Contains(stdout.String(), "gascity/gastown.furiosa") {
		t.Fatalf("stdout = %q, want nudge output naming the namepool pool instance", stdout.String())
	}
}

// TestDoSlingNudgeNamepoolNoRunningInstancePokesController covers ga-mzq6
// acceptance criterion 2: with no live pool member, the zero-instance path
// still pokes the controller for a wake.
func TestDoSlingNudgeNamepoolNoRunningInstancePokesController(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	a := namepoolAgentForNudgeTest()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{a},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.CityPath = t.TempDir() // isolated path so poke cannot hit a real socket

	doSlingNudge(&a, deps.CityName, deps.CityPath, cfg, sp, deps.Store, stdout, stderr)

	// Both poke outcomes (socket present or absent) print "No running sessions
	// for <pool>"; asserting on the shared prefix keeps this independent of
	// whether a controller socket exists on the test host.
	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, `No running sessions for "gascity/gastown.polecat"`) {
		t.Fatalf("stdout=%q stderr=%q, want the zero-instance controller wake path", stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "not found in config") {
		t.Fatalf("stderr = %q, want no config lookup failure on the zero-instance path", stderr.String())
	}
}

// TestSlingNudgeFailureReadsAsWarning covers ga-mzq6 acceptance criterion 3:
// a nudge-delivery failure must not be phrased like a sling failure, because
// the bead was already routed by the time the nudge runs.
func TestSlingNudgeFailureReadsAsWarning(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	a := config.Agent{Name: "mayor", Suspended: true, MaxActiveSessions: intPtr(1)}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{a},
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.CityPath = t.TempDir()

	doSlingNudge(&a, deps.CityName, deps.CityPath, cfg, sp, deps.Store, stdout, stderr)

	got := stderr.String()
	if !strings.HasPrefix(got, "warning: ") {
		t.Fatalf("stderr = %q, want a 'warning: ' prefix so the routed sling is not misread as failed", got)
	}
	if !strings.Contains(got, "bead routed") {
		t.Fatalf("stderr = %q, want the message to state the bead was still routed", got)
	}
}
