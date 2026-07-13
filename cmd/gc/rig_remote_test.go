package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/clientcontext"
	"github.com/gastownhall/gascity/internal/events"
)

func remoteRigTarget(url string) *remoteTarget {
	return &remoteTarget{BaseURL: url, CityName: "mc", Source: "flag"}
}

// writeRigSSEFrame writes one typed SSE frame the client can parse.
func writeRigSSEFrame(w http.ResponseWriter, seq uint64, typ string, payload any) {
	raw, _ := json.Marshal(struct {
		Seq     uint64 `json:"seq"`
		Type    string `json:"type"`
		Payload any    `json:"payload"`
	}{seq, typ, payload})
	_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// acceptThenSucceed is a server that 202-accepts the POST and streams a progress
// frame plus the terminal success. It echoes the CLIENT's minted request_id (the
// real server binds it as the idempotency key) so the client's request_id-verify
// (fix #6) and stream filter both see a consistent id.
func acceptThenSucceed(t *testing.T, captureBody *string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	reqID := ""
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			if captureBody != nil {
				*captureBody = string(b)
			}
			mu.Lock()
			reqID = requestIDFromBody(b)
			id := reqID
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "request_id": id, "event_cursor": "1"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events/stream"):
			mu.Lock()
			id := reqID
			mu.Unlock()
			writeRigSSEFrame(w, 2, events.RigProvisionProgress, api.RigProvisionProgressPayload{RequestID: id, Rig: "web", Step: "clone", Detail: "cloning web"})
			writeRigSSEFrame(w, 3, events.RequestResultRigCreate, api.RigCreateSucceededPayload{RequestID: id, Rig: "web", Prefix: "web", DefaultBranch: "main"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
}

// requestIDFromBody extracts the request_id from a rig-create POST body.
func requestIDFromBody(b []byte) string {
	var body struct {
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(b, &body)
	return body.RequestID
}

// The remote path refuses modes that need local client state, before any wire call.
func TestCmdRigAddRemote_RefusalMatrix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server must not be contacted for a refused mode")
		w.WriteHeader(500)
	}))
	defer srv.Close()
	client := func() *api.Client { return remoteTestClient(t, srv.URL) }
	tgt := remoteRigTarget(srv.URL)

	cases := []struct {
		name string
		run  func() int
		want string
	}{
		{"path-arg", func() int {
			var o, e bytes.Buffer
			return cmdRigAddRemote(client(), tgt, []string{"/some/path"}, "https://h/o/web.git", "", "", "", "", nil, false, false, false, &o, &e)
		}, "filesystem path"},
		{"missing-git-url", func() int {
			var o, e bytes.Buffer
			return cmdRigAddRemote(client(), tgt, nil, "", "", "", "", "", nil, false, false, false, &o, &e)
		}, "requires --git-url"},
		{"adopt", func() int {
			var o, e bytes.Buffer
			return cmdRigAddRemote(client(), tgt, nil, "https://h/o/web.git", "", "", "", "", nil, false, true /*adopt*/, false, &o, &e)
		}, "--adopt"},
		{"include", func() int {
			var o, e bytes.Buffer
			return cmdRigAddRemote(client(), tgt, nil, "https://h/o/web.git", "", "", "", "", []string{"gastown"}, false, false, false, &o, &e)
		}, "--include"},
		{"start-suspended", func() int {
			var o, e bytes.Buffer
			return cmdRigAddRemote(client(), tgt, nil, "https://h/o/web.git", "", "", "", "", nil, true /*startSuspended*/, false, false, &o, &e)
		}, "--start-suspended"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code := tc.run(); code != 1 {
				t.Fatalf("expected exit 1, got %d", code)
			}
		})
	}
}

