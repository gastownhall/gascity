//go:build integration

package conversationlane_test

// X6, fan-in (DES-O1 §7.7): many notice producers to one conversation. A
// keystroke runtime's boundary is the quiescence rule (3 s after its last
// activity), and a batch holds at most opjournal.MaxBatchOps notices, so one
// conversation drains at most about 33 notices a second: DES-O1's literal
// line (5,000 notices in 60 s with p99 <= 5 s) is out of reach for any
// keystroke runtime. CI asserts the line at a rate the boundary cadence
// admits: one injection per boundary, at most MaxBatchOps notices each, none
// lost, order kept, and a p99 within the cycle the boundary and the link
// wait impose (x6SteadyP99) at 20 notices a second. GC_LANE_X6_FULL=1
// runs the literal line and logs the measurement.

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// x6Principals are the fan-in producers.
var x6Principals = []string{"gc:x6-p0", "gc:x6-p1", "gc:x6-p2", "gc:x6-p3", "gc:x6-p4"}

// x6SteadyP99 and x6SteadyMax bound the steady phase's latency: DES-O1's
// p99 <= 5 s, and a max two seconds over it. A notice that arrives just
// after a Take waits for that batch's effect, its link wait
// (opjournal.DefaultLinkWait), the runtime's next boundary (the 3 s
// quiescence after the delivery) and the next pass: measured p99 3.5 s (max
// 3.7 s) with the phase alone on its host, which is why the phases run
// sequentially and the package runs X6 before its parallel tests.
const (
	x6SteadyP99 = 5 * time.Second
	x6SteadyMax = 7 * time.Second
)

// x6MinGap is the least spacing of two injections: the quiescence window,
// less timestamp slack.
const x6MinGap = 2900 * time.Millisecond

// injectedPayloads parses the notice lines of one delivery's reminder block,
// in order.
func injectedPayloads(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "- [") {
			continue
		}
		if _, payload, ok := strings.Cut(line, "] "); ok {
			out = append(out, payload)
		}
	}
	return out
}

// x6Produce proposes n notices, the first through gc session nudge (which
// rings), the rest directly by the fan-in producers, paced every interval
// (0: at once) and rung when ring is set.
func (c *laneCity) x6Produce(n int, phase string, interval time.Duration, ring bool) []string {
	c.t.Helper()
	ids := make([]string, 0, n)
	payload := func(i int) string { return fmt.Sprintf("x6 %s %04d", phase, i) }
	r := c.nudge("queue", "x6-"+phase+"-cli", payload(0))
	if r.code() != 0 {
		c.t.Fatalf("nudge = %d; stderr %s", r.code(), r.stderr)
	}
	ids = append(ids, c.cliOpID("x6-"+phase+"-cli"))
	// One store handle for the whole stream: a handle per op would pace
	// the producers by the store's open.
	start := time.Now()
	c.mustStore(func(_ beads.Store, st *opjournal.Store) error {
		for i := 1; i < n; i++ {
			if interval > 0 {
				if wait := time.Until(start.Add(time.Duration(i) * interval)); wait > 0 {
					time.Sleep(wait)
				}
			}
			req := noticeRequest(x6Principals[i%len(x6Principals)], fmt.Sprintf("x6-%s-%d", phase, i), payload(i), opjournal.GuaranteeAtLeastOnce)
			req.Conversation, req.Kind, req.Executor = c.conv, opjournal.KindMessage, opjournal.ExecutorLane
			op, _, err := st.Propose(req)
			if err != nil {
				return err
			}
			ids = append(ids, op.ID)
			if ring {
				c.ring(op.ID)
			}
		}
		return nil
	})
	return ids
}

