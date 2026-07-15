package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/worker"
)

func newSessionFakeState(t *testing.T) *fakeState {
	t.Helper()
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	return fs
}

const testEventTimeout = 5 * time.Second

func decodeAsyncAccepted(t *testing.T, body io.Reader) asyncAcceptedBody {
	t.Helper()

	var accepted asyncAcceptedBody
	if err := json.NewDecoder(body).Decode(&accepted); err != nil {
		t.Fatalf("decode async accepted body: %v", err)
	}
	if accepted.RequestID == "" {
		t.Fatal("async accepted body missing request_id")
	}
	return accepted
}

// waitForSessionCreateResult waits for either a session create success or a request.failed event
// matching session.create and requestID. Returns the success payload and true, or the failure payload and false.
func waitForSessionCreateResult(t *testing.T, prov events.Provider, requestID string) (*SessionCreateSucceededPayload, *RequestFailedPayload) {
	t.Helper()
	deadline := time.Now().Add(testEventTimeout)
	for time.Now().Before(deadline) {
		successEvents, _ := prov.List(events.Filter{Type: events.RequestResultSessionCreate})
		for _, e := range successEvents {
			var p SessionCreateSucceededPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil && requestIDMatches(p.RequestID, requestID) {
				return &p, nil
			}
		}
		failedEvents, _ := prov.List(events.Filter{Type: events.RequestFailed})
		for _, e := range failedEvents {
			var p RequestFailedPayload
			if json.Unmarshal(e.Payload, &p) == nil && p.Operation == RequestOperationSessionCreate && requestIDMatches(p.RequestID, requestID) {
				return nil, &p
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for session create result")
	return nil, nil
}

func TestWaitForSessionCreateResultMatchesRequestID(t *testing.T) {
	prov := events.NewFake()
	first, err := json.Marshal(SessionCreateSucceededPayload{RequestID: "req-old"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(SessionCreateSucceededPayload{RequestID: "req-want"})
	if err != nil {
		t.Fatal(err)
	}
	prov.Record(events.Event{Type: events.RequestResultSessionCreate, Payload: first})
	prov.Record(events.Event{Type: events.RequestResultSessionCreate, Payload: second})

	success, failure := waitForSessionCreateResult(t, prov, "req-want")
	if failure != nil {
		t.Fatalf("unexpected failure: %+v", failure)
	}
	if success == nil || success.RequestID != "req-want" {
		t.Fatalf("success = %+v, want request_id req-want", success)
	}
}

// waitForSessionMessageResult waits for session message success or failure.
func waitForSessionMessageResult(t *testing.T, prov events.Provider, requestID string) (*SessionMessageSucceededPayload, *RequestFailedPayload) {
	t.Helper()
	deadline := time.Now().Add(testEventTimeout)
	for time.Now().Before(deadline) {
		successEvents, _ := prov.List(events.Filter{Type: events.RequestResultSessionMessage})
		for _, e := range successEvents {
			var p SessionMessageSucceededPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil && requestIDMatches(p.RequestID, requestID) {
				return &p, nil
			}
		}
		failedEvents, _ := prov.List(events.Filter{Type: events.RequestFailed})
		for _, e := range failedEvents {
			var p RequestFailedPayload
			if json.Unmarshal(e.Payload, &p) == nil && p.Operation == RequestOperationSessionMessage && requestIDMatches(p.RequestID, requestID) {
				return nil, &p
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for session message result")
	return nil, nil
}

// waitForSessionSubmitResult waits for session submit success or failure.
func waitForSessionSubmitResult(t *testing.T, prov events.Provider, requestID string) (*SessionSubmitSucceededPayload, *RequestFailedPayload) {
	t.Helper()
	deadline := time.Now().Add(testEventTimeout)
	for time.Now().Before(deadline) {
		successEvents, _ := prov.List(events.Filter{Type: events.RequestResultSessionSubmit})
		for _, e := range successEvents {
			var p SessionSubmitSucceededPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil && requestIDMatches(p.RequestID, requestID) {
				return &p, nil
			}
		}
		failedEvents, _ := prov.List(events.Filter{Type: events.RequestFailed})
		for _, e := range failedEvents {
			var p RequestFailedPayload
			if json.Unmarshal(e.Payload, &p) == nil && p.Operation == RequestOperationSessionSubmit && requestIDMatches(p.RequestID, requestID) {
				return nil, &p
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for session submit result")
	return nil, nil
}

func requestIDMatches(got, want string) bool {
	return got == want
}

// waitForRequestFailed polls for a request.failed event with the given request_id.
func waitForRequestFailed(t *testing.T, prov events.Provider, requestID string, timeout time.Duration) *RequestFailedPayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		failedEvents, _ := prov.List(events.Filter{Type: events.RequestFailed})
		for _, e := range failedEvents {
			var p RequestFailedPayload
			if json.Unmarshal(e.Payload, &p) == nil && p.RequestID == requestID {
				return &p
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for request.failed with request_id=%q", requestID)
	return nil
}

// waitForNSessionCreateEvents waits until at least n session create success events have been published.
func waitForNSessionCreateEvents(t *testing.T, prov events.Provider, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		evts, _ := prov.List(events.Filter{Type: events.RequestResultSessionCreate})
		if len(evts) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	evts, _ := prov.List(events.Filter{Type: events.RequestResultSessionCreate})
	t.Fatalf("timed out waiting for %d session create events (got %d)", n, len(evts))
}

func createTestSession(t *testing.T, store beads.Store, sp *runtime.Fake, title string) session.Info {
	t.Helper()
	mgr := session.NewManagerWithOptions(store, sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "default", Title: title, Command: "echo test", WorkDir: "/tmp", Provider: "test", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return info
}

func suspendSessionForPermissionModeTest(t *testing.T, fs *fakeState, id string) {
	t.Helper()
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(id); err != nil {
		t.Fatalf("suspend session: %v", err)
	}
}

type cachedOnlyListStoreForSessionTest struct {
	*beads.MemStore
	blockList bool
	listCalls int
}

func (s *cachedOnlyListStoreForSessionTest) List(query beads.ListQuery) ([]beads.Bead, error) {
	if s.blockList {
		s.listCalls++
		return nil, errors.New("backing List should not be used")
	}
	return s.MemStore.List(query)
}

func (s *cachedOnlyListStoreForSessionTest) CachedList(query beads.ListQuery) ([]beads.Bead, bool) {
	rows, err := s.MemStore.List(query)
	if err != nil {
		return nil, false
	}
	return rows, true
}

type apiListQueryCaptureStore struct {
	beads.Store
	listCalls []beads.ListQuery
}

func (s *apiListQueryCaptureStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls = append(s.listCalls, query)
	return s.Store.List(query)
}

type partialPrimeSessionStore struct {
	*beads.MemStore
	partialRows    []beads.Bead
	labelListCalls int
}

func (s *partialPrimeSessionStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.List(query)
	if err != nil {
		return nil, err
	}
	// Mimic the bd list partial-result path on the session-bead read
	// queries — both Type=session and Label=gc:session are issued by
	// ListAllSessionBeads, and Prime drives an AllowScan over the cache.
	if query.AllowScan || query.Label == session.LabelSession || query.Type == session.BeadType {
		if query.Label == session.LabelSession {
			s.labelListCalls++
		}
		if s.partialRows != nil {
			rows = append([]beads.Bead(nil), s.partialRows...)
		}
		return rows, &beads.PartialResultError{
			Op:  "bd list",
			Err: errors.New("skipped 1 corrupt bead"),
		}
	}
	return rows, nil
}

func TestSessionReadModelInfosFallsBackAfterPartialCachePrime(t *testing.T) {
	t.Parallel()

	backing := &partialPrimeSessionStore{MemStore: beads.NewMemStore()}
	// Real session beads carry Type=BeadType + LabelSession. Tests used
	// to omit Type because the read-model only queried by Label; after
	// the Type+Label union refactor, IsSessionBeadOrRepairable filters
	// rows whose Type is neither "session" nor "" so the fixtures need
	// to match production shape.
	survivor, err := backing.Create(beads.Bead{
		Title:  "session survivor",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	})
	if err != nil {
		t.Fatalf("Create(survivor): %v", err)
	}
	if _, err := backing.Create(beads.Bead{
		Title:  "dropped session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	}); err != nil {
		t.Fatalf("Create(dropped): %v", err)
	}
	backing.partialRows = []beads.Bead{survivor}

	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// A partial prime makes the cache peek miss, so the typed feed falls through
	// to the direct union (the backing label leg runs) and folds the partial into
	// the partial-error envelope while still serving the survivor.
	infos, partialErrors, err := sessionReadModelInfos(session.NewStore(beads.SessionStore{Store: cache}))
	if err != nil {
		t.Fatalf("sessionReadModelInfos error = %v, want nil (partial folded into the envelope)", err)
	}
	if len(partialErrors) != 1 {
		t.Fatalf("partialErrors = %v, want exactly one folded partial error", partialErrors)
	}
	if backing.labelListCalls != 1 {
		t.Fatalf("label List calls = %d, want 1 backing fallback after partial prime", backing.labelListCalls)
	}
	if len(infos) != 1 || infos[0].ID != survivor.ID {
		t.Fatalf("infos = %+v, want partial survivor %s", infos, survivor.ID)
	}
}

func TestHandleSessionListPreservesPartialRows(t *testing.T) {
	fs := newSessionFakeState(t)
	store := &partialPrimeSessionStore{MemStore: beads.NewMemStore()}
	fs.cityBeadStore = store
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, store, fs.sp, "Session survivor")
	survivor, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", info.ID, err)
	}
	store.partialRows = []beads.Bead{survivor}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Items         []sessionResponse `json:"items"`
		Total         int               `json:"total"`
		Partial       bool              `json:"partial"`
		PartialErrors []string          `json:"partial_errors"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Partial {
		t.Fatal("partial = false, want true")
	}
	if len(body.PartialErrors) == 0 {
		t.Fatal("partial_errors empty")
	}
	if body.Total != 1 || len(body.Items) != 1 || body.Items[0].ID != info.ID {
		t.Fatalf("body = %+v, want surviving session %s", body, info.ID)
	}
}

func writeGeminiHistoryFixtureForAPI(t *testing.T, path, sessionID string, messages ...string) {
	t.Helper()

	body := fmt.Sprintf("{\n  \"sessionId\": %q,\n  \"messages\": [\n    %s\n  ]\n}\n", sessionID, strings.Join(messages, ",\n    "))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write gemini transcript %s: %v", path, err)
	}
}

type cancelStartProvider struct {
	*runtime.Fake
}

func (p *cancelStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.Fake.Start(ctx, name, cfg)
}

type failNudgeProvider struct {
	*runtime.Fake
	err error
}

func (p *failNudgeProvider) Nudge(name string, content []runtime.ContentBlock) error {
	if err := p.Fake.Nudge(name, content); err != nil {
		return err
	}
	if p.err != nil {
		return p.err
	}
	return nil
}

type transportCapableProvider struct {
	*runtime.Fake
}

func (p *transportCapableProvider) SupportsTransport(transport string) bool {
	return transport == "acp"
}

type blockingStartProvider struct {
	*runtime.Fake
	started chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (p *blockingStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.started != nil {
		p.once.Do(func() {
			close(p.started)
		})
	}
	if p.unblock != nil {
		select {
		case <-p.unblock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.Fake.Start(ctx, name, cfg)
}

type blockingNudgeProvider struct {
	*runtime.Fake
	started chan struct{}
	unblock chan struct{}
}

func (p *blockingNudgeProvider) Nudge(name string, content []runtime.ContentBlock) error {
	if p.started != nil {
		close(p.started)
	}
	<-p.unblock
	return p.Fake.Nudge(name, content)
}

type pendingSessionMissingProvider struct {
	*runtime.Fake
}

func (p *pendingSessionMissingProvider) Pending(_ string) (*runtime.PendingInteraction, error) {
	return nil, fmt.Errorf("capturing pane: %w", runtime.ErrSessionNotFound)
}

type stateWithSessionProvider struct {
	*fakeState
	provider runtime.Provider
}

func (s *stateWithSessionProvider) SessionProvider() runtime.Provider {
	return s.provider
}

func seedQueuedWaitNudge(t *testing.T, fs *fakeState, wait beads.Bead, agentName string) string {
	t.Helper()
	nudgeID := "wait-" + wait.ID
	if err := fs.cityBeadStore.SetMetadataBatch(wait.ID, map[string]string{"nudge_id": nudgeID}); err != nil {
		t.Fatalf("set wait nudge_id: %v", err)
	}
	if _, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   "nudge",
		Title:  "nudge:" + nudgeID,
		Labels: []string{"nudge:" + nudgeID},
		Metadata: map[string]string{
			"nudge_id": nudgeID,
			"state":    "queued",
		},
	}); err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	statePath := citylayout.RuntimePath(fs.cityPath, "nudges", "state.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("create nudge queue dir: %v", err)
	}
	data, err := json.MarshalIndent(map[string]any{
		"pending": []map[string]any{{
			"id":    nudgeID,
			"agent": agentName,
		}},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal nudge queue: %v", err)
	}
	if err := os.WriteFile(statePath, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("seed nudge queue: %v", err)
	}
	return nudgeID
}

const sessionCloseWaitOverflowCount = 1001

func createSessionCloseWaitOverflow(t *testing.T, store beads.Store, sessionID string) {
	t.Helper()
	for i := 0; i < sessionCloseWaitOverflowCount; i++ {
		if _, err := store.Create(beads.Bead{
			Type:   session.WaitBeadType,
			Labels: []string{session.WaitBeadLabel, "session:" + sessionID},
			Metadata: map[string]string{
				"session_id": sessionID,
				"state":      "pending",
				"nudge_id":   fmt.Sprintf("wait-nudge-%d", i),
			},
		}); err != nil {
			t.Fatalf("create overflow wait %d: %v", i, err)
		}
	}
}

func createSessionCloseWaitOverflowWithQueuedNudges(t *testing.T, fs *fakeState, sessionID string) (string, string) {
	t.Helper()
	pending := make([]map[string]any, 0, sessionCloseWaitOverflowCount)
	for i := 0; i < sessionCloseWaitOverflowCount; i++ {
		nudgeID := fmt.Sprintf("wait-nudge-%d", i)
		if _, err := fs.cityBeadStore.Create(beads.Bead{
			Type:   session.WaitBeadType,
			Labels: []string{session.WaitBeadLabel, "session:" + sessionID},
			Metadata: map[string]string{
				"session_id": sessionID,
				"state":      "pending",
				"nudge_id":   nudgeID,
			},
		}); err != nil {
			t.Fatalf("create overflow wait %d: %v", i, err)
		}
		if _, err := fs.cityBeadStore.Create(beads.Bead{
			Type:   "nudge",
			Title:  "nudge:" + nudgeID,
			Labels: []string{"nudge:" + nudgeID},
			Metadata: map[string]string{
				"nudge_id": nudgeID,
				"state":    "queued",
			},
		}); err != nil {
			t.Fatalf("create overflow nudge %d: %v", i, err)
		}
		pending = append(pending, map[string]any{
			"id":    nudgeID,
			"agent": "default",
		})
	}
	statePath := citylayout.RuntimePath(fs.cityPath, "nudges", "state.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("create nudge queue dir: %v", err)
	}
	data, err := json.MarshalIndent(map[string]any{"pending": pending}, "", "  ")
	if err != nil {
		t.Fatalf("marshal nudge queue: %v", err)
	}
	if err := os.WriteFile(statePath, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("seed nudge queue: %v", err)
	}
	return "wait-nudge-0", fmt.Sprintf("wait-nudge-%d", sessionCloseWaitOverflowCount-1)
}

func assertSessionCloseWaitsCanceled(t *testing.T, store beads.Store, sessionID string) {
	t.Helper()
	sessionBead, err := store.Get(sessionID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if sessionBead.Status != "closed" {
		t.Fatalf("session status = %q, want closed", sessionBead.Status)
	}
	waits, err := store.List(beads.ListQuery{Label: "session:" + sessionID, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list waits: %v", err)
	}
	for _, wait := range waits {
		if !session.IsWaitBead(wait) {
			continue
		}
		if wait.Status != "closed" || wait.Metadata["state"] != "canceled" {
			t.Fatalf("wait %s status/state = %q/%q, want closed/canceled", wait.ID, wait.Status, wait.Metadata["state"])
		}
	}
}

func assertQueuedWaitNudgesWithdrawn(t *testing.T, fs *fakeState, nudgeIDs ...string) {
	t.Helper()
	state := loadQueuedWaitNudgeState(t, fs.cityPath)
	for _, want := range nudgeIDs {
		for _, item := range append(state.Pending, state.InFlight...) {
			if got, _ := item["id"].(string); got == want {
				t.Fatalf("nudge %q still queued after close", want)
			}
		}
		items, err := fs.cityBeadStore.ListByLabel("nudge:"+want, 0, beads.IncludeClosed)
		if err != nil {
			t.Fatalf("ListByLabel(%s): %v", want, err)
		}
		if len(items) != 1 {
			t.Fatalf("nudge %q bead count = %d, want 1", want, len(items))
		}
		if items[0].Status != "closed" {
			t.Fatalf("nudge %q status = %q, want closed", want, items[0].Status)
		}
		if items[0].Metadata["terminal_reason"] != "wait-canceled" {
			t.Fatalf("nudge %q terminal_reason = %q, want wait-canceled", want, items[0].Metadata["terminal_reason"])
		}
	}
}

func loadQueuedWaitNudgeState(t *testing.T, cityPath string) struct {
	Pending  []map[string]any `json:"pending,omitempty"`
	InFlight []map[string]any `json:"in_flight,omitempty"`
} {
	t.Helper()
	statePath := citylayout.RuntimePath(cityPath, "nudges", "state.json")
	data, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		return struct {
			Pending  []map[string]any `json:"pending,omitempty"`
			InFlight []map[string]any `json:"in_flight,omitempty"`
		}{}
	}
	if err != nil {
		t.Fatalf("read nudge queue: %v", err)
	}
	var state struct {
		Pending  []map[string]any `json:"pending,omitempty"`
		InFlight []map[string]any `json:"in_flight,omitempty"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse nudge queue: %v", err)
	}
	return state
}

func writeNamedSessionJSONL(t *testing.T, searchBase, workDir, fileName string, lines ...string) {
	t.Helper()
	dir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileName)
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type syncResponseRecorder struct {
	*httptest.ResponseRecorder
	mu sync.Mutex
}

func newSyncResponseRecorder() *syncResponseRecorder {
	return &syncResponseRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (r *syncResponseRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ResponseRecorder.Write(p)
}

func (r *syncResponseRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ResponseRecorder.WriteHeader(code)
}

func (r *syncResponseRecorder) WriteString(s string) (int, error) {
	return r.Write([]byte(s))
}

func (r *syncResponseRecorder) BodyString() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Body.String()
}

func waitForRecorderSubstring(t *testing.T, rec *syncResponseRecorder, want string, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		body := rec.BodyString()
		if strings.Contains(body, want) {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	return rec.BodyString()
}

func TestHandleSessionList(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	// Create two sessions.
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session A")
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session B")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp listResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Errorf("got total %d, want 2", resp.Total)
	}
}

func TestHandleSessionListFilterByState(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Suspend")
	createTestSession(t, fs.cityBeadStore, fs.sp, "Stay Active")

	// Suspend one.
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	// List only active.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions?state=active"), nil)
	h.ServeHTTP(w, r)

	var resp listResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("got total %d, want 1 (only active)", resp.Total)
	}
}

func TestHandleSessionListPagination(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	// Create 3 sessions.
	createTestSession(t, fs.cityBeadStore, fs.sp, "S1")
	createTestSession(t, fs.cityBeadStore, fs.sp, "S2")
	createTestSession(t, fs.cityBeadStore, fs.sp, "S3")

	// A truncated cursor-less page carries the keyset continuation cursor —
	// the old offset scheme silently cut here, leaving the remainder
	// unfetchable (the #3208 defect class).
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions?limit=2"), nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("page1: status %d", w.Code)
	}
	var page1 listResponse
	if err := json.NewDecoder(w.Body).Decode(&page1); err != nil {
		t.Fatalf("decode page1: %v", err)
	}
	items1, _ := page1.Items.([]any)
	if len(items1) != 2 {
		t.Errorf("page1: got %d items, want 2", len(items1))
	}
	if page1.Total != 3 {
		t.Errorf("page1: total = %d, want 3", page1.Total)
	}
	if page1.NextCursor == "" {
		t.Fatal("page1: expected next_cursor on a truncated page, got empty")
	}

	// Follow the keyset cursor to the final page.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", cityURL(fs, "/sessions?cursor=")+page1.NextCursor+"&limit=2", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("page2: status %d", w.Code)
	}
	var page2 listResponse
	if err := json.NewDecoder(w.Body).Decode(&page2); err != nil {
		t.Fatalf("decode page2: %v", err)
	}
	items2, _ := page2.Items.([]any)
	if len(items2) != 1 {
		t.Errorf("page2: got %d items, want 1", len(items2))
	}
	if page2.Total != 3 {
		t.Errorf("page2: total = %d, want 3 (full-set meaning, constant across a walk)", page2.Total)
	}
	if page2.NextCursor != "" {
		t.Errorf("page2: got next_cursor %q, want empty (last page)", page2.NextCursor)
	}
}

func TestHandleSessionGet(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "My Session")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID, nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != info.ID {
		t.Errorf("got ID %q, want %q", resp.ID, info.ID)
	}
	if resp.Title != "My Session" {
		t.Errorf("got title %q, want %q", resp.Title, "My Session")
	}
	if resp.State != "active" {
		t.Errorf("got state %q, want %q", resp.State, "active")
	}
	if !resp.Running {
		t.Errorf("got running=%v, want true", resp.Running)
	}
}

func TestHandleSessionListActiveBeadUsesCachedLookup(t *testing.T) {
	fs := newSessionFakeState(t)
	backing := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(backing, nil)
	fs.stores["myrig"] = cache
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "My Session")
	work, err := backing.Create(beads.Bead{Title: "active work"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	status := "in_progress"
	assignee := info.ID
	if err := backing.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	reassigned := "other-session"
	if err := backing.Update(work.ID, beads.UpdateOpts{Assignee: &reassigned}); err != nil {
		t.Fatalf("reassign backing work: %v", err)
	}

	resp := sessionResponse{}
	srv.enrichSessionResponse(&resp, info, fs.Config(), sessionResponseCapabilityHandle{
		state: worker.State{Phase: worker.PhaseReady},
	}, false, false, false, 0)

	if !resp.Running {
		t.Fatal("Running = false, want true")
	}
	if got := resp.ActiveBead; got != work.ID {
		t.Fatalf("active_bead = %q, want cached %q", got, work.ID)
	}
}

func TestHandleSessionListUsesCachedSessionBeadsWhenAvailable(t *testing.T) {
	fs := newSessionFakeState(t)
	store := &cachedOnlyListStoreForSessionTest{MemStore: beads.NewMemStore()}
	fs.cityBeadStore = store
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "My Session")
	store.blockList = true

	req := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []sessionResponse `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || len(resp.Items) != 1 || resp.Items[0].ID != info.ID {
		t.Fatalf("response = %#v, want one session %s", resp, info.ID)
	}
	if store.listCalls != 0 {
		t.Fatalf("backing List calls = %d, want 0", store.listCalls)
	}
}

// newHermeticCodexSessionSearchPath keeps Codex's always-merged default root
// inside test-owned HOME while preserving a separate configured search path.
func newHermeticCodexSessionSearchPath(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	if err := os.MkdirAll(filepath.Join(home, ".codex", "sessions"), 0o755); err != nil {
		t.Fatalf("MkdirAll default codex sessions: %v", err)
	}
	return t.TempDir()
}

func TestHandleSessionListSkipsWorkdirOnlyCodexTranscriptDiscovery(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := newHermeticCodexSessionSearchPath(t)
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}
	h := newTestCityHandlerWith(t, fs, srv)

	workDir := t.TempDir()
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Codex Chat", Command: "codex", WorkDir: workDir, Provider: "codex-max", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.SessionKey != "" {
		t.Fatalf("SessionKey = %q, want empty for codex provider without SessionIDFlag", info.SessionKey)
	}

	codexDir := filepath.Join(searchBase, "2026", "04", "18")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	codexPayload := strings.Join([]string{
		fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q}}`, workDir),
		`{"type":"assistant","message":{"model":"gpt-5.5","usage":{"input_tokens":1000}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(codexDir, "session.jsonl"), []byte(codexPayload), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	req := httptest.NewRequest("GET", cityURL(fs, "/sessions?template=myrig%2Fworker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []sessionResponse `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].ID != info.ID {
		t.Fatalf("items = %#v, want session %s", resp.Items, info.ID)
	}
	if resp.Items[0].Model != "" || resp.Items[0].ContextPct != nil {
		t.Fatalf("session list used workdir-only Codex transcript discovery: model=%q context=%v", resp.Items[0].Model, resp.Items[0].ContextPct)
	}
}

func TestHandleSessionGetAllowsWorkdirOnlyCodexTranscriptDiscovery(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := newHermeticCodexSessionSearchPath(t)
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}
	h := newTestCityHandlerWith(t, fs, srv)

	workDir := t.TempDir()
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Codex Chat", Command: "codex", WorkDir: workDir, Provider: "codex-max", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	codexDir := filepath.Join(searchBase, "2026", "04", "18")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	codexPayload := strings.Join([]string{
		fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q}}`, workDir),
		`{"type":"assistant","message":{"model":"gpt-5.5","usage":{"input_tokens":1000}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(codexDir, "session.jsonl"), []byte(codexPayload), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != info.ID {
		t.Fatalf("ID = %q, want %q", resp.ID, info.ID)
	}
	if resp.Model != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", resp.Model)
	}
}

func TestHandleSessionListActiveBeadUsesCachedListWhenAvailable(t *testing.T) {
	fs := newSessionFakeState(t)
	store := &cachedOnlyListStoreForSessionTest{MemStore: beads.NewMemStore(), blockList: true}
	fs.stores["myrig"] = store
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "My Session")
	work, err := store.Create(beads.Bead{Title: "active work"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	status := "in_progress"
	assignee := info.ID
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}

	resp := sessionResponse{}
	srv.enrichSessionResponse(&resp, info, fs.Config(), sessionResponseCapabilityHandle{
		state: worker.State{Phase: worker.PhaseReady},
	}, false, false, false, 0)

	if got := resp.ActiveBead; got != work.ID {
		t.Fatalf("active_bead = %q, want cached %q", got, work.ID)
	}
	if store.listCalls != 0 {
		t.Fatalf("backing List calls = %d, want 0", store.listCalls)
	}
}

func TestHandleSessionGetActiveBeadUsesLiveLookup(t *testing.T) {
	fs := newSessionFakeState(t)
	backing := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(backing, nil)
	fs.stores["myrig"] = cache
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "My Session")
	work, err := backing.Create(beads.Bead{Title: "active work"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	status := "in_progress"
	assignee := info.ID
	if err := backing.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	reassigned := "other-session"
	if err := backing.Update(work.ID, beads.UpdateOpts{Assignee: &reassigned}); err != nil {
		t.Fatalf("reassign backing work: %v", err)
	}

	resp := sessionResponse{}
	srv.enrichSessionResponse(&resp, info, fs.Config(), sessionResponseCapabilityHandle{
		state: worker.State{Phase: worker.PhaseReady},
	}, false, true, true, 0)

	if !resp.Running {
		t.Fatal("Running = false, want true")
	}
	if got := resp.ActiveBead; got != "" {
		t.Fatalf("active_bead = %q, want empty after external reassignment", got)
	}
}

func TestHandleSessionGetNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/nonexistent"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleSessionSuspend(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Suspend")

	w := httptest.NewRecorder()
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/suspend", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify the session is now suspended.
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != session.StateSuspended {
		t.Errorf("got state %q, want %q", got.State, session.StateSuspended)
	}
}

// TestHandleSessionSuspend_IllegalTransition covers Fix 3j: illegal state
// transitions from the manager surface as 409 Problem Details to the API.
// Drain puts the session in Draining; a subsequent Suspend is illegal
// (the state machine only allows Suspend from Active/Asleep/Quarantined).
func TestHandleSessionSuspend_IllegalTransition(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Drain")

	// Drain the session directly via the manager (the API surface for drain
	// lives elsewhere; this test isolates the transition check).
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	if err := mgr.BeginDrain(info.ID, "shutdown"); err != nil {
		t.Fatalf("BeginDrain: %v", err)
	}

	w := httptest.NewRecorder()
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/suspend", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusConflict, w.Body.String())
	}

	// Response body should be RFC 9457 Problem Details with the
	// `illegal_transition:` semantic prefix in the detail field.
	var problem struct {
		Status int    `json:"status"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(w.Body).Decode(&problem); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if problem.Status != http.StatusConflict {
		t.Errorf("problem.status = %d, want %d", problem.Status, http.StatusConflict)
	}
	if !strings.Contains(problem.Detail, "illegal_transition") {
		t.Errorf("problem.detail = %q, want substring %q", problem.Detail, "illegal_transition")
	}
}

func TestHandleSessionClose(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Close")
	wait, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.WaitBeadType,
		Labels: []string{session.WaitBeadLabel, "session:" + info.ID},
		Metadata: map[string]string{
			"session_id": info.ID,
			"state":      "pending",
		},
	})
	if err != nil {
		t.Fatalf("create wait: %v", err)
	}
	nudgeID := seedQueuedWaitNudge(t, fs, wait, "default")

	w := httptest.NewRecorder()
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/close", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Session should no longer appear in default listing (excludes closed).
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	sessions, err := mgr.List("", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("got %d sessions after close, want 0", len(sessions))
	}
	wait, err = fs.cityBeadStore.Get(wait.ID)
	if err != nil {
		t.Fatalf("get wait: %v", err)
	}
	if wait.Metadata["state"] != "canceled" {
		t.Fatalf("wait state = %q, want canceled", wait.Metadata["state"])
	}
	state := loadQueuedWaitNudgeState(t, fs.cityPath)
	for _, item := range append(state.Pending, state.InFlight...) {
		if got, _ := item["id"].(string); got == nudgeID {
			t.Fatalf("nudge %q still queued after close", nudgeID)
		}
	}
	items, err := fs.cityBeadStore.ListByLabel("nudge:"+nudgeID, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByLabel(nudge): %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("nudge bead count = %d, want 1", len(items))
	}
	if items[0].Status != "closed" {
		t.Fatalf("nudge status = %q, want closed", items[0].Status)
	}
	if items[0].Metadata["terminal_reason"] != "wait-canceled" {
		t.Fatalf("nudge terminal_reason = %q, want wait-canceled", items[0].Metadata["terminal_reason"])
	}
}

func TestHumaSessionCloseContinuesAfterWaitLookupLimit(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Capped Huma Close")
	createSessionCloseWaitOverflow(t, fs.cityBeadStore, info.ID)

	w := httptest.NewRecorder()
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/close", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	assertSessionCloseWaitsCanceled(t, fs.cityBeadStore, info.ID)
}

func TestHumaSessionCloseWithdrawsOverflowQueuedWaitNudges(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Overflow Queued Close")
	firstNudgeID, laterPageNudgeID := createSessionCloseWaitOverflowWithQueuedNudges(t, fs, info.ID)

	w := httptest.NewRecorder()
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/close", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	assertSessionCloseWaitsCanceled(t, fs.cityBeadStore, info.ID)
	assertQueuedWaitNudgesWithdrawn(t, fs, firstNudgeID, laterPageNudgeID)
}

// Regression test for ga-frfj2d: the Huma close handler must route through
// worker.Handle.CloseDetailed (the worker boundary), like the legacy close
// handler, instead of constructing a session.Manager directly. The worker
// boundary emits a "close" worker operation event; the direct manager
// bypass did not.
func TestHumaSessionCloseEmitsWorkerCloseOperationEvent(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Close Op Event")

	w := httptest.NewRecorder()
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/close", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var closeOps []WorkerOperationEventPayload
	for _, event := range fs.eventProv.(*events.Fake).Events {
		if event.Type != events.WorkerOperation {
			continue
		}
		var payload WorkerOperationEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("unmarshal worker operation payload: %v", err)
		}
		if payload.Operation == "close" {
			closeOps = append(closeOps, payload)
		}
	}
	if len(closeOps) != 1 {
		t.Fatalf("got %d close worker operation events, want 1 (Huma close must route through the worker boundary)", len(closeOps))
	}
	if closeOps[0].SessionID != info.ID {
		t.Errorf("close operation session_id = %q, want %q", closeOps[0].SessionID, info.ID)
	}
	if closeOps[0].Result != "succeeded" {
		t.Errorf("close operation result = %q, want %q", closeOps[0].Result, "succeeded")
	}
}

func TestLegacySessionCloseContinuesAfterWaitLookupLimit(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Capped Legacy Close")
	createSessionCloseWaitOverflow(t, fs.cityBeadStore, info.ID)

	w := httptest.NewRecorder()
	r := newPostRequest("/v0/session/"+info.ID+"/close", nil)
	srv.legacySessionHandler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	assertSessionCloseWaitsCanceled(t, fs.cityBeadStore, info.ID)
}

func TestLegacySessionCloseWithdrawsOverflowQueuedWaitNudges(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Overflow Queued Legacy Close")
	firstNudgeID, laterPageNudgeID := createSessionCloseWaitOverflowWithQueuedNudges(t, fs, info.ID)

	w := httptest.NewRecorder()
	r := newPostRequest("/v0/session/"+info.ID+"/close", nil)
	srv.legacySessionHandler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	assertSessionCloseWaitsCanceled(t, fs.cityBeadStore, info.ID)
	assertQueuedWaitNudgesWithdrawn(t, fs, firstNudgeID, laterPageNudgeID)
}

func TestHandleSessionCloseDeleteIgnoresMissingBeadAfterClose(t *testing.T) {
	fs := newSessionFakeState(t)
	mem := beads.NewMemStore()
	fs.cityBeadStore = deleteMissingStore{Store: mem}
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Close And Delete")

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/close?delete=true", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestHandleSessionCloseDeleteRetriesTransientConflict(t *testing.T) {
	fs := newSessionFakeState(t)
	mem := beads.NewMemStore()
	store := &transientDeleteConflictStore{Store: mem}
	fs.cityBeadStore = store
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Transient Delete")

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/close?delete=true", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if store.deleteCalls != 2 {
		t.Fatalf("delete calls = %d, want 2", store.deleteCalls)
	}
	if _, err := mem.Get(info.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(%s) error = %v, want ErrNotFound", info.ID, err)
	}
}

func TestDeleteSessionBeadAfterCloseReturnsLastTransientError(t *testing.T) {
	store := &alwaysTransientDeleteConflictStore{Store: beads.NewMemStore()}

	err := deleteSessionBeadAfterClose(store, "gc-test")

	if err == nil {
		t.Fatal("deleteSessionBeadAfterClose error = nil, want transient failure")
	}
	if store.deleteCalls != 5 {
		t.Fatalf("delete calls = %d, want 5", store.deleteCalls)
	}
	if !strings.Contains(err.Error(), "conflict attempt 5") {
		t.Fatalf("error = %v, want final underlying conflict", err)
	}
}

func TestDeleteSessionBeadAfterCloseDoesNotRetryNonTransientError(t *testing.T) {
	store := &nonTransientDeleteErrorStore{err: errors.New("permission denied")}

	err := deleteSessionBeadAfterClose(store, "gc-test")

	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("deleteSessionBeadAfterClose error = %v, want permission denied", err)
	}
	if store.deleteCalls != 1 {
		t.Fatalf("delete calls = %d, want 1", store.deleteCalls)
	}
}

func TestDeleteSessionBeadAfterCloseLogsAlreadyGone(t *testing.T) {
	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOutput)

	err := deleteSessionBeadAfterClose(deleteMissingStore{Store: beads.NewMemStore()}, "gc-test")
	if err != nil {
		t.Fatalf("deleteSessionBeadAfterClose: %v", err)
	}
	if !strings.Contains(logs.String(), "already gone") {
		t.Fatalf("logs = %q, want already gone signal", logs.String())
	}
}

type deleteMissingStore struct {
	beads.Store
}

func (s deleteMissingStore) Delete(id string) error {
	return fmt.Errorf("deleting bead %q: %w", id, beads.ErrNotFound)
}

type transientDeleteConflictStore struct {
	beads.Store
	deleteCalls int
}

func (s *transientDeleteConflictStore) Delete(id string) error {
	s.deleteCalls++
	if s.deleteCalls == 1 {
		return fmt.Errorf("deleting bead %q: sql commit: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction", id)
	}
	return s.Store.Delete(id)
}

type alwaysTransientDeleteConflictStore struct {
	beads.Store
	deleteCalls int
}

func (s *alwaysTransientDeleteConflictStore) Delete(id string) error {
	s.deleteCalls++
	return fmt.Errorf("deleting bead %q: sql commit: Error 1213 (40001): serialization failure: conflict attempt %d", id, s.deleteCalls)
}

type nonTransientDeleteErrorStore struct {
	beads.Store
	deleteCalls int
	err         error
}

func (s *nonTransientDeleteErrorStore) Delete(string) error {
	s.deleteCalls++
	return s.err
}

func TestHandleSessionWake_DoesNotRewriteHistoricalWaitNudge(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Historical Wait")
	wait, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.WaitBeadType,
		Labels: []string{session.WaitBeadLabel, "session:" + info.ID},
		Metadata: map[string]string{
			"session_id": info.ID,
			"state":      "closed",
			"nudge_id":   "wait-historical",
		},
	})
	if err != nil {
		t.Fatalf("create wait: %v", err)
	}
	if err := fs.cityBeadStore.Close(wait.ID); err != nil {
		t.Fatalf("close wait: %v", err)
	}
	nudge, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   "nudge",
		Title:  "nudge:wait-historical",
		Labels: []string{"nudge:wait-historical"},
		Metadata: map[string]string{
			"nudge_id":        "wait-historical",
			"state":           "injected",
			"commit_boundary": "provider-nudge-return",
		},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	if err := fs.cityBeadStore.Close(nudge.ID); err != nil {
		t.Fatalf("close nudge bead: %v", err)
	}
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"wait_hold":    "true",
		"sleep_intent": "wait-hold",
		"sleep_reason": "wait-hold",
	})

	w := httptest.NewRecorder()
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/wake", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	updated, err := fs.cityBeadStore.Get(nudge.ID)
	if err != nil {
		t.Fatalf("get nudge: %v", err)
	}
	if updated.Metadata["state"] != "injected" {
		t.Fatalf("nudge state = %q, want injected", updated.Metadata["state"])
	}
	if updated.Metadata["terminal_reason"] != "" {
		t.Fatalf("nudge terminal_reason = %q, want empty", updated.Metadata["terminal_reason"])
	}
	if updated.Metadata["commit_boundary"] != "provider-nudge-return" {
		t.Fatalf("nudge commit_boundary = %q, want provider-nudge-return", updated.Metadata["commit_boundary"])
	}
}

func TestHandleSessionNoCityStore(t *testing.T) {
	fs := newFakeState(t) // no cityBeadStore set
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleSessionWake(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Held Session")
	wait, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.WaitBeadType,
		Labels: []string{session.WaitBeadLabel, "session:" + info.ID},
		Metadata: map[string]string{
			"session_id": info.ID,
			"state":      "pending",
		},
	})
	if err != nil {
		t.Fatalf("create wait: %v", err)
	}
	nudgeID := seedQueuedWaitNudge(t, fs, wait, "default")

	// Set hold metadata.
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"held_until":   "9999-12-31T23:59:59Z",
		"wait_hold":    "true",
		"sleep_intent": "wait-hold",
		"sleep_reason": "wait-hold",
	})

	w := httptest.NewRecorder()
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/wake", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify hold cleared.
	b, _ := fs.cityBeadStore.Get(info.ID)
	if b.Metadata["held_until"] != "" {
		t.Errorf("held_until should be cleared, got %q", b.Metadata["held_until"])
	}
	if b.Metadata["wait_hold"] != "" {
		t.Errorf("wait_hold should be cleared, got %q", b.Metadata["wait_hold"])
	}
	if b.Metadata["sleep_intent"] != "" {
		t.Errorf("sleep_intent should be cleared, got %q", b.Metadata["sleep_intent"])
	}
	if b.Metadata["sleep_reason"] != "" {
		t.Errorf("sleep_reason should be cleared, got %q", b.Metadata["sleep_reason"])
	}
	wait, err = fs.cityBeadStore.Get(wait.ID)
	if err != nil {
		t.Fatalf("get wait: %v", err)
	}
	if wait.Metadata["state"] != "canceled" {
		t.Fatalf("wait state = %q, want canceled", wait.Metadata["state"])
	}
	state := loadQueuedWaitNudgeState(t, fs.cityPath)
	for _, item := range append(state.Pending, state.InFlight...) {
		if got, _ := item["id"].(string); got == nudgeID {
			t.Fatalf("nudge %q still queued after wake", nudgeID)
		}
	}
	items, err := fs.cityBeadStore.ListByLabel("nudge:"+nudgeID, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByLabel(nudge): %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("nudge bead count = %d, want 1", len(items))
	}
	if items[0].Status != "closed" {
		t.Fatalf("nudge status = %q, want closed", items[0].Status)
	}
	if items[0].Metadata["terminal_reason"] != "wait-canceled" {
		t.Fatalf("nudge terminal_reason = %q, want wait-canceled", items[0].Metadata["terminal_reason"])
	}
}

func TestHandleSessionWakeStartsSuspendedRuntime(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Suspended Session")
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if fs.sp.IsRunning(info.SessionName) {
		t.Fatalf("session %q running after suspend", info.SessionName)
	}

	w := httptest.NewRecorder()
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/wake", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	deadline := time.Now().Add(testEventTimeout)
	for !fs.sp.IsRunning(info.SessionName) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !fs.sp.IsRunning(info.SessionName) {
		t.Fatalf("session %q should be running after async POST /wake start", info.SessionName)
	}
}

func TestHandleSessionWakeClosed(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Closed Session")
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	_ = mgr.Close(info.ID)

	w := httptest.NewRecorder()
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/wake", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleSessionGetByTemplateName(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Named Session")

	// Set alias metadata on the bead so public resolution works.
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"alias": "overseer",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/overseer"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != info.ID {
		t.Errorf("got ID %q, want %q", resp.ID, info.ID)
	}
}

func TestHandleSessionPatchTitle(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")

	body := `{"title":"Updated Title"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/session/")+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Title != "Updated Title" {
		t.Errorf("got title %q, want %q", resp.Title, "Updated Title")
	}
}

func TestHandleSessionPatchAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")

	body := `{"alias":"mayor"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/session/")+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Alias != "mayor" {
		t.Errorf("got alias %q, want %q", resp.Alias, "mayor")
	}
}

func TestHandleSessionPatchAliasRejectsManagedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")
	if err := fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"agent_name": "mayor",
	}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}

	body := `{"alias":"new-mayor"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/session/")+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusForbidden, w.Body.String())
	}
}

func TestHandleSessionPatchRejectsReservedQualifiedAliasOnFork(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(
		context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Fork", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	body := `{"alias":"myrig/worker"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/session/")+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleSessionPatchImmutableField(t *testing.T) {
	// Fix 3f(remnant): PATCH body is now a typed struct with
	// additionalProperties:false on the schema, so unknown fields like
	// "template" are rejected by Huma's validation layer (422) rather
	// than the handler-side 403. This is a stricter error class for the
	// same underlying constraint.
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Test")

	body := `{"template":"hacked"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/session/")+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusUnprocessableEntity, w.Body.String())
	}
}

func TestHandleSessionListIncludesReason(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Held")

	// Set sleep reason on bead.
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"sleep_reason": "user-hold",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}

	// Parse into raw JSON to check for reason field.
	var raw struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(raw.Items))
	}
	var item sessionResponse
	if err := json.Unmarshal(raw.Items[0], &item); err != nil {
		t.Fatalf("unmarshal item: %v", err)
	}
	if item.Reason != "user-hold" {
		t.Errorf("got reason %q, want %q", item.Reason, "user-hold")
	}
}

func TestHandleSessionListShowsResetPendingForLiveRuntime(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Reset Pending")
	if !fs.sp.IsRunning(info.SessionName) {
		t.Fatalf("session %q should be running in fake provider", info.SessionName)
	}
	if err := fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"restart_requested": "true",
		"sleep_reason":      "user-hold",
	}); err != nil {
		t.Fatalf("set reset metadata: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var body struct {
		Items []sessionResponse `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(body.Items))
	}
	if body.Items[0].Reason != "reset-pending" {
		t.Fatalf("reason = %q, want reset-pending", body.Items[0].Reason)
	}
}

