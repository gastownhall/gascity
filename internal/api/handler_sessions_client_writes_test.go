package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestSessionPatchWritesClientMetadata (architecture B, the #7742 review's
// ruling): the session route, not the generic bead API, writes a client's
// real_world_app_* keys on a session row: PATCH /session/{id} sets and
// clears them, and refuses any other key with 422 at its location, writing
// nothing.
func TestSessionPatchWritesClientMetadata(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandlerWith(t, fs, New(fs))
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "S")

	rec := beadRequest(t, h, "PATCH", cityURL(fs, "/session/")+info.ID, `{"metadata":{"real_world_app_project_id":"p-1","real_world_app_session_kind":"provider"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if got, _ := fs.cityBeadStore.Get(info.ID); got.Metadata["real_world_app_project_id"] != "p-1" || got.Metadata["real_world_app_session_kind"] != "provider" {
		t.Fatalf("metadata %v, want the client's keys written", got.Metadata)
	}
	if rec := beadRequest(t, h, "PATCH", cityURL(fs, "/session/")+info.ID, `{"metadata":{"real_world_app_project_id":""}}`); rec.Code != http.StatusOK {
		t.Fatalf("clear: status %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if got, _ := fs.cityBeadStore.Get(info.ID); got.Metadata["real_world_app_project_id"] != "" {
		t.Fatalf("metadata %v, want the key cleared", got.Metadata)
	}
	before, _ := fs.cityBeadStore.Get(info.ID)
	rec = beadRequest(t, h, "PATCH", cityURL(fs, "/session/")+info.ID, `{"title":"T","metadata":{"real_world_app_x":"1","pin_awake":"true"}}`)
	got, _ := fs.cityBeadStore.Get(info.ID)
	if rec.Code != http.StatusUnprocessableEntity || strings.Join(problemLocations(t, rec), ",") != "body.metadata.pin_awake" || got.Title != before.Title || got.Metadata["real_world_app_x"] != "" {
		t.Fatalf("status %d (%s), row %q %v: want 422 at body.metadata.pin_awake, nothing written", rec.Code, rec.Body.String(), got.Title, got.Metadata)
	}
}

// TestSessionPinRoutes (the #7742 review's ruling): POST /session/{id}/pin
// and /unpin set and clear pin_awake, the write gc session pin and unpin
// make, now that the generic bead API never writes a session row; a closed
// session is a 409.
func TestSessionPinRoutes(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandlerWith(t, fs, New(fs))
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "S")
	for _, c := range []struct{ path, want string }{{"/pin", "true"}, {"/unpin", ""}} {
		if rec := beadRequest(t, h, "POST", cityURL(fs, "/session/")+info.ID+c.path, ""); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d (%s), want 200", c.path, rec.Code, rec.Body.String())
		}
		if got, _ := fs.cityBeadStore.Get(info.ID); got.Metadata["pin_awake"] != c.want {
			t.Fatalf("%s: pin_awake %q, want %q", c.path, got.Metadata["pin_awake"], c.want)
		}
	}
	if err := fs.cityBeadStore.Close(info.ID); err != nil {
		t.Fatal(err)
	}
	if rec := beadRequest(t, h, "POST", cityURL(fs, "/session/")+info.ID+"/pin", ""); rec.Code != http.StatusConflict {
		t.Fatalf("closed: status %d (%s), want 409", rec.Code, rec.Body.String())
	}
}

// TestSessionCreateRefusesNonClientMetadata: POST /sessions takes only a
// client's real_world_app_* keys; any other is refused with 422 at its
// location before anything is created.
func TestSessionCreateRefusesNonClientMetadata(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandlerWith(t, fs, New(fs))
	before, _ := fs.cityBeadStore.ListOpen()
	rec := beadRequest(t, h, "POST", cityURL(fs, "/sessions"), `{"kind":"agent","name":"myrig/worker","metadata":{"held_until":"9999-01-01T00:00:00Z"}}`)
	after, _ := fs.cityBeadStore.ListOpen()
	if rec.Code != http.StatusUnprocessableEntity || strings.Join(problemLocations(t, rec), ",") != "body.metadata.held_until" || len(after) != len(before) {
		t.Fatalf("status %d (%s), beads %d -> %d: want 422 at body.metadata.held_until, nothing created", rec.Code, rec.Body.String(), len(before), len(after))
	}
}

// TestSessionCreateWritesClientMetadata (the #7742 review): POST /sessions,
// agent and provider, writes the client's real_world_app_* keys with the
// create itself, so the row carries them before the controller can see it
// and before success is emitted: a later write of them is held here, and the
// row already carries them at success.
func TestSessionCreateWritesClientMetadata(t *testing.T) {
	for _, body := range []string{
		`{"kind":"provider","name":"test-agent","title":"t","metadata":{"real_world_app_session_kind":"provider"}}`,
		`{"kind":"agent","name":"myrig/worker","async":true,"metadata":{"real_world_app_session_kind":"provider"}}`,
	} {
		fs := newSessionFakeState(t)
		blocking := &blockingSetMetadataBatchStore{
			Store:       fs.cityBeadStore,
			shouldBlock: func(kvs map[string]string) bool { _, ok := kvs["real_world_app_session_kind"]; return ok },
			entered:     make(chan struct{}),
			release:     make(chan struct{}),
		}
		fs.cityBeadStore = blocking
		h := newTestCityHandlerWith(t, fs, New(fs))
		rec := beadRequest(t, h, "POST", cityURL(fs, "/sessions"), body)
		if rec.Code != http.StatusAccepted {
			close(blocking.release)
			t.Fatalf("%s: status %d (%s), want 202", body, rec.Code, rec.Body.String())
		}
		var resp struct {
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			close(blocking.release)
			t.Fatal(err)
		}
		success, failure := waitForSessionCreateResult(t, fs.eventProv, resp.RequestID)
		got := beads.Bead{}
		if success != nil {
			got, _ = blocking.Get(success.Session.ID)
		}
		close(blocking.release)
		if success == nil {
			t.Fatalf("%s: create failed: %+v", body, failure)
		}
		if got.Metadata["real_world_app_session_kind"] != "provider" {
			t.Fatalf("%s: metadata at success %v, want the client's key written with the create", body, got.Metadata)
		}
	}
}

// TestSessionRoutesRefuseAnInvalidClientKey (R05): a real_world_app_* key
// that is not a valid bead-metadata key (bd would split "x=y") is refused
// with 422 at its location by PATCH /session/{id} and POST /sessions,
// writing nothing.
func TestSessionRoutesRefuseAnInvalidClientKey(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandlerWith(t, fs, New(fs))
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "S")
	before, _ := fs.cityBeadStore.Get(info.ID)
	rec := beadRequest(t, h, "PATCH", cityURL(fs, "/session/")+info.ID, `{"metadata":{"real_world_app_x=held_until":"1"}}`)
	got, _ := fs.cityBeadStore.Get(info.ID)
	if rec.Code != http.StatusUnprocessableEntity || strings.Join(problemLocations(t, rec), ",") != "body.metadata.real_world_app_x=held_until" || len(got.Metadata) != len(before.Metadata) {
		t.Fatalf("patch: status %d (%s), metadata %v: want 422 at the key, nothing written", rec.Code, rec.Body.String(), got.Metadata)
	}
	all, _ := fs.cityBeadStore.ListOpen()
	rec = beadRequest(t, h, "POST", cityURL(fs, "/sessions"), `{"kind":"agent","name":"myrig/worker","metadata":{"real_world_app_x=y":"1"}}`)
	after, _ := fs.cityBeadStore.ListOpen()
	if rec.Code != http.StatusUnprocessableEntity || strings.Join(problemLocations(t, rec), ",") != "body.metadata.real_world_app_x=y" || len(after) != len(all) {
		t.Fatalf("create: status %d (%s), beads %d -> %d: want 422 at the key, nothing created", rec.Code, rec.Body.String(), len(all), len(after))
	}
}
