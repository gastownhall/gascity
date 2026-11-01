package doctor

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// The tests below pin the bounded-read contract of order-outcome-healthy
// (ga-rislhg): the event reads are tails with a byte budget, and an order whose
// tail evidence cannot settle its failure streak is re-walked from the
// authoritative order-run store instead of being reported from a window that may
// have evicted its history.

var orderOutcomeAtLeastRE = regexp.MustCompile(`at least \d+ consecutive failures`)

// outcomeRunFailed, outcomeRunOK and outcomeRunActive build the OrderRun shapes
// the store-backed fallback returns. Open runs with no outcome are in flight.
func outcomeRunFailed(scoped string, at time.Time) orders.OrderRun {
	return orders.OrderRun{ID: "run-f", Scoped: scoped, Outcome: orders.RunOutcomeExecFailed, CreatedAt: at}
}

func outcomeRunOK(scoped string, at time.Time) orders.OrderRun {
	return orders.OrderRun{ID: "run-ok", Scoped: scoped, Outcome: orders.RunOutcomeExec, CreatedAt: at}
}

func outcomeRunActive(scoped string, at time.Time) orders.OrderRun {
	return orders.OrderRun{ID: "run-active", Scoped: scoped, Outcome: orders.RunOutcomeNone, CreatedAt: at, Open: true}
}

// outcomeFallbackSpy is a store-backed fallback that records how it was called.
type outcomeFallbackSpy struct {
	mu    sync.Mutex
	calls []string
	limit []int
	runs  map[string][]orders.OrderRun
	err   error
}

func (s *outcomeFallbackSpy) fn(order orders.Order, limit int) ([]orders.OrderRun, error) {
	s.mu.Lock()
	s.calls = append(s.calls, order.ScopedName())
	s.limit = append(s.limit, limit)
	runs := s.runs[order.ScopedName()]
	err := s.err
	s.mu.Unlock()
	return runs, err
}

func (s *outcomeFallbackSpy) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func outcomeFallbackCheck(t *testing.T, names []string, spy *outcomeFallbackSpy) (*OrderOutcomeHealthyCheck, string) {
	t.Helper()
	cityPath, cfg := orderFiringTestCity(t)
	for _, name := range names {
		writeOrderFiringTestOrder(t, cityPath, name, "cooldown", "5m")
	}
	return NewOrderOutcomeHealthyCheck(cfg, cityPath, WithOrderOutcomeRecentRunsFunc(spy.fn)), cityPath
}

func outcomeDetailFor(t *testing.T, result *CheckResult, name string) string {
	t.Helper()
	for _, d := range result.Details {
		if strings.HasPrefix(d, name+": ") {
			return d
		}
	}
	t.Fatalf("no detail for %q in %v", name, result.Details)
	return ""
}

// (a) Tail evidence that cannot settle the streak: the order's older failures
// were pushed out of the tail window, so the log shows no outcome at all. The
// fallback must change the classification from "no completed runs yet" to a
// warning, and the count must read "at least N" because the walk was capped.
func TestOrderOutcomeHealthy_FallbackFlagsStreakMissingFromTail(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	spy := &outcomeFallbackSpy{runs: map[string][]orders.OrderRun{
		"quiet-order": {
			outcomeRunFailed("quiet-order", now.Add(-1*time.Hour)),
			outcomeRunFailed("quiet-order", now.Add(-2*time.Hour)),
			outcomeRunFailed("quiet-order", now.Add(-3*time.Hour)),
			outcomeRunFailed("quiet-order", now.Add(-4*time.Hour)),
			outcomeRunOK("quiet-order", now.Add(-5*time.Hour)),
		},
	}}
	check, cityPath := outcomeFallbackCheck(t, []string{"quiet-order"}, spy)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderCompleted, Subject: "some-other-order", Ts: now.Add(-time.Hour)})

	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusWarning {
		t.Fatalf("status = %v (%s), want StatusWarning from the store-backed streak; details=%v", result.Status, result.Message, result.Details)
	}
	if result.Severity != SeverityAdvisory {
		t.Fatalf("severity = %v, want SeverityAdvisory", result.Severity)
	}
	detail := outcomeDetailFor(t, result, "quiet-order")
	if !orderOutcomeAtLeastRE.MatchString(detail) {
		t.Fatalf("detail = %q, want an \"at least N consecutive failures\" count: the fallback walk is capped", detail)
	}
	if got := spy.callCount(); got != 1 {
		t.Fatalf("fallback calls = %d, want 1 for the one insufficient order", got)
	}
	if spy.calls[0] != "quiet-order" {
		t.Fatalf("fallback asked for %q, want the order's scoped name", spy.calls[0])
	}
	if spy.limit[0] < orderOutcomeFailureThreshold {
		t.Fatalf("fallback limit = %d, want at least the threshold %d so a streak can be seen", spy.limit[0], orderOutcomeFailureThreshold)
	}
}