func TestHandleSessionListShowsCircuitOpenReason(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Circuit Open")
	if err := fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		session.SessionCircuitStateMetadataKey: session.SessionCircuitStateOpen,
		"sleep_reason":                         "user-hold",
	}); err != nil {
		t.Fatalf("set circuit metadata: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var body struct {
		Items []sessionResponse `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(body.Items))
	}
	if body.Items[0].Reason != session.LifecycleReasonCircuitOpen {
		t.Fatalf("reason = %q, want circuit-open", body.Items[0].Reason)
	}
}

func TestHandleSessionRename(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")

	body := `{"title":"Renamed"}`
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/rename", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Title != "Renamed" {
		t.Errorf("got title %q, want %q", resp.Title, "Renamed")
	}
}

func TestHandleSessionRenameEmptyTitle(t *testing.T) {
	// Fix 3k(remnant): title now has minLength:"1"; empty-string bodies
	// are rejected by Huma's validation layer (422) rather than the
	// handler-side 400.
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Test")

	body := `{"title":""}`
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/rename", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusUnprocessableEntity, w.Body.String())
	}
}

func TestHandleSessionAmbiguousAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	// Create two sessions with the same public alias.
	info1 := createTestSession(t, fs.cityBeadStore, fs.sp, "Worker 1")
	info2 := createTestSession(t, fs.cityBeadStore, fs.sp, "Worker 2")
	_ = fs.cityBeadStore.SetMetadataBatch(info1.ID, map[string]string{"alias": "worker"})
	_ = fs.cityBeadStore.SetMetadataBatch(info2.ID, map[string]string{"alias": "worker"})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/worker"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d (ambiguous); body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleSessionGetEnrichment(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Enriched Session")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID, nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !resp.Running {
		t.Error("running = false, want true for active session")
	}
	if resp.DisplayName != "Test" {
		t.Errorf("display_name = %q, want %q", resp.DisplayName, "Test")
	}
}

func TestHandleSessionListPeek(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	createTestSession(t, fs.cityBeadStore, fs.sp, "Peek Session")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	h.ServeHTTP(w, r)

	var resp struct {
		Items []sessionResponse `json:"items"`
	}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Items) == 0 {
		t.Fatal("no sessions returned")
	}
	if resp.Items[0].LastOutput != "" {
		t.Errorf("last_output = %q without peek param, want empty", resp.Items[0].LastOutput)
	}
}

func TestHandleSessionCreate(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	req.Header.Set("Idempotency-Key", "sess-create-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	var accepted asyncAcceptedBody
	if err := json.NewDecoder(w.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if accepted.RequestID == "" {
		t.Fatal("response must include request_id")
	}

	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	resp := success.Session
	if resp.Template != "myrig/worker" {
		t.Errorf("Template = %q, want %q", resp.Template, "myrig/worker")
	}
	if resp.Title != "myrig/worker" {
		t.Errorf("Title = %q, want default %q", resp.Title, "myrig/worker")
	}
	bead, err := fs.cityBeadStore.Get(resp.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", resp.ID, err)
	}
	if got := bead.Metadata[session.MCPIdentityMetadataKey]; got != "" {
		t.Fatalf("mcp_identity = %q, want empty for non-ACP agent session", got)
	}
	if got := bead.Metadata[session.MCPServersSnapshotMetadataKey]; got != "" {
		t.Fatalf("mcp_servers_snapshot = %q, want empty for non-ACP agent session", got)
	}
	// Agent sessions are always created async — not running until the
	// reconciler starts the process.
	if resp.Running {
		t.Errorf("Running = %v, want false for async create", resp.Running)
	}
	if resp.DisplayName != "Test Agent" {
		t.Errorf("DisplayName = %q, want %q", resp.DisplayName, "Test Agent")
	}
}

func TestHandleSessionCreateUsesACPTransportCommandForAgentTemplate(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "opencode"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  &transportCapableProvider{Fake: runtime.NewFake()},
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	var accepted asyncAcceptedBody
	if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if accepted.RequestID == "" {
		t.Fatal("response must include request_id")
	}
	success, failure := waitForSessionCreateResult(t, state.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	bead, err := state.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", success.Session.ID, err)
	}
	if got, want := bead.Metadata["command"], "/bin/echo acp"; got != want {
		t.Fatalf("command metadata = %q, want %q", got, want)
	}
	if got, want := bead.Metadata["transport"], "acp"; got != want {
		t.Fatalf("transport metadata = %q, want %q", got, want)
	}
}

func TestHumaHandleSessionCreateUsesACPTransportCommandForAgentTemplate(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "opencode"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  &transportCapableProvider{Fake: runtime.NewFake()},
	}
	srv := New(state)

	out, err := srv.humaHandleSessionCreate(context.Background(), &SessionCreateInput{
		Body: sessionCreateBody{
			Kind: "agent",
			Name: "myrig/worker",
		},
	})
	if err != nil {
		t.Fatalf("humaHandleSessionCreate: %v", err)
	}
	if got, want := out.Status, http.StatusAccepted; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if out.Body.RequestID == "" {
		t.Fatal("request_id is empty")
	}
	success, failure := waitForSessionCreateResult(t, state.eventProv, out.Body.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	bead, err := state.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", success.Session.ID, err)
	}
	if got, want := bead.Metadata["command"], "/bin/echo acp"; got != want {
		t.Fatalf("command metadata = %q, want %q", got, want)
	}
	if got, want := bead.Metadata["transport"], "acp"; got != want {
		t.Fatalf("transport metadata = %q, want %q", got, want)
	}
}

func TestHandleSessionCreateRejectsACPAgentWithoutACPRouting(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "opencode"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "requires ACP transport") {
		t.Fatalf("body = %q, want ACP transport error", rec.Body.String())
	}
}

func TestHandleSessionCreateRejectsExplicitTmuxAgentWhenCitySessionProviderIsACP(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Session.Provider = "acp"
	fs.cfg.Agents[0].Provider = "opencode"
	fs.cfg.Agents[0].Session = "tmux"
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
	}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  &transportCapableProvider{Fake: runtime.NewFake()},
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "requires tmux transport") {
		t.Fatalf("body = %q, want tmux transport error", rec.Body.String())
	}
	items, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("session bead count = %d, want 0", len(items))
	}
}

func TestHumaHandleSessionCreateRejectsACPAgentWithoutACPRouting(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "opencode"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	srv := New(fs)

	if _, err := srv.humaHandleSessionCreate(context.Background(), &SessionCreateInput{
		Body: sessionCreateBody{
			Kind: "agent",
			Name: "myrig/worker",
		},
	}); err == nil {
		t.Fatal("humaHandleSessionCreate() error = nil, want ACP routing error")
	} else if !strings.Contains(err.Error(), "requires ACP transport") {
		t.Fatalf("humaHandleSessionCreate() error = %v, want ACP transport error", err)
	}
}

func TestHandleSessionCreateRejectsACPAgentWhenProviderLacksACP(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "custom"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["custom"] = config.ProviderSpec{
		DisplayName: "Custom",
		Command:     "/bin/echo",
		PathCheck:   "true",
	}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  &transportCapableProvider{Fake: runtime.NewFake()},
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "does not support ACP transport") {
		t.Fatalf("body = %q, want provider ACP support error", rec.Body.String())
	}
}

func TestHumaHandleSessionCreatePropagatesMCPResolutionErrorForACPAgent(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "opencode"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "filesystem.toml"), []byte(`
name = "filesystem"
command = [broken
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  &transportCapableProvider{Fake: runtime.NewFake()},
	}
	srv := New(state)

	if _, err := srv.humaHandleSessionCreate(context.Background(), &SessionCreateInput{
		Body: sessionCreateBody{
			Kind: "agent",
			Name: "myrig/worker",
		},
	}); err == nil {
		t.Fatal("humaHandleSessionCreate() error = nil, want MCP resolution error")
	} else if !strings.Contains(err.Error(), "loading effective MCP") {
		t.Fatalf("humaHandleSessionCreate() error = %v, want MCP resolution error", err)
	}
}

func TestHandleSessionCreateIgnoresBrokenMCPWithoutACPTransport(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "filesystem.toml"), []byte(`
name = "filesystem"
command = [broken
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}

	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
}

func TestHandleSessionCreateProviderReturns202WithRequestID(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	body := `{"kind":"provider","name":"test-agent","project_id":"alpha","title":"contract test","alias":"contract-test"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("provider session create status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	var resp struct {
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RequestID == "" {
		t.Fatal("response must include request_id for async correlation")
	}

	success, failure := waitForSessionCreateResult(t, fs.eventProv, resp.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if success.Session.ID == "" {
		t.Fatal("session create event must include session.id")
	}
}

func TestHandleSessionCreateAsync(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","alias":"sky","async":true}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	var accepted asyncAcceptedBody
	if err := json.NewDecoder(w.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if accepted.RequestID == "" {
		t.Fatal("response must include request_id")
	}

	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if success.Session.Alias != "sky" {
		t.Fatalf("Alias = %q, want %q", success.Session.Alias, "sky")
	}
	if fs.pokeCount != 1 {
		t.Fatalf("pokeCount = %d, want 1", fs.pokeCount)
	}
}

func TestHandleSessionCreateAsyncResultIsCommandable(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	body := `{"kind":"agent","name":"myrig/worker","alias":"commandable","async":true}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if success.Session.State == string(session.StateCreating) {
		t.Fatalf("session create result state = %q, want commandable state", success.Session.State)
	}

	suspendReq := newPostRequest(cityURL(fs, "/session/")+success.Session.ID+"/suspend", nil)
	suspendRec := httptest.NewRecorder()
	h.ServeHTTP(suspendRec, suspendReq)

	if suspendRec.Code != http.StatusOK {
		t.Fatalf("suspend status = %d, want %d; body: %s", suspendRec.Code, http.StatusOK, suspendRec.Body.String())
	}
	bead, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", success.Session.ID, err)
	}
	if got := bead.Metadata["state"]; got != string(session.StateSuspended) {
		t.Fatalf("state after suspend = %q, want %q", got, session.StateSuspended)
	}
}

func TestHandleSessionCreateAsyncEmitsBeforeOptionalMetadataPersistenceCompletes(t *testing.T) {
	fs := newSessionFakeState(t)
	blocking := &blockingSetMetadataBatchStore{
		Store: fs.cityBeadStore,
		shouldBlock: func(kvs map[string]string) bool {
			return kvs["real_world_app_project_id"] == "myrig"
		},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	fs.cityBeadStore = blocking
	defer close(blocking.release)

	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	body := `{"kind":"agent","name":"myrig/worker","alias":"sky","async":true,"project_id":"myrig"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)

	select {
	case <-blocking.entered:
	case <-time.After(testEventTimeout):
		t.Fatal("SetMetadataBatch was not reached")
	}

	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		successEvents, _ := fs.eventProv.List(events.Filter{Type: events.RequestResultSessionCreate})
		for _, e := range successEvents {
			var p SessionCreateSucceededPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil && requestIDMatches(p.RequestID, accepted.RequestID) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("session create result was not emitted while metadata persistence was blocked")
}

type blockingSetMetadataBatchStore struct {
	beads.Store
	shouldBlock func(map[string]string) bool
	entered     chan struct{}
	release     chan struct{}
	once        sync.Once
}

func (s *blockingSetMetadataBatchStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if s.shouldBlock != nil && s.shouldBlock(kvs) {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func TestHandleSessionCreateAsyncAcceptsInlineMessage(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	// Agent sessions are always async; messages are stored as initial_message
	// in template_overrides for the reconciler to pick up.
	body := `{"kind":"agent","name":"myrig/worker","async":true,"message":"hello"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
}

func TestHandleSessionCreateAsync_PoolTemplateWithoutAliasUsesGeneratedWorkDirIdentity(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:              "ant",
		Dir:               "myrig",
		Provider:          "test-agent",
		WorkDir:           ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(4),
	}}
	fs.cfg.NamedSessions = nil
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	for i := 0; i < 2; i++ {
		req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/ant"}`))
		req.Header.Set("Idempotency-Key", "pool-create-"+string(rune('a'+i)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("create #%d status = %d, want %d; body: %s", i+1, rec.Code, http.StatusAccepted, rec.Body.String())
		}
		// Wait for the async goroutine to finish before issuing the next create,
		// so the lock/uniqueness checks see the previous session.
		waitForNSessionCreateEvents(t, fs.eventProv, i+1, 5*time.Second)
	}

	items, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("session bead count = %d, want 2", len(items))
	}
	seenWorkDir := make(map[string]bool, len(items))
	for _, bead := range items {
		if got := bead.Metadata["alias"]; got != "" {
			t.Fatalf("alias = %q, want empty", got)
		}
		sessionName := bead.Metadata["session_name"]
		if got := bead.Metadata["session_name_explicit"]; got != "true" {
			t.Fatalf("session_name_explicit = %q, want %q", got, "true")
		}
		if !strings.HasPrefix(sessionName, "ant-adhoc-") {
			t.Fatalf("session_name = %q, want ant-adhoc-*", sessionName)
		}
		workDir := bead.Metadata["work_dir"]
		if filepath.Dir(workDir) != filepath.Join(fs.cityPath, ".gc", "worktrees", "myrig", "ants") {
			t.Fatalf("work_dir parent = %q, want %q", filepath.Dir(workDir), filepath.Join(fs.cityPath, ".gc", "worktrees", "myrig", "ants"))
		}
		base := filepath.Base(workDir)
		if !strings.HasPrefix(base, "ant-adhoc-") {
			t.Fatalf("work_dir base = %q, want ant-adhoc-*", base)
		}
		if seenWorkDir[workDir] {
			t.Fatalf("duplicate work_dir %q", workDir)
		}
		seenWorkDir[workDir] = true
		if got := bead.Metadata["agent_name"]; got != "myrig/"+sessionName {
			t.Fatalf("agent_name(%q) = %q, want %q", sessionName, got, "myrig/"+sessionName)
		}
	}
}

func TestResolveAgentCreateContextUsesConcreteIdentityForMCPMaterialization(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:              "ant",
		Dir:               "myrig",
		Provider:          "opencode",
		Session:           "acp",
		WorkDir:           ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(4),
	}}
	fs.cfg.NamedSessions = nil
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "identity.template.toml"), []byte(`
name = "identity"
command = "/bin/mcp"
args = ["{{.AgentName}}", "{{.WorkDir}}", "{{.TemplateName}}"]
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}

	srv := New(fs)
	createCtx, err := srv.resolveAgentCreateContext("myrig/ant", "")
	if err != nil {
		t.Fatalf("resolveAgentCreateContext: %v", err)
	}
	mcpServers, err := srv.sessionMCPServers("myrig/ant", "opencode", createCtx.Identity, createCtx.WorkDir, "acp", "agent", nil)
	if err != nil {
		t.Fatalf("sessionMCPServers: %v", err)
	}
	if len(mcpServers) != 1 {
		t.Fatalf("len(mcpServers) = %d, want 1", len(mcpServers))
	}
	if got, want := mcpServers[0].Args[0], createCtx.Identity; got != want {
		t.Fatalf("Args[0] = %q, want %q", got, want)
	}
	if got, want := mcpServers[0].Args[1], createCtx.WorkDir; got != want {
		t.Fatalf("Args[1] = %q, want %q", got, want)
	}
	if got, want := mcpServers[0].Args[2], "myrig/ant"; got != want {
		t.Fatalf("Args[2] = %q, want %q", got, want)
	}
}

func TestHandleSessionCreateAsync_PoolTemplateCanonicalizesAliasCollisions(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:              "ant",
		Dir:               "myrig",
		Provider:          "test-agent",
		WorkDir:           ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(4),
	}}
	fs.cfg.NamedSessions = nil
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/ant","alias":"ant-fenrir"}`))
	req.Header.Set("Idempotency-Key", "pool-alias-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first create status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("first create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if success.Session.Alias != "myrig/ant-fenrir" {
		t.Fatalf("Alias = %q, want canonical qualified alias", success.Session.Alias)
	}

	req = newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/ant","alias":"myrig/ant-fenrir"}`))
	req.Header.Set("Idempotency-Key", "pool-alias-2")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("second create status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	accepted2 := decodeAsyncAccepted(t, rec.Body)
	// The second create should fail asynchronously due to alias collision.
	failure2 := waitForRequestFailed(t, fs.eventProv, accepted2.RequestID, 5*time.Second)
	if failure2 == nil {
		t.Fatal("expected second create to fail due to alias collision")
	}
}

func TestHandleProviderSessionCreateRejectsAsync(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"provider","name":"test-agent","async":true}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "async session creation is only supported for configured agent templates") {
		t.Fatalf("body = %q, want provider async guidance", w.Body.String())
	}
	if fs.pokeCount != 0 {
		t.Fatalf("pokeCount = %d, want 0", fs.pokeCount)
	}
}

func TestMaterializeNamedSession_RebrandedSingletonKeepsTemplateWorkDirIdentity(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:              "witness",
		Dir:               "myrig",
		Provider:          "test-agent",
		WorkDir:           ".gc/worktrees/{{.Rig}}/{{.AgentBase}}",
		MaxActiveSessions: intPtr(1),
	}}
	fs.cfg.NamedSessions = []config.NamedSession{{
		Name:     "boot",
		Template: "witness",
		Dir:      "myrig",
	}}
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "myrig/boot")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget: %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec")
	}
	id, err := srv.materializeNamedSession(fs.cityBeadStore, spec)
	if err != nil {
		t.Fatalf("materializeNamedSession: %v", err)
	}
	bead, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	wantWorkDir := filepath.Join(fs.cityPath, ".gc", "worktrees", "myrig", "witness")
	if got := bead.Metadata["work_dir"]; got != wantWorkDir {
		t.Fatalf("work_dir = %q, want %q", got, wantWorkDir)
	}
}

