//go:build integration

package conversationlane_test

// X8, order across a lane-host restart (DES-O1 §7.7): two producers write
// notices to one conversation while the controller's lane host dies by
// SIGKILL mid-stream and restarts. The first delivery of every op follows
// the arrival order recomputed from the rows, a repeat follows only a crash
// and carries the repeat line, and every op closes succeeded. The producers
// alternate in one goroutine, so commit order is created_at order:
// CompareOrder's interim key is the producer's created_at, which commit skew
// could reorder for truly concurrent producers until arrival_seq (OJ-10a).
// The stream goes on until the restarted host has taken x8AfterRestart of
// its ops live.

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
)

const (
	x8ProducerA = "gc:x8-a"
	x8OpsA      = 160
	x8OpsB      = 40
	// x8AfterRestart is how many ops at least the stream proposes after the
	// restarted controller answers.
	x8AfterRestart = 40
)

// laneOrder sorts ops as the lane delivers notices: CompareOrder, then each
// principal's client_seq ops in client_seq order within the slots they hold.
func laneOrder(ops []opjournal.Op) []opjournal.Op {
	out := append([]opjournal.Op(nil), ops...)
	slices.SortStableFunc(out, opjournal.CompareOrder)
	slots := map[string][]int{}
	var principals []string
	for i, op := range out {
		if op.ClientSeq == 0 {
			continue
		}
		if _, ok := slots[op.Producer.Principal]; !ok {
			principals = append(principals, op.Producer.Principal)
		}
		slots[op.Producer.Principal] = append(slots[op.Producer.Principal], i)
	}
	for _, p := range principals {
		idx := slots[p]
		members := make([]opjournal.Op, len(idx))
		for j, i := range idx {
			members[j] = out[i]
		}
		slices.SortStableFunc(members, func(a, b opjournal.Op) int {
			if a.ClientSeq != b.ClientSeq {
				if a.ClientSeq < b.ClientSeq {
					return -1
				}
				return 1
			}
			return opjournal.CompareOrder(a, b)
		})
		for j, i := range idx {
			out[i] = members[j]
		}
	}
	return out
}

func TestX8OrderAcrossLaneHostRestart(t *testing.T) {
	t.Parallel() // after the tests that measure latency, which run alone
	for _, tc := range []struct {
		name, failpoint string
	}{
		{name: "between deliveries", failpoint: "ctl:done:5"},
		{name: "inside a delivery", failpoint: "ctl:effect:5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parallelCity(t)
			runX8(t, tc.failpoint)
		})
	}
}

func runX8(t *testing.T, failpoint string) {
	c := newLaneCity(t, armSQLite, activityNone)
	killed := c.startController(failpoint)
	type restart struct {
		p   *ctlProc
		err error
	}
	restarted := make(chan restart, 1)
	go func() {
		<-killed.done
		p, err := c.launchController("")
		restarted <- restart{p, err}
	}()

	// The stream runs past its minimum until x8AfterRestart ops followed
	// the restarted controller's coming up, so the host restarts mid-stream
	// and takes the rest of the stream live. The SQLite handle is held
	// across the stream without blocking the restart's own store reads.
	var ids []string
	a, b := 0, 0
	var r restart
	var upAt time.Time
	afterUp := 0
	deadline := time.Now().Add(3 * time.Minute)
	c.mustStore(func(_ beads.Store, st *opjournal.Store) error {
		for a+b < x8OpsA+x8OpsB || afterUp < x8AfterRestart {
			if upAt.IsZero() {
				select {
				case r = <-restarted:
					if r.err != nil {
						return fmt.Errorf("restarting the controller: %w", r.err)
					}
					upAt = time.Now()
				default:
				}
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("after %d ops the controller had not died at %s and come back up (up at %v)", a+b, failpoint, upAt)
			}
			if !upAt.IsZero() {
				afterUp++
			}
			if (a+b)%5 == 4 {
				key := fmt.Sprintf("x8-b-%d", b)
				res := c.nudge("queue", key, fmt.Sprintf("x8 b %03d", b))
				if res.code() != 0 {
					return fmt.Errorf("nudge %s = %d: %s", key, res.code(), res.stderr)
				}
				ids = append(ids, c.cliOpID(key))
				b++
				continue
			}
			req := noticeRequest(x8ProducerA, fmt.Sprintf("x8-a-%d", a), fmt.Sprintf("x8 a %03d", a), opjournal.GuaranteeAtLeastOnce)
			req.Conversation, req.Kind, req.Executor = c.conv, opjournal.KindMessage, opjournal.ExecutorLane
			req.ClientSeq = uint64(a + 1)
			op, _, err := st.Propose(req)
			if err != nil {
				return err
			}
			ids = append(ids, op.ID)
			c.ring(op.ID)
			a++
			time.Sleep(40 * time.Millisecond)
		}
		return nil
	})
	t.Logf("x8: %d ops, the last %d after the restarted controller came up", len(ids), afterUp)
	if upAt.IsZero() || afterUp < x8AfterRestart {
		t.Fatalf("%d ops followed the restart, want at least %d", afterUp, x8AfterRestart)
	}
	c.ctl = r.p
	assertKilledByFailpoint(t, killed.cmd.ProcessState, readFile(killed.log), failpoint)
	ops := c.waitClosed(ctlLeaseRecovery+2*time.Minute, ids...)
	time.Sleep(settleWindow)

	byPayload := map[string]opjournal.Op{}
	for _, op := range ops {
		op = c.get(op.ID)
		if op.Outcome.Arm != opjournal.ArmSucceeded {
			t.Fatalf("op %s closed %s{%s}", op.ID, op.Outcome.Arm, op.Outcome.Reason)
		}
		byPayload[op.Payload] = op
	}
	deliveries := deliveriesOf(c.deliveries(), "x8 ")
	var first []string
	seen := map[string]int{}
	last := map[string]int{}
	for i, d := range deliveries {
		for _, p := range injectedPayloads(d.Text) {
			if _, ok := byPayload[p]; !ok {
				t.Fatalf("delivery %d carries %q, no op of this run", i, p)
			}
			seen[p]++
			last[p] = i
			if seen[p] == 1 {
				first = append(first, p)
				continue
			}
			if byPayload[p].MaybeDelivered == 0 || !strings.Contains(d.Text, opjournal.RedeliveryNote) {
				t.Fatalf("op %q repeated in delivery %d without a recorded crash or the repeat line", p, i)
			}
		}
	}
	var want []string
	for _, op := range laneOrder(ops) {
		want = append(want, op.Payload)
	}
	if !slices.Equal(first, want) {
		t.Fatalf("first deliveries in order\n%v\nwant\n%v", first, want)
	}
	seqOf := map[int]uint64{}
	for p, i := range last {
		seq := byPayload[p].Outcome.Result.Delivery.Seq
		if s, ok := seqOf[i]; ok && s != seq {
			t.Fatalf("delivery %d's ops record seqs %d and %d", i, s, seq)
		}
		seqOf[i] = seq
	}
	var prev uint64
	for i := range deliveries {
		seq, ok := seqOf[i]
		if !ok {
			continue
		}
		if seq <= prev {
			t.Fatalf("delivery %d records seq %d after %d", i, seq, prev)
		}
		prev = seq
	}
	c.assertNoIdentifier(c.ops())
}
