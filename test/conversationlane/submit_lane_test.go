//go:build integration

package conversationlane_test

// gc session submit under lane authority with the controller down (slice
// 7b, SESSION-START-025): with no API to answer, the command accepts the
// operator's message as the API does (lanesubmit.Accept). Its content-only
// inline pass delivers a message at an idle boundary by itself, once; a
// busy session's message waits in the lane, and the restarted controller
// delivers it once. A same-key rerun replays the op.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
)

func TestCLISubmitUnderLaneControllerDown(t *testing.T) {
	t.Parallel() // after the tests that measure latency, which run alone
	t.Run("idle", func(t *testing.T) {
		parallelCity(t)
		c := newLaneCity(t, armSQLite, activityNone)
		c.startController("")
		c.killController()

		r := c.cli(nil, "session", "submit", "--key", "submit-1", sessionName, "submit delivers alpha")
		if r.code() != 0 || r.stdout != "Submitted to "+sessionName+"\n" {
			t.Fatalf("submit = %d, %q; stderr %s", r.code(), r.stdout, r.stderr)
		}
		if strings.Contains(r.stderr, "gc session submit") {
			t.Fatalf("stderr %q, want no submit warning on a delivered message", r.stderr)
		}
		op := c.get(c.cliOpID("submit-1"))
		if op.Executor != opjournal.ExecutorLane || op.Outcome.Arm != opjournal.ArmSucceeded || op.Outcome.Result.Via != "live" {
			t.Fatalf("op %+v, want a lane op succeeded via live", op)
		}
		for _, tr := range op.Transitions[1:] {
			if !strings.HasPrefix(tr.By, "lane:cli.") {
				t.Fatalf("transition %+v, want the CLI's lane", tr)
			}
		}
		if ds := deliveriesOf(c.deliveries(), "submit delivers alpha"); len(ds) != 1 || ds[0].Text != "submit delivers alpha" {
			t.Fatalf("deliveries %+v, want the message verbatim once", ds)
		}

		replay := c.cli(nil, "session", "submit", "--json", "--key", "submit-1", sessionName, "submit delivers alpha")
		got := submitJSON(t, replay)
		if got["outcome"] != "submitted" || got["queued"] != false || got["replayed"] != true || got["op_id"] != op.ID || got["key"] != "submit-1" ||
			!strings.Contains(replay.stderr, "replayed op "+op.ID) {
			t.Fatalf("replay JSON %v, stderr %q", got, replay.stderr)
		}
		if ds := deliveriesOf(c.deliveries(), "submit delivers alpha"); len(ds) != 1 {
			t.Fatalf("deliveries %+v after the replay, want still one", ds)
		}

		second := c.cli(nil, "session", "submit", "--json", "--key", "submit-2", sessionName, "submit delivers beta")
		got = submitJSON(t, second)
		if got["outcome"] != "submitted" || got["queued"] != false || got["op_id"] != c.cliOpID("submit-2") || got["key"] != "submit-2" {
			t.Fatalf("JSON %v; stderr %s", got, second.stderr)
		}
		c.assertNoIdentifier(c.ops())
	})
	t.Run("busy", func(t *testing.T) {
		parallelCity(t)
		c := newLaneCity(t, armSQLite, activityLastNudge)
		c.startController("")
		c.killController()
		c.setActivity(time.Now().Add(10 * time.Second))

		r := c.cli(nil, "session", "submit", "--key", "submit-busy", sessionName, "submit busy delta")
		if r.code() != 0 || r.stdout != "Queued follow-up for "+sessionName+"\n" {
			t.Fatalf("submit = %d, %q; stderr %s", r.code(), r.stdout, r.stderr)
		}
		if !strings.Contains(r.stderr, "gc session submit: warning: no controller lane host answered") || len(c.deliveries()) != 0 {
			t.Fatalf("stderr %q, deliveries %+v; want the no-host warning and none", r.stderr, c.deliveries())
		}
		queued := submitJSON(t, c.cli(nil, "session", "submit", "--json", "--key", "submit-busy", sessionName, "submit busy delta"))
		if queued["outcome"] != "queued" || queued["queued"] != true || queued["op_id"] != c.cliOpID("submit-busy") {
			t.Fatalf("same-key rerun JSON %v, want the open op queued", queued)
		}
		c.startController("")
		op := c.waitClosed(time.Minute, c.cliOpID("submit-busy"))[0]
		ds := deliveriesOf(c.deliveries(), "submit busy delta")
		if len(ds) != 1 || ds[0].Text != "submit busy delta" || op.Outcome.Arm != opjournal.ArmSucceeded {
			t.Fatalf("deliveries %+v, op %+v; want the controller's one delivery", ds, op)
		}
		c.assertNoIdentifier(c.ops())
	})
}

// submitJSON decodes gc session submit --json's result, which must exit 0.
func submitJSON(t *testing.T, r cliResult) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil || r.code() != 0 {
		t.Fatalf("submit --json = %d, %q (%v); stderr %s", r.code(), r.stdout, err, r.stderr)
	}
	return got
}