func TestMaterializeNamedSessionStampsProviderFamilyMetadata(t *testing.T) {
	fs := newSessionFakeState(t)
	base := "builtin:claude"
	fs.cfg.Agents = []config.Agent{{
		Name:              "worker",
		Dir:               "myrig",
		Provider:          "claude-max",
		MaxActiveSessions: intPtr(1),
	}}
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"claude-max": {Base: &base, PathCheck: "true"},
	}
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "myrig/worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget: %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec")
	}
	id, err := srv.materializeNamedSession(fs.cityBeadStore, spec)
	if err != nil {
		t.Fatalf("materializeNamedSession: %v", err)
	}
	bead, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	if got := bead.Metadata["provider"]; got != "claude-max" {
		t.Fatalf("provider = %q, want claude-max", got)
	}
	if got := bead.Metadata["provider_kind"]; got != "claude" {
		t.Fatalf("provider_kind = %q, want claude", got)
	}
	if got := bead.Metadata["builtin_ancestor"]; got != "claude" {
		t.Fatalf("builtin_ancestor = %q, want claude", got)
	}
	cfg := fs.sp.LastStartConfig(bead.Metadata["session_name"])
	if cfg == nil {
		t.Fatalf("Start call not recorded: %#v", fs.sp.Calls)
	}
	if got := cfg.Env["GC_PROVIDER"]; got != "claude" {
		t.Fatalf("GC_PROVIDER = %q, want claude", got)
	}
}

