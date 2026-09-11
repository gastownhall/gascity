package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// writeRigFrame writes one typed SSE envelope frame (seq/type/payload) and
// flushes it, mirroring the wire the server emits on /events/stream.
func writeRigFrame(t *testing.T, w http.ResponseWriter, seq uint64, typ string, payload any) {
	t.Helper()
	raw, err := json.Marshal(struct {
		Seq     uint64 `json:"seq"`
		Type    string `json:"type"`
		Payload any    `json:"payload"`
	}{seq, typ, payload})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, raw)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// writeRigHeartbeat writes a heartbeat frame (no seq/type keys), the live-peer
// keep-alive that resets the idle watchdog and silent-attempt budget but must
// never regress the resume cursor.
func writeRigHeartbeat(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	_, _ = fmt.Fprintf(w, "event: heartbeat\ndata: {\"ts\":%q}\n\n", time.Now().UTC().Format(time.RFC3339))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeAccepted(t *testing.T, w http.ResponseWriter, cursor string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":       "accepted",
		"request_id":   "r1",
		"event_cursor": cursor,
	})
}

func fastRigParams() rigWaitParams {
	return rigWaitParams{maxSilentAttempts: 4, reconnectInitial: time.Millisecond, reconnectMaxDelay: 5 * time.Millisecond}
}

// Happy path: POST → 202 {cursor:7}; the stream resumes at after_seq=7, emits a
// heartbeat, two progress frames (one warn), and the terminal success.
func TestRigCreate_AcceptedThenSuccess(t *testing.T) {
	var posts int32
	var afterSeq string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GC-Request") == "" {
			t.Error("missing X-GC-Request on " + r.URL.Path)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0/city/c/rigs":
			atomic.AddInt32(&posts, 1)
			writeAccepted(t, w, "7")
		case r.Method == http.MethodGet && r.URL.Path == "/v0/city/c/events/stream":
			afterSeq = r.URL.Query().Get("after_seq")
			writeRigHeartbeat(t, w)
			writeRigFrame(t, w, 8, events.RigProvisionProgress, RigProvisionProgressPayload{RequestID: "r1", Rig: "web", Step: "clone", Detail: "cloning"})
			writeRigFrame(t, w, 9, events.RigProvisionProgress, RigProvisionProgressPayload{RequestID: "r1", Rig: "web", Step: "beads-init", Detail: "slow", Warn: true})
			writeRigFrame(t, w, 10, events.RequestResultRigCreate, RigCreateSucceededPayload{RequestID: "r1", Rig: "web", Prefix: "web", DefaultBranch: "main"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, err := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var steps []RigProvisionProgressPayload
	res, err := c.RigCreate(RigCreateRequest{Name: "web", GitURL: "https://h/o/web.git", RequestID: "r1"}, func(p RigProvisionProgressPayload) {
		steps = append(steps, p)
	})
	if err != nil {
		t.Fatalf("RigCreate: %v", err)
	}
	if res.Status != "provisioned" || res.Rig != "web" || res.Prefix != "web" || res.DefaultBranch != "main" || res.RequestID != "r1" {
		t.Errorf("result = %+v", res)
	}
	if afterSeq != "7" {
		t.Errorf("after_seq = %q, want 7 (the 202 cursor)", afterSeq)
	}
	if atomic.LoadInt32(&posts) != 1 {
		t.Errorf("posts = %d, want 1", posts)
	}
	if len(steps) != 2 || steps[0].Step != "clone" || steps[1].Step != "beads-init" || !steps[1].Warn {
		t.Errorf("progress steps = %+v", steps)
	}
}

// Reconnect across a mid-stream EOF: connect #1 emits progress seq=8 then closes;
// connect #2 must resume at after_seq=8 and deliver the terminal.
func TestRigCreate_ReconnectResumesAfterSeq(t *testing.T) {
	var connects int32
	var secondAfterSeq string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&connects, 1)
		if n == 1 {
			writeRigFrame(t, w, 8, events.RigProvisionProgress, RigProvisionProgressPayload{RequestID: "r1", Rig: "web", Step: "clone"})
			return // return => EOF mid-stream
		}
		secondAfterSeq = r.URL.Query().Get("after_seq")
		writeRigFrame(t, w, 9, events.RequestResultRigCreate, RigCreateSucceededPayload{RequestID: "r1", Rig: "web", Prefix: "web", DefaultBranch: "main"})
	}))
	defer srv.Close()

	c, err := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var terminals int
	tap := func(env *sseEnvelope) {
		if env.Type == events.RequestResultRigCreate {
			terminals++
		}
	}
	env, err := c.waitForEventReconnecting(t.Context(), "r1", events.RequestResultRigCreate, RequestOperationRigCreate, "7", tap, fastRigParams())
	if err != nil {
		t.Fatalf("waitForEventReconnecting: %v", err)
	}
	if env.Type != events.RequestResultRigCreate || env.Seq != 9 {
		t.Errorf("terminal env = %+v", env)
	}
	if secondAfterSeq != "8" {
		t.Errorf("reconnect after_seq = %q, want 8", secondAfterSeq)
	}
	if atomic.LoadInt32(&connects) != 2 {
		t.Errorf("connects = %d, want 2", connects)
	}
}

