package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestMailSenderIsAConfiguredIdentity (architecture B v2, mc-v8g4p): with no
// caller identity to bind a sender to (mc-8e9ci), POST /mail and its reply
// send only as a configured identity: the operator ("human", or the
// dashboard's DASHBOARD_OPERATOR_WIRE_ALIAS), a configured agent, or no
// sender at all. Any other is refused with 422 at body.from, nothing sent.
func TestMailSenderIsAConfiguredIdentity(t *testing.T) {
	t.Setenv("DASHBOARD_OPERATOR_WIRE_ALIAS", "ops")
	for _, c := range []struct {
		from string
		want int
	}{
		{"", http.StatusCreated},
		{"human", http.StatusCreated},
		{"ops", http.StatusCreated},
		{"worker", http.StatusCreated},
		{"myrig/worker", http.StatusCreated},
		{"mayor", http.StatusUnprocessableEntity},
		{"Human ", http.StatusUnprocessableEntity},
	} {
		state := newFakeState(t)
		h := newTestCityHandler(t, state)
		before, _ := state.cityMailProv.Inbox("myrig/worker")
		rec := beadRequest(t, h, "POST", cityURL(state, "/mail"), `{"from":"`+c.from+`","to":"worker","subject":"s","body":"b"}`)
		after, _ := state.cityMailProv.Inbox("myrig/worker")
		if rec.Code != c.want {
			t.Fatalf("from %q: status %d (%s), want %d", c.from, rec.Code, rec.Body.String(), c.want)
		}
		if c.want == http.StatusUnprocessableEntity && (strings.Join(problemLocations(t, rec), ",") != "body.from" || len(after) != len(before)) {
			t.Fatalf("from %q: errors %v, inbox %d -> %d: want body.from and nothing sent", c.from, problemLocations(t, rec), len(before), len(after))
		}
	}
	// A reply is held to the same senders.
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	msg, err := state.cityMailProv.Send("human", "myrig/worker", "Hi", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if rec := beadRequest(t, h, "POST", cityURL(state, "/mail/")+msg.ID+"/reply", `{"from":"mallory","subject":"re","body":"r"}`); rec.Code != http.StatusUnprocessableEntity || strings.Join(problemLocations(t, rec), ",") != "body.from" {
		t.Fatalf("reply from mallory: status %d (%s), want 422 at body.from", rec.Code, rec.Body.String())
	}
	if rec := beadRequest(t, h, "POST", cityURL(state, "/mail/")+msg.ID+"/reply", `{"from":"worker","subject":"re","body":"r"}`); rec.Code != http.StatusCreated {
		t.Fatalf("reply from worker: status %d (%s), want 201", rec.Code, rec.Body.String())
	}
}