// The tail shows two failures and no success (insufficient: an older success may
// sit just past the window). The fallback replaces the tail result outright; here
// the authoritative history shows the order recovered, so it reads healthy.
func TestOrderOutcomeHealthy_FallbackReplacesInsufficientTailResult(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	spy := &outcomeFallbackSpy{runs: map[string][]orders.OrderRun{
		"recovering": {outcomeRunOK("recovering", now.Add(-10*time.Minute))},
	}}
	check, cityPath := outcomeFallbackCheck(t, []string{"recovering"}, spy)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFailed, Subject: "recovering", Ts: now.Add(-3 * time.Hour), Message: "exit status 1"},
		events.Event{Type: events.OrderFailed, Subject: "recovering", Ts: now.Add(-2 * time.Hour), Message: "exit status 1"},
	)

	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK {
		t.Fatalf("status = %v (%s), want StatusOK: the store says the newest run succeeded; details=%v", result.Status, result.Message, result.Details)
	}
	if got := outcomeDetailFor(t, result, "recovering"); !strings.Contains(got, "last run succeeded") {
		t.Fatalf("detail = %q, want it to report the store-backed success", got)
	}
	if got := spy.callCount(); got != 1 {
		t.Fatalf("fallback calls = %d, want 1", got)
	}
}

// (b) A tail that already settles the streak never touches the store: three
// failures reach the threshold, and a trailing success ends the walk.
func TestOrderOutcomeHealthy_SufficientTailNeverCallsFallback(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	spy := &outcomeFallbackSpy{runs: map[string][]orders.OrderRun{}}
	check, cityPath := outcomeFallbackCheck(t, []string{"broken", "healthy"}, spy)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFailed, Subject: "broken", Ts: now.Add(-6 * time.Hour), Message: "boom"},
		events.Event{Type: events.OrderFailed, Subject: "broken", Ts: now.Add(-5 * time.Hour), Message: "boom"},
		events.Event{Type: events.OrderFailed, Subject: "broken", Ts: now.Add(-4 * time.Hour), Message: "boom"},
		events.Event{Type: events.OrderFailed, Subject: "healthy", Ts: now.Add(-3 * time.Hour), Message: "boom"},
		events.Event{Type: events.OrderCompleted, Subject: "healthy", Ts: now.Add(-2 * time.Hour)},
	)

	result := check.Run(&CheckContext{CityPath: cityPath})

	if got := spy.callCount(); got != 0 {
		t.Fatalf("fallback called %d time(s) for %v, want 0: the tail already settles both orders", got, spy.calls)
	}
	if result.Status != StatusWarning {
		t.Fatalf("status = %v (%s), want StatusWarning for the 3-failure order", result.Status, result.Message)
	}
	if got := outcomeDetailFor(t, result, "broken"); !strings.Contains(got, "3 consecutive failures") {
		t.Fatalf("detail = %q, want the tail-derived streak", got)
	}
	if got := outcomeDetailFor(t, result, "healthy"); !strings.Contains(got, "last run succeeded") {
		t.Fatalf("detail = %q, want the tail-derived success", got)
	}
}

// (c) An in-flight run is neither a success nor a failure: it must be skipped
// without breaking the streak, wherever it sits.
func TestOrderOutcomeHealthy_FallbackSkipsActiveRuns(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	spy := &outcomeFallbackSpy{runs: map[string][]orders.OrderRun{
		"streaky": {
			outcomeRunActive("streaky", now.Add(-1*time.Minute)),
			outcomeRunFailed("streaky", now.Add(-1*time.Hour)),
			outcomeRunActive("streaky", now.Add(-90*time.Minute)),
			outcomeRunFailed("streaky", now.Add(-2*time.Hour)),
			outcomeRunFailed("streaky", now.Add(-3*time.Hour)),
		},
		"just-started": {
			outcomeRunActive("just-started", now.Add(-1*time.Minute)),
			outcomeRunOK("just-started", now.Add(-1*time.Hour)),
		},
		"only-active": {
			outcomeRunActive("only-active", now.Add(-1*time.Minute)),
		},
	}}
	check, cityPath := outcomeFallbackCheck(t, []string{"streaky", "just-started", "only-active"}, spy)

	result := check.Run(&CheckContext{CityPath: cityPath})

	if got := outcomeDetailFor(t, result, "streaky"); !strings.Contains(got, "consecutive failures") || strings.Contains(got, "under threshold") {
		t.Fatalf("streaky detail = %q, want a warning: active runs must not break the 3-failure streak", got)
	}
	if got := outcomeDetailFor(t, result, "just-started"); !strings.Contains(got, "last run succeeded") {
		t.Fatalf("just-started detail = %q, want success: an active run is not a failure", got)
	}
	if got := outcomeDetailFor(t, result, "only-active"); !strings.Contains(got, "no completed runs yet") {
		t.Fatalf("only-active detail = %q, want no completed runs: an active run is no outcome", got)
	}
}