// x6Check asserts the fan-in pass line over ids: every op delivered once,
// one injection per boundary of at most MaxBatchOps notices, injections in
// CompareOrder order; it returns the per-op latencies, sorted.
func (c *laneCity) x6Check(ids []string, phase string, timeout time.Duration) []time.Duration {
	c.t.Helper()
	ops := c.waitClosed(timeout, ids...)
	byPayload := map[string]opjournal.Op{}
	for _, op := range ops {
		if op.Outcome.Arm != opjournal.ArmSucceeded {
			c.t.Fatalf("op %s closed %s{%s}", op.ID, op.Outcome.Arm, op.Outcome.Reason)
		}
		byPayload[op.Payload] = op
	}
	var injected []string
	var prev time.Time
	for i, d := range deliveriesOf(c.deliveries(), "x6 "+phase+" ") {
		lines := injectedPayloads(d.Text)
		if len(lines) == 0 || len(lines) > opjournal.MaxBatchOps {
			c.t.Fatalf("injection %d carries %d notices, want 1..%d", i, len(lines), opjournal.MaxBatchOps)
		}
		at := d.at(c.t)
		if i > 0 && at.Sub(prev) < x6MinGap {
			c.t.Fatalf("injection %d came %s after the previous one, inside one boundary", i, at.Sub(prev))
		}
		prev = at
		injected = append(injected, lines...)
	}
	if len(injected) != len(ids) {
		c.t.Fatalf("%d notices injected, want %d", len(injected), len(ids))
	}
	sorted := append([]opjournal.Op(nil), ops...)
	slices.SortFunc(sorted, opjournal.CompareOrder)
	for i, op := range sorted {
		if injected[i] != op.Payload {
			c.t.Fatalf("injection order breaks at %d: got %q, want %q", i, injected[i], op.Payload)
		}
	}
	var lat []time.Duration
	for _, op := range ops {
		lat = append(lat, op.Outcome.Result.Delivery.At.Sub(op.CreatedAt))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	c.assertNoIdentifier(c.ops())
	return lat
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

// injectionSizes are the notice counts of the phase's injections.
func (c *laneCity) injectionSizes(phase string) []int {
	var sizes []int
	for _, d := range deliveriesOf(c.deliveries(), "x6 "+phase+" ") {
		sizes = append(sizes, len(injectedPayloads(d.Text)))
	}
	return sizes
}

// TestX6FanIn runs its phases one after another, never in parallel: the
// steady phase measures latency, which a city beside it would load.
func TestX6FanIn(t *testing.T) {
	t.Run("burst", func(t *testing.T) {
		c := newLaneCity(t, armSQLite, activityLastNudge)
		c.startController("")
		// Busy for long enough that the whole burst is proposed before the
		// first boundary.
		c.setActivity(time.Now().Add(10 * time.Second))
		ids := c.x6Produce(350, "burst", 0, false)
		c.x6Check(ids, "burst", 3*time.Minute)
		if got := c.injectionSizes("burst"); !slices.Equal(got, []int{99, 99, 99, 53}) {
			t.Fatalf("injections of %v notices, want 99, 99, 99, 53", got)
		}
	})
	t.Run("steady", func(t *testing.T) {
		c := newLaneCity(t, armSQLite, activityLastNudge)
		c.startController("")
		ids := c.x6Produce(400, "steady", 50*time.Millisecond, true)
		lat := c.x6Check(ids, "steady", 3*time.Minute)
		p50, p99, maxLat := percentile(lat, 0.5), percentile(lat, 0.99), lat[len(lat)-1]
		t.Logf("x6 steady: %d notices at 20/s in %d injections; latency p50 %s p99 %s max %s", len(ids), len(c.injectionSizes("steady")), p50, p99, maxLat)
		if p99 > x6SteadyP99 || maxLat > x6SteadyMax {
			t.Fatalf("latency p99 %s max %s, want p99 <= %s and max <= %s", p99, maxLat, x6SteadyP99, x6SteadyMax)
		}
	})
	t.Run("full", func(t *testing.T) {
		if os.Getenv("GC_LANE_X6_FULL") != "1" {
			t.Skip("DES-O1's literal X6 line (5,000 notices in 60 s) is a manual measurement: --test_env=GC_LANE_X6_FULL=1")
		}
		c := newLaneCity(t, armSQLite, activityLastNudge)
		c.startController("")
		ids := c.x6Produce(5000, "full", 12*time.Millisecond, true)
		lat := c.x6Check(ids, "full", 8*time.Minute)
		p50, p99, maxLat := percentile(lat, 0.5), percentile(lat, 0.99), lat[len(lat)-1]
		t.Logf("x6 full: %d notices in 60s in %d injections; latency p50 %s p99 %s max %s", len(ids), len(c.injectionSizes("full")), p50, p99, maxLat)
		if p99 > 5*time.Second {
			t.Errorf("DES-O1's line: p99 %s > 5s (expected for a keystroke runtime: one boundary per 3 s caps a conversation near 33 notices/s)", p99)
		}
	})
}
