package doctor

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// Bounded mode of order-outcome-healthy (ga-w17lm4u, ruling ga-k2v17pq 4.5).
//
// The soundness rule these tests pin: a bounded read may UNDER-claim and must
// never OVER-claim. For every order it yields exactly one of
//   - the exact verdict (identical status and detail text),
//   - a lower-bound warning ("at least N consecutive failures"), only where the
//     exact verdict is a warning of at least N failures, or
//   - unresolved, never OK and never a warning.
//
// Timestamps are assumed monotone with Seq (the log is append-only and the
// recorder stamps as it appends); every generated log honors that.

var boundedTestStart = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// writeBoundedTestLog writes evts under cityPath/.gc: the first `archived`
// events into a gzipped archive, the rest into the active events.jsonl.
// It returns the active path and the active file's size in bytes.
func writeBoundedTestLog(t testing.TB, cityPath string, evts []events.Event, archived int) (string, int64) {
	t.Helper()
	dir := filepath.Join(cityPath, ".gc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	encode := func(part []events.Event) []byte {
		var buf bytes.Buffer
		for _, e := range part {
			raw, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			buf.Write(raw)
			buf.WriteByte('\n')
		}
		return buf.Bytes()
	}
	if archived > 0 {
		name := fmt.Sprintf("events.jsonl.archive-20260101T000000Z-seq-%d-%d.gz", evts[0].Seq, evts[archived-1].Seq)
		var gz bytes.Buffer
		gw := gzip.NewWriter(&gz)
		if _, err := gw.Write(encode(evts[:archived])); err != nil {
			t.Fatal(err)
		}
		if err := gw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), gz.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	active := encode(evts[archived:])
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, active, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, int64(len(active))
}

// boundCoveringNewest returns a scan bound that covers the newest k lines of
// the file at path and not the line before them. The +1 lets the walk see the
// newline that ends line k+1, so line k is not mistaken for a partial one.
func boundCoveringNewest(t testing.TB, path string, k int) int64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	total := 0
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-k; i-- {
		total += len(lines[i]) + 1
	}
	return int64(total) + 1
}

type seqBuilder struct {
	seq  uint64
	evts []events.Event
}

func (b *seqBuilder) add(typ, subject string, ts time.Time, message string) {
	b.seq++
	b.evts = append(b.evts, events.Event{Seq: b.seq, Type: typ, Actor: "t", Subject: subject, Ts: ts, Message: message})
}

func (b *seqBuilder) noise(ts time.Time) {
	b.add("bead.updated", "gc-1", ts, strings.Repeat("n", 200))
}

type legacyVerdict struct {
	status CheckStatus
	detail string
}

// legacyOutcomeVerdicts is the pre-bounded-mode algorithm: one full read, then
// the unchanged consecutiveOrderFailures / classifyOrderOutcome pair.
func legacyOutcomeVerdicts(t testing.TB, eventPath string, ords []orders.Order, threshold int) map[string]legacyVerdict {
	t.Helper()
	log, err := readOrderOutcomeLog(nil, eventPath)
	if err != nil {
		t.Fatalf("legacy read: %v", err)
	}
	out := make(map[string]legacyVerdict, len(ords))
	for _, o := range ords {
		name := o.ScopedName()
		streak, last, saw, skipped := consecutiveOrderFailures(log.bySubject[name], name, log.starts, orderOutcomeStartGrace)
		status, detail := classifyOrderOutcome(o, streak, threshold, last, saw, skipped)
		out[name] = legacyVerdict{status, detail}
	}
	return out
}

func boundedVerdict(t testing.TB, eventPath string, bound int64, o orders.Order, threshold int) orderOutcomeVerdict {
	t.Helper()
	log, err := readOrderOutcomeLogBounded(nil, eventPath, bound)
	if err != nil {
		t.Fatalf("bounded read (bound=%d): %v", bound, err)
	}
	return judgeOrderOutcome(o, log, threshold, orderOutcomeStartGrace)
}

func testOrder(name string) orders.Order { return orders.Order{Name: name, Trigger: "cooldown"} }