func TestMaterializeNamedSessionSeedsCityRuntimeEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "named-anthropic-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://process.example.test")
	t.Setenv("OLLAMA_API_KEY", "named-ollama-token")
	t.Setenv("GC_RIG", "caller-rig")
	t.Setenv("GC_SESSION_NAME", "caller-session")

	fs := newSessionFakeState(t)
	fs.cfg.Providers["test-agent"] = config.ProviderSpec{
		DisplayName: "Test Agent",
		Command:     "/bin/echo",
		Env: map[string]string{
			"ANTHROPIC_BASE_URL": "https://resolved.example.test",
			"GC_CITY":            "/wrong/city",
			"GC_CITY_PATH":       "/wrong/city",
			"PROVIDER_TOKEN":     "ok",
		},
	}
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget: %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec")
	}
	id, err := srv.materializeNamedSession(fs.cityBeadStore, spec)
	if err != nil {
		t.Fatalf("materializeNamedSession: %v", err)
	}
	bead, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	cfg := fs.sp.LastStartConfig(bead.Metadata["session_name"])
	if cfg == nil {
		t.Fatalf("Start call not recorded: %#v", fs.sp.Calls)
	}
	if got := cfg.Env["GC_CITY"]; got != fs.cityPath {
		t.Errorf("Env[GC_CITY] = %q, want %q", got, fs.cityPath)
	}
	if got := cfg.Env["GC_CITY_PATH"]; got != fs.cityPath {
		t.Errorf("Env[GC_CITY_PATH] = %q, want %q", got, fs.cityPath)
	}
	wantRuntimeDir := filepath.Join(fs.cityPath, ".gc", "runtime")
	if got := cfg.Env["GC_CITY_RUNTIME_DIR"]; got != wantRuntimeDir {
		t.Errorf("Env[GC_CITY_RUNTIME_DIR] = %q, want %q", got, wantRuntimeDir)
	}
	if got := cfg.Env["PROVIDER_TOKEN"]; got != "ok" {
		t.Errorf("Env[PROVIDER_TOKEN] = %q, want ok", got)
	}
	for key, want := range map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "named-anthropic-token",
		"ANTHROPIC_BASE_URL":   "https://resolved.example.test",
		"OLLAMA_API_KEY":       "named-ollama-token",
	} {
		if got := cfg.Env[key]; got != want {
			t.Errorf("Env[%s] = %q, want %q", key, got, want)
		}
	}
	if got, present := cfg.Env["GC_RIG"]; present {
		t.Errorf("Env[GC_RIG] = %q present, want absent caller context", got)
	}
	if got := cfg.Env["GC_SESSION_NAME"]; got == "caller-session" {
		t.Errorf("Env[GC_SESSION_NAME] = %q, want runtime session name not caller context", got)
	}
	if got, present := cfg.Env["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]; present {
		t.Errorf("Env[GC_CONTROL_DISPATCHER_TRACE_DEFAULT] = %q present, want absent", got)
	}
}

func TestMaterializeNamedSessionRejectsACPTemplateWithoutACPRouting(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "opencode"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget: %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec")
	}
	if _, err := srv.materializeNamedSession(fs.cityBeadStore, spec); err == nil {
		t.Fatal("materializeNamedSession() error = nil, want ACP routing error")
	} else if !strings.Contains(err.Error(), "requires ACP transport") {
		t.Fatalf("materializeNamedSession() error = %v, want ACP transport error", err)
	}
	items, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("session bead count = %d, want 0", len(items))
	}
}

func TestMaterializeNamedSessionPersistsStoredMCPMetadata(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "opencode"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "identity.template.toml"), []byte(`
name = "identity"
command = "/bin/mcp"
args = ["{{.AgentName}}", "{{.WorkDir}}", "{{.TemplateName}}"]
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  &transportCapableProvider{Fake: runtime.NewFake()},
	}
	srv := New(state)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget: %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec")
	}
	id, err := srv.materializeNamedSession(fs.cityBeadStore, spec)
	if err != nil {
		t.Fatalf("materializeNamedSession: %v", err)
	}
	bead, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got, want := bead.Metadata[session.MCPIdentityMetadataKey], spec.Identity; got != want {
		t.Fatalf("mcp_identity = %q, want %q", got, want)
	}
	if got := bead.Metadata[session.MCPServersSnapshotMetadataKey]; got == "" {
		t.Fatal("mcp_servers_snapshot = empty, want persisted snapshot")
	}
}

func TestHandleProviderSessionCreateWithMessageUsesProviderDefaultNudge(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	body := `{"kind":"provider","name":"test-agent","message":"hello"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
}

func TestHandleProviderSessionCreateUsesACPTransportCommand(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	defaultSP := runtime.NewFake()
	acpSP := &transportCapableProvider{Fake: runtime.NewFake()}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  sessionauto.New(defaultSP, acpSP),
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"opencode"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	var accepted asyncAcceptedBody
	if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	success, failure := waitForSessionCreateResult(t, state.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	start := acpSP.LastStartConfig(success.Session.SessionName)
	if start == nil {
		t.Fatalf("LastStartConfig(%q) = nil", success.Session.SessionName)
	}
	if got, want := start.Command, "/bin/echo acp"; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
	bead, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", success.Session.ID, err)
	}
	if got, want := bead.Metadata["transport"], "acp"; got != want {
		t.Fatalf("transport metadata = %q, want %q", got, want)
	}
	if defaultSP.IsRunning(success.Session.SessionName) {
		t.Fatalf("default backend should not own ACP session %q", success.Session.SessionName)
	}
}

func TestHumaCreateProviderSessionUsesACPTransportCommand(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	defaultSP := runtime.NewFake()
	acpSP := &transportCapableProvider{Fake: runtime.NewFake()}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  sessionauto.New(defaultSP, acpSP),
	}
	srv := New(state)

	out, err := srv.humaCreateProviderSession(context.Background(), fs.SessionsBeadStore(), sessionCreateBody{
		Kind: "provider",
		Name: "opencode",
	}, "opencode")
	if err != nil {
		t.Fatalf("humaCreateProviderSession: %v", err)
	}
	if got, want := out.Status, http.StatusAccepted; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if out.Body.RequestID == "" {
		t.Fatal("request_id is empty")
	}
	success, failure := waitForSessionCreateResult(t, fs.eventProv, out.Body.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	bead, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", success.Session.ID, err)
	}
	sessionName := bead.Metadata["session_name"]
	start := acpSP.LastStartConfig(sessionName)
	if start == nil {
		t.Fatalf("LastStartConfig(%q) = nil", sessionName)
	}
	if got, want := start.Command, "/bin/echo acp"; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
	if got, want := bead.Metadata["transport"], "acp"; got != want {
		t.Fatalf("transport metadata = %q, want %q", got, want)
	}
	if defaultSP.IsRunning(sessionName) {
		t.Fatalf("default backend should not own ACP session %q", sessionName)
	}
}

func TestHandleProviderSessionCreateUsesACPTransportCapabilityProvider(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	provider := &transportCapableProvider{Fake: runtime.NewFake()}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  provider,
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"opencode"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	var accepted asyncAcceptedBody
	if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	success, failure := waitForSessionCreateResult(t, state.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	start := provider.LastStartConfig(success.Session.SessionName)
	if start == nil {
		t.Fatalf("LastStartConfig(%q) = nil", success.Session.SessionName)
	}
	if got, want := start.Command, "/bin/echo acp"; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
	bead, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", success.Session.ID, err)
	}
	if got, want := bead.Metadata["transport"], "acp"; got != want {
		t.Fatalf("transport metadata = %q, want %q", got, want)
	}
}

func TestHandleProviderSessionCreateUsesPerSessionMCPIdentity(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "identity.template.toml"), []byte(`
name = "identity"
command = "/bin/mcp"
args = ["{{.AgentName}}", "{{.WorkDir}}", "{{.TemplateName}}"]
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}
	provider := &transportCapableProvider{Fake: runtime.NewFake()}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  provider,
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"opencode"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	var accepted asyncAcceptedBody
	if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	success, failure := waitForSessionCreateResult(t, state.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	start := provider.LastStartConfig(success.Session.SessionName)
	if start == nil {
		t.Fatalf("LastStartConfig(%q) = nil", success.Session.SessionName)
	}
	if len(start.MCPServers) != 1 {
		t.Fatalf("Start MCPServers len = %d, want 1", len(start.MCPServers))
	}
	bead, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", success.Session.ID, err)
	}
	if got := bead.Metadata[session.MCPIdentityMetadataKey]; got == "" {
		t.Fatal("mcp_identity metadata = empty, want per-session identity")
	}
	if got, want := start.MCPServers[0].Args[0], bead.Metadata[session.MCPIdentityMetadataKey]; got != want {
		t.Fatalf("Start MCP identity = %q, want %q", got, want)
	}
	if got := bead.Metadata[session.MCPIdentityMetadataKey]; got == "opencode" {
		t.Fatalf("mcp_identity metadata = %q, want unique per-session identity", got)
	}
	if got, want := start.MCPServers[0].Args[1], fs.cityPath; got != want {
		t.Fatalf("Start workdir arg = %q, want %q", got, want)
	}
	if got, want := start.MCPServers[0].Args[2], bead.Metadata[session.MCPIdentityMetadataKey]; got != want {
		t.Fatalf("Start template arg = %q, want %q", got, want)
	}
}

func TestHandleProviderSessionCreateRejectsACPProviderWithoutACPRouting(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"opencode"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "requires ACP transport") {
		t.Fatalf("body = %q, want ACP transport error", rec.Body.String())
	}
}

func TestHumaCreateProviderSessionRejectsACPProviderWithoutACPRouting(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	srv := New(fs)

	if _, err := srv.humaCreateProviderSession(context.Background(), fs.SessionsBeadStore(), sessionCreateBody{
		Kind: "provider",
		Name: "opencode",
	}, "opencode"); err == nil {
		t.Fatal("humaCreateProviderSession() error = nil, want ACP routing error")
	}
}

func TestHandleProviderSessionCreateWithMessageRollsBackOnDeliveryFailure(t *testing.T) {
	fs := newSessionFakeState(t)
	provider := &failNudgeProvider{Fake: runtime.NewFake(), err: errors.New("nudge failed")}
	wrappedState := &stateWithSessionProvider{fakeState: fs, provider: provider}
	srv := New(wrappedState)
	h := newTestCityHandlerWith(t, wrappedState, srv)

	body := `{"kind":"provider","name":"test-agent","message":"hello","title":"Retryable"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	accepted := decodeAsyncAccepted(t, rec.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success != nil {
		t.Fatalf("session create succeeded unexpectedly: %+v", success)
	}
	if failure == nil {
		t.Fatal("expected session create failure event")
	}
	if failure.ErrorCode != "message_delivery_failed" {
		t.Fatalf("failure error_code = %q, want message_delivery_failed; message=%s", failure.ErrorCode, failure.ErrorMessage)
	}
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, provider)
	sessions, err := mgr.List("", "")
	if err != nil {
		t.Fatalf("list sessions after rollback: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("got %d sessions after rollback, want 0: %+v", len(sessions), sessions)
	}
}

func TestHandleSessionCreatePersistsAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","alias":"sky"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	resp := success.Session
	if resp.Alias != "sky" {
		t.Fatalf("Alias = %q, want sky", resp.Alias)
	}
	if resp.SessionName == "sky" {
		t.Fatalf("SessionName = %q, want bead-derived runtime name", resp.SessionName)
	}
}

func TestHandleSessionCreateRejectsReservedQualifiedAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","alias":"myrig/worker"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	var accepted asyncAcceptedBody
	if err := json.NewDecoder(w.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if failure == nil {
		t.Fatal("expected session create to fail for reserved alias")
	}
}

func TestHandleProviderSessionCreateRejectsReservedQualifiedAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	body := `{"kind":"provider","name":"test-agent","alias":"myrig/worker"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	accepted := decodeAsyncAccepted(t, w.Body)
	_, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if failure == nil {
		t.Fatalf("expected session create to fail for reserved alias, got success")
	}
}

func TestHandleSessionCreateRejectsInvalidAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","alias":"bad:name"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleSessionCreateRejectsLegacySessionNameField(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","session_name":"mayor"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "use alias") {
		t.Fatalf("body = %q, want use alias guidance", w.Body.String())
	}
}

func TestHandleSessionCreateRejectsEmptyLegacySessionNameField(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","session_name":""}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "use alias") {
		t.Fatalf("body = %q, want use alias guidance", w.Body.String())
	}
}

func TestHandleSessionCreateRejectsDuplicateAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	first := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker","alias":"sky"}`))
	firstW := httptest.NewRecorder()
	h.ServeHTTP(firstW, first)
	if firstW.Code != http.StatusAccepted {
		t.Fatalf("first create status %d, want %d; body: %s", firstW.Code, http.StatusAccepted, firstW.Body.String())
	}
	var accepted asyncAcceptedBody
	if err := json.NewDecoder(firstW.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode first 202: %v", err)
	}
	// Wait for the first create to finish so the alias is persisted.
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("first create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}

	second := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker","alias":"sky"}`))
	secondW := httptest.NewRecorder()
	h.ServeHTTP(secondW, second)

	if secondW.Code != http.StatusAccepted {
		t.Fatalf("second create status = %d, want %d; body: %s", secondW.Code, http.StatusAccepted, secondW.Body.String())
	}
	var accepted2 asyncAcceptedBody
	if err := json.NewDecoder(secondW.Body).Decode(&accepted2); err != nil {
		t.Fatalf("decode second 202: %v", err)
	}
	failure2 := waitForRequestFailed(t, fs.eventProv, accepted2.RequestID, 5*time.Second)
	if failure2 == nil {
		t.Fatal("expected second create to fail due to duplicate alias")
	}
}

func TestHandleSessionCreateCanonicalizesBareTemplate(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"worker"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	var accepted asyncAcceptedBody
	if err := json.NewDecoder(w.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if accepted.RequestID == "" {
		t.Fatal("missing request_id")
	}
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	resp := success.Session
	if resp.Template != "myrig/worker" {
		t.Errorf("Template = %q, want %q", resp.Template, "myrig/worker")
	}
	if resp.Title != "myrig/worker" {
		t.Errorf("Title = %q, want %q", resp.Title, "myrig/worker")
	}
}

// newSessionFakeStateWithOptions creates a test state where the provider has
// OptionsSchema and OptionDefaults, mimicking the builtin claude provider.
func newSessionFakeStateWithOptions(t *testing.T) *fakeState {
	t.Helper()
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"test-agent": {
			DisplayName: "Test Agent",
			Command:     "echo",
			OptionDefaults: map[string]string{
				"permission_mode": "unrestricted",
				"effort":          "max",
			},
			OptionsSchema: []config.ProviderOption{
				{
					Key: "permission_mode", Label: "Permission Mode", Type: "select",
					Default: "auto-edit",
					Choices: []config.OptionChoice{
						{Value: "auto-edit", Label: "Auto edit", FlagArgs: []string{"--permission-mode", "auto-edit"}},
						{Value: "unrestricted", Label: "Unrestricted", FlagArgs: []string{"--skip-permissions"}},
						{Value: "plan", Label: "Plan", FlagArgs: []string{"--permission-mode", "plan"}},
					},
				},
				{
					Key: "effort", Label: "Effort", Type: "select",
					Default: "",
					Choices: []config.OptionChoice{
						{Value: "", Label: "Default", FlagArgs: nil},
						{Value: "low", Label: "Low", FlagArgs: []string{"--effort", "low"}},
						{Value: "max", Label: "Max", FlagArgs: []string{"--effort", "max"}},
						{Value: "high", Label: "High", FlagArgs: []string{"--effort", "high"}},
					},
				},
			},
		},
	}
	return fs
}