// The OrderRun walk uses CreatedAt against the same controller-start grace the
// event walk uses: failures right after a restart are skipped, not counted.
func TestOrderOutcomeHealthy_FallbackAppliesStartGraceToRuns(t *testing.T) {
	start := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	spy := &outcomeFallbackSpy{runs: map[string][]orders.OrderRun{
		"post-restart": {
			outcomeRunFailed("post-restart", start.Add(3*time.Minute)),
			outcomeRunFailed("post-restart", start.Add(2*time.Minute)),
			outcomeRunFailed("post-restart", start.Add(1*time.Minute)),
			outcomeRunFailed("post-restart", start.Add(-1*time.Hour)),
			outcomeRunFailed("post-restart", start.Add(-2*time.Hour)),
			outcomeRunOK("post-restart", start.Add(-3*time.Hour)),
		},
	}}
	check, cityPath := outcomeFallbackCheck(t, []string{"post-restart"}, spy)
	writeOrderFiringTestEvents(t, cityPath, events.Event{Type: events.ControllerStarted, Ts: start})

	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK {
		t.Fatalf("status = %v (%s), want StatusOK: 3 of the 5 failures are post-restart noise; details=%v", result.Status, result.Message, result.Details)
	}
	if got := outcomeDetailFor(t, result, "post-restart"); !strings.Contains(got, "2 consecutive failure(s), under threshold") {
		t.Fatalf("detail = %q, want 2 counted failures under threshold", got)
	}
}

// A store error for one order is reported for that order, advisory, and never
// swallowed into a tail-derived verdict.
func TestOrderOutcomeHealthy_FallbackErrorIsReportedAdvisory(t *testing.T) {
	spy := &outcomeFallbackSpy{err: errors.New("dolt unreachable")}
	check, cityPath := outcomeFallbackCheck(t, []string{"unreadable"}, spy)

	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusError {
		t.Fatalf("status = %v (%s), want StatusError", result.Status, result.Message)
	}
	if result.Severity != SeverityAdvisory {
		t.Fatalf("severity = %v, want SeverityAdvisory: an unreadable store must not gate gc doctor", result.Severity)
	}
	if got := outcomeDetailFor(t, result, "unreadable"); !strings.Contains(got, "cannot read order history") || !strings.Contains(got, "dolt unreachable") {
		t.Fatalf("detail = %q, want the store error named for the order", got)
	}
}

// The fallback fan-out runs concurrently, bounded, once per insufficient order:
// the lookups are store round-trips and serial calls are what blow the budget.
func TestOrderOutcomeHealthy_FallbackFansOutBoundedConcurrently(t *testing.T) {
	const total = 20
	names := make([]string, total)
	for i := range names {
		names[i] = "fan-" + string(rune('a'+i))
	}
	cityPath, cfg := orderFiringTestCity(t)
	for _, name := range names {
		writeOrderFiringTestOrder(t, cityPath, name, "cooldown", "5m")
	}

	var inflight, peak, calls atomic.Int32
	overlap := make(chan struct{})
	var once sync.Once
	fn := func(order orders.Order, _ int) ([]orders.OrderRun, error) {
		calls.Add(1)
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		if n >= 2 {
			once.Do(func() { close(overlap) })
		}
		select {
		case <-overlap:
		case <-time.After(2 * time.Second):
		}
		inflight.Add(-1)
		return []orders.OrderRun{outcomeRunOK(order.ScopedName(), time.Now())}, nil
	}
	check := NewOrderOutcomeHealthyCheck(cfg, cityPath, WithOrderOutcomeRecentRunsFunc(fn))

	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK {
		t.Fatalf("status = %v (%s), want StatusOK", result.Status, result.Message)
	}
	if got := calls.Load(); got != total {
		t.Fatalf("fallback calls = %d, want exactly %d (one per insufficient order, none repeated by the classification loop)", got, total)
	}
	if got := peak.Load(); got < 2 {
		t.Fatalf("peak concurrent fallback calls = %d, want >= 2: lookups must not run serially", got)
	}
	if got := peak.Load(); got > orderFiringLastRunConcurrency {
		t.Fatalf("peak concurrent fallback calls = %d, want <= %d", got, orderFiringLastRunConcurrency)
	}
}

// The default shape (no fallback supplied) stays tail-only: an order with no
// outcome in the log is "no completed runs yet", exactly as before.
func TestOrderOutcomeHealthy_NilFallbackKeepsTailOnlyVerdict(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "never-ran", "cooldown", "5m")

	result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

	if got := outcomeDetailFor(t, result, "never-ran"); !strings.Contains(got, "no completed runs yet") {
		t.Fatalf("detail = %q, want the tail-only verdict", got)
	}
}

