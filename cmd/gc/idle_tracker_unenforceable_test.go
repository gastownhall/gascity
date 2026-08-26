package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// noActivityProvider mirrors the herdr provider's real contract: it declares
// CanReportActivity false and returns the ZERO time with a NIL error. The nil
// error is the important half — it is what made this indistinguishable from
// "active just now" and kept the failure silent (ga-7ye).
type noActivityProvider struct{ runtime.Provider }

func (noActivityProvider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{CanReportActivity: false}
}

func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()
	fn()
	return buf.String()
}

// TestCheckIdleReportsUnenforceableTimeout is the gate for ga-7ye. celilo ran
// provider="herdr" with idle_timeout="5m" for months; herdr cannot report
// activity, so checkIdle returned false on every tick, DecideIdleTimeout was
// never entered, and six workers held in_progress beads for 3-5 hours. Across
// 1,208,783 trace records on that city the idle ladder had produced no decision
// of any kind. The setting was accepted, stored, and inert, with no diagnostic.
func TestCheckIdleReportsUnenforceableTimeout(t *testing.T) {
	tr := newIdleTracker()
	tr.setTimeout("worker-1", 5*time.Minute)

	var idle bool
	out := captureLog(t, func() {
		idle = tr.checkIdle("worker-1", "tmpl", noActivityProvider{}, time.Now())
	})

	if idle {
		t.Fatal("a session whose activity cannot be measured must not be reported idle")
	}
	if !strings.Contains(out, "cannot be enforced") {
		t.Fatalf("silent inert idle_timeout: no diagnostic logged.\ngot: %q", out)
	}
	for _, want := range []string{"worker-1", "5m0s", "ga-7ye"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostic must name %q so an operator can act on it; got: %q", want, out)
		}
	}
}

// The reconciler calls checkIdle for every session every cycle. An
// unconditional log would emit the same line thousands of times an hour and be
// filtered out as noise, which is the failure mode this is meant to escape.
func TestCheckIdleReportsUnenforceableOncePerSession(t *testing.T) {
	tr := newIdleTracker()
	tr.setTimeout("worker-1", time.Minute)

	out := captureLog(t, func() {
		for i := 0; i < 50; i++ {
			tr.checkIdle("worker-1", "tmpl", noActivityProvider{}, time.Now())
		}
	})

	if got := strings.Count(out, "cannot be enforced"); got != 1 {
		t.Fatalf("logged %d times across 50 ticks, want exactly 1", got)
	}
}

// The CAPABILITY diagnostic must not fire on a provider that reports activity,
// or every healthy city gets the line and learns to ignore it.
//
// Scoped to the capability branch on purpose. The later "no activity timestamp"
// branch is not reachable from here: the lookup resolves a worker handle rather
// than querying the provider by bare session name, so a standalone Fake never
// gets asked. That branch is exercised in the herdr-shaped case above, where the
// capability gate short-circuits before it. Asserting on the generic text would
// make this test pass for the wrong reason.
func TestCheckIdleStaysQuietWhenProviderCanReportActivity(t *testing.T) {
	tr := newIdleTracker()
	tr.setTimeout("worker-1", time.Minute)

	// runtime.Fake declares CanReportActivity true and answers with a real
	// timestamp, which is what a healthy provider looks like.
	fake := runtime.NewFake()
	fake.Activity = map[string]time.Time{"worker-1": time.Now()}

	out := captureLog(t, func() {
		tr.checkIdle("worker-1", "tmpl", fake, time.Now())
	})

	if strings.Contains(out, "provider cannot report activity") {
		t.Fatalf("capability diagnostic fired on a provider that reports activity; got: %q", out)
	}
}

// No timeout configured is not a misconfiguration, so it must stay silent.
func TestCheckIdleStaysQuietWithNoTimeoutConfigured(t *testing.T) {
	tr := newIdleTracker()
	out := captureLog(t, func() {
		tr.checkIdle("worker-1", "tmpl", noActivityProvider{}, time.Now())
	})
	if out != "" {
		t.Fatalf("no idle_timeout configured means nothing to warn about; got: %q", out)
	}
}