func TestHandleSessionCreateDoesNotApplyProviderDefaultsToAgentCommand(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}

	b, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	cmd := b.Metadata["command"]
	if strings.Contains(cmd, "--skip-permissions") {
		t.Errorf("command %q should not contain provider default flags for deferred agent create", cmd)
	}
	if strings.Contains(cmd, "--effort max") {
		t.Errorf("command %q should not contain provider default effort=max for deferred agent create", cmd)
	}
}

func TestHandleSessionCreateStoresExplicitOverridesWithoutCommandRewrite(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","options":{"effort":"high"}}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}

	b, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	cmd := b.Metadata["command"]
	if strings.Contains(cmd, "--skip-permissions") || strings.Contains(cmd, "--effort high") || strings.Contains(cmd, "--effort max") {
		t.Errorf("command %q should not be rewritten from provider defaults or explicit overrides", cmd)
	}
	ovr := b.Metadata["template_overrides"]
	if ovr == "" {
		t.Fatal("template_overrides not set")
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(ovr), &parsed); err != nil {
		t.Fatalf("parse template_overrides: %v", err)
	}
	if parsed["effort"] != "high" {
		t.Errorf("effort = %q, want %q", parsed["effort"], "high")
	}
	if _, ok := parsed["permission_mode"]; ok {
		t.Errorf("permission_mode override unexpectedly present: %#v", parsed)
	}
}

func TestHandleSessionCreatePersistsExplicitOptionsInTemplateOverrides(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","options":{"permission_mode":"plan","effort":"low"}}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}

	b, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	cmd := b.Metadata["command"]
	if strings.Contains(cmd, "--permission-mode plan") || strings.Contains(cmd, "--skip-permissions") || strings.Contains(cmd, "--effort low") {
		t.Errorf("command %q should not be rewritten from explicit overrides", cmd)
	}
	ovr := b.Metadata["template_overrides"]
	if ovr == "" {
		t.Fatal("template_overrides not set")
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(ovr), &parsed); err != nil {
		t.Fatalf("parse template_overrides: %v", err)
	}
	if parsed["permission_mode"] != "plan" {
		t.Errorf("permission_mode = %q, want %q", parsed["permission_mode"], "plan")
	}
	if parsed["effort"] != "low" {
		t.Errorf("effort = %q, want %q", parsed["effort"], "low")
	}
}

func TestHandleSessionCreatePreservesInitialMessageWithOptions(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	// Create session with BOTH options AND a message.
	// Regression: the old code overwrote template_overrides with just the
	// options, clobbering the initial_message that was set at creation time.
	body := `{"kind":"agent","name":"myrig/worker","message":"Hello from Discord!","options":{"effort":"high"}}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}

	b, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	ovr := b.Metadata["template_overrides"]
	if ovr == "" {
		t.Fatal("template_overrides not set")
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(ovr), &parsed); err != nil {
		t.Fatalf("parse template_overrides: %v", err)
	}
	if parsed["initial_message"] != "Hello from Discord!" {
		t.Errorf("initial_message = %q, want %q", parsed["initial_message"], "Hello from Discord!")
	}
	if parsed["effort"] != "high" {
		t.Errorf("effort = %q, want %q", parsed["effort"], "high")
	}
}

func TestHandleSessionPermissionModeUpdatesSchemaBackedOverride(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	body := `{"kind":"agent","name":"myrig/worker","message":"keep me","options":{"effort":"high"}}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	suspendSessionForPermissionModeTest(t, fs, success.Session.ID)

	req = newPostRequest(cityURL(fs, "/session/"+success.Session.ID+"/permission-mode"), strings.NewReader(`{"permission_mode":"plan"}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("permission-mode status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := resp.Options["permission_mode"]; got != "plan" {
		t.Fatalf("response options.permission_mode = %q, want plan", got)
	}
	if got := w.Header().Get("X-GC-Index"); got == "" {
		t.Fatal("permission-mode response missing X-GC-Index")
	}
	if got := resp.Options["effort"]; got != "high" {
		t.Fatalf("response options.effort = %q, want high", got)
	}

	b, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	var overrides map[string]string
	if err := json.Unmarshal([]byte(b.Metadata["template_overrides"]), &overrides); err != nil {
		t.Fatalf("parse template_overrides: %v", err)
	}
	if got := overrides["permission_mode"]; got != "plan" {
		t.Fatalf("template_overrides.permission_mode = %q, want plan", got)
	}
	if got := b.Metadata["opt_permission_mode"]; got != "plan" {
		t.Fatalf("opt_permission_mode = %q, want plan", got)
	}
	if got := overrides["effort"]; got != "high" {
		t.Fatalf("template_overrides.effort = %q, want high", got)
	}
	if got := overrides["initial_message"]; got != "keep me" {
		t.Fatalf("template_overrides.initial_message = %q, want keep me", got)
	}
}

func TestLegacySessionPermissionModeRouteUpdatesSchemaBackedOverride(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	suspendSessionForPermissionModeTest(t, fs, success.Session.ID)

	req = newPostRequest("/v0/session/"+success.Session.ID+"/permission-mode", strings.NewReader(`{"permission_mode":"plan"}`))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("legacy permission-mode status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := resp.Options["permission_mode"]; got != "plan" {
		t.Fatalf("response options.permission_mode = %q, want plan", got)
	}
	if got := w.Header().Get("X-GC-Index"); got == "" {
		t.Fatal("legacy permission-mode response missing X-GC-Index")
	}

	b, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	if got := b.Metadata["opt_permission_mode"]; got != "plan" {
		t.Fatalf("opt_permission_mode = %q, want plan", got)
	}
	var overrides map[string]string
	if err := json.Unmarshal([]byte(b.Metadata["template_overrides"]), &overrides); err != nil {
		t.Fatalf("parse template_overrides: %v", err)
	}
	if got := overrides["permission_mode"]; got != "plan" {
		t.Fatalf("template_overrides.permission_mode = %q, want plan", got)
	}
}

func TestLegacySessionPermissionModeRouteRequiresCSRFHeader(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)

	req := httptest.NewRequest(http.MethodPost, "/v0/session/session-1/permission-mode", strings.NewReader(`{"permission_mode":"plan"}`))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("legacy permission-mode status = %d, want %d; body: %s", w.Code, http.StatusForbidden, w.Body.String())
	}
}

func TestHandleSessionPermissionModeRejectsValueOutsideProviderSchema(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	suspendSessionForPermissionModeTest(t, fs, success.Session.ID)

	req = newPostRequest(cityURL(fs, "/session/"+success.Session.ID+"/permission-mode"), strings.NewReader(`{"permission_mode":"bypassPermissions"}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("permission-mode status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	b, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	if strings.Contains(b.Metadata["template_overrides"], "bypassPermissions") {
		t.Fatalf("invalid schema value was persisted: %s", b.Metadata["template_overrides"])
	}
}

func TestHandleSessionPermissionModeRejectsProviderWithoutPermissionModeOption(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	fs.cfg.Providers["test-agent"] = config.ProviderSpec{
		DisplayName: "Test Agent",
		Command:     "echo",
		OptionsSchema: []config.ProviderOption{{
			Key: "effort", Label: "Effort", Type: "select",
			Choices: []config.OptionChoice{{Value: "high", Label: "High", FlagArgs: []string{"--effort", "high"}}},
		}},
	}
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	suspendSessionForPermissionModeTest(t, fs, success.Session.ID)

	req = newPostRequest(cityURL(fs, "/session/"+success.Session.ID+"/permission-mode"), strings.NewReader(`{"permission_mode":"plan"}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("permission-mode status = %d, want %d; body: %s", w.Code, http.StatusNotImplemented, w.Body.String())
	}
}

func TestHandleSessionPermissionModeUpdatesProviderSessionOptions(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"test-agent"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	suspendSessionForPermissionModeTest(t, fs, success.Session.ID)

	req = newPostRequest(cityURL(fs, "/session/"+success.Session.ID+"/permission-mode"), strings.NewReader(`{"permission_mode":"auto-edit"}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("permission-mode status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := resp.Options["permission_mode"]; got != "auto-edit" {
		t.Fatalf("response options.permission_mode = %q, want auto-edit", got)
	}

	req = httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/"+success.Session.ID), nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if got := resp.Options["permission_mode"]; got != "auto-edit" {
		t.Fatalf("get options.permission_mode = %q, want auto-edit", got)
	}
	if got := resp.Options["effort"]; got != "max" {
		t.Fatalf("get options.effort = %q, want max default", got)
	}
}

func TestHandleSessionPermissionModePreservesProviderCreateOptions(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"test-agent","options":{"permission_mode":"plan","effort":"high"}}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	suspendSessionForPermissionModeTest(t, fs, success.Session.ID)

	req = newPostRequest(cityURL(fs, "/session/"+success.Session.ID+"/permission-mode"), strings.NewReader(`{"permission_mode":"auto-edit"}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("permission-mode status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := resp.Options["permission_mode"]; got != "auto-edit" {
		t.Fatalf("response options.permission_mode = %q, want auto-edit", got)
	}
	if got := resp.Options["effort"]; got != "high" {
		t.Fatalf("response options.effort = %q, want high from create-time provider option", got)
	}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	bead, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("Get bead: %v", err)
	}
	runtimeCfg, err := srv.resolveWorkerSessionRuntimeWithMetadata(info, "", bead.Metadata)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntimeWithMetadata: %v", err)
	}
	if runtimeCfg == nil {
		t.Fatal("resolveWorkerSessionRuntimeWithMetadata() = nil")
	}
	if !strings.Contains(runtimeCfg.Command, "--permission-mode auto-edit") {
		t.Fatalf("runtime command %q missing updated permission_mode", runtimeCfg.Command)
	}
	if !strings.Contains(runtimeCfg.Command, "--effort high") {
		t.Fatalf("runtime command %q missing preserved effort", runtimeCfg.Command)
	}
}

func TestLegacyHandleProviderSessionCreatePersistsOptionsInTemplateOverrides(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)

	req := newPostRequest("/v0/sessions", strings.NewReader(`{"kind":"provider","name":"test-agent","options":{"permission_mode":"plan","effort":"high"}}`))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	bead, err := fs.cityBeadStore.Get(resp.ID)
	if err != nil {
		t.Fatalf("Get bead: %v", err)
	}
	overrides, err := session.ParseTemplateOverrides(bead.Metadata)
	if err != nil {
		t.Fatalf("ParseTemplateOverrides: %v", err)
	}
	if got := overrides["permission_mode"]; got != "plan" {
		t.Fatalf("template_overrides.permission_mode = %q, want plan", got)
	}
	if got := overrides["effort"]; got != "high" {
		t.Fatalf("template_overrides.effort = %q, want high", got)
	}
}

func TestHandleSessionPermissionModePrefersPersistedProviderOverTemplateProvider(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	fs.cfg.Providers["template-provider"] = config.ProviderSpec{
		DisplayName: "Template Provider",
		Command:     "echo",
		OptionsSchema: []config.ProviderOption{{
			Key:     "permission_mode",
			Label:   "Permission Mode",
			Type:    "select",
			Default: "plan",
			Choices: []config.OptionChoice{{
				Value:    "plan",
				Label:    "Plan",
				FlagArgs: []string{"--permission-mode", "plan"},
			}},
		}},
	}
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"test-agent"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	suspendSessionForPermissionModeTest(t, fs, success.Session.ID)
	if err := fs.cityBeadStore.SetMetadata(success.Session.ID, "template", "template-provider"); err != nil {
		t.Fatalf("SetMetadata(template): %v", err)
	}

	req = newPostRequest(cityURL(fs, "/session/"+success.Session.ID+"/permission-mode"), strings.NewReader(`{"permission_mode":"auto-edit"}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("permission-mode status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := resp.Options["permission_mode"]; got != "auto-edit" {
		t.Fatalf("response options.permission_mode = %q, want auto-edit", got)
	}
	if got := resp.Options["effort"]; got != "max" {
		t.Fatalf("response options.effort = %q, want max from persisted provider", got)
	}
}

func TestHandleSessionPermissionModeDoesNotFallbackToAgentWhenProviderMissing(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"test-agent"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	suspendSessionForPermissionModeTest(t, fs, success.Session.ID)
	delete(fs.cfg.Providers, "test-agent")
	if err := fs.cityBeadStore.SetMetadata(success.Session.ID, "template", "myrig/worker"); err != nil {
		t.Fatalf("SetMetadata(template): %v", err)
	}

	req = newPostRequest(cityURL(fs, "/session/"+success.Session.ID+"/permission-mode"), strings.NewReader(`{"permission_mode":"plan"}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("permission-mode status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
	b, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	if strings.Contains(b.Metadata["template_overrides"], "plan") {
		t.Fatalf("permission_mode persisted after missing provider: %s", b.Metadata["template_overrides"])
	}
}

func TestHandleSessionGetUsesAgentDefaultsForConfiguredNamedSession(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	fs.cfg.Agents[0].OptionDefaults = map[string]string{
		"permission_mode": "plan",
	}
	fs.cfg.Providers["myrig/worker"] = config.ProviderSpec{
		DisplayName: "Colliding Provider",
		Command:     "echo",
		OptionDefaults: map[string]string{
			"permission_mode": "auto-edit",
		},
		OptionsSchema: fs.cfg.Providers["test-agent"].OptionsSchema,
	}
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "worker", Command: "echo test", WorkDir: "/tmp", Provider: "test-agent", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	suspendSessionForPermissionModeTest(t, fs, info.ID)
	if err := fs.cityBeadStore.SetMetadata(info.ID, apiNamedSessionMetadataKey, "true"); err != nil {
		t.Fatalf("SetMetadata(%s): %v", apiNamedSessionMetadataKey, err)
	}

	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/"+info.ID), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := resp.Options["permission_mode"]; got != "plan" {
		t.Fatalf("get options.permission_mode = %q, want plan from configured agent defaults", got)
	}
	if got := resp.Kind; got != "agent" {
		t.Fatalf("get kind = %q, want agent", got)
	}
}

func TestHandleSessionGetUsesLegacyProviderKindForNameCollision(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	fs.cfg.Agents = []config.Agent{{
		Name:     "codex",
		Provider: "codex",
		OptionDefaults: map[string]string{
			"permission_mode": "plan",
			"effort":          "low",
		},
	}}
	fs.cfg.Providers["codex"] = config.ProviderSpec{
		DisplayName: "Codex",
		Command:     "echo",
		OptionDefaults: map[string]string{
			"permission_mode": "auto-edit",
			"effort":          "max",
		},
		OptionsSchema: fs.cfg.Providers["test-agent"].OptionsSchema,
	}
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "codex", Title: "codex", Command: "echo", WorkDir: "/tmp/provider", Provider: "codex", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	suspendSessionForPermissionModeTest(t, fs, info.ID)
	if err := fs.cityBeadStore.SetMetadata(info.ID, "real_world_app_session_kind", "provider"); err != nil {
		t.Fatalf("SetMetadata(real_world_app_session_kind): %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/"+info.ID), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := resp.Options["permission_mode"]; got != "auto-edit" {
		t.Fatalf("get options.permission_mode = %q, want provider default auto-edit", got)
	}
	if got := resp.Options["effort"]; got != "max" {
		t.Fatalf("get options.effort = %q, want provider default max", got)
	}
	if got := resp.Kind; got != "provider" {
		t.Fatalf("get kind = %q, want provider", got)
	}
}

func TestHandleSessionPermissionModeRepairsMalformedTemplateOverrides(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	suspendSessionForPermissionModeTest(t, fs, success.Session.ID)
	if err := fs.cityBeadStore.SetMetadata(success.Session.ID, "template_overrides", "{not-json"); err != nil {
		t.Fatalf("SetMetadata(template_overrides): %v", err)
	}

	req = newPostRequest(cityURL(fs, "/session/"+success.Session.ID+"/permission-mode"), strings.NewReader(`{"permission_mode":"plan"}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("permission-mode status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	b, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	overrides, err := session.ParseTemplateOverrides(b.Metadata)
	if err != nil {
		t.Fatalf("ParseTemplateOverrides: %v", err)
	}
	if got := overrides["permission_mode"]; got != "plan" {
		t.Fatalf("permission_mode = %q, want plan", got)
	}
}

func TestHandleSessionPermissionModeRejectsMissingAgentTemplate(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	suspendSessionForPermissionModeTest(t, fs, success.Session.ID)
	if err := fs.cityBeadStore.SetMetadata(success.Session.ID, "template", "myrig/missing-agent"); err != nil {
		t.Fatalf("SetMetadata(template): %v", err)
	}
	if err := fs.cityBeadStore.SetMetadata(success.Session.ID, "agent_name", "myrig/missing-agent"); err != nil {
		t.Fatalf("SetMetadata(agent_name): %v", err)
	}

	req = newPostRequest(cityURL(fs, "/session/"+success.Session.ID+"/permission-mode"), strings.NewReader(`{"permission_mode":"plan"}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("permission-mode status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}

	b, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	if strings.Contains(b.Metadata["template_overrides"], "plan") {
		t.Fatalf("permission_mode persisted for missing agent template: %s", b.Metadata["template_overrides"])
	}
}

func TestHandleSessionGetDoesNotExposeOptionsForMissingAgentTemplate(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker","options":{"effort":"high"}}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if err := fs.cityBeadStore.SetMetadata(success.Session.ID, "template", "myrig/missing-agent"); err != nil {
		t.Fatalf("SetMetadata(template): %v", err)
	}
	if err := fs.cityBeadStore.SetMetadata(success.Session.ID, "agent_name", "myrig/missing-agent"); err != nil {
		t.Fatalf("SetMetadata(agent_name): %v", err)
	}

	req = httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/"+success.Session.ID), nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Options) != 0 {
		t.Fatalf("response options = %#v, want none for missing agent template", resp.Options)
	}
}

func TestHandleSessionPermissionModeRejectsRunningSession(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}

	req = newPostRequest(cityURL(fs, "/session/"+success.Session.ID+"/permission-mode"), strings.NewReader(`{"permission_mode":"plan"}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("permission-mode status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestSessionPermissionModeRuntimeActiveStates(t *testing.T) {
	for _, state := range []session.State{session.StateActive, session.StateAwake, session.StateCreating, session.StateDraining, session.StateQuarantined} {
		if !session.IsTemplateOverrideRuntimeActive(state) {
			t.Fatalf("IsTemplateOverrideRuntimeActive(%q) = false, want true", state)
		}
	}
	for _, state := range []session.State{session.StateAsleep, session.StateSuspended, session.StateDrained, session.StateArchived} {
		if session.IsTemplateOverrideRuntimeActive(state) {
			t.Fatalf("IsTemplateOverrideRuntimeActive(%q) = true, want false", state)
		}
	}
}

func TestHandleSessionPermissionModeReturnsOverrideWithoutProviderDefault(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	provider := fs.cfg.Providers["test-agent"]
	delete(provider.OptionDefaults, "permission_mode")
	for i := range provider.OptionsSchema {
		if provider.OptionsSchema[i].Key == "permission_mode" {
			provider.OptionsSchema[i].Default = ""
		}
	}
	fs.cfg.Providers["test-agent"] = provider
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	suspendSessionForPermissionModeTest(t, fs, success.Session.ID)

	req = newPostRequest(cityURL(fs, "/session/"+success.Session.ID+"/permission-mode"), strings.NewReader(`{"permission_mode":"plan"}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("permission-mode status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := resp.Options["permission_mode"]; got != "plan" {
		t.Fatalf("response options.permission_mode = %q, want plan", got)
	}
}

func TestHandleSessionMessageMaterializedNamedSessionUsesLaunchCommandDefaults(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/session/worker/messages"), strings.NewReader(`{"message":"hello"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var resp asyncAcceptedBody
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	success, failure := waitForSessionMessageResult(t, fs.eventProv, resp.RequestID)
	if success == nil {
		t.Fatalf("session message failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	id := success.SessionID
	if id == "" {
		t.Fatal("session message event missing session_id")
	}

	bead, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	cmd := bead.Metadata["command"]
	if !strings.Contains(cmd, "--skip-permissions") {
		t.Fatalf("command %q missing permission default", cmd)
	}
	if !strings.Contains(cmd, "--effort max") {
		t.Fatalf("command %q missing effort default", cmd)
	}
}

func TestHandleSessionMessageQueuesSuspendedSessionMessage(t *testing.T) {
	fs := newSessionFakeState(t)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Resume Me")
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	blocker := &blockingStartProvider{
		Fake:    fs.sp,
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	var unblockOnce sync.Once
	unblock := func() {
		unblockOnce.Do(func() {
			close(blocker.unblock)
		})
	}
	t.Cleanup(unblock)

	srv := New(&stateWithSessionProvider{fakeState: fs, provider: blocker})
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/messages", strings.NewReader(`{"message":"hello"}`))
	req.Header.Set("Idempotency-Key", "sess-msg-1")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(w, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler blocked on suspended-session start instead of returning accepted")
	}

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)

	select {
	case <-blocker.started:
	case <-time.After(testEventTimeout):
		t.Fatal("provider start was not reached")
	}
	unblock()

	success, failure := waitForSessionMessageResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session message failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
}

func TestHandleSessionMessageMaterializesNamedSessionAsync(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/session/worker/messages"), strings.NewReader(`{"message":"hello"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("message status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var resp asyncAcceptedBody
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RequestID == "" {
		t.Fatal("response missing request_id")
	}
	if resp.Status != "accepted" {
		t.Fatalf("response status = %q, want accepted", resp.Status)
	}

	success, failure := waitForSessionMessageResult(t, fs.eventProv, resp.RequestID)
	if success == nil {
		t.Fatalf("session message failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	if success.SessionID == "" {
		t.Fatal("event missing session_id")
	}
}

func TestHandleSessionMessageEmitsFailureWhenProviderNudgeHangs(t *testing.T) {
	fs := newSessionFakeState(t)
	blocker := &blockingNudgeProvider{
		Fake:    fs.sp,
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	t.Cleanup(func() {
		close(blocker.unblock)
	})
	prevTimeout := sessionMessageAsyncTimeout
	sessionMessageAsyncTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		sessionMessageAsyncTimeout = prevTimeout
	})

	srv := New(&stateWithSessionProvider{fakeState: fs, provider: blocker})
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "blocked-message")
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/messages", strings.NewReader(`{"message":"hello"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("message status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)

	select {
	case <-blocker.started:
	case <-time.After(testEventTimeout):
		t.Fatal("provider nudge was not reached")
	}
	success, failure := waitForSessionMessageResult(t, fs.eventProv, accepted.RequestID)
	if success != nil {
		t.Fatalf("unexpected success: %+v", success)
	}
	if failure == nil {
		t.Fatal("expected request.failed for blocked provider nudge")
	}
	if failure.ErrorCode != "timeout" {
		t.Fatalf("failure error_code = %q, want timeout", failure.ErrorCode)
	}
}

func TestSessionMessageAsyncTimeoutMatchesClientTimeout(t *testing.T) {
	if sessionMessageAsyncTimeout != sessionMessageTimeout {
		t.Fatalf("sessionMessageAsyncTimeout = %s, want client timeout %s", sessionMessageAsyncTimeout, sessionMessageTimeout)
	}
}

func TestHandleSessionMessageLogsLateProviderResultAfterTimeout(t *testing.T) {
	fs := newSessionFakeState(t)
	blocker := &blockingNudgeProvider{
		Fake:    fs.sp,
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	prevTimeout := sessionMessageAsyncTimeout
	sessionMessageAsyncTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		sessionMessageAsyncTimeout = prevTimeout
	})

	var logs bytes.Buffer
	oldOutput := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		log.SetFlags(oldFlags)
	})

	srv := New(&stateWithSessionProvider{fakeState: fs, provider: blocker})
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "late-message")
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/messages", strings.NewReader(`{"message":"hello"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("message status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)

	select {
	case <-blocker.started:
	case <-time.After(testEventTimeout):
		t.Fatal("provider nudge was not reached")
	}
	_, failure := waitForSessionMessageResult(t, fs.eventProv, accepted.RequestID)
	if failure == nil || failure.ErrorCode != "timeout" {
		t.Fatalf("failure = %+v, want timeout", failure)
	}

	close(blocker.unblock)
	deadline := time.Now().Add(testEventTimeout)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), "late session.message result after timeout") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("logs = %q, want late session.message result after timeout", logs.String())
}

func TestHandleSessionMessageMaterializesBoundNamedSessionUsingQualifiedIdentity(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:         "alex",
		BindingName:  "employees",
		Provider:     "test-agent",
		StartCommand: "true",
	}}
	fs.cfg.NamedSessions = []config.NamedSession{{
		Name:        "corp--alex",
		Template:    "alex",
		BindingName: "employees",
	}}
	srv := New(fs)

	req := newPostRequest(cityURL(fs, "/session/employees.corp--alex/messages"), strings.NewReader(`{"message":"hello"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("message status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	accepted := decodeAsyncAccepted(t, rec.Body)
	success, failure := waitForSessionMessageResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session message failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	id := success.SessionID
	if id == "" {
		t.Fatal("session message event missing session_id")
	}
	b, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	if got := b.Metadata[apiNamedSessionMetadataKey]; got != "true" {
		t.Fatalf("configured_named_session = %q, want true", got)
	}
	if got := b.Metadata["alias"]; got != "employees.corp--alex" {
		t.Fatalf("alias = %q, want employees.corp--alex", got)
	}
}

func TestResolveSessionIDMaterializingNamedWithContext_RollsBackCanceledCreate(t *testing.T) {
	fs := newSessionFakeState(t)
	provider := &cancelStartProvider{Fake: runtime.NewFake()}
	srv := New(&stateWithSessionProvider{fakeState: fs, provider: provider})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := srv.resolveSessionIDMaterializingNamedWithContext(ctx, fs.cityBeadStore, "worker")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveSessionIDMaterializingNamedWithContext: %v, want context canceled", err)
	}

	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	for _, b := range all {
		if b.Status != "closed" {
			t.Fatalf("session bead %s status = %q, want closed after canceled create rollback", b.ID, b.Status)
		}
	}
}

func TestHandleSessionGetIncludesConfiguredNamedSessionFlag(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget: %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec for worker")
	}
	id, err := srv.materializeNamedSession(fs.cityBeadStore, spec)
	if err != nil {
		t.Fatalf("materializeNamedSession: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+id, nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.ConfiguredNamedSession {
		t.Fatal("ConfiguredNamedSession = false, want true")
	}
}

func TestHandleSessionMessageInvalidNamedTargetDoesNotMaterialize(t *testing.T) {
	// Fix 3k(remnant): whitespace-only messages are rejected by the
	// pattern:"\\S" validation on the body; Huma returns 422 before
	// the handler runs, so no session materializes.
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	req := newPostRequest(cityURL(fs, "/session/worker/messages"), strings.NewReader(`{"message":"   "}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("message status = %d, want %d; body: %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}
	items, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("invalid message materialized sessions unexpectedly: %#v", items)
	}
}

func TestHandleSessionGetReservedNamedTargetIgnoresClosedHistoricalBead(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(
		context.Background(), session.CreateOptions{Alias: "myrig/worker", ExplicitName: "", Template: "myrig/worker", Title: "Historic Worker", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Transport: "", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSessionNamedWithTransport: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", cityURL(fs, "/session/worker"), nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("get status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandleSessionCloseRejectsAlwaysNamedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.NamedSessions[0].Mode = "always"
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget: %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec for worker")
	}
	id, err := srv.materializeNamedSession(fs.cityBeadStore, spec)
	if err != nil {
		t.Fatalf("materializeNamedSession: %v", err)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/")+id+"/close", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("close status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestFindNamedSessionSpecForTarget_RequiresFullyQualifiedWhenAmbiguous(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{
		{Name: "worker", Dir: "rig-a", Provider: "test-agent"},
		{Name: "worker", Dir: "rig-b", Provider: "test-agent"},
	}
	fs.cfg.NamedSessions = []config.NamedSession{
		{Template: "worker", Dir: "rig-a"},
		{Template: "worker", Dir: "rig-b"},
	}
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	if _, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker"); err == nil || ok {
		t.Fatalf("findNamedSessionSpecForTarget(worker) = ok=%v err=%v, want ambiguous error", ok, err)
	}

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "rig-a/worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget(rig-a/worker): %v", err)
	}
	if !ok {
		t.Fatal("expected fully qualified named session target to resolve")
	}
	if got := spec.Identity; got != "rig-a/worker" {
		t.Fatalf("Identity = %q, want rig-a/worker", got)
	}
}

func TestResolveSessionIDMaterializingNamed_QualifiedAliasBasenameDoesNotStealNamedTarget(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	ordinary, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-other-worker",
			"alias":        "other/worker",
		},
	})
	if err != nil {
		t.Fatalf("create ordinary session bead: %v", err)
	}

	id, err := srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker): %v", err)
	}
	if id == ordinary.ID {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker) returned qualified alias basename match %q; want canonical named session", id)
	}
	bead, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got := bead.Metadata["alias"]; got != "myrig/worker" {
		t.Fatalf("alias = %q, want myrig/worker", got)
	}
	if got := bead.Metadata[apiNamedSessionMetadataKey]; got != "true" {
		t.Fatalf("configured_named_session = %q, want true", got)
	}
}

