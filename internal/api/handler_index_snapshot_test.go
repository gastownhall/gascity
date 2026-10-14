package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// lateWriteStore models a write that commits after a list handler has read
// its snapshot but before the handler finishes. The first List returns the
// pre-write rows, then creates a bead and records its event, exactly as a
// concurrent writer would.
type lateWriteStore struct {
	beads.Store
	rec  events.Recorder
	once sync.Once
	late string
}

func (s *lateWriteStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(q)
	s.once.Do(func() {
		b, createErr := s.Create(beads.Bead{Title: "late write"})
		if createErr != nil {
			panic(createErr)
		}
		s.late = b.ID
		s.rec.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: b.ID})
	})
	return rows, err
}

// lateEventStore models an event that commits after a handler has read a
// bead but before it responds. The first Get returns the bead as stored, then
// records the event, exactly as a concurrent session wake would.
type lateEventStore struct {
	beads.Store
	ep      *events.Fake
	once    sync.Once
	lateSeq uint64
}

func (s *lateEventStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	s.once.Do(func() {
		s.ep.Record(events.Event{Type: events.SessionWoke, Actor: "test", Subject: id})
		seq, err := s.ep.LatestSeq()
		if err != nil {
			panic(err)
		}
		s.lateSeq = seq
	})
	return b, err
}

func responseIndex(t *testing.T, rec *httptest.ResponseRecorder) uint64 {
	t.Helper()
	raw := rec.Header().Get("X-GC-Index")
	if raw == "" {
		t.Fatal("response missing X-GC-Index")
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		t.Fatalf("X-GC-Index %q: %v", raw, err)
	}
	return n
}

func decodeBeadList(t *testing.T, rec *httptest.ResponseRecorder) boundedListBody {
	t.Helper()
	var body boundedListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	return body
}

func bodyHasBead(items []beads.Bead, id string) bool {
	for _, b := range items {
		if b.ID == id {
			return true
		}
	}
	return false
}

// TestListIndexDoesNotSkipChangesMissingFromSnapshot pins the X-GC-Index
// contract that resume and blocking queries rely on: a client that resumes
// the event stream after the returned index, or blocks on it, must observe
// every change the returned body does not already reflect. Reading the index
// after building the snapshot lets a concurrent write land in between; its
// event is then at or below the index, so a resuming client never sees it.
func TestListIndexDoesNotSkipChangesMissingFromSnapshot(t *testing.T) {
	fs := newFakeState(t)
	ep := fs.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: "seed"})
	store := &lateWriteStore{Store: fs.stores["myrig"], rec: ep}
	fs.stores["myrig"] = store

	h := newTestCityHandler(t, fs)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(fs, "/beads"), nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	body := decodeBeadList(t, rec)
	if store.late == "" {
		t.Fatal("late write did not run; the list path no longer reads this store")
	}
	if bodyHasBead(body.Items, store.late) {
		t.Fatal("snapshot already contains the late write; the test no longer models the race")
	}
	lateSeq := ep.Events[len(ep.Events)-1].Seq
	if idx := responseIndex(t, rec); idx >= lateSeq {
		t.Fatalf("X-GC-Index = %d, want < %d: the body omits bead %s, but resuming after the index skips its event", idx, lateSeq, store.late)
	}
}

