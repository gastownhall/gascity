//go:build windows

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSlingForceRefusesABeadIDCmdExeWouldRead pins that a forced API sling,
// whose bead_id is caller text the store never checks, does not reach a
// custom sling_query on Windows when cmd.exe would read the ID as code.
func TestSlingForceRefusesABeadIDCmdExeWouldRead(t *testing.T) {
	state := newFakeMutatorState(t)
	state.cfg.Rigs[0].Prefix = "gc"
	state.cfg.Agents[0].SlingQuery = "custom-dispatch {}"
	srv := New(state)
	var calls []string
	srv.SlingRunnerFunc = func(_ string, command string, _ map[string]string) (string, error) {
		calls = append(calls, command)
		return "", nil
	}
	h := newTestCityHandlerWith(t, state, srv)

	body, err := json.Marshal(map[string]any{"target": "myrig/worker", "bead": "gc-1&calc", "force": true})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(string(body))))

	if rec.Code == http.StatusOK {
		t.Errorf("status = 200, want a refusal; body = %s", rec.Body.String())
	}
	if len(calls) != 0 {
		t.Errorf("the sling ran %q", calls)
	}
}