func TestResolveConfiguredNamedSessionIDWithContext_BoundedListCalls(t *testing.T) {
	fs := newSessionFakeState(t)
	store := &apiListQueryCaptureStore{Store: beads.NewMemStore()}
	fs.cityBeadStore = store
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(store, "worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget(worker): %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec for worker")
	}
	for i := 0; i < 200; i++ {
		if _, err := store.Create(beads.Bead{
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"session_name": fmt.Sprintf("worker-%d", i),
			},
		}); err != nil {
			t.Fatalf("create irrelevant session %d: %v", i, err)
		}
	}
	target, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":             spec.SessionName,
			"alias":                    spec.Identity,
			apiNamedSessionMetadataKey: "true",
			apiNamedSessionIdentityKey: spec.Identity,
			apiNamedSessionModeKey:     spec.Mode,
		},
	})
	if err != nil {
		t.Fatalf("create canonical named session: %v", err)
	}

	id, matched, err := srv.resolveConfiguredNamedSessionIDWithContext(context.Background(), store, "worker", apiSessionResolveOptions{})
	if err != nil {
		t.Fatalf("resolveConfiguredNamedSessionIDWithContext(worker): %v", err)
	}
	if !matched {
		t.Fatal("matched = false, want true")
	}
	if id != target.ID {
		t.Fatalf("id = %q, want canonical %q", id, target.ID)
	}
	if len(store.listCalls) != 1 {
		t.Fatalf("List calls = %d, want 1 canonical lookup", len(store.listCalls))
	}
	assertSessionResolverMetadataFilteredListCalls(t, store.listCalls)
}

func TestResolveConfiguredNamedSessionIDWithContext_BoundedConflictListCalls(t *testing.T) {
	fs := newSessionFakeState(t)
	store := &apiListQueryCaptureStore{Store: beads.NewMemStore()}
	fs.cityBeadStore = store
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(store, "worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget(worker): %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec for worker")
	}
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": spec.SessionName,
			"template":     "other/worker",
			"agent_name":   "other/worker",
			"state":        "asleep",
		},
	}); err != nil {
		t.Fatalf("create wrong-template runtime bead: %v", err)
	}

	_, matched, err := srv.resolveConfiguredNamedSessionIDWithContext(context.Background(), store, "worker", apiSessionResolveOptions{})
	if err == nil {
		t.Fatal("resolveConfiguredNamedSessionIDWithContext(worker) succeeded, want conflict")
	}
	if !matched {
		t.Fatal("matched = false, want true")
	}
	if !errors.Is(err, errConfiguredNamedSessionConflict) {
		t.Fatalf("error = %v, want errConfiguredNamedSessionConflict", err)
	}
	if len(store.listCalls) > 4 {
		t.Fatalf("List calls = %d, want bounded small constant without duplicate session_name lookup", len(store.listCalls))
	}
	assertSessionResolverMetadataFilteredListCalls(t, store.listCalls)
}

func assertSessionResolverMetadataFilteredListCalls(t *testing.T, calls []beads.ListQuery) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("expected at least one List call")
	}
	for i, query := range calls {
		if len(query.Metadata) == 0 {
			t.Fatalf("List call #%d has no metadata filter (would scan broad bead sets): %+v", i, query)
		}
	}
}

func TestResolveSessionIDMaterializingNamed_AdoptsCanonicalRuntimeSessionNameBead(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget(worker): %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec for worker")
	}
	bead, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": spec.SessionName,
			"template":     spec.Identity,
			"agent_name":   spec.Identity,
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatalf("create canonical runtime bead: %v", err)
	}

	id, err := srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker): %v", err)
	}
	if id != bead.ID {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker) = %q, want adopted bead %q", id, bead.ID)
	}
}

func TestResolveSessionIDMaterializingNamed_DoesNotAdoptOrdinaryPoolSessionForSameTemplate(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	ordinary, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-ordinary-worker",
			"template":     "myrig/worker",
			"agent_name":   "myrig/worker",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatalf("create ordinary pool worker: %v", err)
	}

	id, err := srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker): %v", err)
	}
	if id == ordinary.ID {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker) adopted ordinary pool worker %q", ordinary.ID)
	}

	named, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got := named.Metadata[apiNamedSessionMetadataKey]; got != "true" {
		t.Fatalf("configured_named_session = %q, want true", got)
	}
	if got := named.Metadata["alias"]; got != "myrig/worker" {
		t.Fatalf("alias = %q, want myrig/worker", got)
	}

	preserved, err := fs.cityBeadStore.Get(ordinary.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", ordinary.ID, err)
	}
	if preserved.Status != "open" {
		t.Fatalf("ordinary pool worker status = %q, want open", preserved.Status)
	}
	if got := preserved.Metadata[apiNamedSessionMetadataKey]; got != "" {
		t.Fatalf("ordinary pool worker configured_named_session = %q, want empty", got)
	}
}

func TestResolveSessionIDMaterializingNamed_RuntimeSessionNameWrongTemplateConflicts(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget(worker): %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec for worker")
	}
	if _, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": spec.SessionName,
			"template":     "other/worker",
			"agent_name":   "other/worker",
			"state":        "asleep",
		},
	}); err != nil {
		t.Fatalf("create wrong-template runtime bead: %v", err)
	}

	_, err = srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "worker")
	if err == nil || !strings.Contains(err.Error(), "conflicts with configured named session") {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker) error = %v, want configured named session conflict", err)
	}
}

func TestHandleSessionWakeMaterializesNamedSessionAndStartsRuntime(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/worker/wake"), nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("wake status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id := resp["id"]
	if id == "" {
		t.Fatal("wake response missing session id")
	}
	b, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	if got := b.Metadata[apiNamedSessionMetadataKey]; got != "true" {
		t.Fatalf("configured_named_session = %q, want true", got)
	}
	if got := b.Metadata["alias"]; got != "myrig/worker" {
		t.Fatalf("alias = %q, want myrig/worker", got)
	}
	sessionName := b.Metadata["session_name"]
	if sessionName == "" {
		t.Fatal("materialized named session missing session_name")
	}
	if !fs.sp.IsRunning(sessionName) {
		t.Fatalf("session %q should be running after POST /wake", sessionName)
	}
}

func TestHandleSessionWakeCanceledNamedCreateRollsBack(t *testing.T) {
	fs := newSessionFakeState(t)
	provider := &cancelStartProvider{Fake: runtime.NewFake()}
	wrappedState := &stateWithSessionProvider{fakeState: fs, provider: provider}
	srv := New(wrappedState)
	h := newTestCityHandlerWith(t, wrappedState, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/worker/wake"), nil).WithContext(ctx)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("wake status = %d, want %d; body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	for _, b := range all {
		if b.Status != "closed" {
			t.Fatalf("session bead %s status = %q, want closed after canceled wake rollback", b.ID, b.Status)
		}
	}
}

func TestHandleSessionTranscriptUsesSessionKey(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	writeNamedSessionJSONL(t, searchBase, workDir, "latest.jsonl",
		`{"uuid":"9","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"wrong file\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp SessionStreamMessageEvent
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Format != "conversation" {
		t.Errorf("Format = %q, want %q", resp.Format, "conversation")
	}
	if len(resp.Turns) != 2 || resp.Turns[1].Text != "world" {
		t.Fatalf("Turns = %+v, want hello/world from session key file", resp.Turns)
	}
}

func TestHandleSessionTranscriptClosedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?tail=0", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp SessionStreamMessageEvent
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Turns) != 2 || resp.Turns[0].Text != "hello" || resp.Turns[1].Text != "world" {
		t.Fatalf("Turns = %+v, want closed-session transcript hello/world", resp.Turns)
	}
}

func TestHandleSessionTranscriptAfterCursor(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"first\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"second\"}","timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"3","parentUuid":"2","type":"user","message":"{\"role\":\"user\",\"content\":\"third\"}","timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"4","parentUuid":"3","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"fourth\"}","timestamp":"2025-01-01T00:00:03Z"}`,
	)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?after=2", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp SessionStreamMessageEvent
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Turns) != 2 {
		t.Fatalf("got %d turns, want 2 (entries after uuid 2); turns: %+v", len(resp.Turns), resp.Turns)
	}
	if resp.Turns[0].Text != "third" {
		t.Errorf("Turns[0].Text = %q, want %q", resp.Turns[0].Text, "third")
	}
	if resp.Turns[1].Text != "fourth" {
		t.Errorf("Turns[1].Text = %q, want %q", resp.Turns[1].Text, "fourth")
	}
}