// A heartbeat (seq 0) after a real frame must not regress the resume cursor.
func TestRigCreate_HeartbeatDoesNotRegressCursor(t *testing.T) {
	var connects int32
	var secondAfterSeq string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&connects, 1)
		if n == 1 {
			writeRigFrame(t, w, 5, events.RigProvisionProgress, RigProvisionProgressPayload{RequestID: "r1", Rig: "web", Step: "clone"})
			writeRigHeartbeat(t, w) // seq 0 — must not lower the cursor from 5
			return
		}
		secondAfterSeq = r.URL.Query().Get("after_seq")
		writeRigFrame(t, w, 6, events.RequestResultRigCreate, RigCreateSucceededPayload{RequestID: "r1", Rig: "web"})
	}))
	defer srv.Close()

	c, _ := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{})
	if _, err := c.waitForEventReconnecting(t.Context(), "r1", events.RequestResultRigCreate, RequestOperationRigCreate, "0", nil, fastRigParams()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if secondAfterSeq != "5" {
		t.Errorf("reconnect after_seq = %q, want 5 (heartbeat must not regress)", secondAfterSeq)
	}
}

// A 401 on the stream connect re-mints the bearer per attempt; a second fresh
// token succeeds.
func TestRigCreate_StreamReauthPerAttempt(t *testing.T) {
	tokens := []string{"t1", "t2"}
	var i int32
	tokenSrc := func() (string, error) {
		n := atomic.AddInt32(&i, 1)
		return tokens[int(n)-1], nil
	}
	var connects int32
	var secondBearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&connects, 1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		secondBearer = r.Header.Get("Authorization")
		writeRigFrame(t, w, 2, events.RequestResultRigCreate, RigCreateSucceededPayload{RequestID: "r1", Rig: "web"})
	}))
	defer srv.Close()

	c, err := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{Token: tokenSrc})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.waitForEventReconnecting(t.Context(), "r1", events.RequestResultRigCreate, RequestOperationRigCreate, "1", nil, fastRigParams()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if secondBearer != "Bearer t2" {
		t.Errorf("second connect bearer = %q, want Bearer t2", secondBearer)
	}
}

// Two consecutive 401s ⇒ permanent (a revoked credential), no third dial — but
// with a backoff BEFORE the second attempt (fix #2) so a sub-second credential
// blip cannot trip the two-strike cap on a still-valid credential.
func TestRigCreate_StreamReauthAntiSpin(t *testing.T) {
	var connects int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&connects, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c, _ := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{Token: func() (string, error) { return "tok", nil }})
	// A measurable reconnect-initial makes the inter-401 delay observable without
	// a real sleep; the anti-spin still caps at 2 dials.
	params := rigWaitParams{maxSilentAttempts: 4, reconnectInitial: 40 * time.Millisecond, reconnectMaxDelay: 80 * time.Millisecond}
	start := time.Now()
	_, err := c.waitForEventReconnecting(t.Context(), "r1", events.RequestResultRigCreate, RequestOperationRigCreate, "1", nil, params)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error after two 401s")
	}
	if got := atomic.LoadInt32(&connects); got != 2 {
		t.Errorf("connects = %d, want exactly 2 (anti-spin)", got)
	}
	if elapsed < params.reconnectInitial {
		t.Errorf("elapsed %v < reconnect-initial %v: the second 401 attempt had no backoff (fix #2)", elapsed, params.reconnectInitial)
	}
}

