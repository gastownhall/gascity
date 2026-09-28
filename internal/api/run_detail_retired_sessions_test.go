package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/dashboardbff"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// seedClosedSessions creates n session beads shaped like production ones
// (Type=BeadType + LabelSession) and closes them, returning their ids. It is the
// "mature store" fixture for the ga-3cs9p performance contract: thousands of
// closed session beads that no dashboard read may scan.
func seedClosedSessions(t *testing.T, store beads.Store, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		b, err := store.Create(beads.Bead{
			Title:    "seat-" + strconv.Itoa(i),
			Type:     session.BeadType,
			Labels:   []string{session.LabelSession},
			Metadata: map[string]string{"session_name": "gc__seat-" + strconv.Itoa(i), "state": "active"},
		})
		if err != nil {
			t.Fatalf("create session %d: %v", i, err)
		}
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("close session %d: %v", i, err)
		}
		ids = append(ids, b.ID)
	}
	return ids
}

// TestSessionGetResolvesClosedSessionByExactIDWithoutListing pins the read the
// dashboard BFF now relies on for a finished run's retired seats: GET
// /v0/city/{city}/session/{id} resolves a CLOSED session bead by exact id with
// zero store.List calls — a point read, independent of how many closed beads the
// store holds — while the default sessions listing the run tailer polls stays
// open-only (a closed session never appears in it, so the poll never scans the
// closed population either).
func TestSessionGetResolvesClosedSessionByExactIDWithoutListing(t *testing.T) {
	fs := newSessionFakeState(t)
	counting := &readModelCountingStore{Store: beads.NewMemStore()}
	fs.cityBeadStore = counting
	closed := seedClosedSessions(t, counting, 2000)
	open := createTestSession(t, counting, fs.sp, "Live seat")
	h := newTestCityHandlerWith(t, fs, New(fs))

	counting.listCalls = 0
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/")+closed[1234], nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET closed session: status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != closed[1234] || got.Title != "seat-1234" {
		t.Fatalf("closed session = {id %q title %q}, want {%q %q}", got.ID, got.Title, closed[1234], "seat-1234")
	}
	if counting.listCalls != 0 {
		t.Fatalf("GET /session/{id} for a closed bead issued %d store.List call(s), want 0 (exact-id point read)", counting.listCalls)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/sessions?limit=1000"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET sessions: status %d; body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []sessionResponse `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Total != 1 || len(list.Items) != 1 || list.Items[0].ID != open.ID {
		t.Fatalf("default sessions listing = total %d items %d, want exactly the one open session %q (closed beads must stay out of the poll)", list.Total, len(list.Items), open.ID)
	}
}

// selfReadRecorder wraps the supervisor's loopback transport and records every
// self-read the dashboard plane issues, so the composition test can count the
// per-id session reads and inspect the listing's query.
type selfReadRecorder struct {
	next http.RoundTripper
	mu   sync.Mutex
	urls []string
}

func (r *selfReadRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.urls = append(r.urls, req.URL.RequestURI())
	r.mu.Unlock()
	return r.next.RoundTrip(req)
}

func (r *selfReadRecorder) reset() {
	r.mu.Lock()
	r.urls = nil
	r.mu.Unlock()
}

func (r *selfReadRecorder) count(match func(string) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, u := range r.urls {
		if match(u) {
			n++
		}
	}
	return n
}

func (r *selfReadRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.urls...)
}

type singleCityPaths map[string]string

func (m singleCityPaths) CityPath(n string) (string, bool) { p, ok := m[n]; return p, ok }