// No --request-id ⇒ a fresh UUIDv4 is minted into the POST body; the rig name is
// derived from the git URL basename.
func TestCmdRigAddRemote_MintsRequestIDAndDerivesName(t *testing.T) {
	var body string
	srv := acceptThenSucceed(t, &body)
	defer srv.Close()

	var out, errb bytes.Buffer
	code := cmdRigAddRemote(remoteTestClient(t, srv.URL), remoteRigTarget(srv.URL), nil,
		"https://h/o/web.git", "", "", "", "", nil, false, false, false, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	var sent struct {
		Name      string `json:"name"`
		GitURL    string `json:"git_url"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("POST body not JSON: %v (%q)", err, body)
	}
	if sent.Name != "web" {
		t.Errorf("derived name = %q, want web", sent.Name)
	}
	if sent.GitURL != "https://h/o/web.git" {
		t.Errorf("git_url = %q", sent.GitURL)
	}
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRe.MatchString(sent.RequestID) {
		t.Errorf("minted request_id = %q, want a UUIDv4", sent.RequestID)
	}
	if !strings.Contains(out.String(), "provisioned → web") {
		t.Errorf("stdout = %q", out.String())
	}
}

// --request-id is forwarded verbatim; --name overrides the derivation.
func TestCmdRigAddRemote_ForwardsRequestIDAndNameOverride(t *testing.T) {
	var body string
	srv := acceptThenSucceed(t, &body)
	defer srv.Close()

	var out, errb bytes.Buffer
	code := cmdRigAddRemote(remoteTestClient(t, srv.URL), remoteRigTarget(srv.URL), nil,
		"https://h/o/web.git", "r-123", "override", "", "", nil, false, false, false, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	var sent struct {
		Name      string `json:"name"`
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal([]byte(body), &sent)
	if sent.RequestID != "r-123" {
		t.Errorf("request_id = %q, want r-123", sent.RequestID)
	}
	if sent.Name != "override" {
		t.Errorf("name = %q, want override", sent.Name)
	}
}

// --json emits exactly one JSONL object with rigAddJSONSummary parity + status +
// request_id, and no progress on stdout.
func TestCmdRigAddRemote_JSONParity(t *testing.T) {
	srv := acceptThenSucceed(t, nil)
	defer srv.Close()

	var out, errb bytes.Buffer
	code := cmdRigAddRemote(remoteTestClient(t, srv.URL), remoteRigTarget(srv.URL), nil,
		"https://h/o/web.git", "r-9", "", "", "", nil, false, false, true /*json*/, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want a single JSONL object, got %d lines: %q", len(lines), out.String())
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, lines[0])
	}
	for _, k := range []string{"schema_version", "ok", "command", "action", "name", "rig", "status", "request_id"} {
		if _, ok := got[k]; !ok {
			t.Errorf("json missing key %q: %v", k, got)
		}
	}
	if got["command"] != "rig add" || got["action"] != "add" || got["rig"] != "web" || got["status"] != "provisioned" || got["request_id"] != "r-9" {
		t.Errorf("json = %v", got)
	}
	if strings.Contains(out.String(), "cloning web") {
		t.Errorf("progress leaked onto --json stdout: %q", out.String())
	}
}

// A lost stream prints the CLIENT request_id and an idempotent re-attach recipe.
// gc events is gated to a local city, so the recovery text must NOT emit one.
func TestCmdRigAddRemote_WaitErrorRecipe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "request_id": requestIDFromBody(b), "event_cursor": "1"})
			return
		}
		w.WriteHeader(http.StatusNotFound) // permanent stream status ⇒ lost stream
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := cmdRigAddRemote(remoteTestClient(t, srv.URL), remoteRigTarget(srv.URL), nil,
		"https://h/o/web.git", "r-keep", "", "", "", nil, false, false, false, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	s := errb.String()
	if !strings.Contains(s, "request_id=r-keep") {
		t.Errorf("missing client request_id in recipe: %q", s)
	}
	// Idempotent re-attach recipe: same request_id, shell-quoted git URL + name.
	if !strings.Contains(s, "rig add --git-url 'https://h/o/web.git' --name 'web' --request-id 'r-keep'") {
		t.Errorf("missing idempotent re-attach recipe: %q", s)
	}
	// gc events cannot target a remote city, so it must never appear.
	if strings.Contains(s, "events --") {
		t.Errorf("recovery emits a gated gc events recipe: %q", s)
	}
}

// A structured 409 rig_name_conflict must NOT suggest re-POSTing a body (that
// would 409 again) and must NOT emit a gated gc events recipe. It surfaces the
// in-flight request_id and tells the operator to wait for that provision.
func TestCmdRigAddRemote_ConflictRecipe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": 409, "title": "Conflict", "detail": "rig name taken",
			"errors": []map[string]any{
				{"location": "body.code", "value": "rig_name_conflict"},
				{"location": "body.name", "value": "web"},
				{"location": "body.in_flight_request_id", "value": "r-inflight"},
				{"location": "body.event_cursor", "value": "9"},
			},
		})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := cmdRigAddRemote(remoteTestClient(t, srv.URL), remoteRigTarget(srv.URL), nil,
		"https://h/o/web.git", "r-new", "", "", "", nil, false, false, false, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	s := errb.String()
	// It must NOT suggest re-POSTing your body under the in-flight id.
	if strings.Contains(s, "rig add --git-url") {
		t.Errorf("conflict recipe must not re-POST a body: %q", s)
	}
	// gc events cannot target a remote city, so it must never appear.
	if strings.Contains(s, "events --") {
		t.Errorf("conflict recovery emits a gated gc events recipe: %q", s)
	}
	if !strings.Contains(s, "request_id=r-inflight") {
		t.Errorf("conflict must surface the in-flight request_id: %q", s)
	}
	if !strings.Contains(s, "Wait for it to finish") {
		t.Errorf("conflict must advise waiting for the in-flight provision: %q", s)
	}
}

// Recipe acceptance: every recovery path a remote rig add can print must point
// at a command the remote CLI actually accepts. gc events is gated to a local
// city (resolveEventsScope → "does not support a remote city"), so no recovery
// may emit a `gc <remote-flags> events` recipe; the only recovery command is the
// idempotent `gc rig add --request-id` re-attach. Covers both
// remoteInvocationFlags shapes (named context and ad-hoc --city-url/--city-name).
func TestRenderRemoteRigAddError_RecipesAvoidGatedEvents(t *testing.T) {
	targets := []*remoteTarget{
		{Ctx: &clientcontext.Context{Name: "prod"}, Source: "flag"},
		{BaseURL: "https://box:9443", CityName: "mc", Source: "flag"},
	}
	errCases := []error{
		&api.RigCreateWaitError{RequestID: "r-1", Err: errors.New("stream lost")},
		&api.RigCreateDeadlineError{RequestID: "r-2", Timeout: 30 * time.Minute},
		&api.RigCreateFailedError{RequestID: "r-3", Code: "clone_failed", Message: "boom"},
		&api.RigCreateConflictError{Code: "rig_name_conflict", Rig: "web", InFlightRequestID: "r-live", EventCursor: "9"},
	}
	for _, tgt := range targets {
		for _, e := range errCases {
			var out, errb bytes.Buffer
			renderRemoteRigAddError(e, tgt, "https://h/o/web.git", "web", "", "", false, &out, &errb)
			s := errb.String()
			if strings.Contains(s, "events --") {
				t.Errorf("recovery for %T emits a gated gc events recipe: %q", e, s)
			}
			// Any emitted recipe line must be a `rig add` re-attach. Recipes are
			// indented; the column-0 "gc rig add: <error>" diagnostic and prose are
			// not recipes, so restrict the check to indented `gc ` lines.
			for _, line := range strings.Split(s, "\n") {
				if !strings.HasPrefix(line, " ") {
					continue
				}
				trimmed := strings.TrimSpace(line)
				if !strings.HasPrefix(trimmed, "gc ") {
					continue
				}
				if !strings.Contains(trimmed, "rig add ") {
					t.Errorf("recovery for %T emits a non-rig-add gc recipe: %q", e, trimmed)
				}
			}
		}
	}
}

func TestDeriveRigNameFromGitURL(t *testing.T) {
	cases := map[string]string{
		"https://h/o/repo.git":  "repo",
		"https://h/o/repo":      "repo",
		"git@host:o/repo.git":   "repo",
		"https://h/o/repo.git/": "repo",
		"repo.git":              "repo",
	}
	for in, want := range cases {
		if got := deriveRigNameFromGitURL(in); got != want {
			t.Errorf("deriveRigNameFromGitURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// recipeFlag pulls a single-quoted flag value out of a printed recipe line.
func recipeFlag(t *testing.T, recipe, flag string) string {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(flag) + ` '([^']*)'`)
	m := re.FindStringSubmatch(recipe)
	if m == nil {
		t.Fatalf("flag %q not found in recipe: %q", flag, recipe)
	}
	return m[1]
}

