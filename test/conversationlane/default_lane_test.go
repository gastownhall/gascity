//go:build integration

package conversationlane_test

// The builtin default (OJ-21 21a): a SQLite split city with no [ops]
// authoritative runs message, interrupt and respond on the conversation
// lane. Its controller's lane host latches the default, the API's submit
// proposes a lane op the host delivers, and gc session nudge's queue-mode
// notice is a lane op the host delivers at the boundary.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/loopbackport"
	"github.com/gastownhall/gascity/internal/opjournal"
)

func TestSQLiteDefaultConfigDeliversOnTheLane(t *testing.T) {
	parallelCity(t)
	port, err := loopbackport.Reserve()
	if err != nil {
		t.Fatalf("reserving the API port: %v", err)
	}
	lc := newLaneCityWith(t, armSQLite, activityNone, cityOptions{apiPort: port, defaultOps: true})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	hc := &http.Client{Timeout: x12RequestTimeout}
	raw, err := genclient.NewClient(base, genclient.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	typed, err := genclient.NewClientWithResponses(base, genclient.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	c := &x12City{laneCity: lc, port: port, name: filepath.Base(lc.root), raw: raw, typed: typed}
	lc.startController("")

	reply, err := lc.socket("lane-status")
	if err != nil {
		t.Fatalf("lane-status: %v", err)
	}
	var st struct {
		State        string   `json:"state"`
		Kinds        []string `json:"kinds"`
		KindsDefault bool     `json:"kinds_default"`
	}
	if err := json.Unmarshal([]byte(reply), &st); err != nil {
		t.Fatalf("decoding lane-status %q: %v", reply, err)
	}
	if st.State != "running" || !st.KindsDefault || !slices.Equal(st.Kinds, []string{"message", "interrupt", "respond"}) {
		t.Fatalf("lane-status %+v, want the default kinds running", st)
	}

	const text = "default lane delivers epsilon"
	a := c.apiPost(x12Verb{endpoint: "submit"}, sessionName, "default-1", text)
	if a.err != nil || a.status != http.StatusAccepted || a.requestID == "" {
		t.Fatalf("submit = %s (%v)", a.String(), a.err)
	}
	op := lc.waitClosed(time.Minute, a.requestID)[0]
	if op.Executor != opjournal.ExecutorLane || op.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op %+v, want a lane op closed succeeded", op)
	}
	if ds := deliveriesOf(lc.deliveries(), text); len(ds) != 1 || ds[0].Text != text {
		t.Fatalf("deliveries %+v, want the prompt verbatim once", ds)
	}

	const notice = "default lane notice zeta"
	q := lc.nudge("queue", "default-notice-1", notice)
	if q.code() != 0 || q.stdout != "Queued nudge for "+sessionName+" in the conversation lane\n" {
		t.Fatalf("queue nudge = %d, %q; stderr %s", q.code(), q.stdout, q.stderr)
	}
	lc.waitClosed(time.Minute, lc.cliOpID("default-notice-1"))
	ds := deliveriesOf(lc.deliveries(), notice)
	if len(ds) != 1 || !strings.Contains(ds[0].Text, notice) {
		t.Fatalf("deliveries %+v, want the notice once", ds)
	}
	lc.assertNoIdentifier(lc.ops())
}
