//go:build integration

package conversationlane_test

// The CLI inline pass with the controller down (DES-O1 §7.6, §7.7): gc
// session nudge delivers a prompt at a boundary by itself, leaves a busy
// session's prompt and every queue-mode notice to the controller's lane
// host, and the restarted controller delivers them.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
)

func TestCLIInlinePassControllerDown(t *testing.T) {
	t.Parallel() // after the tests that measure latency, which run alone
	t.Run("idle", func(t *testing.T) {
		parallelCity(t)
		c := newLaneCity(t, armSQLite, activityNone)
		c.startController("")
		c.killController()

		r := c.nudge("wait-idle", "inline-1", "inline delivers alpha")
		if r.code() != 0 || r.stdout != "Nudged "+sessionName+"\n" {
			t.Fatalf("nudge = %d, %q; stderr %s", r.code(), r.stdout, r.stderr)
		}
		// The city's own config warnings aside, the nudge prints nothing:
		// the ring is silent for a delivered op.
		if strings.Contains(r.stderr, "gc session nudge") {
			t.Fatalf("stderr %q, want no nudge warning on a delivered nudge", r.stderr)
		}
		op := c.get(c.cliOpID("inline-1"))
		if op.Outcome.Arm != opjournal.ArmSucceeded || op.Outcome.Result.Via != "live" {
			t.Fatalf("op %+v, want succeeded via live", op)
		}
		for _, tr := range op.Transitions[1:] {
			if !strings.HasPrefix(tr.By, "lane:cli.") {
				t.Fatalf("transition %+v, want the CLI's lane", tr)
			}
		}

		r = c.cli(nil, "session", "nudge", "--json", "--key", "inline-2", sessionName, "inline delivers beta")
		var got map[string]any
		if err := json.Unmarshal([]byte(r.stdout), &got); err != nil || r.code() != 0 {
			t.Fatalf("nudge --json = %d, %q (%v); stderr %s", r.code(), r.stdout, err, r.stderr)
		}
		if got["outcome"] != "delivered" || got["queued"] != false || got["op_id"] != c.cliOpID("inline-2") || got["key"] != "inline-2" {
			t.Fatalf("JSON %v", got)
		}

		q := c.nudge("queue", "inline-3", "inline queues gamma")
		if q.code() != 0 || q.stdout != "Queued nudge for "+sessionName+" in the conversation lane\n" {
			t.Fatalf("queue nudge = %d, %q; stderr %s", q.code(), q.stdout, q.stderr)
		}
		if !strings.Contains(q.stderr, "warning: no controller lane host answered") {
			t.Fatalf("stderr %q, want the no-host warning", q.stderr)
		}
		if calls := c.runtimeCallsBy(q.state.Pid()); len(calls) != 0 {
			t.Fatalf("queue mode made runtime calls %v", calls)
		}
		c.startController("")
		notice := c.waitClosed(time.Minute, c.cliOpID("inline-3"))[0]
		ds := deliveriesOf(c.deliveries(), "inline queues gamma")
		if len(ds) != 1 || ds[0].Text != render(t, false, notice) || !strings.Contains(ds[0].Text, "- [operator] inline queues gamma") {
			t.Fatalf("deliveries %+v, want the notice in the reminder block", ds)
		}
		c.assertNoIdentifier(c.ops())
	})
	t.Run("busy", func(t *testing.T) {
		parallelCity(t)
		c := newLaneCity(t, armSQLite, activityLastNudge)
		c.startController("")
		c.killController()
		c.setActivity(time.Now().Add(10 * time.Second))

		r := c.nudge("wait-idle", "inline-busy", "inline busy delta")
		want := "Queued nudge for " + sessionName + " in the conversation lane (the session is mid-turn; the controller delivers it at the next idle boundary)\n"
		if r.code() != 0 || r.stdout != want {
			t.Fatalf("nudge = %d, %q; stderr %s", r.code(), r.stdout, r.stderr)
		}
		if !strings.Contains(r.stderr, "warning: no controller lane host answered") || len(c.deliveries()) != 0 {
			t.Fatalf("stderr %q, deliveries %+v", r.stderr, c.deliveries())
		}
		c.startController("")
		op := c.waitClosed(time.Minute, c.cliOpID("inline-busy"))[0]
		ds := c.deliveries()
		if len(ds) != 1 || ds[0].Text != "inline busy delta" || op.Outcome.Arm != opjournal.ArmSucceeded {
			t.Fatalf("deliveries %+v, op %+v", ds, op)
		}
		if at := ds[0].at(t); at.Before(time.Now().Add(-time.Minute)) {
			t.Fatalf("delivered at %s", at)
		}
	})
}

// runtimeCallsBy lists the fake runtime ops a process with pid ran on a
// session: the capability handshake (protocol), which names none, is not
// one.
func (c *laneCity) runtimeCallsBy(pid int) []string {
	c.t.Helper()
	data, err := os.ReadFile(filepath.Join(c.state, "ops.jsonl"))
	if err != nil {
		c.t.Fatalf("reading the runtime's op log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var inv struct {
			PPID int    `json:"ppid"`
			Op   string `json:"op"`
		}
		if err := json.Unmarshal([]byte(line), &inv); err != nil {
			c.t.Fatalf("op line %q: %v", line, err)
		}
		if inv.PPID == pid && inv.Op != "protocol" {
			out = append(out, inv.Op)
		}
	}
	return out
}