// TestCachedListIndexDoesNotSkipChangesMissingFromBody covers the response
// cache: a cached body must be served with the index at which it was built,
// not the current head, or changes since the build are skipped on resume.
func TestCachedListIndexDoesNotSkipChangesMissingFromBody(t *testing.T) {
	oldTTL := timeBucketResponseCacheTTL
	timeBucketResponseCacheTTL = time.Hour
	t.Cleanup(func() { timeBucketResponseCacheTTL = oldTTL })
	fs := newFakeState(t)
	ep := fs.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: "seed"})
	h := newTestCityHandler(t, fs)

	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest("GET", cityURL(fs, "/beads")+"?all=true", nil))
	if first.Code != 200 {
		t.Fatalf("first status = %d (body=%q)", first.Code, first.Body.String())
	}

	b, err := fs.stores["myrig"].Create(beads.Bead{Title: "after cache fill"})
	if err != nil {
		t.Fatal(err)
	}
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: b.ID})
	changeSeq := ep.Events[len(ep.Events)-1].Seq

	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest("GET", cityURL(fs, "/beads")+"?all=true", nil))
	if second.Code != 200 {
		t.Fatalf("second status = %d (body=%q)", second.Code, second.Body.String())
	}
	body := decodeBeadList(t, second)
	if bodyHasBead(body.Items, b.ID) {
		t.Fatal("second response was rebuilt, not served from the cache; the test no longer models a cached body")
	}
	if idx := responseIndex(t, second); idx >= changeSeq {
		t.Fatalf("cached body omits bead %s but X-GC-Index = %d, want < %d", b.ID, idx, changeSeq)
	}
}

// TestIdempotentCreateReplayIndexDoesNotSkipLaterChanges covers
// Idempotency-Key replays of the creates that return X-GC-Index. A replay
// serves the body the original create cached, so it must also serve the index
// captured before that create: pairing the cached body with the live head lets
// a client that resumes after the header skip every later change to the
// object it created.
func TestIdempotentCreateReplayIndexDoesNotSkipLaterChanges(t *testing.T) {
	retitle := func(t *testing.T, fs *fakeMutatorState, id string) {
		t.Helper()
		title := "changed after create"
		if err := fs.stores["myrig"].Update(id, beads.UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("update %s: %v", id, err)
		}
		fs.eventProv.(*events.Fake).Record(events.Event{Type: events.BeadUpdated, Actor: "test", Subject: id})
	}
	markRead := func(t *testing.T, fs *fakeMutatorState, id string) {
		t.Helper()
		if err := fs.cityMailProv.MarkRead(id); err != nil {
			t.Fatalf("mark read %s: %v", id, err)
		}
		fs.eventProv.(*events.Fake).Record(events.Event{Type: events.MailRead, Actor: "test", Subject: id})
	}
	tests := []struct {
		name string
		// request seeds whatever the create needs and returns its URL and body.
		request func(t *testing.T, fs *fakeMutatorState) (url, body string)
		// mutate changes the created object and records the change's event.
		mutate func(t *testing.T, fs *fakeMutatorState, id string)
	}{
		{
			name: "bead create",
			request: func(_ *testing.T, fs *fakeMutatorState) (string, string) {
				return cityURL(fs, "/beads"), `{"rig":"myrig","title":"created"}`
			},
			mutate: retitle,
		},
		{
			name: "convoy create",
			request: func(t *testing.T, fs *fakeMutatorState) (string, string) {
				item, err := fs.stores["myrig"].Create(beads.Bead{Title: "item"})
				if err != nil {
					t.Fatalf("create item: %v", err)
				}
				return cityURL(fs, "/convoys"), `{"rig":"myrig","title":"convoy","items":["` + item.ID + `"]}`
			},
			mutate: retitle,
		},
		{
			name: "mail send",
			request: func(_ *testing.T, fs *fakeMutatorState) (string, string) {
				return cityURL(fs, "/mail"), `{"rig":"test-city","from":"human","to":"worker","subject":"Hi","body":"hello"}`
			},
			mutate: markRead,
		},
		{
			name: "mail reply",
			request: func(t *testing.T, fs *fakeMutatorState) (string, string) {
				msg, err := fs.cityMailProv.Send("alice", "worker", "Initial", "content")
				if err != nil {
					t.Fatalf("send original: %v", err)
				}
				return cityURL(fs, "/mail/") + msg.ID + "/reply", `{"from":"worker","subject":"Re: Initial","body":"Done!"}`
			},
			mutate: markRead,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeMutatorState(t)
			ep := fs.eventProv.(*events.Fake)
			ep.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: "seed"})
			h := newTestCityHandler(t, fs)
			url, body := tt.request(t, fs)

			first := postIdempotent(t, h, url, "replay-index", body)
			if first.Code != http.StatusCreated {
				t.Fatalf("first create: status = %d, want 201; body = %s", first.Code, first.Body.String())
			}
			var created struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil || created.ID == "" {
				t.Fatalf("decode created id: %v (body=%q)", err, first.Body.String())
			}

			tt.mutate(t, fs, created.ID)
			changeSeq := ep.Events[len(ep.Events)-1].Seq

			replay := postIdempotent(t, h, url, "replay-index", body)
			if replay.Code != http.StatusCreated {
				t.Fatalf("replay: status = %d, want 201; body = %s", replay.Code, replay.Body.String())
			}
			if replay.Body.String() != first.Body.String() {
				t.Fatalf("replay body = %s, want the original %s; the test no longer models a cached replay", replay.Body.String(), first.Body.String())
			}
			if idx := responseIndex(t, replay); idx >= changeSeq {
				t.Fatalf("replayed body predates the change to %s but X-GC-Index = %d, want < %d", created.ID, idx, changeSeq)
			}
		})
	}
}

