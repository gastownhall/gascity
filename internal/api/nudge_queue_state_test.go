package api

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/session"
)

// recordingNudgeQueue is a NudgeQueue that keeps what the API sends it.
type recordingNudgeQueue struct {
	mu        sync.Mutex
	deferred  []nudgequeue.Item
	withdrawn [][]string
}

func (q *recordingNudgeQueue) EnqueueDeferred(item nudgequeue.Item) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.deferred = append(q.deferred, item)
	return nil
}

func (q *recordingNudgeQueue) WithdrawQueuedWaitNudges(ids []string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.withdrawn = append(q.withdrawn, append([]string(nil), ids...))
	return nil
}

func (q *recordingNudgeQueue) withdrewAll(ids ...string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	seen := map[string]bool{}
	for _, call := range q.withdrawn {
		for _, id := range call {
			seen[id] = true
		}
	}
	for _, id := range ids {
		if !seen[id] {
			return false
		}
	}
	return true
}

func requireStateJSONUnwritten(t *testing.T, cityPath string) {
	t.Helper()
	if _, err := os.Stat(nudgequeue.StatePath(cityPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state.json stat = %v, want the API to leave it to State.NudgeQueue", err)
	}
}

// A follow-up submit the session cannot take yet is queued through
// State.NudgeQueue.
func TestSessionSubmitFollowUpQueuesThroughStateNudgeQueue(t *testing.T) {
	fs := newSessionFakeState(t)
	queue := &recordingNudgeQueue{}
	fs.nudgeQueue = queue
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Queue Me")

	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/submit", strings.NewReader(`{"message":"later please","intent":"follow_up"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var accepted asyncAcceptedBody
	if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if success, failure := waitForSessionSubmitResult(t, fs.eventProv, accepted.RequestID); success == nil {
		t.Fatalf("session submit failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}

	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.deferred) != 1 || queue.deferred[0].SessionID != info.ID || queue.deferred[0].Message != "later please" {
		t.Fatalf("deferred = %+v, want the follow-up for %s", queue.deferred, info.ID)
	}
	requireStateJSONUnwritten(t, fs.cityPath)
}

// Closing a session, by either handler, withdraws its queued wait nudges
// through State.NudgeQueue.
func TestSessionCloseWithdrawsThroughStateNudgeQueue(t *testing.T) {
	for _, handler := range []string{"huma", "legacy"} {
		t.Run(handler, func(t *testing.T) {
			fs := newSessionFakeState(t)
			queue := &recordingNudgeQueue{}
			fs.nudgeQueue = queue
			srv := New(fs)
			info := createTestSession(t, fs.cityBeadStore, fs.sp, "Close Withdraws")
			first, later := createSessionCloseWaitOverflowWithQueuedNudges(t, fs, info.ID)

			w := httptest.NewRecorder()
			if handler == "huma" {
				newTestCityHandlerWith(t, fs, srv).ServeHTTP(w, newPostRequest(cityURL(fs, "/session/")+info.ID+"/close", nil))
			} else {
				srv.legacySessionHandler().ServeHTTP(w, newPostRequest("/v0/session/"+info.ID+"/close", nil))
			}
			if w.Code != http.StatusOK {
				t.Fatalf("close status = %d; body: %s", w.Code, w.Body.String())
			}
			if !queue.withdrewAll(first, later) {
				t.Fatalf("withdrawn = %v, want %s and %s", queue.withdrawn, first, later)
			}
		})
	}
}

// Waking a held session, by either handler, withdraws the nudges of the waits
// it cancels through State.NudgeQueue.
func TestSessionWakeWithdrawsThroughStateNudgeQueue(t *testing.T) {
	for _, handler := range []string{"huma", "legacy"} {
		t.Run(handler, func(t *testing.T) {
			fs := newSessionFakeState(t)
			queue := &recordingNudgeQueue{}
			fs.nudgeQueue = queue
			srv := New(fs)
			info := createTestSession(t, fs.cityBeadStore, fs.sp, "Wake Withdraws")
			if _, err := fs.cityBeadStore.Create(beads.Bead{
				Type:     session.WaitBeadType,
				Labels:   []string{session.WaitBeadLabel, "session:" + info.ID},
				Metadata: map[string]string{"session_id": info.ID, "state": "pending", "nudge_id": "wait-wake-1"},
			}); err != nil {
				t.Fatalf("create wait: %v", err)
			}
			if err := fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
				"wait_hold": "true", "sleep_intent": "wait-hold", "sleep_reason": "wait-hold",
			}); err != nil {
				t.Fatalf("hold the session: %v", err)
			}

			w := httptest.NewRecorder()
			if handler == "huma" {
				newTestCityHandlerWith(t, fs, srv).ServeHTTP(w, newPostRequest(cityURL(fs, "/session/")+info.ID+"/wake", nil))
			} else {
				srv.legacySessionHandler().ServeHTTP(w, newPostRequest("/v0/session/"+info.ID+"/wake", nil))
			}
			if w.Code != http.StatusOK {
				t.Fatalf("wake status = %d; body: %s", w.Code, w.Body.String())
			}
			if !queue.withdrewAll("wait-wake-1") {
				t.Fatalf("withdrawn = %v, want wait-wake-1", queue.withdrawn)
			}
		})
	}
}

// No non-test file in internal/api touches the state.json queue directly or
// picks the file queue itself: the API writes through State.NudgeQueue, so the
// controller, not the API, decides the queue's mode (DES-NQ D8). The one
// StateFileQueue the API builds is seededState's (dashport_support.go), which
// stands in for a controller over a seeded single-store city.
func TestAPIReachesTheNudgeQueueOnlyThroughState(t *testing.T) {
	var dir string
	if root := bazeltest.OverrideRoot(); root != "" {
		dir = filepath.Join(root, "internal", "api")
	} else {
		_, file, _, ok := goruntime.Caller(0)
		if !ok {
			t.Fatal("runtime.Caller failed")
		}
		dir = filepath.Dir(file)
	}
	forbidden := map[string]bool{"WithState": true, "LoadState": true, "WithdrawWaitNudges": true, "StatePath": true, "SortState": true}
	allowedFileQueues := map[string]int{"dashport_support.go": 1}
	fileQueues := map[string]int{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.CompositeLit); ok {
				if typ, ok := lit.Type.(*ast.SelectorExpr); ok {
					if pkg, ok := typ.X.(*ast.Ident); ok && pkg.Name == "nudgequeue" && typ.Sel.Name == "StateFileQueue" {
						fileQueues[name]++
					}
				}
				return true
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "nudgequeue" && forbidden[sel.Sel.Name] {
				t.Errorf("%s calls nudgequeue.%s; write through State.NudgeQueue", name, sel.Sel.Name)
			}
			return true
		})
	}
	for name, got := range fileQueues {
		if got != allowedFileQueues[name] {
			t.Errorf("%s builds nudgequeue.StateFileQueue %d times; take the queue from State.NudgeQueue", name, got)
		}
	}
	for name, want := range allowedFileQueues {
		if fileQueues[name] != want {
			t.Errorf("%s builds nudgequeue.StateFileQueue %d times, want exactly %d; remove a retired exception", name, fileQueues[name], want)
		}
	}
}
