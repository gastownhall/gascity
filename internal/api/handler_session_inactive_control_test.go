package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// tmuxLikeProvider fails idle waits for a session with no runtime, as the
// tmux provider does ("session not found").
type tmuxLikeProvider struct {
	*runtime.Fake
}

func (p tmuxLikeProvider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	if !p.IsRunning(name) {
		return errors.New("session not found")
	}
	return p.Fake.WaitForIdle(ctx, name, timeout)
}

// TestStopAndKillOfInactiveSessionAreIdempotent pins the outcome of stopping
// or killing a session whose runtime is already gone, as observed on a live
// city after a kill: the record is asleep and there is no pane. There is no
// turn to interrupt and no process to end, so both operations already hold.
// They must report success instead of a 500 that a client cannot tell apart
// from a server failure; killing a closed session is already treated this way.
func TestStopAndKillOfInactiveSessionAreIdempotent(t *testing.T) {
	fs := newSessionFakeState(t)
	state := &stateWithSessionProvider{fakeState: fs, provider: tmuxLikeProvider{Fake: fs.sp}}
	h := newTestCityHandlerWith(t, state, New(state))
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "default", Title: "inactive-control", Command: "codex", WorkDir: "/tmp", Provider: "codex",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := fs.sp.Stop(info.SessionName); err != nil {
		t.Fatalf("stop runtime: %v", err)
	}
	if err := fs.cityBeadStore.SetMetadata(info.ID, "state", "asleep"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	for _, op := range []string{"kill", "stop"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/")+info.ID+"/"+op, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s of an asleep session with no runtime: status = %d, want 200; body: %s", op, rec.Code, rec.Body.String())
			continue
		}
		var body struct {
			Status string `json:"status"`
			ID     string `json:"id"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Errorf("%s: decode response: %v", op, err)
			continue
		}
		if body.Status != "ok" || body.ID != info.ID {
			t.Errorf("%s response = %+v, want status ok id %q", op, body, info.ID)
		}
	}
}
