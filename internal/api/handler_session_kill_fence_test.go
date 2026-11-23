package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// killStopHookProvider runs atStop when the kill stops the runtime, so a test
// can read the row at that instant.
type killStopHookProvider struct {
	runtime.Provider
	atStop func()
}

func (p *killStopHookProvider) Stop(name string) error {
	if p.atStop != nil {
		p.atStop()
	}
	return p.Provider.Stop(name)
}

// apiKillRoutes are the two API session kill routes: the supervisor's city
// route (Huma) and the legacy session mux.
func apiKillRoutes(t *testing.T, fs *fakeState) map[string]func(id string) *httptest.ResponseRecorder {
	return map[string]func(id string) *httptest.ResponseRecorder{
		"city route": func(id string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			newTestCityHandler(t, fs).ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/")+id+"/kill", nil))
			return rec
		},
		"legacy mux": func(id string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			New(fs).legacySessionHandler().ServeHTTP(rec, newPostRequest("/v0/session/"+id+"/kill", nil))
			return rec
		},
	}
}

// An API session kill writes the kill fence before it stops the runtime, as
// `gc session kill` does (mc-fr77m): at the stop the row already reads
// asleep/killed with state_reason kill-pending, so a controller pass in that
// window leaves it alone instead of reading a crash. Afterwards the fence is
// lifted and the asleep/killed intent stays.
func TestAPISessionKillWritesKillFence(t *testing.T) {
	for route := range apiKillRoutes(t, nil) {
		t.Run(route, func(t *testing.T) {
			fs := newSessionFakeState(t)
			var atStop beads.Bead
			stops := 0
			fs.sessionProvider = &killStopHookProvider{Provider: fs.sp}
			info := createTestSession(t, fs.cityBeadStore, fs.sp, "kill-fence")
			fs.sessionProvider.(*killStopHookProvider).atStop = func() {
				stops++
				atStop, _ = fs.cityBeadStore.Get(info.ID)
			}

			rec := apiKillRoutes(t, fs)[route](info.ID)
			if rec.Code != http.StatusOK {
				t.Fatalf("kill status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			if stops != 1 {
				t.Fatalf("runtime stops = %d, want 1", stops)
			}
			if got := atStop.Metadata["state"]; got != string(session.StateAsleep) {
				t.Errorf("state at the stop = %q, want asleep", got)
			}
			if got := atStop.Metadata["sleep_reason"]; got != string(session.SleepReasonKilled) {
				t.Errorf("sleep_reason at the stop = %q, want killed", got)
			}
			if got := atStop.Metadata["state_reason"]; got != session.KillPendingReason {
				t.Errorf("state_reason at the stop = %q, want %q (the kill fence)", got, session.KillPendingReason)
			}
			final, _ := fs.cityBeadStore.Get(info.ID)
			if final.Metadata["state"] != string(session.StateAsleep) || final.Metadata["sleep_reason"] != string(session.SleepReasonKilled) || final.Metadata["state_reason"] != "" {
				t.Errorf("row after the kill = %v, want asleep/killed with the fence lifted", final.Metadata)
			}
		})
	}
}

// A stop that fails and leaves the runtime alive fails the API kill, and the
// fence is rolled back: the row must not claim a teardown that did not happen.
func TestAPISessionKillStopFailureRestoresRow(t *testing.T) {
	for route := range apiKillRoutes(t, nil) {
		t.Run(route, func(t *testing.T) {
			fs := newSessionFakeState(t)
			info := createTestSession(t, fs.cityBeadStore, fs.sp, "kill-fence-fail")
			before, _ := fs.cityBeadStore.Get(info.ID)
			fs.sp.StopErrors = map[string]error{info.SessionName: errors.New("stop refused")}

			rec := apiKillRoutes(t, fs)[route](info.ID)
			if rec.Code == http.StatusOK {
				t.Fatalf("kill status = 200 with the runtime still running, want a failure")
			}
			after, _ := fs.cityBeadStore.Get(info.ID)
			for _, k := range []string{"state", "sleep_reason", "state_reason", "slept_at"} {
				if after.Metadata[k] != before.Metadata[k] {
					t.Errorf("%s = %q after the failed kill, want %q restored", k, after.Metadata[k], before.Metadata[k])
				}
			}
		})
	}
}