// writeRetiredRunEventLog writes a graph.v2 run root plus one closed step per session id
// into the city's append-only event log, the source the run tailer folds.
func writeRetiredRunEventLog(t *testing.T, cityPath string, sessionIDs []string) {
	t.Helper()
	beadEvent := func(seq uint64, b beads.Bead) events.Event {
		payload, err := json.Marshal(struct {
			Bead beads.Bead `json:"bead"`
		}{b})
		if err != nil {
			t.Fatalf("marshal bead: %v", err)
		}
		return events.Event{Seq: seq, Type: events.BeadCreated, Ts: time.Now(), Payload: payload}
	}
	evts := []events.Event{beadEvent(1, beads.Bead{
		ID: "run1", Title: "mol-adopt-pr-v2", Status: "closed", Type: "molecule", Ref: "mol-adopt-pr-v2",
		CreatedAt: time.Now().Add(-2 * time.Hour), UpdatedAt: time.Now().Add(-time.Hour),
		Metadata: map[string]string{
			"gc.formula_contract": "graph.v2", "gc.kind": "run", "gc.formula": "mol-adopt-pr-v2",
			"gc.run_target": "rig:demo", "gc.root_store_ref": "rig:demo", "gc.scope_kind": "rig", "gc.scope_ref": "demo",
		},
	})}
	for i, id := range sessionIDs {
		step := "step-" + strconv.Itoa(i)
		evts = append(evts, beadEvent(uint64(i+2), beads.Bead{
			ID: "run1." + strconv.Itoa(i+1), Title: step, Status: "closed", Type: "task", ParentID: "run1", Ref: "mol-adopt-pr-v2." + step,
			CreatedAt: time.Now().Add(-2 * time.Hour), UpdatedAt: time.Now().Add(-time.Hour),
			Metadata: map[string]string{
				"gc.kind": "step", "gc.root_bead_id": "run1", "gc.step_id": step, "gc.scope_ref": "demo",
				beadmeta.SessionIDMetadataKey: id,
			},
		}))
	}
	path := filepath.Join(cityPath, ".gc", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var sb strings.Builder
	for _, e := range evts {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		sb.Write(line)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
}

// runDetailSessionNames decodes a run-detail body to bead id → attached session
// link name ("" when the instance carries no attached link).
func runDetailSessionNames(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var detail struct {
		Nodes []struct {
			ExecutionInstances []struct {
				BeadID  string `json:"beadId"`
				Session struct {
					Kind string `json:"kind"`
					Link struct {
						SessionID   string `json:"sessionId"`
						SessionName string `json:"sessionName"`
					} `json:"link"`
				} `json:"session"`
			} `json:"executionInstances"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("decode detail: %v; body=%s", err, body)
	}
	out := map[string]string{}
	for _, n := range detail.Nodes {
		for _, inst := range n.ExecutionInstances {
			if inst.Session.Kind == "attached" {
				out[inst.BeadID] = inst.Session.Link.SessionName
			}
		}
	}
	return out
}

// TestRunDetailRetiredSeatsResolveAgainstRealStore is the ga-3cs9p composition
// proof and measurement: the dashboard plane's run detail, reading the REAL
// supervisor API over its loopback transport against a store holding thousands
// of closed session beads, names every retired seat of a finished run from its
// closed session bead — through exactly one by-id read per link, with the
// listing it polls unchanged (open-only). Wall time is logged per link count so
// the PR can report that the enrichment cost tracks the run's links, not the
// store's closed population: links=0 is the pre-change path (no by-id reads at
// all), and the closed-bead count is varied so its independence is visible.
func TestRunDetailRetiredSeatsResolveAgainstRealStore(t *testing.T) {
	for _, closedBeads := range []int{0, 5000} {
		for _, links := range []int{0, 1, 20} {
			t.Run(fmt.Sprintf("closed=%d/links=%d", closedBeads, links), func(t *testing.T) {
				fs := newSessionFakeState(t)
				closed := seedClosedSessions(t, fs.cityBeadStore, closedBeads)
				createTestSession(t, fs.cityBeadStore, fs.sp, "Live seat")
				// Link the run's steps to the LAST closed sessions so a scan-based
				// resolution would have had to walk the whole population.
				linked := make([]string, 0, links)
				for i := 0; i < links; i++ {
					if i < len(closed) {
						linked = append(linked, closed[len(closed)-1-i])
					} else {
						linked = append(linked, seedClosedSessions(t, fs.cityBeadStore, 1)[0])
					}
				}
				writeRetiredRunEventLog(t, fs.cityPath, linked)

				sm := NewSupervisorMux(&stateCityResolver{state: fs}, nil, false, "test", "", time.Now())
				sm.cacheMu.Lock()
				sm.cache[fs.CityName()] = cachedCityServer{state: fs, srv: New(fs)}
				sm.cacheMu.Unlock()
				reads := &selfReadRecorder{next: sm.LoopbackTransport()}
				plane := dashboardbff.New(dashboardbff.Deps{
					Resolver:          singleCityPaths{fs.CityName(): fs.cityPath},
					SupervisorBaseURL: "http://supervisor.loopback",
					SelfReadTransport: reads,
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				plane.Start(ctx)
				defer plane.Stop()

				// Warm the tailer's fold (the cold replay) so the timed read below
				// measures the enrichment path, not the first-request replay wait.
				warm := httptest.NewRecorder()
				plane.Handler().ServeHTTP(warm, httptest.NewRequest(http.MethodGet, "/api/city/"+fs.CityName()+"/runs/run1/detail", nil))
				if warm.Code != http.StatusOK {
					t.Fatalf("warm detail: status %d; body=%s", warm.Code, warm.Body.String())
				}
				reads.reset()

				// A fresh plane so the per-id cache is cold: this read pays every
				// by-id lookup the run needs and is the number the PR reports.
				coldPlane := dashboardbff.New(dashboardbff.Deps{
					Resolver:          singleCityPaths{fs.CityName(): fs.cityPath},
					SupervisorBaseURL: "http://supervisor.loopback",
					SelfReadTransport: reads,
				})
				coldPlane.Start(ctx)
				defer coldPlane.Stop()
				start := time.Now()
				rec := httptest.NewRecorder()
				coldPlane.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/"+fs.CityName()+"/runs/run1/detail", nil))
				elapsed := time.Since(start)
				if rec.Code != http.StatusOK {
					t.Fatalf("detail: status %d; body=%s", rec.Code, rec.Body.String())
				}

				names := runDetailSessionNames(t, rec.Body.Bytes())
				for i, id := range linked {
					beadID := "run1." + strconv.Itoa(i+1)
					stored, err := fs.cityBeadStore.Get(id)
					if err != nil {
						t.Fatalf("get %s: %v", id, err)
					}
					wantName := stored.Title
					if got := names[beadID]; got != wantName {
						t.Fatalf("%s: link name = %q, want the closed session's title %q (links=%v)", beadID, got, wantName, names)
					}
				}

				// Count only reads that ask for the exact-id point read: a by-id
				// read without the flag would resolve a miss through the name
				// ladder's closed-inclusive scans (TestSessionGetExactIDMissIssuesNoListing).
				byID := reads.count(func(u string) bool { return strings.Contains(u, "/session/") && strings.Contains(u, "exact_id=true") })
				if byID != links {
					t.Fatalf("exact-id session reads = %d, want %d (one per retired link); reads=%v", byID, links, reads.all())
				}
				if all := reads.count(func(u string) bool { return strings.Contains(u, "/session/") }); all != byID {
					t.Fatalf("%d by-id session read(s) lack exact_id=true; reads=%v", all-byID, reads.all())
				}
				for _, u := range reads.all() {
					if strings.Contains(u, "/sessions?") && (strings.Contains(u, "state=") || strings.Contains(u, "closed")) {
						t.Fatalf("sessions listing %q must stay the default open-only read", u)
					}
				}
				t.Logf("closed session beads in store=%d retired links=%d by-id reads=%d cold run-detail wall=%s", closedBeads, links, byID, elapsed)
			})
		}
	}
}

// TestSessionGetExactIDMissIssuesNoListing pins the MISS half of the point-read
// contract the dashboard BFF relies on: GET /v0/city/{city}/session/{id} with
// exact_id=true answers an id the store does not hold with 404 after the single
// store.Get — zero store.List calls — instead of walking the target ladder
// (configured name → live → alias → closed) whose closed-inclusive metadata
// scans cost a full pass over every closed session bead. The same flag turns a
// name that the ladder WOULD resolve into a 404, proving the ladder is skipped
// rather than merely reordered, while a closed bead still resolves by exact id.
// The control asserts the default (flag-less) read of the same absent id does
// issue List calls, so the zero above is a measured property of the flag and
// not of a counter that never fires.
func TestSessionGetExactIDMissIssuesNoListing(t *testing.T) {
	fs := newSessionFakeState(t)
	counting := &readModelCountingStore{Store: beads.NewMemStore()}
	fs.cityBeadStore = counting
	closed := seedClosedSessions(t, counting, 2000)
	h := newTestCityHandlerWith(t, fs, New(fs))

	get := func(identifier string, exact bool) *httptest.ResponseRecorder {
		t.Helper()
		u := cityURL(fs, "/session/") + identifier
		if exact {
			u += "?exact_id=true"
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u, nil))
		return rec
	}

	const absent = "gcg-session-00000000000000000000000000000000"
	counting.listCalls = 0
	if rec := get(absent, true); rec.Code != http.StatusNotFound {
		t.Fatalf("exact_id miss: status %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if counting.listCalls != 0 {
		t.Fatalf("exact_id miss issued %d store.List call(s), want 0 (a point read must not scan the closed population)", counting.listCalls)
	}

	// A closed session's runtime session_name resolves through the ladder's
	// closed-inclusive metadata scan by default; under exact_id it is a miss.
	byName := "gc__seat-1234"
	counting.listCalls = 0
	if rec := get(byName, true); rec.Code != http.StatusNotFound {
		t.Fatalf("exact_id by session_name: status %d, want 404 (ladder skipped); body=%s", rec.Code, rec.Body.String())
	}
	if counting.listCalls != 0 {
		t.Fatalf("exact_id by session_name issued %d store.List call(s), want 0", counting.listCalls)
	}

	counting.listCalls = 0
	rec := get(closed[1234], true)
	if rec.Code != http.StatusOK {
		t.Fatalf("exact_id closed hit: status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != closed[1234] {
		t.Fatalf("exact_id closed hit resolved %q, want %q", got.ID, closed[1234])
	}
	if counting.listCalls != 0 {
		t.Fatalf("exact_id closed hit issued %d store.List call(s), want 0", counting.listCalls)
	}

	// Control: the default read of the same absent id walks the ladder, whose
	// closed-inclusive steps List. This is the cost exact_id exists to avoid.
	counting.listCalls = 0
	if rec := get(absent, false); rec.Code != http.StatusNotFound {
		t.Fatalf("default miss: status %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if counting.listCalls == 0 {
		t.Fatal("default miss issued 0 store.List calls; the control must show the ladder scanning so the exact_id zero is meaningful")
	}
	if rec := get(byName, false); rec.Code != http.StatusOK {
		t.Fatalf("default by session_name: status %d, want 200 (ladder resolves closed names); body=%s", rec.Code, rec.Body.String())
	}
}