// Every event read is a bounded tail: a positive limit (limit <= 0 falls back
// to the unbounded all-archives read) and, for the rare controller.started
// type, a byte budget so a miss cannot degrade into a full-file backward walk.
func TestOrderOutcomeHealthy_EventReadsAreBoundedTails(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "bounded", "cooldown", "5m")

	var mu sync.Mutex
	type readCall struct {
		path   string
		filter events.Filter
		limit  int
	}
	var reads []readCall
	check := NewOrderOutcomeHealthyCheck(cfg, cityPath)
	check.readEvents = func(path string, filter events.Filter, limit int) ([]events.Event, error) {
		mu.Lock()
		defer mu.Unlock()
		reads = append(reads, readCall{path, filter, limit})
		return nil, nil
	}

	check.Run(&CheckContext{CityPath: cityPath})

	if len(reads) == 0 {
		t.Fatal("no event reads went through the injected tail reader; the check must read the log only via check.readEvents")
	}
	wantPath := filepath.Join(cityPath, ".gc", "events.jsonl")
	var sawOutcomeRead, sawStartRead bool
	for _, r := range reads {
		if r.path != wantPath {
			t.Errorf("read path = %q, want %q", r.path, wantPath)
		}
		if r.limit <= 0 {
			t.Errorf("read of %v has limit %d, want a positive limit: limit <= 0 is the unbounded all-archives read", r.filter.Types, r.limit)
		}
		types := append([]string{r.filter.Type}, r.filter.Types...)
		hasType := func(want string) bool {
			for _, ty := range types {
				if ty == want {
					return true
				}
			}
			return false
		}
		if hasType(events.OrderFailed) && hasType(events.OrderCompleted) {
			sawOutcomeRead = true
			if r.limit != orderOutcomeEventTailLimit {
				t.Errorf("outcome tail limit = %d, want orderOutcomeEventTailLimit (%d)", r.limit, orderOutcomeEventTailLimit)
			}
			if hasType(events.ControllerStarted) {
				t.Errorf("outcome read also asks for controller.started; its rare-type read needs its own byte budget")
			}
		}
		if hasType(events.ControllerStarted) {
			sawStartRead = true
			if r.filter.MaxScanBytes <= 0 {
				t.Errorf("controller.started read has MaxScanBytes %d, want a positive byte budget (#4418)", r.filter.MaxScanBytes)
			}
		}
	}
	if !sawOutcomeRead {
		t.Error("no read covered order.completed and order.failed together; one contiguous window is required so per-type tails cannot leave gaps in a streak")
	}
	if !sawStartRead {
		t.Error("no read covered controller.started")
	}
}

// A tail read that fails is an error result, not a silent empty verdict.
func TestOrderOutcomeHealthy_TailReadErrorIsReported(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "unread", "cooldown", "5m")
	check := NewOrderOutcomeHealthyCheck(cfg, cityPath)
	check.readEvents = func(string, events.Filter, int) ([]events.Event, error) {
		return nil, errors.New("disk on fire")
	}

	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusError || !strings.Contains(result.Message, "disk on fire") {
		t.Fatalf("status/message = %v / %q, want StatusError naming the read failure", result.Status, result.Message)
	}
	if result.Severity != SeverityAdvisory {
		t.Fatalf("severity = %v, want SeverityAdvisory", result.Severity)
	}
}

// A stalled store lookup must not wedge gc doctor: Run answers with an
// inconclusive, advisory, TimedOut result, the same shape order-firing-current
// uses.
func TestOrderOutcomeHealthy_TimesOutStalledOrderHistory(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "stalled", "cooldown", "5m")
	check := NewOrderOutcomeHealthyCheck(cfg, cityPath, WithOrderOutcomeRecentRunsFunc(
		func(orders.Order, int) ([]orders.OrderRun, error) {
			<-release
			return nil, nil
		}))
	check.historyTimeout = 20 * time.Millisecond

	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusError {
		t.Fatalf("status = %v (%s), want StatusError", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "order outcome history lookup timed out after 20ms") {
		t.Fatalf("message = %q, want the timeout diagnostic", result.Message)
	}
	if result.Severity != SeverityAdvisory {
		t.Fatalf("severity = %v, want SeverityAdvisory (a timeout is inconclusive, not proof of failing orders)", result.Severity)
	}
	if !result.TimedOut {
		t.Fatal("TimedOut = false, want true so callers can tell this from a confirmed failure")
	}
	if result.FixHint != orderFiringTimeoutHint {
		t.Fatalf("FixHint = %q, want %q", result.FixHint, orderFiringTimeoutHint)
	}
	if result.Name != orderOutcomeHealthyName {
		t.Fatalf("Name = %q, want %q", result.Name, orderOutcomeHealthyName)
	}
}
