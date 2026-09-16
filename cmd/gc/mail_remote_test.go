package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/clientcontext"
)

const remoteMailMessageJSON = `{"id":"mc-wisp-1","from":"alpha/mayor","to":"mayor","subject":"hello","body":"round trip","created_at":"2026-08-18T17:00:00Z","read":false}`

// clearRemoteMailIdentityEnv strips the identity env so tests control the
// default remote sender deterministically.
func clearRemoteMailIdentityEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GC_ALIAS", "GC_AGENT", "GC_SESSION_ID", "GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT", "GC_RIG", "GC_DIR"} {
		t.Setenv(k, "")
	}
}

// seedLocalCity creates a minimal local city named "alpha" and points the
// explicit city env at it, so the remote sender resolves "<local city>/…".
func seedLocalCity(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "alpha-home")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"alpha\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", dir)
	return dir
}

// The default remote sender is city-qualified ("<local city>/<identity>") with
// alias > agent > session id, and --from wins verbatim.
func TestRemoteMailSender(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	if got := remoteMailSender("citadel/mayor"); got != "citadel/mayor" {
		t.Errorf("--from must pass verbatim, got %q", got)
	}
	// Chdir to a non-city dir so cwd discovery cannot find a real city.
	t.Chdir(t.TempDir())
	if got := remoteMailSender(""); got != "human" {
		t.Errorf("no city, no identity: want human, got %q", got)
	}
	t.Setenv("GC_SESSION_ID", "s-1")
	if got := remoteMailSender(""); got != "s-1" {
		t.Errorf("session id fallback: got %q", got)
	}
	t.Setenv("GC_AGENT", "rig/worker")
	if got := remoteMailSender(""); got != "rig/worker" {
		t.Errorf("agent beats session id: got %q", got)
	}
	t.Setenv("GC_ALIAS", "mayor")
	if got := remoteMailSender(""); got != "mayor" {
		t.Errorf("alias beats agent: got %q", got)
	}
	seedLocalCity(t)
	if got := remoteMailSender(""); got != "alpha/mayor" {
		t.Errorf("city-qualified sender: got %q", got)
	}
}

func TestRemoteMailSubject(t *testing.T) {
	if got := remoteMailSubject("s", "b"); got != "s" {
		t.Errorf("explicit subject: %q", got)
	}
	if got := remoteMailSubject("", "first line\nsecond"); got != "first line" {
		t.Errorf("first-line derivation: %q", got)
	}
	long := strings.Repeat("x", 100)
	if got := remoteMailSubject("", long); len(got) != 80 || !strings.HasSuffix(got, "...") {
		t.Errorf("truncation: %d %q", len(got), got)
	}
	// A body that starts with blank lines still yields a non-empty subject
	// (the API requires minLength 1), and an all-blank body yields "" so the
	// caller refuses locally instead of round-tripping to a 422.
	if got := remoteMailSubject("", "\n\n  hello\nworld"); got != "hello" {
		t.Errorf("leading blank lines: %q", got)
	}
	if got := remoteMailSubject("  ", "\n \t\n"); got != "" {
		t.Errorf("all-blank must be empty, got %q", got)
	}
}

// Local-only modes are refused before the wire is touched.
func TestCmdMailSendRemote_RefusesLocalOnlyModes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server must not be contacted for a refused mode")
		w.WriteHeader(500)
	}))
	defer srv.Close()
	cases := []struct {
		name        string
		notify, all bool
		args        []string
		want        string
	}{
		{"all", false, true, []string{"body"}, "--all"},
		{"notify", true, false, []string{"mayor", "body"}, "--notify"},
		{"no-recipient", false, false, nil, "missing recipient"},
		{"no-body", false, false, []string{"mayor"}, "usage"},
		{"blank-body", false, false, []string{"mayor", "\n\t "}, "usage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := cmdMailSendRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), tc.args, tc.notify, tc.all, "", "", "", "", false, &out, &errb)
			if code == 0 || !strings.Contains(errb.String(), tc.want) {
				t.Errorf("exit=%d stderr=%q (want %q)", code, errb.String(), tc.want)
			}
		})
	}
	// Reply refuses --notify the same way.
	var out, errb bytes.Buffer
	if code := cmdMailReplyRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mc-1"}, "", "b", true, false, &out, &errb); code == 0 || !strings.Contains(errb.String(), "--notify") {
		t.Errorf("reply --notify: exit=%d stderr=%q", code, errb.String())
	}
}

