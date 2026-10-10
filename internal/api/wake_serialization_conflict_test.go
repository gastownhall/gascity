package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// liveWakeConflictErr is the error the native Dolt store returned once its
// bounded merge-race retry (retryOnNativeDoltMergeRace, nativeWriteAttempts)
// was exhausted on the session wisp, verbatim from the HTTP 503 body that
// failed TestGCLiveContract_BeadsAndEvents in the ga-o01pz0 gate (ga-19ii7x).
const liveWakeConflictErr = "commit update wisp: Error 1213 (40001): serialization failure: " +
	"this transaction conflicts with a committed transaction from another client, try restarting transaction."

// wakeBatchConflictStore fails the session wake batch (the SetMetadataBatch
// that records wake_request=explicit) with liveWakeConflictErr.
type wakeBatchConflictStore struct {
	beads.Store
	sessionID   string
	wakeBatches int
}

func (s *wakeBatchConflictStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if id == s.sessionID && kvs["wake_request"] != "" {
		s.wakeBatches++
		return errors.New(liveWakeConflictErr)
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// A wake whose session write loses every bounded retry to a concurrent writer
// answers with the declared, retryable 503 store_conflict — the exact response
// TestGCLiveContract_BeadsAndEvents received under host load (ga-19ii7x).
func TestHandleSessionWake_SerializationConflictIsDeclaredRetryable503(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Conflicted")
	if err := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath)).Suspend(info.ID); err != nil {
		t.Fatalf("suspend session: %v", err)
	}
	conflicts := &wakeBatchConflictStore{Store: fs.cityBeadStore, sessionID: info.ID}
	fs.cityBeadStore = conflicts

	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newPostRequest(cityURL(fs, "/session/")+info.ID+"/wake", nil))

	if conflicts.wakeBatches != 1 {
		t.Fatalf("wake batch writes = %d, want 1; status=%d body=%s", conflicts.wakeBatches, w.Code, w.Body.String())
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("wake status = %d, want %d (declared, retryable); body: %s",
			w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem body: %v; body: %s", err, w.Body.String())
	}
	if problem.Code != "store-unavailable" || problem.Detail != "store_conflict: "+liveWakeConflictErr {
		t.Fatalf("problem = %+v, want code=store-unavailable detail=%q", problem, "store_conflict: "+liveWakeConflictErr)
	}
}