// TestBoundedFailuresNearAnUnscannedControllerStartAreUnresolved: the grace
// relation of a failure depends on every controller.started within the grace
// window before it. Here the only start is in an archive the bounded read never
// opens, so counting these failures could over-claim (exact skips all three as
// startup noise and says OK), and so could ignoring them. They are unresolved.
func TestBoundedFailuresNearAnUnscannedControllerStartAreUnresolved(t *testing.T) {
	var b seqBuilder
	b.add(events.ControllerStarted, "gc", boundedTestStart, "")
	b.noise(boundedTestStart.Add(30 * time.Second))
	for i := 1; i <= 3; i++ {
		b.add(events.OrderFailed, "a", boundedTestStart.Add(time.Duration(i)*time.Minute), "exit status 1")
	}
	path, size := writeBoundedTestLog(t, t.TempDir(), b.evts, 1)

	exact := legacyOutcomeVerdicts(t, path, []orders.Order{testOrder("a")}, 3)["a"]
	if exact.status != StatusOK {
		t.Fatalf("fixture drifted: exact status = %v (%s), want OK (all failures inside the grace window)", exact.status, exact.detail)
	}
	got := boundedVerdict(t, path, size+1, testOrder("a"), 3)

	if !got.Unresolved {
		t.Fatalf("verdict = %+v, want unresolved: the start that decides these failures is outside the scan", got)
	}
	if !strings.HasPrefix(got.Detail, "a: unresolved") {
		t.Fatalf("detail = %q, want it to name the order and say unresolved", got.Detail)
	}
}

// TestBoundedCountsFailuresPastTheGraceHorizonAsALowerBound: when the oldest
// scanned event is more than the grace before a failure, no unscanned start can
// be near it, so the failure counts. The streak is still only a lower bound
// because older failures may exist beyond the window.
func TestBoundedCountsFailuresPastTheGraceHorizonAsALowerBound(t *testing.T) {
	var b seqBuilder
	b.add(events.ControllerStarted, "gc", boundedTestStart, "")
	b.noise(boundedTestStart.Add(time.Hour))
	for i := 2; i <= 4; i++ {
		b.add(events.OrderFailed, "a", boundedTestStart.Add(time.Duration(i)*time.Hour), "exit status 1")
	}
	path, size := writeBoundedTestLog(t, t.TempDir(), b.evts, 1)

	got := boundedVerdict(t, path, size+1, testOrder("a"), 3)

	if got.Unresolved || got.Status != StatusWarning {
		t.Fatalf("verdict = %+v, want a warning: 3 failures are provably past every start", got)
	}
	if !strings.HasPrefix(got.Detail, "a: at least 3 consecutive failures") || !strings.Contains(got.Detail, "older history not scanned") {
		t.Fatalf("detail = %q, want the lower-bound wording", got.Detail)
	}
}

// TestBoundedRareOrderBeyondTheWindowIsUnresolvedNotOK is the starvation shape
// a global tail limit gets wrong: the busy order floods the window and the
// rare, broken order has all of its failures outside it.
func TestBoundedRareOrderBeyondTheWindowIsUnresolvedNotOK(t *testing.T) {
	var b seqBuilder
	for i := 0; i < 3; i++ {
		b.add(events.OrderFailed, "rare", boundedTestStart.Add(time.Duration(i)*time.Hour), "exit status 1")
	}
	for i := 0; i < 200; i++ {
		b.noise(boundedTestStart.Add(10*time.Hour + time.Duration(i)*time.Minute))
		b.add(events.OrderCompleted, "busy", boundedTestStart.Add(10*time.Hour+time.Duration(i)*time.Minute+time.Second), "")
	}
	path, _ := writeBoundedTestLog(t, t.TempDir(), b.evts, 0)

	exact := legacyOutcomeVerdicts(t, path, []orders.Order{testOrder("rare")}, 3)["rare"]
	if exact.status != StatusWarning {
		t.Fatalf("fixture drifted: exact = %v (%s), want a warning", exact.status, exact.detail)
	}
	got := boundedVerdict(t, path, boundCoveringNewest(t, path, 100), testOrder("rare"), 3)

	if !got.Unresolved {
		t.Fatalf("verdict = %+v, want unresolved: its history is outside the window", got)
	}
}