// A remote send POSTs /v0/city/{city}/mail with the CSRF header, the
// city-qualified sender, and a derived subject when -s is absent; it echoes the
// resolved target on stderr and renders the server's message.
func TestCmdMailSendRemote_PostsAndRenders(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_ALIAS", "mayor")
	seedLocalCity(t)

	var gotPath, gotReq, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotReq, gotMethod = r.URL.Path, r.Header.Get("X-GC-Request"), r.Method
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(remoteMailMessageJSON))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := cmdMailSendRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor", "round trip"}, false, false, "", "", "", "", false, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/mc/mail" || gotReq == "" {
		t.Errorf("method=%q path=%q req=%q", gotMethod, gotPath, gotReq)
	}
	if gotBody["to"] != "mayor" || gotBody["from"] != "alpha/mayor" || gotBody["subject"] != "round trip" || gotBody["body"] != "round trip" {
		t.Errorf("body=%v", gotBody)
	}
	if !strings.Contains(out.String(), "Sent message mc-wisp-1 to mayor (as alpha/mayor)") {
		t.Errorf("stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "target:") || !strings.Contains(errb.String(), "mc @") {
		t.Errorf("remote send did not echo the resolved target: %q", errb.String())
	}
}

// --to / -s / -m and --from are honored; --json emits the local mail.send shape
// and no human target echo.
func TestCmdMailSendRemote_FlagsAndJSON(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(remoteMailMessageJSON))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := cmdMailSendRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), nil, false, false, "citadel/mayor", "mayor", "hello", "round trip", true, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if gotBody["to"] != "mayor" || gotBody["from"] != "citadel/mayor" || gotBody["subject"] != "hello" || gotBody["body"] != "round trip" {
		t.Errorf("body=%v", gotBody)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not JSON: %v (%q)", err, out.String())
	}
	if got["ok"] != true || got["command"] != "mail.send" || got["id"] != "mc-wisp-1" {
		t.Errorf("json=%v", got)
	}
	if strings.Contains(errb.String(), "target:") {
		t.Errorf("json mode leaked the human target echo: %q", errb.String())
	}
}

// A server error surfaces (non-fallbackable) with a non-zero exit.
func TestCmdMailSendRemote_ServerErrorSurfaces(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401,"detail":"write grant required"}`))
	}))
	defer srv.Close()
	var out, errb bytes.Buffer
	code := cmdMailSendRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor", "x"}, false, false, "", "", "", "", false, &out, &errb)
	if code == 0 || !strings.Contains(errb.String(), "gc mail send:") {
		t.Errorf("exit=%d stderr=%q", code, errb.String())
	}
}

// A remote reply POSTs /v0/city/{city}/mail/{id}/reply with the qualified
// sender and renders the reply.
func TestCmdMailReplyRemote_Posts(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_ALIAS", "mayor")
	seedLocalCity(t)
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"mc-wisp-2","from":"alpha/mayor","to":"jadegate/mayor","subject":"Re: hello","body":"ack","created_at":"2026-08-18T17:01:00Z","read":false,"reply_to":"mc-wisp-1"}`))
	}))
	defer srv.Close()
	var out, errb bytes.Buffer
	code := cmdMailReplyRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mc-wisp-1", "ack"}, "", "", false, false, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/mc/mail/mc-wisp-1/reply" {
		t.Errorf("method=%q path=%q", gotMethod, gotPath)
	}
	if gotBody["from"] != "alpha/mayor" || gotBody["body"] != "ack" {
		t.Errorf("body=%v", gotBody)
	}
	if !strings.Contains(out.String(), "Replied to mc-wisp-1 — sent message mc-wisp-2 to jadegate/mayor (as alpha/mayor)") {
		t.Errorf("stdout=%q", out.String())
	}
}