func TestHandleSessionTranscriptAfterCursorRaw(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"first\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"second\"}","timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"3","parentUuid":"2","type":"user","message":"{\"role\":\"user\",\"content\":\"third\"}","timestamp":"2025-01-01T00:00:02Z"}`,
	)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=raw&after=1", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var raw struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Messages) != 2 {
		t.Fatalf("got %d raw messages, want 2 (entries after uuid 1)", len(raw.Messages))
	}
}

func TestHandleSessionTranscriptBeforeAndAfterExclusive(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?before=3&after=1", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got status %d, want %d (before+after exclusive); body: %s", w.Code, http.StatusUnprocessableEntity, w.Body.String())
	}
}

func TestHandleSessionTranscriptAfterCursorNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?after=nonexistent", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp SessionStreamMessageEvent
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Turns) != 2 {
		t.Fatalf("got %d turns, want 2 (cursor not found = full set)", len(resp.Turns))
	}
}

func TestHandleCityPendingAggregate(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	// Two active sessions; only one is awaiting a human decision.
	pendingInfo := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(pendingInfo.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})
	_ = createTestSession(t, fs.cityBeadStore, fs.sp, "Idle") // no pending interaction

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/pending"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("city pending status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp ListBody[cityPendingEntry]
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode city pending: %v", err)
	}
	if resp.Total != 1 || len(resp.Items) != 1 {
		t.Fatalf("got %d items (total %d), want exactly 1; resp=%#v", len(resp.Items), resp.Total, resp)
	}
	got := resp.Items[0]
	if got.SessionID != pendingInfo.ID || got.RequestID != "req-1" || got.Kind != "approval" {
		t.Fatalf("entry = %#v, want session=%s request_id=req-1 kind=approval", got, pendingInfo.ID)
	}
}

func TestHandleCityPendingEmptyWhenNoneAwaiting(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	_ = createTestSession(t, fs.cityBeadStore, fs.sp, "Idle") // no pending interaction

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/pending"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("city pending status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp ListBody[cityPendingEntry]
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode city pending: %v", err)
	}
	if resp.Total != 0 || len(resp.Items) != 0 {
		t.Fatalf("got %d items (total %d), want 0; resp=%#v", len(resp.Items), resp.Total, resp)
	}
}

// TestHandleCityPendingIncludesLegacyEmptyStateSession proves a legacy
// empty-state ("none") session bead — the upgrade/bootstrap shape the
// codebase treats as active — is still probed for a pending interaction.
// A pre-fix filter of state=="active" alone dropped these beads, hiding a
// live runtime's pending decision from the city-wide aggregate.
func TestHandleCityPendingIncludesLegacyEmptyStateSession(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	// A live session awaiting a decision, then downgraded to a legacy
	// empty-state bead (state metadata absent) as a pre-metadata city
	// would have it on disk.
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-legacy",
		Kind:      "approval",
		Prompt:    "approve?",
	})
	if err := fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{"state": ""}); err != nil {
		t.Fatalf("clear state metadata: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/pending"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("city pending status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp ListBody[cityPendingEntry]
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode city pending: %v", err)
	}
	if resp.Total != 1 || len(resp.Items) != 1 {
		t.Fatalf("got %d items (total %d), want exactly 1; resp=%#v", len(resp.Items), resp.Total, resp)
	}
	if got := resp.Items[0]; got.SessionID != info.ID || got.RequestID != "req-legacy" {
		t.Fatalf("entry = %#v, want session=%s request_id=req-legacy", got, info.ID)
	}
}

// pendingPerSessionErrorProvider injects a Pending() failure for one named
// session while delegating every other session to the embedded Fake. It lets
// the city aggregate's partial-degradation branch be exercised: one dead
// runtime session must not blind the operator to the healthy ones.
type pendingPerSessionErrorProvider struct {
	*runtime.Fake
	failName string
	failErr  error
}

func (p *pendingPerSessionErrorProvider) Pending(name string) (*runtime.PendingInteraction, error) {
	if name == p.failName {
		return nil, p.failErr
	}
	return p.Fake.Pending(name)
}

// TestHandleCityPendingPartialWhenOneFails verifies the endpoint's defining
// contract: a per-session probe failure is surfaced as Partial/PartialErrors
// rather than failing the whole aggregate, and the healthy session still
// appears in the result.
func TestHandleCityPendingPartialWhenOneFails(t *testing.T) {
	fs := newSessionFakeState(t)

	// Healthy session awaiting a decision.
	healthy := createTestSession(t, fs.cityBeadStore, fs.sp, "Healthy")
	fs.sp.SetPendingInteraction(healthy.SessionName, &runtime.PendingInteraction{
		RequestID: "req-healthy",
		Kind:      "approval",
		Prompt:    "approve?",
	})
	// Second session whose runtime probe blows up.
	failing := createTestSession(t, fs.cityBeadStore, fs.sp, "Failing")

	state := &stateWithSessionProvider{
		fakeState: fs,
		provider: &pendingPerSessionErrorProvider{
			Fake:     fs.sp,
			failName: failing.SessionName,
			failErr:  fmt.Errorf("capturing pane: probe blew up"),
		},
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/pending"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("city pending status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp ListBody[cityPendingEntry]
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode city pending: %v", err)
	}
	if !resp.Partial {
		t.Fatalf("Partial = false, want true when one session probe fails; resp=%#v", resp)
	}
	if len(resp.PartialErrors) != 1 {
		t.Fatalf("PartialErrors = %#v, want exactly one entry naming the failed session", resp.PartialErrors)
	}
	if !strings.Contains(resp.PartialErrors[0], failing.ID) {
		t.Fatalf("PartialErrors[0] = %q, want it to name failing session %s", resp.PartialErrors[0], failing.ID)
	}
	if resp.Total != 1 || len(resp.Items) != 1 {
		t.Fatalf("got %d items (total %d), want exactly the healthy session; resp=%#v", len(resp.Items), resp.Total, resp)
	}
	if got := resp.Items[0]; got.SessionID != healthy.ID || got.RequestID != "req-healthy" {
		t.Fatalf("entry = %#v, want healthy session %s req-healthy", got, healthy.ID)
	}
}

func TestHandleSessionPendingAndRespond(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/pending", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("pending status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var pendingResp sessionPendingResponse
	if err := json.NewDecoder(w.Body).Decode(&pendingResp); err != nil {
		t.Fatalf("decode pending: %v", err)
	}
	if !pendingResp.Supported || pendingResp.Pending == nil || pendingResp.Pending.RequestID != "req-1" {
		t.Fatalf("pending response = %#v, want req-1", pendingResp)
	}

	respondReq := newPostRequest(cityURL(fs, "/session/")+info.ID+"/respond", strings.NewReader(`{"action":"approve"}`))
	respondReq.Header.Set("Idempotency-Key", "sess-respond-1")
	respondRec := httptest.NewRecorder()
	h.ServeHTTP(respondRec, respondReq)

	if respondRec.Code != http.StatusAccepted {
		t.Fatalf("respond status = %d, want %d; body: %s", respondRec.Code, http.StatusAccepted, respondRec.Body.String())
	}
	if got := fs.sp.Responses[info.SessionName]; len(got) != 1 || got[0].Action != "approve" {
		t.Fatalf("responses = %#v, want single approve", got)
	}
}

func TestHandleSessionPendingReturnsEmptyWhenRuntimeSessionMissing(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  &pendingSessionMissingProvider{Fake: fs.sp},
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/pending", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("pending status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var pendingResp sessionPendingResponse
	if err := json.NewDecoder(rec.Body).Decode(&pendingResp); err != nil {
		t.Fatalf("decode pending: %v", err)
	}
	if !pendingResp.Supported {
		t.Fatalf("Supported = false, want true for interaction-capable provider")
	}
	if pendingResp.Pending != nil {
		t.Fatalf("Pending = %#v, want nil when runtime session is gone", pendingResp.Pending)
	}

	respondReq := newPostRequest(cityURL(fs, "/session/")+info.ID+"/respond", strings.NewReader(`{"action":"approve"}`))
	respondRec := httptest.NewRecorder()
	h.ServeHTTP(respondRec, respondReq)

	if respondRec.Code != http.StatusConflict {
		t.Fatalf("respond status = %d, want %d; body: %s", respondRec.Code, http.StatusConflict, respondRec.Body.String())
	}
	if !strings.Contains(respondRec.Body.String(), "no_pending") {
		t.Fatalf("respond body = %q, want no_pending problem", respondRec.Body.String())
	}
}

func TestHandleSessionMessageRejectsPendingInteraction(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/messages", strings.NewReader(`{"message":"hello"}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("message status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	accepted := decodeAsyncAccepted(t, rec.Body)
	_, failure := waitForSessionMessageResult(t, fs.eventProv, accepted.RequestID)
	if failure == nil {
		t.Fatalf("expected session message to fail (pending interaction should reject), got success")
	}
}

func TestHandleSessionMessageRejectsClosedNamedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{ExplicitName: "sky", Template: "myrig/worker", Title: "Sky", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Transport: "", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSessionNamedWithTransport: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/sky/messages"), strings.NewReader(`{"message":"hello"}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("message status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	accepted := decodeAsyncAccepted(t, rec.Body)
	_, failure := waitForSessionMessageResult(t, fs.eventProv, accepted.RequestID)
	if failure == nil {
		t.Fatalf("expected session message to fail for closed session, got success")
	}
}

func TestHandleSessionRespondMismatchedRequest(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})

	respondReq := newPostRequest(cityURL(fs, "/session/")+info.ID+"/respond", strings.NewReader(`{"request_id":"req-2","action":"approve"}`))
	respondRec := httptest.NewRecorder()
	h.ServeHTTP(respondRec, respondReq)

	if respondRec.Code != http.StatusConflict {
		t.Fatalf("respond status = %d, want %d; body: %s", respondRec.Code, http.StatusConflict, respondRec.Body.String())
	}
}

func TestHandleSessionStreamSSEHeaders(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()
	<-done

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}
	if !strings.Contains(rec.Body.String(), "event: turn") || !strings.Contains(rec.Body.String(), "hello") {
		t.Errorf("stream body missing initial turn: %s", rec.Body.String())
	}
}

func TestHandleSessionStreamStoppedWithoutOutputReturnsNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
	srv.sessionLogSearchPaths = []string{t.TempDir()}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "default", Title: "No Output", Command: "echo test", WorkDir: t.TempDir(), Provider: "test", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandleSessionStreamRawStoppedWithoutOutputReturnsNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{t.TempDir()}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "default", Title: "No Output", Command: "echo test", WorkDir: t.TempDir(), Provider: "test", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream?format=raw", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestLegacySessionStreamRawStoppedWithoutOutputReturnsNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{t.TempDir()}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "default", Title: "No Output", Command: "echo test", WorkDir: t.TempDir(), Provider: "test", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream?format=raw", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandleSessionStreamClosedSessionReturnsSnapshot(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reqCtx, cancelReq := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelReq()
	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream", nil).WithContext(reqCtx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closed session stream should return without waiting for request cancellation")
	}

	if !strings.Contains(rec.Body.String(), "event: turn") || !strings.Contains(rec.Body.String(), "hello") || !strings.Contains(rec.Body.String(), "world") {
		t.Errorf("stream body missing closed-session snapshot: %s", rec.Body.String())
	}
	for _, event := range fs.eventProv.(*events.Fake).Events {
		if event.Type == events.WorkerOperation {
			t.Fatalf("closed session stream emitted worker operation event: %#v", event)
		}
	}
}

func TestHandleSessionStreamStoppedSessionCommitsStatusHeaders(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}
	h := newTestCityHandlerWith(t, fs, srv)

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	ts := httptest.NewServer(h)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+cityURL(fs, "/session/")+info.ID+"/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	cancel()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := resp.Header.Get("GC-Session-State"); got != "suspended" {
		t.Fatalf("committed GC-Session-State = %q, want %q", got, "suspended")
	}
	if got := resp.Header.Get("GC-Session-Status"); got != "stopped" {
		t.Fatalf("committed GC-Session-Status = %q, want %q", got, "stopped")
	}
}

func TestHandleSessionStreamClosedNamedSessionReturnsSnapshot(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{ExplicitName: "sky", Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Transport: "", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSessionNamedWithTransport: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reqCtx, cancelReq := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelReq()
	req := httptest.NewRequest("GET", cityURL(fs, "/session/sky/stream"), nil).WithContext(reqCtx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closed named session stream should return without waiting for request cancellation")
	}

	if !strings.Contains(rec.Body.String(), "event: turn") || !strings.Contains(rec.Body.String(), "hello") || !strings.Contains(rec.Body.String(), "world") {
		t.Errorf("stream body missing closed-session snapshot: %s", rec.Body.String())
	}
}

func TestStreamSessionTranscriptHistoryDoesNotSkipTurnsAcrossCompactionBoundaries(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	searchBase := t.TempDir()
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"a","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"before compaction\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"cb0","parentUuid":"a","type":"system","subtype":"compact_boundary","timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"b","parentUuid":"cb0","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"after first boundary\"}","timestamp":"2025-01-01T00:00:02Z"}`,
	)

	handle, err := srv.workerHandleForSession(fs.cityBeadStore, info.ID)
	if err != nil {
		t.Fatalf("workerHandleForSession: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	defer cancel()

	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		initial, histErr := handle.History(ctx, worker.HistoryRequest{})
		if histErr != nil {
			t.Errorf("History(initial): %v", histErr)
			close(done)
			return
		}
		srv.streamSessionTranscriptHistory(ctx, rec, info, handle, initial)
		close(done)
	}()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(rec.BodyString(), "after first boundary") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	logDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	logPath := filepath.Join(logDir, info.SessionKey+".jsonl")
	appendFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, err = appendFile.WriteString(strings.Join([]string{
		`{"uuid":"c","parentUuid":"b","type":"user","message":"{\"role\":\"user\",\"content\":\"bridge turn\"}","timestamp":"2025-01-01T00:00:03Z"}`,
		`{"uuid":"cb1","parentUuid":"c","type":"system","subtype":"compact_boundary","timestamp":"2025-01-01T00:00:04Z"}`,
		`{"uuid":"d","parentUuid":"cb1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"after second boundary\"}","timestamp":"2025-01-01T00:00:05Z"}`,
	}, "\n") + "\n")
	if closeErr := appendFile.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("append transcript: %v", err)
	}

	<-done

	body := rec.BodyString()
	if !strings.Contains(body, "bridge turn") {
		t.Fatalf("stream body missing turn written before new compact boundary: %s", body)
	}
	if !strings.Contains(body, "after second boundary") {
		t.Fatalf("stream body missing turn written after new compact boundary: %s", body)
	}
}

func TestStreamSessionTranscriptHistoryReloadsChangesWrittenAfterInitialHistory(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	searchBase := t.TempDir()
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"a","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"before initial history\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)

	handle, err := srv.workerHandleForSession(fs.cityBeadStore, info.ID)
	if err != nil {
		t.Fatalf("workerHandleForSession: %v", err)
	}
	initial, err := handle.History(context.Background(), worker.HistoryRequest{})
	if err != nil {
		t.Fatalf("History(initial): %v", err)
	}

	logPath := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir), info.SessionKey+".jsonl")
	appendFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := appendFile.WriteString(
		`{"uuid":"b","parentUuid":"a","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"after initial history\"}","timestamp":"2025-01-01T00:00:01Z"}` + "\n",
	); err != nil {
		_ = appendFile.Close()
		t.Fatalf("append transcript: %v", err)
	}
	if err := appendFile.Close(); err != nil {
		t.Fatalf("close transcript: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		srv.streamSessionTranscriptHistory(ctx, rec, info, handle, initial)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "after initial history", time.Second); !strings.Contains(body, "after initial history") {
		t.Fatalf("stream body missing post-initial-history turn: %s", body)
	}
	cancel()
	<-done
}

func TestCityScopedSessionStreamReloadsRotatedGeminiTranscriptAcrossRestart(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	base := t.TempDir()
	workDir := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workDir: %v", err)
	}

	searchRoot := filepath.Join(base, ".gemini", "tmp")
	srv.sessionLogSearchPaths = []string{searchRoot}
	projectDir := filepath.Join(searchRoot, "project-a")
	chatsDir := filepath.Join(projectDir, "chats")
	for _, dir := range []string{searchRoot, projectDir, chatsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".project_root"), []byte(workDir), 0o644); err != nil {
		t.Fatalf("write .project_root: %v", err)
	}

	firstTranscript := filepath.Join(chatsDir, "session-2026-04-17T03-12-before.json")
	writeGeminiHistoryFixtureForAPI(t, firstTranscript, "before-session",
		`{"id":"u1","timestamp":"2026-04-17T03:12:00Z","type":"user","content":"first-remembered-input"}`,
		`{"id":"a1","timestamp":"2026-04-17T03:12:01Z","type":"gemini","content":"first-remembered-output"}`,
	)
	firstTime := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(firstTranscript, firstTime, firstTime); err != nil {
		t.Fatalf("chtimes(first transcript): %v", err)
	}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "gemini", WorkDir: workDir, Provider: "gemini", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "first-remembered-output", time.Second); !strings.Contains(body, "first-remembered-output") {
		t.Fatalf("stream body missing initial transcript turn: %s", body)
	}

	secondTranscript := filepath.Join(chatsDir, "session-2026-04-17T03-15-after.json")
	writeGeminiHistoryFixtureForAPI(t, secondTranscript, "after-session",
		`{"id":"u2","timestamp":"2026-04-17T03:15:00Z","type":"user","content":"second-continued-input"}`,
		`{"id":"a2","timestamp":"2026-04-17T03:15:01Z","type":"gemini","content":"second-continued-output"}`,
	)
	secondTime := time.Now().Add(-1 * time.Minute)
	if err := os.Chtimes(secondTranscript, secondTime, secondTime); err != nil {
		t.Fatalf("chtimes(second transcript): %v", err)
	}

	fs.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.WorkerOperation,
		Actor:   "worker",
		Subject: info.ID,
	})

	body := waitForRecorderSubstring(t, rec, "second-continued-output", 1500*time.Millisecond)

	cancel()
	<-done

	if !strings.Contains(body, "second-continued-input") || !strings.Contains(body, "second-continued-output") {
		t.Fatalf("city-scoped stream body missing rotated transcript turns after wake: %s", body)
	}
}

func TestCityScopedSessionStreamFollowsRotatedGeminiTranscriptAfterWake(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	base := t.TempDir()
	workDir := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workDir: %v", err)
	}

	searchRoot := filepath.Join(base, ".gemini", "tmp")
	srv.sessionLogSearchPaths = []string{searchRoot}
	projectDir := filepath.Join(searchRoot, "project-a")
	chatsDir := filepath.Join(projectDir, "chats")
	for _, dir := range []string{searchRoot, projectDir, chatsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".project_root"), []byte(workDir), 0o644); err != nil {
		t.Fatalf("write .project_root: %v", err)
	}

	firstTranscript := filepath.Join(chatsDir, "session-2026-04-17T03-12-before.json")
	writeGeminiHistoryFixtureForAPI(t, firstTranscript, "before-session",
		`{"id":"u1","timestamp":"2026-04-17T03:12:00Z","type":"user","content":"first-input"}`,
		`{"id":"a1","timestamp":"2026-04-17T03:12:01Z","type":"gemini","content":"first-output"}`,
	)
	firstTime := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(firstTranscript, firstTime, firstTime); err != nil {
		t.Fatalf("chtimes(first transcript): %v", err)
	}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "gemini", WorkDir: workDir, Provider: "gemini", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "first-output", 3*time.Second); !strings.Contains(body, "first-output") {
		t.Fatalf("stream body missing initial transcript turn: %s", body)
	}

	secondTranscript := filepath.Join(chatsDir, "session-2026-04-17T03-15-after.json")
	writeGeminiHistoryFixtureForAPI(t, secondTranscript, "after-session",
		`{"id":"u2","timestamp":"2026-04-17T03:15:00Z","type":"user","content":"second-input"}`,
		`{"id":"a2","timestamp":"2026-04-17T03:15:01Z","type":"gemini","content":"second-output"}`,
	)
	secondTime := time.Now().Add(-1 * time.Minute)
	if err := os.Chtimes(secondTranscript, secondTime, secondTime); err != nil {
		t.Fatalf("chtimes(second transcript): %v", err)
	}

	fs.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.WorkerOperation,
		Actor:   "worker",
		Subject: info.ID,
	})

	if body := waitForRecorderSubstring(t, rec, "second-output", 5*time.Second); !strings.Contains(body, "second-output") {
		t.Fatalf("stream body missing rotated transcript after wake: %s", body)
	}

	writeGeminiHistoryFixtureForAPI(t, secondTranscript, "after-session",
		`{"id":"u2","timestamp":"2026-04-17T03:15:00Z","type":"user","content":"second-input"}`,
		`{"id":"a2","timestamp":"2026-04-17T03:15:01Z","type":"gemini","content":"second-output"}`,
		`{"id":"u3","timestamp":"2026-04-17T03:15:02Z","type":"user","content":"third-input"}`,
		`{"id":"a3","timestamp":"2026-04-17T03:15:03Z","type":"gemini","content":"third-output"}`,
	)
	currentTime := time.Now()
	if err := os.Chtimes(secondTranscript, currentTime, currentTime); err != nil {
		t.Fatalf("chtimes(updated second transcript): %v", err)
	}

	body := waitForRecorderSubstring(t, rec, "third-output", 5*time.Second)

	cancel()
	<-done

	if !strings.Contains(body, "third-input") || !strings.Contains(body, "third-output") {
		t.Fatalf("city-scoped stream body missing writes to rotated transcript after wake: %s", body)
	}
}

func TestHandleSessionStreamWorkerOperationEventWakesTranscriptReload(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "hello", time.Second); !strings.Contains(body, "hello") {
		t.Fatalf("stream body missing initial turn: %s", body)
	}

	logDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	logPath := filepath.Join(logDir, info.SessionKey+".jsonl")
	appendFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, err = appendFile.WriteString(strings.Join([]string{
		`{"uuid":"3","parentUuid":"2","type":"user","message":"{\"role\":\"user\",\"content\":\"wake now\"}","timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"4","parentUuid":"3","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"event wake turn\"}","timestamp":"2025-01-01T00:00:03Z"}`,
	}, "\n") + "\n")
	if closeErr := appendFile.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("append transcript: %v", err)
	}

	fs.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.WorkerOperation,
		Actor:   "worker",
		Subject: info.ID,
	})

	body := waitForRecorderSubstring(t, rec, "event wake turn", 1500*time.Millisecond)

	cancel()
	<-done

	if !strings.Contains(body, "event wake turn") {
		t.Fatalf("stream body missing turn after worker operation wakeup: %s", body)
	}
}

func TestHandleSessionStreamRawWorkerOperationEventWakesTranscriptReload(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream?format=raw", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "hello", time.Second); !strings.Contains(body, "hello") {
		t.Fatalf("raw stream body missing initial transcript: %s", body)
	}

	logDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	logPath := filepath.Join(logDir, info.SessionKey+".jsonl")
	appendFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, err = appendFile.WriteString(`{"uuid":"3","parentUuid":"2","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"raw event wake\"}","timestamp":"2025-01-01T00:00:02Z"}` + "\n")
	if closeErr := appendFile.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("append transcript: %v", err)
	}

	fs.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.WorkerOperation,
		Actor:   "worker",
		Subject: info.ID,
	})

	body := waitForRecorderSubstring(t, rec, "raw event wake", 1500*time.Millisecond)

	cancel()
	<-done

	if !strings.Contains(body, "raw event wake") {
		t.Fatalf("raw stream body missing message after worker operation wakeup: %s", body)
	}
}

func TestHandleSessionStreamRawStallEmitsPendingWithoutTranscriptGrowth(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	prevStallTimeout := sessionStreamPendingStallTimeout
	sessionStreamPendingStallTimeout = 50 * time.Millisecond
	defer func() {
		sessionStreamPendingStallTimeout = prevStallTimeout
	}()

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream?format=raw", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "hello", time.Second); !strings.Contains(body, "hello") {
		t.Fatalf("raw stream body missing initial transcript: %s", body)
	}

	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "Proceed?",
	})

	body := waitForRecorderSubstring(t, rec, "req-1", time.Second)

	cancel()
	<-done

	if !strings.Contains(body, "req-1") {
		t.Fatalf("raw stream body missing pending interaction after idle stall: %s", body)
	}
}

func TestHandleSessionStreamRawStallEmitsPendingEventOnCityRoute(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	prevStallTimeout := sessionStreamPendingStallTimeout
	sessionStreamPendingStallTimeout = 50 * time.Millisecond
	defer func() {
		sessionStreamPendingStallTimeout = prevStallTimeout
	}()

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream?format=raw", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "hello", time.Second); !strings.Contains(body, "hello") {
		t.Fatalf("raw stream body missing initial transcript: %s", body)
	}

	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "Proceed?",
	})

	body := waitForRecorderSubstring(t, rec, "req-1", time.Second)

	cancel()
	<-done

	if !strings.Contains(body, "event: pending") {
		t.Fatalf("raw stream body missing pending SSE event name: %s", body)
	}
}

func TestHandleSessionStreamRawRunningSessionWithoutTranscriptOpensImmediately(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream?format=raw", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	body := waitForRecorderSubstring(t, rec, `"messages":[]`, time.Second)
	cancel()
	<-done

	if !strings.Contains(body, `"messages":[]`) {
		t.Fatalf("raw stream body missing initial empty message payload: %s", body)
	}
	if !strings.Contains(body, `"format":"raw"`) {
		t.Fatalf("raw stream body missing raw format payload: %s", body)
	}
}

func TestHandleSessionStreamTranscriptWriteWakesWithoutPolling(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "hello", time.Second); !strings.Contains(body, "hello") {
		t.Fatalf("stream body missing initial turn: %s", body)
	}

	logDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	logPath := filepath.Join(logDir, info.SessionKey+".jsonl")
	appendFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, err = appendFile.WriteString(strings.Join([]string{
		`{"uuid":"3","parentUuid":"2","type":"user","message":"{\"role\":\"user\",\"content\":\"file wake\"}","timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"4","parentUuid":"3","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"fsnotify wake turn\"}","timestamp":"2025-01-01T00:00:03Z"}`,
	}, "\n") + "\n")
	if closeErr := appendFile.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("append transcript: %v", err)
	}

	body := waitForRecorderSubstring(t, rec, "fsnotify wake turn", 1500*time.Millisecond)

	cancel()
	<-done

	if !strings.Contains(body, "fsnotify wake turn") {
		t.Fatalf("stream body missing turn after transcript write wakeup: %s", body)
	}
}

func TestHandleSessionStreamConversationFiltersNonDisplayEntries(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"3","parentUuid":"2","type":"tool_use","message":"{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"name\":\"debugtool\"}]}","timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"4","parentUuid":"3","type":"tool_result","message":"{\"role\":\"tool\",\"content\":\"internal raw detail\"}","timestamp":"2025-01-01T00:00:03Z"}`,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "hello") || !strings.Contains(body, "world") {
		t.Fatalf("conversation stream body missing display turns: %s", body)
	}
	if strings.Contains(body, "debugtool") || strings.Contains(body, "internal raw detail") {
		t.Fatalf("conversation stream leaked non-display transcript entries: %s", body)
	}
}