// Fix #3: a resume recipe from a --prefix/--default-branch provision carries
// those digest-affecting flags, so replaying it hits the SAME server digest
// (200-exists), not a 409 request_id_conflict.
func TestCmdRigAddRemote_WaitRecipeReplaysSameDigest(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			var body map[string]string
			_ = json.Unmarshal(b, &body)
			mu.Lock()
			bodies = append(bodies, body)
			n := len(bodies)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				w.WriteHeader(http.StatusAccepted) // then the stream 404s ⇒ wait error
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "request_id": body["request_id"], "event_cursor": "1"})
				return
			}
			w.WriteHeader(http.StatusOK) // replay ⇒ idempotent 200-exists
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "exists", "rig": "web", "prefix": body["prefix"], "default_branch": body["default_branch"], "request_id": body["request_id"]})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := cmdRigAddRemote(remoteTestClient(t, srv.URL), remoteRigTarget(srv.URL), nil,
		"https://h/o/web.git", "r-keep", "", "p1", "release", nil, false, false, false, &out, &errb)
	if code != 1 {
		t.Fatalf("first attempt should fail with a wait error, got exit %d; stderr=%q", code, errb.String())
	}
	recipe := errb.String()
	if !strings.Contains(recipe, "--prefix 'p1'") || !strings.Contains(recipe, "--default-branch 'release'") {
		t.Fatalf("recipe omits digest-affecting flags: %q", recipe)
	}

	// Replay exactly what the recipe encodes.
	var out2, errb2 bytes.Buffer
	code = cmdRigAddRemote(remoteTestClient(t, srv.URL), remoteRigTarget(srv.URL), nil,
		recipeFlag(t, recipe, "--git-url"), recipeFlag(t, recipe, "--request-id"),
		recipeFlag(t, recipe, "--name"), recipeFlag(t, recipe, "--prefix"),
		recipeFlag(t, recipe, "--default-branch"), nil, false, false, false, &out2, &errb2)
	if code != 0 {
		t.Fatalf("recipe replay should 200-exist, got exit %d; stderr=%q", code, errb2.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("want 2 POSTs, got %d", len(bodies))
	}
	for _, k := range []string{"name", "prefix", "default_branch", "git_url", "request_id"} {
		if bodies[0][k] != bodies[1][k] {
			t.Errorf("provisioning field %q differs across replay: %q vs %q (would 409 on digest)", k, bodies[0][k], bodies[1][k])
		}
	}
}

