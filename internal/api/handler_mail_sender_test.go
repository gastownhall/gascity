package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestMailSenderIsAConfiguredIdentity (architecture B v2, mc-v8g4p): with no
// caller identity to bind a sender to (mc-8e9ci), POST /mail and its reply
// send only as a configured identity: the operator ("human", or the
// dashboard's DASHBOARD_OPERATOR_WIRE_ALIAS) or a configured agent. Any
// other, an empty one included (v5.10f), is refused with 422 at body.from,
// nothing sent.
func TestMailSenderIsAConfiguredIdentity(t *testing.T) {
	t.Setenv("DASHBOARD_OPERATOR_WIRE_ALIAS", "ops")
	for _, c := range []struct {
		from string
		want int
	}{
		{"", http.StatusUnprocessableEntity},
		{"  ", http.StatusUnprocessableEntity},
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

// TestMailSenderResolvesSessionIdentities (the #7766 review, M01-M03):
// besides the operator and a configured agent, a sender is a configured
// named session's identity, or a live session of a configured agent (a pool
// instance such as myrig/worker-3, by its alias); a live session whose
// template is no configured agent is no sender.
func TestMailSenderResolvesSessionIdentities(t *testing.T) {
	for _, c := range []struct {
		name, from string
		want       int
	}{
		{"a configured named session", "reviewer", http.StatusCreated},
		{"a live pool instance of a configured agent", "myrig/worker-3", http.StatusCreated},
		{"a live session of no configured agent", "ghost-1", http.StatusUnprocessableEntity},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := newFakeState(t)
			state.cfg.NamedSessions = append(state.cfg.NamedSessions, config.NamedSession{Name: "reviewer", Template: "worker", Dir: "myrig"})
			state.cityBeadStore = beads.NewMemStore()
			store := state.SessionsBeadStore().Store
			createTestSessionBead(t, store, map[string]string{"template": "myrig/worker", "alias": "myrig/worker-3", "session_name": "worker-3", "state": "active"}, "worker-3")
			createTestSessionBead(t, store, map[string]string{"template": "gone", "alias": "ghost-1", "session_name": "ghost-1", "state": "active"}, "ghost-1")
			h := newTestCityHandler(t, state)
			rec := beadRequest(t, h, "POST", cityURL(state, "/mail"), `{"from":"`+c.from+`","to":"worker","subject":"s","body":"b"}`)
			if rec.Code != c.want {
				t.Fatalf("from %q: %d %s, want %d", c.from, rec.Code, rec.Body.String(), c.want)
			}
		})
	}
}