// A 404 on the stream is permanent — the wait returns immediately, and RigCreate
// wraps it as *RigCreateWaitError carrying the request_id.
func TestRigCreate_PermanentStreamStatusWraps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeAccepted(t, w, "3")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c, _ := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{})
	_, err := c.RigCreate(RigCreateRequest{Name: "web", GitURL: "https://h/o/web.git", RequestID: "r1"}, nil)
	var we *RigCreateWaitError
	if err == nil || !asError(err, &we) {
		t.Fatalf("want *RigCreateWaitError, got %v", err)
	}
	if we.RequestID != "r1" {
		t.Errorf("wait error request_id = %q, want r1", we.RequestID)
	}
}

// The absolute watchdog (ctx deadline) fires while the stream is silent.
func TestRigCreate_WatchdogViaContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeRigHeartbeat(t, w)
		<-r.Context().Done() // hold the connection open, delivering no terminal
	}))
	defer srv.Close()

	c, _ := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{})
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()
	_, err := c.waitForEventReconnecting(ctx, "r1", events.RequestResultRigCreate, RequestOperationRigCreate, "1", nil, fastRigParams())
	if err == nil {
		t.Fatal("expected a deadline error")
	}
}

// The silent-attempt budget trips when connects deliver zero frames.
func TestRigCreate_SilentAttemptBudget(t *testing.T) {
	var connects int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&connects, 1)
		// return immediately => zero frames, clean EOF
	}))
	defer srv.Close()

	c, _ := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{})
	_, err := c.waitForEventReconnecting(t.Context(), "r1", events.RequestResultRigCreate, RequestOperationRigCreate, "1", nil, rigWaitParams{maxSilentAttempts: 3, reconnectInitial: time.Millisecond, reconnectMaxDelay: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "no frames") {
		t.Fatalf("want a silent-budget error, got %v", err)
	}
	if got := atomic.LoadInt32(&connects); got != 3 {
		t.Errorf("connects = %d, want 3", got)
	}
}

// 429/503 honor Retry-After then reconnect; a delivered frame resets the budget.
func TestRigCreate_TransientRetriesThenSucceeds(t *testing.T) {
	var connects int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&connects, 1)
		switch n {
		case 1:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			writeRigFrame(t, w, 4, events.RequestResultRigCreate, RigCreateSucceededPayload{RequestID: "r1", Rig: "web"})
		}
	}))
	defer srv.Close()

	c, _ := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{})
	env, err := c.waitForEventReconnecting(t.Context(), "r1", events.RequestResultRigCreate, RequestOperationRigCreate, "1", nil, fastRigParams())
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if env.Type != events.RequestResultRigCreate {
		t.Errorf("env = %+v", env)
	}
	if got := atomic.LoadInt32(&connects); got != 3 {
		t.Errorf("connects = %d, want 3", got)
	}
}

func TestClassifyRigStreamStatus(t *testing.T) {
	if c := classifyRigStreamStatus(http.StatusUnauthorized, "", time.Second); !c.reauth {
		t.Error("401 should be reauth")
	}
	if c := classifyRigStreamStatus(http.StatusNotFound, "", time.Second); !c.permanent {
		t.Error("404 should be permanent")
	}
	if c := classifyRigStreamStatus(http.StatusForbidden, "", time.Second); !c.permanent {
		t.Error("403 should be permanent")
	}
	if c := classifyRigStreamStatus(http.StatusServiceUnavailable, "2", time.Minute); c.permanent || c.reauth || c.delay != 2*time.Second {
		t.Errorf("503 retry-after=2 => %+v", c)
	}
	if c := classifyRigStreamStatus(http.StatusTooManyRequests, "999999", 30*time.Second); c.delay != 120*time.Second {
		t.Errorf("429 retry-after bound = %v, want 120s", c.delay)
	}
}

