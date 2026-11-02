package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// retryExhaustedConflictStore returns the serialization conflict that
// NativeDoltStore.Update surfaces once its bounded retry
// (retryOnNativeDoltSerializationConflict, nativeWriteAttempts) is exhausted.
// The text is verbatim from the observed HTTP 500 body in
// /var/tmp/gc-local-tests.Py5Sbj/integration-rest-full-2-of-8.log (ga-67pslx).
type retryExhaustedConflictStore struct {
	beads.Store
	suspendUpdates int
}

func (s *retryExhaustedConflictStore) Update(id string, opts beads.UpdateOpts) error {
	if opts.Metadata["state"] == "suspended" {
		s.suspendUpdates++
		return fmt.Errorf("updating bead %q: sql commit (regular): Error 1213 (40001): "+
			"serialization failure: this transaction conflicts with a committed transaction "+
			"from another client, try restarting transaction", id)
	}
	return s.Store.Update(id, opts)
}

// A retry-exhausted Dolt serialization conflict is retryable and must not reach
// the client as 500. The suspend route declares 401/403/404/409/503 and not 500
// (internal/api/supervisor_city_routes.go), so a 500 here is also an undeclared
// response.
func TestHandleSessionSuspend_SerializationConflictIsNotUndeclared500(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Conflicted")
	conflicts := &retryExhaustedConflictStore{Store: fs.cityBeadStore}
	fs.cityBeadStore = conflicts

	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	w := httptest.NewRecorder()
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/suspend", nil)
	h.ServeHTTP(w, r)

	if conflicts.suspendUpdates == 0 {
		t.Fatalf("suspension-state write never reached the store; status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Code == http.StatusInternalServerError {
		t.Fatalf("suspend leaked a retryable serialization conflict as an undeclared 500; body: %s", w.Body.String())
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("suspend status = %d, want %d (declared, retryable); body: %s",
			w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
}