// Fix #7: a credential-bearing git URL is redacted and shell-quoted in the recipe
// on BOTH the human (stderr) and --json (stdout) paths.
func TestCmdRigAddRemote_RecipeRedactsAndQuotesCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "request_id": requestIDFromBody(b), "event_cursor": "1"})
			return
		}
		w.WriteHeader(http.StatusNotFound) // ⇒ wait error ⇒ recipe printed
	}))
	defer srv.Close()

	const secretURL = "https://user:s3cr3t-token@github.com/o/web.git"
	for _, jsonOut := range []bool{false, true} {
		var out, errb bytes.Buffer
		code := cmdRigAddRemote(remoteTestClient(t, srv.URL), remoteRigTarget(srv.URL), nil,
			secretURL, "r-sec", "web", "", "", nil, false, false, jsonOut, &out, &errb)
		if code != 1 {
			t.Fatalf("json=%v: exit %d", jsonOut, code)
		}
		all := out.String() + errb.String()
		if strings.Contains(all, "s3cr3t-token") {
			t.Errorf("json=%v: credential leaked into recipe output: %q", jsonOut, all)
		}
		if !strings.Contains(all, "--git-url 'https://***@github.com/o/web.git'") {
			t.Errorf("json=%v: git URL not redacted+quoted in recipe: %q", jsonOut, all)
		}
	}
}

// Fix #5a: the absolute-watchdog deadline renders an honest "still running"
// message (distinct from a lost stream) with the request_id and a resume recipe
// that carries the digest-affecting flags.
func TestRenderRemoteRigAddError_DeadlineHonest(t *testing.T) {
	var out, errb bytes.Buffer
	err := &api.RigCreateDeadlineError{RequestID: "r-dl", Timeout: 30 * time.Minute}
	code := renderRemoteRigAddError(err, remoteRigTarget("https://h"),
		"https://h/o/web.git", "web", "p1", "main", false, &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	s := errb.String()
	if !strings.Contains(s, "still running after 30m0s") || !strings.Contains(s, "request_id=r-dl") {
		t.Errorf("deadline message not honest: %q", s)
	}
	if !strings.Contains(s, "--prefix 'p1'") || !strings.Contains(s, "--default-branch 'main'") {
		t.Errorf("deadline resume recipe missing digest-affecting flags: %q", s)
	}
}

// A JSON-mode resolve/refusal returns a machine-readable error object.
func TestCmdRigAddRemote_JSONRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server must not be contacted")
		w.WriteHeader(500)
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := cmdRigAddRemote(remoteTestClient(t, srv.URL), remoteRigTarget(srv.URL), nil,
		"", "", "", "", "", nil, false, false, true /*json*/, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("refusal not JSON: %v (%q)", err, out.String())
	}
	if got["error"] == nil && got["code"] == nil {
		t.Errorf("json refusal missing error/code: %v", got)
	}
}