// Terminal request.failed => *RigCreateFailedError; a DIFFERENT request_id on
// both the success- and failed-type frames is ignored (concurrent isolation).
func TestRigCreate_TerminalFailedAndConcurrentIsolation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeAccepted(t, w, "1")
			return
		}
		// Frames for another provision (r2) must be skipped.
		writeRigFrame(t, w, 2, events.RequestResultRigCreate, RigCreateSucceededPayload{RequestID: "r2", Rig: "other"})
		writeRigFrame(t, w, 3, events.RequestFailed, RequestFailedPayload{RequestID: "r2", Operation: RequestOperationRigCreate, ErrorCode: "clone_failed", ErrorMessage: "nope"})
		// Our terminal failure.
		writeRigFrame(t, w, 4, events.RequestFailed, RequestFailedPayload{RequestID: "r1", Operation: RequestOperationRigCreate, ErrorCode: "blocked_host", ErrorMessage: "SSRF"})
	}))
	defer srv.Close()

	c, _ := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{})
	_, err := c.RigCreate(RigCreateRequest{Name: "web", GitURL: "https://h/o/web.git", RequestID: "r1"}, nil)
	var fe *RigCreateFailedError
	if err == nil || !asError(err, &fe) {
		t.Fatalf("want *RigCreateFailedError, got %v", err)
	}
	if fe.Code != "blocked_host" || fe.RequestID != "r1" {
		t.Errorf("failed error = %+v", fe)
	}
}

// 200 exists / 201 created return without ever dialing the stream.
func TestRigCreate_NoWaitShapes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   map[string]string
		want   string
	}{
		{"exists", http.StatusOK, map[string]string{"status": "exists", "rig": "web", "prefix": "web", "default_branch": "main", "request_id": "r1"}, "exists"},
		{"created", http.StatusCreated, map[string]string{"status": "created", "rig": "web", "request_id": "r1"}, "created"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v0/city/c/events/stream" {
					t.Error("must not dial the stream for a no-wait shape")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			defer srv.Close()
			c, _ := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{})
			res, err := c.RigCreate(RigCreateRequest{Name: "web", GitURL: "https://h/o/web.git", RequestID: "r1"}, nil)
			if err != nil {
				t.Fatalf("RigCreate: %v", err)
			}
			if res.Status != tc.want || res.Rig != "web" {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

// Grant discipline (gate G18): the grant rides the POST with a digest over the
// exact body, and never the SSE GET.
func TestRigCreate_GrantOnPostNeverOnStream(t *testing.T) {
	var mu sync.Mutex
	var postGrant, streamGrant string
	var mintedBodies []string
	grant := func(b GrantBinding) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		mintedBodies = append(mintedBodies, b.Method+" "+b.Path)
		return "grant-token", nil
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postGrant = r.Header.Get("X-GC-City-Write")
			writeAccepted(t, w, "1")
			return
		}
		streamGrant = r.Header.Get("X-GC-City-Write")
		writeRigFrame(t, w, 2, events.RequestResultRigCreate, RigCreateSucceededPayload{RequestID: "r1", Rig: "web"})
	}))
	defer srv.Close()

	c, _ := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{Grant: grant})
	if _, err := c.RigCreate(RigCreateRequest{Name: "web", GitURL: "https://h/o/web.git", RequestID: "r1"}, nil); err != nil {
		t.Fatalf("RigCreate: %v", err)
	}
	if postGrant != "grant-token" {
		t.Errorf("POST grant = %q, want grant-token", postGrant)
	}
	if streamGrant != "" {
		t.Errorf("stream carried a grant (%q); reads must not", streamGrant)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(mintedBodies) != 1 || !strings.HasPrefix(mintedBodies[0], "POST ") {
		t.Errorf("grant minted for = %v, want exactly one POST", mintedBodies)
	}
}

// A git_url add with no request_id is a hard client-side error (never minted).
func TestRigCreate_RequiresRequestIDWithGitURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("must not contact the server without a request_id")
	}))
	defer srv.Close()
	c, _ := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{})
	if _, err := c.RigCreate(RigCreateRequest{Name: "web", GitURL: "https://h/o/web.git"}, nil); err == nil {
		t.Fatal("expected an error for git_url without request_id")
	}
}