// TestStructuredTranscriptFallbackIndexDoesNotSkipLateEvents covers the
// structured transcript fallback. Its body comes from the session read at
// handler entry, so it must report the index captured before that read, not
// one taken after an event the body does not reflect.
func TestStructuredTranscriptFallbackIndexDoesNotSkipLateEvents(t *testing.T) {
	isolateProviderDiscovery(t)
	fs := newSessionFakeState(t)
	ep := fs.eventProv.(*events.Fake)
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: "seed"})
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "myrig/worker", Title: "Chat", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	store := &lateEventStore{Store: fs.cityBeadStore, ep: ep}
	fs.sessionsBeadStore = store
	h := newTestCityHandler(t, fs)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=structured", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var resp sessionTranscriptGetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	if resp.History == nil || resp.History.Continuity.Status != "degraded" {
		t.Fatalf("response did not take the structured fallback; the test no longer covers it (body=%s)", rec.Body.String())
	}
	if store.lateSeq == 0 {
		t.Fatal("late event did not fire; the transcript path no longer reads this store")
	}
	if idx := responseIndex(t, rec); idx >= store.lateSeq {
		t.Fatalf("X-GC-Index = %d, want < %d: the fallback body comes from the session read that preceded the late event", idx, store.lateSeq)
	}
}

// TestSessionPinIndexDoesNotSkipLateEvents covers POST /session/{id}/pin and
// /unpin. Their body comes from session reads that begin at handler entry, so
// they must report the index captured before the first read, not one taken
// after an event the body does not reflect.
func TestSessionPinIndexDoesNotSkipLateEvents(t *testing.T) {
	for _, verb := range []string{"pin", "unpin"} {
		t.Run(verb, func(t *testing.T) {
			fs := newSessionFakeState(t)
			ep := fs.eventProv.(*events.Fake)
			ep.Record(events.Event{Type: events.BeadCreated, Actor: "test", Subject: "seed"})
			info := createTestSession(t, fs.cityBeadStore, fs.sp, "S")
			store := &lateEventStore{Store: fs.cityBeadStore, ep: ep}
			fs.sessionsBeadStore = store
			h := newTestCityHandler(t, fs)

			rec := beadRequest(t, h, "POST", cityURL(fs, "/session/")+info.ID+"/"+verb, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
			}
			if store.lateSeq == 0 {
				t.Fatal("late event did not fire; the pin path no longer reads this store")
			}
			if idx := responseIndex(t, rec); idx >= store.lateSeq {
				t.Fatalf("X-GC-Index = %d, want < %d: the body comes from session reads that began before the late event", idx, store.lateSeq)
			}
		})
	}
}
