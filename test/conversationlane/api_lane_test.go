//go:build integration

package conversationlane_test

// The API's message producers under lane authority (design slice 6): with
// message listed, POST /submit proposes a lane op the controller's lane host
// delivers, and the request's terminal event arrives from the op's outcome.

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/loopbackport"
	"github.com/gastownhall/gascity/internal/opjournal"
)

func TestAPISubmitUnderLaneIsDeliveredByTheHost(t *testing.T) {
	parallelCity(t)
	port, err := loopbackport.Reserve()
	if err != nil {
		t.Fatalf("reserving the API port: %v", err)
	}
	lc := newLaneCityWith(t, armSQLite, activityNone, cityOptions{apiPort: port})
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

	const text = "api lane delivers delta"
	a := c.apiPost(x12Verb{endpoint: "submit"}, sessionName, "api-lane-1", text)
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
	o := c.pollOutcome(a.requestID, a.cursor)
	if o.err != nil || o.state == "pending" || (o.class != classDelivered && o.class != classQueued) {
		t.Fatalf("outcome = %+v, want the request's result", o)
	}
	lc.assertNoIdentifier(lc.ops())
}