// A structured 409 decodes to *RigCreateConflictError carrying the in-flight id.
func TestRigCreate_StructuredConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": 409,
			"title":  "Conflict",
			"detail": "rig name taken",
			"errors": []map[string]any{
				{"location": "body.code", "value": "rig_name_conflict"},
				{"location": "body.name", "value": "web"},
				{"location": "body.in_flight_request_id", "value": "r-inflight"},
				{"location": "body.event_cursor", "value": "42"},
			},
		})
	}))
	defer srv.Close()
	c, _ := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{})
	_, err := c.RigCreate(RigCreateRequest{Name: "web", GitURL: "https://h/o/web.git", RequestID: "r1"}, nil)
	var ce *RigCreateConflictError
	if err == nil || !asError(err, &ce) {
		t.Fatalf("want *RigCreateConflictError, got %v", err)
	}
	if ce.Code != "rig_name_conflict" || ce.Rig != "web" || ce.InFlightRequestID != "r-inflight" || ce.EventCursor != "42" {
		t.Errorf("conflict = %+v", ce)
	}
}

// Fix #1: a terminal frame whose payload cannot decode must NOT advance the
// resume cursor past it. The reconnect re-reads the SAME seq; a second decode
// failure at that seq surfaces a permanent, honest error (not a 30-min hang).
func TestRigCreate_PoisonTerminalPayloadPermanent(t *testing.T) {
	var connects int32
	var secondAfterSeq string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&connects, 1)
		if n == 2 {
			secondAfterSeq = r.URL.Query().Get("after_seq")
		}
		// A JSON string where an object is expected ⇒ payloadContainsRequestID fails.
		writeRigFrame(t, w, 5, events.RequestResultRigCreate, "not-an-object")
	}))
	defer srv.Close()

	c, _ := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{})
	_, err := c.waitForEventReconnecting(t.Context(), "r1", events.RequestResultRigCreate, RequestOperationRigCreate, "1", nil, fastRigParams())
	if err == nil {
		t.Fatal("expected a permanent malformed-terminal error")
	}
	if !strings.Contains(err.Error(), "malformed terminal event at seq 5") {
		t.Errorf("error = %v, want a malformed-terminal-at-seq-5 error", err)
	}
	if got := atomic.LoadInt32(&connects); got != 2 {
		t.Errorf("connects = %d, want exactly 2 (retry once, then permanent — not a watchdog hang)", got)
	}
	if secondAfterSeq != "1" {
		t.Errorf("reconnect after_seq = %q, want 1 (the poison frame must be re-read, not skipped)", secondAfterSeq)
	}
}

// Fix #1: a transient terminal-payload decode failure (a truncated frame) is
// re-read on reconnect and succeeds, because the cursor never advanced past it.
func TestRigCreate_PoisonTerminalPayloadTransientRecovers(t *testing.T) {
	var connects int32
	var secondAfterSeq string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&connects, 1)
		if n == 1 {
			writeRigFrame(t, w, 5, events.RequestResultRigCreate, "truncated") // undecodable
			return
		}
		secondAfterSeq = r.URL.Query().Get("after_seq")
		writeRigFrame(t, w, 5, events.RequestResultRigCreate, RigCreateSucceededPayload{RequestID: "r1", Rig: "web"})
	}))
	defer srv.Close()

	c, _ := NewRemoteCityScopedClient(srv.URL, "c", RemoteOptions{})
	env, err := c.waitForEventReconnecting(t.Context(), "r1", events.RequestResultRigCreate, RequestOperationRigCreate, "1", nil, fastRigParams())
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if env.Type != events.RequestResultRigCreate || env.Seq != 5 {
		t.Errorf("terminal env = %+v, want the re-read seq-5 success", env)
	}
	if secondAfterSeq != "1" {
		t.Errorf("reconnect after_seq = %q, want 1 (re-read, not skip)", secondAfterSeq)
	}
	if got := atomic.LoadInt32(&connects); got != 2 {
		t.Errorf("connects = %d, want 2 (poison then clean re-read)", got)
	}
}

// The sseEnvelope decodes a seq key without disturbing the type match — the
// session-wait non-regression pin.
func TestSSEEnvelopeDecodesSeqForSessions(t *testing.T) {
	var env sseEnvelope
	if err := json.Unmarshal([]byte(`{"seq":11,"type":"request.result.session.submit","payload":{"request_id":"x"}}`), &env); err != nil {
		t.Fatal(err)
	}
	if env.Seq != 11 || env.Type != "request.result.session.submit" {
		t.Errorf("env = %+v", env)
	}
}

// asError is a tiny errors.As wrapper kept local so tests read cleanly.
func asError(err error, target any) bool {
	return errors.As(err, target)
}