// TestBoundedSuccessInsideTheWindowResolvesLikeExact: a success newer than the
// older history ends the streak, so older history cannot change the verdict.
func TestBoundedSuccessInsideTheWindowResolvesLikeExact(t *testing.T) {
	var b seqBuilder
	for i := 0; i < 5; i++ {
		b.add(events.OrderFailed, "a", boundedTestStart.Add(time.Duration(i)*time.Hour), "exit status 1")
	}
	b.add(events.OrderCompleted, "a", boundedTestStart.Add(6*time.Hour), "")
	for i := 0; i < 100; i++ {
		b.noise(boundedTestStart.Add(7*time.Hour + time.Duration(i)*time.Minute))
	}
	path, _ := writeBoundedTestLog(t, t.TempDir(), b.evts, 0)

	exact := legacyOutcomeVerdicts(t, path, []orders.Order{testOrder("a")}, 3)["a"]
	got := boundedVerdict(t, path, boundCoveringNewest(t, path, 101), testOrder("a"), 3)

	if got.Unresolved || got.Status != exact.status || got.Detail != exact.detail {
		t.Fatalf("verdict = %+v, want the exact verdict %+v", got, exact)
	}
}

// TestBoundedFullCoverageEqualsExact: a bound that covers a log with no archives
// is the whole history, so every verdict is the exact one — including the
// no-event order.
func TestBoundedFullCoverageEqualsExact(t *testing.T) {
	var b seqBuilder
	b.add(events.ControllerStarted, "gc", boundedTestStart, "")
	b.add(events.OrderFailed, "a", boundedTestStart.Add(time.Minute), "exit status 1")
	b.add(events.OrderFailed, "a", boundedTestStart.Add(time.Hour), "exit status 1")
	b.add(events.OrderFailed, "a", boundedTestStart.Add(2*time.Hour), "exit status 1")
	b.add(events.OrderCompleted, "b", boundedTestStart.Add(3*time.Hour), "")
	path, size := writeBoundedTestLog(t, t.TempDir(), b.evts, 0)

	ords := []orders.Order{testOrder("a"), testOrder("b"), testOrder("never-ran")}
	exact := legacyOutcomeVerdicts(t, path, ords, 3)
	for _, o := range ords {
		got := boundedVerdict(t, path, size*2, o, 3)
		want := exact[o.ScopedName()]
		if got.Unresolved || got.Status != want.status || got.Detail != want.detail {
			t.Errorf("%s: verdict = %+v, want exact %+v", o.Name, got, want)
		}
	}
}

// TestBoundedOrderWithNoEventsIsUnresolvedWhileAnArchiveExists: "no completed
// runs yet" is a claim about the whole history, which an archive makes
// unreadable here.
func TestBoundedOrderWithNoEventsIsUnresolvedWhileAnArchiveExists(t *testing.T) {
	var b seqBuilder
	b.add(events.OrderFailed, "old", boundedTestStart, "exit status 1")
	b.add(events.OrderCompleted, "b", boundedTestStart.Add(time.Hour), "")
	path, size := writeBoundedTestLog(t, t.TempDir(), b.evts, 1)

	got := boundedVerdict(t, path, size*2, testOrder("never-ran"), 3)

	if !got.Unresolved {
		t.Fatalf("verdict = %+v, want unresolved: an archive may hold its runs", got)
	}
}