// A remote inbox GETs /v0/city/{city}/mail?agent=… — the explicit recipient,
// or this client's own qualified identity by default — and renders like a
// local inbox.
func TestCmdMailInboxRemote_ListsAndDefaultsRecipient(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_ALIAS", "mayor")
	seedLocalCity(t)
	var gotAgents []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/mc/mail" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		gotAgents = append(gotAgents, r.URL.Query().Get("agent"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[` + remoteMailMessageJSON + `],"total":1}`))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := cmdMailInboxRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor"}, false, &out, &errb); code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "mc-wisp-1") || !strings.Contains(out.String(), "alpha/mayor") {
		t.Errorf("stdout=%q", out.String())
	}
	out.Reset()
	errb.Reset()
	if code := cmdMailInboxRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), nil, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if len(gotAgents) != 2 || gotAgents[0] != "mayor" || gotAgents[1] != "alpha/mayor" {
		t.Errorf("agent params=%v", gotAgents)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not JSON: %v (%q)", err, out.String())
	}
	if got["recipient"] != "alpha/mayor" {
		t.Errorf("json recipient=%v", got["recipient"])
	}
}

// End-to-end dispatch: with --context set, `gc mail send|reply|inbox` route to
// the remote city instead of the (absent) local store, and the sticky/flag
// resolution never falls back locally on a remote error.
func TestCmdMail_ContextDispatchesRemote(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"items":[],"total":0}`))
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(remoteMailMessageJSON))
		}
	}))
	defer srv.Close()
	// A loopback http URL is accepted for a context (TLS is required only for
	// non-loopback hosts).
	prev := contextFlag
	contextFlag = "peer"
	t.Cleanup(func() { contextFlag = prev })
	var out, errb bytes.Buffer
	if code := doContextAdd(clientcontext.Context{Name: "peer", URL: srv.URL, City: "mc"}, &out, &errb); code != 0 {
		t.Fatalf("seed context: %q", errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := cmdMailSendJSON([]string{"mayor", "hi"}, false, false, "", "", "", "", false, &out, &errb); code != 0 {
		t.Fatalf("send exit %d; stderr=%q", code, errb.String())
	}
	if code := cmdMailReplyJSON([]string{"mc-wisp-1", "ack"}, "", "", false, false, &out, &errb); code != 0 {
		t.Fatalf("reply exit %d; stderr=%q", code, errb.String())
	}
	if code := cmdMailInboxWithJSON([]string{"mayor"}, false, &out, &errb); code != 0 {
		t.Fatalf("inbox exit %d; stderr=%q", code, errb.String())
	}
	want := []string{"POST /v0/city/mc/mail", "POST /v0/city/mc/mail/mc-wisp-1/reply", "GET /v0/city/mc/mail"}
	if strings.Join(hits, ",") != strings.Join(want, ",") {
		t.Errorf("hits=%v want %v", hits, want)
	}
}

// The remote inbox pages through the server's keyset pagination until the
// mailbox is exhausted, and a partial aggregate read is an error rather than a
// silently short list.
func TestCmdMailInboxRemote_PagesAndRefusesPartial(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	var cursors []string
	partial := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := r.URL.Query().Get("cursor")
		cursors = append(cursors, c)
		w.Header().Set("Content-Type", "application/json")
		if partial {
			_, _ = w.Write([]byte(`{"items":[` + remoteMailMessageJSON + `],"total":1,"partial":true,"partial_errors":["rig alpha: provider timeout"]}`))
			return
		}
		switch c {
		case "":
			_, _ = w.Write([]byte(`{"items":[` + remoteMailMessageJSON + `],"total":3,"next_cursor":"p2"}`))
		case "p2":
			_, _ = w.Write([]byte(`{"items":[` + strings.Replace(remoteMailMessageJSON, "mc-wisp-1", "mc-wisp-2", 1) + `],"total":3,"next_cursor":"p3"}`))
		default:
			_, _ = w.Write([]byte(`{"items":[` + strings.Replace(remoteMailMessageJSON, "mc-wisp-1", "mc-wisp-3", 1) + `],"total":3}`))
		}
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := cmdMailInboxRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor"}, false, &out, &errb); code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if strings.Join(cursors, ",") != ",p2,p3" {
		t.Errorf("cursors=%v (want first page, p2, p3)", cursors)
	}
	for _, id := range []string{"mc-wisp-1", "mc-wisp-2", "mc-wisp-3"} {
		if !strings.Contains(out.String(), id) {
			t.Errorf("stdout missing %s: %q", id, out.String())
		}
	}
	partial = true
	out.Reset()
	errb.Reset()
	if code := cmdMailInboxRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor"}, false, &out, &errb); code == 0 || !strings.Contains(errb.String(), "partial") || !strings.Contains(errb.String(), "provider timeout") {
		t.Errorf("partial read must fail: exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	if strings.Contains(out.String(), "mc-wisp") {
		t.Errorf("partial read rendered messages: %q", out.String())
	}
}

// Dispatch matrix beyond the happy path: a remote failure surfaces non-zero
// with no local fallback; GC_NO_API plus --context is a loud conflict; and
// with no remote selector and no local city, the "no city" error is the local
// one (the remote arm never engages).
func TestCmdMail_ContextDispatchMatrix(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"title":"Internal","status":500,"detail":"boom"}`))
	}))
	defer srv.Close()
	var out, errb bytes.Buffer
	if code := doContextAdd(clientcontext.Context{Name: "peer", URL: srv.URL, City: "mc"}, &out, &errb); code != 0 {
		t.Fatalf("seed context: %q", errb.String())
	}
	prev := contextFlag
	t.Cleanup(func() { contextFlag = prev })

	// 1. Remote failure: non-zero, no "Sent message", no local fallback.
	contextFlag = "peer"
	out.Reset()
	errb.Reset()
	if code := cmdMailSendJSON([]string{"mayor", "hi"}, false, false, "", "", "", "", false, &out, &errb); code == 0 || strings.Contains(out.String(), "Sent message") || !strings.Contains(errb.String(), "gc mail send:") {
		t.Errorf("remote failure: exit=%d stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	// 2. GC_NO_API + --context is a conflict, surfaced before any wire call.
	t.Setenv("GC_NO_API", "1")
	out.Reset()
	errb.Reset()
	if code := cmdMailSendJSON([]string{"mayor", "hi"}, false, false, "", "", "", "", false, &out, &errb); code == 0 || !strings.Contains(errb.String(), "GC_NO_API") {
		t.Errorf("GC_NO_API conflict: exit=%d stderr=%q", code, errb.String())
	}
	t.Setenv("GC_NO_API", "")
	// 3. No remote selector, no local city: the local "not in a city" error,
	//    i.e. the remote arm stays out of the way.
	contextFlag = ""
	out.Reset()
	errb.Reset()
	if code := cmdMailSendJSON([]string{"mayor", "hi"}, false, false, "", "", "", "", false, &out, &errb); code == 0 || !strings.Contains(errb.String(), "city") {
		t.Errorf("no city: exit=%d stderr=%q", code, errb.String())
	}
	if strings.Contains(errb.String(), "target:") {
		t.Errorf("no remote selector must not echo a remote target: %q", errb.String())
	}
}