func TestHandleSessionStreamConversationRedactsThinkingText(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":[{\"type\":\"thinking\",\"thinking\":\"private chain of thought\"},{\"type\":\"text\",\"text\":\"visible answer\"}]}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "visible answer") {
		t.Fatalf("conversation stream body missing visible assistant answer: %s", body)
	}
	if strings.Contains(body, "private chain of thought") {
		t.Fatalf("conversation stream leaked thinking text: %s", body)
	}
}

func TestHandleSessionStreamRawUsesLatestCompactionTail(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"a","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"before compaction\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"cb0","parentUuid":"a","type":"system","subtype":"compact_boundary","timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"b","parentUuid":"cb0","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"after first boundary\"}","timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"cb1","parentUuid":"b","type":"system","subtype":"compact_boundary","timestamp":"2025-01-01T00:00:03Z"}`,
		`{"uuid":"c","parentUuid":"cb1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"after second boundary\"}","timestamp":"2025-01-01T00:00:04Z"}`,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream?format=raw", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "after second boundary") {
		t.Fatalf("raw stream body missing latest compaction tail: %s", body)
	}
	if strings.Contains(body, "before compaction") || strings.Contains(body, "after first boundary") {
		t.Fatalf("raw stream replayed full transcript instead of latest compaction tail: %s", body)
	}
}

func TestHandleSessionTranscriptRawIncludesAllTypes(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Write entries of different types, including tool_use and progress.
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"3","parentUuid":"2","type":"tool_use","message":"{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"name\":\"read\"}]}","timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"4","parentUuid":"3","type":"tool_result","message":"{\"role\":\"tool\",\"content\":\"file contents\"}","timestamp":"2025-01-01T00:00:03Z"}`,
	)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=raw&tail=0", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp SessionStreamRawMessageEvent
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Format != "raw" {
		t.Errorf("Format = %q, want %q", resp.Format, "raw")
	}
	// Raw format should include ALL entry types (user, assistant, tool_use, tool_result).
	if len(resp.Messages) != 4 {
		t.Fatalf("got %d raw messages, want 4 (all types included)", len(resp.Messages))
	}
}

func TestHandleSessionTranscriptRawIncludesCodexCustomToolCalls(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := newHermeticCodexSessionSearchPath(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "codex", WorkDir: workDir, Provider: "codex", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	codexDir := filepath.Join(searchBase, "2026", "05", "02")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatalf("MkdirAll codex session dir: %v", err)
	}
	codexPayload := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"cwd":%q}}`, workDir),
		`{"timestamp":"2025-01-01T00:00:04Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"call-edit","name":"apply_patch","input":"*** Begin Patch\n*** Update File: city.toml\n@@\n+# Created by Chris Sells\n [workspace]\n*** End Patch\n"}}`,
		`{"timestamp":"2025-01-01T00:00:05Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call-edit","output":"{\"output\":\"Success. Updated the following files:\\nM city.toml\\n\"}"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(codexDir, "rollout-2026-05-02T00-00-00-test.jsonl"), []byte(codexPayload), 0o644); err != nil {
		t.Fatalf("WriteFile codex session: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=raw&tail=0", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp SessionStreamRawMessageEvent
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("got %d raw messages, want 2 Codex custom tool frames", len(resp.Messages))
	}
	rawJSON, err := json.Marshal(resp.Messages)
	if err != nil {
		t.Fatalf("marshal raw transcript messages: %v", err)
	}
	rawJoined := string(rawJSON)
	if !strings.Contains(rawJoined, `"custom_tool_call"`) ||
		!strings.Contains(rawJoined, `"custom_tool_call_output"`) {
		t.Fatalf("raw transcript missing Codex custom tool frames: %s", rawJoined)
	}
	if !strings.Contains(rawJoined, "apply_patch") ||
		!strings.Contains(rawJoined, "Created by Chris Sells") ||
		!strings.Contains(rawJoined, "Success. Updated the following files") {
		t.Fatalf("raw transcript missing Codex custom tool payloads: %s", rawJoined)
	}
}

func TestHandleSessionTranscriptConversationIncludesCodexErrorFrame(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := newHermeticCodexSessionSearchPath(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "codex", WorkDir: workDir, Provider: "codex", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	codexDir := filepath.Join(searchBase, "2026", "05", "02")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatalf("MkdirAll codex session dir: %v", err)
	}
	codexPayload := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"cwd":%q}}`, workDir),
		`{"timestamp":"2025-01-01T00:00:04Z","type":"event_msg","payload":{"type":"error","message":"You've hit your usage limit.","codex_error_info":"usage_limit_exceeded"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(codexDir, "rollout-2026-05-02T00-00-00-test.jsonl"), []byte(codexPayload), 0o644); err != nil {
		t.Fatalf("WriteFile codex session: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?tail=0", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionTranscriptResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Format != "conversation" {
		t.Fatalf("Format = %q, want conversation", resp.Format)
	}
	if got := len(resp.Turns); got != 1 {
		t.Fatalf("turns = %d, want Codex error turn; body: %s", got, w.Body.String())
	}
	if resp.Turns[0].Role != "system" {
		t.Fatalf("turn role = %q, want system", resp.Turns[0].Role)
	}
	if !strings.Contains(resp.Turns[0].Text, "usage_limit_exceeded") || !strings.Contains(resp.Turns[0].Text, "You've hit your usage limit.") {
		t.Fatalf("turn text = %q, want Codex error code and message", resp.Turns[0].Text)
	}
}

func TestHandleSessionStreamConversationIncludesCodexErrorFrame(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := newHermeticCodexSessionSearchPath(t)
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "codex", WorkDir: workDir, Provider: "codex", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	codexDir := filepath.Join(searchBase, "2026", "05", "02")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatalf("MkdirAll codex session dir: %v", err)
	}
	codexPayload := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"cwd":%q}}`, workDir),
		`{"timestamp":"2025-01-01T00:00:04Z","type":"event_msg","payload":{"type":"error","message":"You've hit your usage limit.","codex_error_info":"usage_limit_exceeded"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(codexDir, "rollout-2026-05-02T00-00-00-test.jsonl"), []byte(codexPayload), 0o644); err != nil {
		t.Fatalf("WriteFile codex session: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "usage_limit_exceeded") || !strings.Contains(body, "You've hit your usage limit.") {
		t.Fatalf("conversation stream body missing Codex error frame: %s", body)
	}
}

func TestHandleSessionGetActivity(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Activity Test", Command: "claude", WorkDir: workDir, Provider: "claude", Env: nil, Resume: resume, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Write JSONL ending with end_turn → expect "idle".
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"stop_reason\":\"end_turn\",\"content\":\"done\",\"model\":\"claude-opus-4-5-20251101\",\"usage\":{\"input_tokens\":1000}}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID, nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Activity != "idle" {
		t.Errorf("Activity = %q, want %q", resp.Activity, "idle")
	}
}

func TestFilterMetadataAllowlistsRealWorldAppPrefix(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{
			name: "nil metadata",
			in:   nil,
			want: nil,
		},
		{
			name: "only internal keys",
			in:   map[string]string{"session_key": "abc", "command": "claude", "work_dir": "/tmp"},
			want: nil,
		},
		{
			name: "real_world_app_ keys preserved except removed session kind discriminator",
			in:   map[string]string{"real_world_app_session_kind": "agent", "real_world_app_permission_mode": "plan", "session_key": "secret"},
			want: map[string]string{"real_world_app_permission_mode": "plan"},
		},
		{
			name: "mixed keys",
			in:   map[string]string{"real_world_app_project_id": "proj-1", "quarantined_until": "2025-01-01", "held_until": "2025-01-02"},
			want: map[string]string{"real_world_app_project_id": "proj-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterMetadata(tt.in)
			if tt.want == nil {
				if got != nil {
					t.Errorf("got %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("got %d keys, want %d: %v", len(got), len(tt.want), got)
				return
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestHandleSessionGetMetadataFiltered(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Test")

	// Set metadata with both real_world_app_ and internal keys.
	if err := fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"real_world_app_project_id":  "proj-1",
		"session_key":                "secret-key",
		"command":                    "claude --skip",
		"work_dir":                   "/private/dir",
		"sleep_reason":               "",
		"real_world_app_custom_mode": "plan",
	}); err != nil {
		t.Fatalf("set metadata: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID, nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Only real_world_app_ prefixed keys should be present.
	if len(resp.Metadata) != 2 {
		t.Fatalf("got %d metadata keys, want 2: %v", len(resp.Metadata), resp.Metadata)
	}
	if resp.Metadata["real_world_app_project_id"] != "proj-1" {
		t.Errorf("real_world_app_project_id = %q, want %q", resp.Metadata["real_world_app_project_id"], "proj-1")
	}
	if resp.Metadata["real_world_app_custom_mode"] != "plan" {
		t.Errorf("real_world_app_custom_mode = %q, want %q", resp.Metadata["real_world_app_custom_mode"], "plan")
	}
	// Internal keys must NOT be present.
	if _, ok := resp.Metadata["session_key"]; ok {
		t.Error("session_key should not be exposed in API response")
	}
	if _, ok := resp.Metadata["command"]; ok {
		t.Error("command should not be exposed in API response")
	}
}

// TestSessionToResponse_BaseOnlyDescendant_InheritsDisplayName mirrors
// the /v0/agents base-only test for /v0/sessions: the session response
// must pick up the builtin ancestor's DisplayName when the leaf
// provider doesn't declare one, routed through the resolved cache.
func TestSessionToResponse_BaseOnlyDescendant_InheritsDisplayName(t *testing.T) {
	baseCodex := "builtin:codex"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Providers: map[string]config.ProviderSpec{
			"codex-max": {Base: &baseCodex}, // no DisplayName, no Command
		},
		ResolvedProviders: map[string]config.ResolvedProvider{
			"codex-max": {
				Name:            "codex-max",
				BuiltinAncestor: "codex",
				Command:         "codex",
			},
		},
	}

	info := session.Info{
		ID:       "sess-1",
		Template: "myrig/mayor",
		Provider: "codex-max",
	}
	resp := sessionToResponse(info, cfg)

	if resp.Provider != "codex-max" {
		t.Errorf("Provider = %q, want codex-max", resp.Provider)
	}
	// DisplayName inherited from builtin:codex via the resolved cache.
	if resp.DisplayName != "Codex CLI" {
		t.Errorf("DisplayName = %q, want %q (inherited)", resp.DisplayName, "Codex CLI")
	}
}

// TestSessionToResponse_AgentKindClassification covers the three buckets the
// dashboard uses to route sessions to panels (crew/pool/role) plus the
// fallback when no agent matches the template. The classifier is structural
// — never inspects role names — so the test agents avoid using any specific
// role-name string.
func TestSessionToResponse_AgentKindClassification(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "kind-city"},
		Agents: []config.Agent{
			// Crew member: dir ends with "/crew", singleton.
			{Name: "alpha", Dir: "rig-a/crew", MaxActiveSessions: intPtr(1)},
			// Pool agent: multi-instance, lives in a rig dir (not crew).
			{Name: "scaler", Dir: "rig-a", MaxActiveSessions: intPtr(5)},
			// Singleton role agent: max=1, no crew dir.
			{Name: "singleton", Dir: "rig-a", MaxActiveSessions: intPtr(1)},
			// Top-level crew agent: dir is exactly "crew".
			{Name: "bravo", Dir: "crew", MaxActiveSessions: intPtr(1)},
		},
	}

	cases := []struct {
		name     string
		template string
		want     string
	}{
		{name: "crew-rig-scoped", template: "rig-a/crew/alpha", want: "crew"},
		{name: "crew-top-level", template: "crew/bravo", want: "crew"},
		{name: "pool-multi-instance", template: "rig-a/scaler", want: "pool"},
		{name: "role-singleton", template: "rig-a/singleton", want: "role"},
		{name: "unknown-template-omits-kind", template: "unrecognized/template", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := session.Info{ID: "sess-" + tc.name, Template: tc.template, Provider: "fake"}
			resp := sessionToResponse(info, cfg)
			if resp.AgentKind != tc.want {
				t.Errorf("AgentKind for template %q = %q, want %q", tc.template, resp.AgentKind, tc.want)
			}
		})
	}
}

func TestSessionToResponse_ProjectsLastNudgeDeliveredAt(t *testing.T) {
	stamp := time.Date(2026, 5, 13, 3, 45, 0, 0, time.UTC)
	resp := sessionToResponse(session.Info{
		ID:                   "sess-1",
		Template:             "myrig/worker",
		Provider:             "codex",
		CreatedAt:            stamp.Add(-time.Hour),
		LastNudgeDeliveredAt: stamp,
	}, nil)

	if resp.LastNudgeDeliveredAt != stamp.Format(time.RFC3339) {
		t.Fatalf("LastNudgeDeliveredAt = %q, want %q", resp.LastNudgeDeliveredAt, stamp.Format(time.RFC3339))
	}
}

func TestHandleSessionStopReturnsOKWithID(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "stop-test")

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/stop", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stop status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Status string `json:"status"`
		ID     string `json:"id"`
	}
	json.NewDecoder(rec.Body).Decode(&body) //nolint:errcheck
	if body.ID != info.ID {
		t.Errorf("stop response id = %q, want %q", body.ID, info.ID)
	}
	if body.Status != "ok" {
		t.Errorf("stop response status = %q, want %q", body.Status, "ok")
	}
}

func TestHandleSessionKillReturnsOKWithID(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "kill-test")

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/kill", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("kill status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Status string `json:"status"`
		ID     string `json:"id"`
	}
	json.NewDecoder(rec.Body).Decode(&body) //nolint:errcheck
	if body.ID != info.ID {
		t.Errorf("kill response id = %q, want %q", body.ID, info.ID)
	}
	if body.Status != "ok" {
		t.Errorf("kill response status = %q, want %q", body.Status, "ok")
	}
}

func TestHandleSessionKillClosedSessionIsOK(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "kill-closed-test")
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/kill", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("kill closed status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Status string `json:"status"`
		ID     string `json:"id"`
	}
	json.NewDecoder(rec.Body).Decode(&body) //nolint:errcheck
	if body.ID != info.ID {
		t.Errorf("kill closed response id = %q, want %q", body.ID, info.ID)
	}
	if body.Status != "ok" {
		t.Errorf("kill closed response status = %q, want %q", body.Status, "ok")
	}
}

func TestHandleSessionKillNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/nonexistent/kill"), nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("kill nonexistent status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandleSessionMessageQueuesWhenSuspended(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "queue-test")
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/messages", strings.NewReader(`{"message":"hello after suspend"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("suspended message status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	body := decodeAsyncAccepted(t, w.Body)

	success, failure := waitForSessionMessageResult(t, fs.eventProv, body.RequestID)
	if success == nil {
		t.Fatalf("session message failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
}