// TestOrderOutcomeBoundedRunReportsUnresolvedByName covers the aggregate: an
// unresolved order is never counted as failing, never as succeeding, and is
// listed by name in the additive payload field.
func TestOrderOutcomeBoundedRunReportsUnresolvedByName(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "orders"), "a", "cooldown", "5m")
	writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "orders"), "b", "cooldown", "5m")
	var bld seqBuilder
	for i := 0; i < 3; i++ {
		bld.add(events.OrderFailed, "b", boundedTestStart.Add(time.Duration(i)*time.Hour), "exit status 1")
	}
	bld.add(events.OrderCompleted, "a", boundedTestStart.Add(5*time.Hour), "")
	_, size := writeBoundedTestLog(t, cityPath, bld.evts, 3)

	result := NewOrderOutcomeHealthyCheck(cfg, cityPath, WithOrderOutcomeScanBound(size*2)).Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK {
		t.Fatalf("status = %v (%s), want OK: nothing decided is failing", result.Status, result.Message)
	}
	if result.Message == "all scheduled orders succeeding" || !strings.Contains(result.Message, "1 order(s) unresolved") {
		t.Fatalf("message = %q, want it to say 1 order is unresolved instead of claiming all succeed", result.Message)
	}
	payload, ok := result.Payload.(OrderOutcomePayload)
	if !ok || !reflect.DeepEqual(payload.Unresolved, []string{"b"}) {
		t.Fatalf("payload = %#v, want OrderOutcomePayload{Unresolved: [b]}", result.Payload)
	}
	raw, err := json.Marshal(result.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"unresolved":["b"]}` {
		t.Fatalf("payload JSON = %s, want only the additive unresolved list", raw)
	}
	if len(result.Details) != 2 {
		t.Fatalf("details = %v, want one line per monitored order", result.Details)
	}
}

// TestOrderOutcomeBoundedRunEqualsExactWhenEveryOrderResolves: bounded mode
// must be invisible whenever the bound was enough — same status, message,
// details, hint and no payload.
func TestOrderOutcomeBoundedRunEqualsExactWhenEveryOrderResolves(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "orders"), "a", "cooldown", "5m")
	writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "orders"), "b", "cron", "0 * * * *")
	var bld seqBuilder
	bld.add(events.ControllerStarted, "gc", boundedTestStart, "")
	for i := 1; i <= 3; i++ {
		bld.add(events.OrderFailed, "a", boundedTestStart.Add(time.Duration(i)*time.Hour), "exit status 1")
	}
	bld.add(events.OrderCompleted, "b", boundedTestStart.Add(5*time.Hour), "")
	_, size := writeBoundedTestLog(t, cityPath, bld.evts, 0)
	ctx := &CheckContext{CityPath: cityPath}

	exact := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(ctx)
	bounded := NewOrderOutcomeHealthyCheck(cfg, cityPath, WithOrderOutcomeScanBound(size*2)).Run(ctx)

	if exact.Status != StatusWarning || exact.Payload != nil {
		t.Fatalf("fixture drifted: exact = %v payload=%v", exact.Status, exact.Payload)
	}
	if !reflect.DeepEqual(exact, bounded) {
		t.Fatalf("bounded result differs from exact although every order resolved:\n exact   %+v\n bounded %+v", exact, bounded)
	}
}

type boundedTally struct {
	equal, equalIncomplete, lowerBound, unresolved, complete int
}

// TestOrderOutcomeBoundedDifferential runs seeded generated logs — archives,
// controller restarts with failures inside the grace window, thresholds 2/3/4,
// orders that never ran, interleaved orders — through three properties:
//  1. exact mode (Run with no bound) equals the legacy full-read algorithm;
//  2. wherever a bounded verdict resolves, it is the exact verdict;
//  3. otherwise it is a lower-bound warning that exact confirms, or unresolved;
//     never OK where exact warns, never a warning where exact is OK.
func TestOrderOutcomeBoundedDifferential(t *testing.T) {
	if testing.Short() {
		t.Skip("generates and replays ~120 logs")
	}
	const orderCount = 8
	var ords []orders.Order
	for i := 0; i < orderCount; i++ {
		o := testOrder(fmt.Sprintf("o%d", i))
		if i%2 == 1 {
			o.Trigger = "cron"
		}
		ords = append(ords, o)
	}
	var tally boundedTally

	for seed := int64(1); seed <= 60; seed++ {
		evts := generateBoundedOutcomeEvents(rand.New(rand.NewSource(seed)), ords[:orderCount-2])
		for _, archived := range []int{0, len(evts) / 3} {
			cityPath, cfg := orderFiringTestCity(t)
			for _, o := range ords {
				trigger, timing := "cooldown", "5m"
				if o.Trigger == "cron" {
					trigger, timing = "cron", "0 * * * *"
				}
				writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "orders"), o.Name, trigger, timing)
			}
			path, size := writeBoundedTestLog(t, cityPath, evts, archived)

			for _, threshold := range []int{2, 3, 4} {
				exact := legacyOutcomeVerdicts(t, path, ords, threshold)

				// Property 1: Run without a bound is the legacy algorithm.
				check := NewOrderOutcomeHealthyCheck(cfg, cityPath)
				check.threshold = threshold
				run := check.Run(&CheckContext{CityPath: cityPath})
				if run.Payload != nil {
					t.Fatalf("seed %d: exact run carries payload %v", seed, run.Payload)
				}
				for _, o := range ords {
					if !containsString(run.Details, exact[o.Name].detail) {
						t.Fatalf("seed %d th=%d: exact run lacks legacy detail %q in %v", seed, threshold, exact[o.Name].detail, run.Details)
					}
				}

				for _, frac := range []float64{0.03, 0.15, 0.4, 0.8, 1.0, 2.0} {
					bound := max(int64(frac*float64(size)), 1)
					log, err := readOrderOutcomeLogBounded(nil, path, bound)
					if err != nil {
						t.Fatalf("seed %d: bounded read: %v", seed, err)
					}
					if log.complete {
						tally.complete++
					}
					for _, o := range ords {
						assertBoundedVerdictSound(t, &tally, log.complete, judgeOrderOutcome(o, log, threshold, orderOutcomeStartGrace), exact[o.Name], o.Name,
							fmt.Sprintf("seed=%d archived=%d th=%d bound=%d", seed, archived, threshold, bound))
					}
				}
			}
		}
	}

	t.Logf("verdict classes: %+v", tally)
	// Guard against a generator that never reaches an interesting case: each
	// class must have occurred, or the properties above proved nothing.
	if tally.equal == 0 || tally.equalIncomplete == 0 || tally.lowerBound == 0 || tally.unresolved == 0 || tally.complete == 0 {
		t.Fatalf("differential run is vacuous for some class: %+v", tally)
	}
}

func assertBoundedVerdictSound(t *testing.T, tally *boundedTally, complete bool, got orderOutcomeVerdict, exact legacyVerdict, name, where string) {
	t.Helper()
	switch {
	case got.Unresolved:
		tally.unresolved++
		if !strings.HasPrefix(got.Detail, name+": unresolved") {
			t.Fatalf("%s %s: unresolved detail = %q", where, name, got.Detail)
		}
	case got.Status == exact.status && got.Detail == exact.detail:
		tally.equal++
		if !complete {
			tally.equalIncomplete++
		}
	case got.Status == StatusWarning && exact.status == StatusWarning && strings.HasPrefix(got.Detail, name+": at least "):
		tally.lowerBound++
		var atLeast, actual int
		if _, err := fmt.Sscanf(strings.TrimPrefix(got.Detail, name+": at least "), "%d consecutive", &atLeast); err != nil {
			t.Fatalf("%s %s: unparsable lower bound %q: %v", where, name, got.Detail, err)
		}
		if _, err := fmt.Sscanf(strings.TrimPrefix(exact.detail, name+": "), "%d consecutive", &actual); err != nil {
			t.Fatalf("%s %s: unparsable exact %q: %v", where, name, exact.detail, err)
		}
		if atLeast > actual {
			t.Fatalf("%s %s: lower bound %d exceeds the exact streak %d (%q vs %q)", where, name, atLeast, actual, got.Detail, exact.detail)
		}
	default:
		t.Fatalf("%s %s: bounded verdict {%v %q} is neither exact {%v %q}, a confirmed lower bound, nor unresolved",
			where, name, got.Status, got.Detail, exact.status, exact.detail)
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// generateBoundedOutcomeEvents builds a seq- and time-monotone log: padded
// noise, order outcomes with a per-order failure bias, and controller restarts
// that are often followed by a burst of failures inside the grace window. The
// last orders passed in are never picked, so callers get no-event orders.
func generateBoundedOutcomeEvents(rng *rand.Rand, active []orders.Order) []events.Event {
	failBias := make([]float64, len(active))
	for i := range failBias {
		failBias[i] = []float64{0.05, 0.5, 0.9, 1.0}[rng.Intn(4)]
	}
	var b seqBuilder
	ts := boundedTestStart
	advance := func() {
		switch rng.Intn(8) {
		case 0:
			ts = ts.Add(time.Duration(1+rng.Intn(6)) * time.Hour)
		default:
			ts = ts.Add(time.Duration(20+rng.Intn(280)) * time.Second)
		}
	}
	steps := 150 + rng.Intn(150)
	for i := 0; i < steps; i++ {
		advance()
		switch r := rng.Intn(100); {
		case r < 45:
			b.noise(ts)
		case r < 52:
			b.add(events.ControllerStarted, "gc", ts, "")
			if rng.Intn(10) < 6 {
				for j, n := 0, 1+rng.Intn(4); j < n; j++ {
					ts = ts.Add(time.Duration(30+rng.Intn(120)) * time.Second)
					b.add(events.OrderFailed, active[rng.Intn(len(active))].Name, ts, "boom")
				}
			}
		default:
			idx := rng.Intn(len(active))
			if rng.Float64() < failBias[idx] {
				b.add(events.OrderFailed, active[idx].Name, ts, fmt.Sprintf("exit status %d", 1+rng.Intn(3)))
			} else {
				b.add(events.OrderCompleted, active[idx].Name, ts, "")
			}
		}
	}
	return b.evts
}
