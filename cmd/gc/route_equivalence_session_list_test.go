package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/chartest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// sessionListAdditiveKeys are the optional keys only an API-served read
// prints: the server's own observations, which the CLI never computes.
var sessionListAdditiveKeys = []string{"_cache_age_s", "running", "reason", "last_output", "program_status"}

// sessionListRedactedKeys are the row keys only a local read prints: the API
// redacts them.
var sessionListRedactedKeys = []string{"command", "session_key", "resume_flag", "resume_style", "resume_command"}

// sessionListEquivalenceSeed records the ids the equivalence seed created.
type sessionListEquivalenceSeed struct {
	active, suspended, asleep, closed, stale string
}

// seedSessionListEquivalence seeds the rows that exposed the route gaps: an
// active session with every identity field, a suspended rig session, a
// never-active session whose rig only its session name carries, a closed
// session sharing the active one's agent_name, and a session whose metadata
// says active but whose runtime is not running.
func seedSessionListEquivalence(t *testing.T, store beads.Store) sessionListEquivalenceSeed {
	t.Helper()
	var ids sessionListEquivalenceSeed
	ids.active = seedSessionListBead(t, store, "active", map[string]string{
		"template": "worker", "session_name": "eq-active", "alias": "eq-boss", "agent_name": "worker",
		"provider": "claude", "transport": "tmux", "work_dir": "/w/home", "worker_dir": " /w/tree ",
		"state":                              "active",
		session.MetadataLastNudgeDeliveredAt: "2026-10-28T14:00:00.5+02:00",
		"command":                            "claude --x", "session_key": "key-1",
		"resume_flag": "--resume", "resume_style": "flag", "resume_command": "claude --resume key-1",
	})
	ids.suspended = seedSessionListBead(t, store, "suspended", map[string]string{
		"template": "alpha/rigbot", "session_name": "alpha--rigbot", "state": "suspended",
	})
	ids.asleep = seedSessionListBead(t, store, "asleep", map[string]string{
		"template": "worker", "session_name": "omega--worker", "state": "asleep",
	})
	ids.closed = seedSessionListBead(t, store, "closed", map[string]string{
		"template": "worker", "session_name": "eq-closed", "agent_name": "worker", "state": "asleep",
	})
	if err := store.Close(ids.closed); err != nil {
		t.Fatalf("store.Close(%s): %v", ids.closed, err)
	}
	ids.stale = seedSessionListBead(t, store, "stale", map[string]string{
		"template": "worker", "session_name": "eq-stale", "state": "active",
	})
	return ids
}

// runSessionListCLI runs the real `gc session list` command against cityPath
// and returns stdout and stderr.
func runSessionListCLI(t *testing.T, cityPath string, noAPI bool, args ...string) (string, string) {
	t.Helper()
	t.Setenv("GC_NO_API", "")
	if noAPI {
		t.Setenv("GC_NO_API", "1")
	}
	clearBuiltinImportWarningCache()
	full := append([]string{"--city", cityPath, "session", "list"}, args...)
	var stdout, stderr bytes.Buffer
	if code := run(full, &stdout, &stderr); code != 0 {
		t.Fatalf("run(%v) = %d; stderr:\n%s", full, code, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// stripRedacted removes the local-only redacted keys from every row of a
// session list document.
func stripRedacted(t *testing.T, doc []byte) []byte {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(doc, &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, doc)
	}
	for _, row := range sessionListRows(t, v) {
		for _, k := range sessionListRedactedKeys {
			delete(row, k)
		}
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// decodeSessionListDoc validates and decodes one `gc session list --json`
// document.
func decodeSessionListDoc(t *testing.T, raw string) map[string]any {
	t.Helper()
	validateJSONAgainstBuiltinSchema(t, []string{"session", "list"}, []byte(raw))
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, raw)
	}
	return doc
}

// TestSessionListRouteEquivalence runs `gc session list --json` on the local
// route, the supervisor-hosted route (the real routing ladder into the real
// SupervisorMux) and a remote route against one seeded city and one shared
// runtime, and asserts the three documents are identical modulo the declared
// API-only additive keys and the local-only redacted keys (#8453). A filter
// that lists closed sessions reads the supervisor-hosted city locally, so
// that route prints the local document byte for byte, and the remote route
// alone takes the API.
func TestSessionListRouteEquivalence(t *testing.T) {
	clearInheritedCityRoutingEnv(t)
	var ids sessionListEquivalenceSeed
	h := newCharCity(t, charCityBasic+"\n[session]\nprovider = \"fake\"\n", func(t *testing.T, store beads.Store) {
		ids = seedSessionListEquivalence(t, store)
	})
	fake, ok := h.cs.SessionProvider().(*runtime.Fake)
	if !ok {
		t.Fatalf("harness session provider is %T, want *runtime.Fake", h.cs.SessionProvider())
	}
	origBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return fake, nil
	}
	if err := fake.Start(context.Background(), "eq-active", runtime.Config{}); err != nil {
		t.Fatalf("fake.Start: %v", err)
	}
	fake.SetActivity("eq-active", time.Date(2026, 10, 28, 14, 0, 0, 123456789, time.FixedZone("+02", 2*3600)))
	fake.SetAttached("eq-active", true)

	var remote, alive *api.Client
	for _, lane := range h.lanes(t) {
		switch lane.name {
		case "remote":
			remote = lane.client
		case "alive":
			alive = lane.client
		}
	}
	origIdentity, origSup, origGrants := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook
	t.Cleanup(func() {
		buildSessionProviderByName = origBuild
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
		apiRouteSupervisorGrantsHook = origGrants
	})
	apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
		return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
	}
	apiRouteSupervisorClientHook = func(string) *api.Client { return alive }
	apiRouteSupervisorGrantsHook = func() bool { return false }

	filters := []struct {
		name, state, template string
	}{
		{name: "default"},
		{name: "all", state: "all"},
		{name: "closed", state: "closed"},
		{name: "active", state: "active"},
		{name: "suspended", state: "suspended"},
		{name: "open", state: "open"},
		{name: "active-or-closed", state: "active,closed"},
		{name: "template", template: "alpha/rigbot"},
	}
	for _, f := range filters {
		t.Run(f.name, func(t *testing.T) {
			var args []string
			if f.state != "" {
				args = append(args, "--state", f.state)
			}
			if f.template != "" {
				args = append(args, "--template", f.template)
			}
			args = append(args, "--json")

			localOut, localErr := runSessionListCLI(t, h.cityPath, true, args...)
			if !strings.Contains(localErr, "route=fallback reason=escape-hatch") {
				t.Fatalf("local stderr = %q, want the escape-hatch route line", localErr)
			}
			apiServed := map[string]string{}
			hostedOut, hostedErr := runSessionListCLI(t, h.cityPath, false, args...)
			if session.StateFilterIncludesClosed(f.state) {
				if !strings.Contains(hostedErr, "cmd=session list route=fallback reason="+sessionListClosedHistoryReason) {
					t.Fatalf("supervisor-hosted stderr = %q, want the closed-history fallback", hostedErr)
				}
				if hostedOut != localOut {
					t.Errorf("supervisor-hosted stdout = %s\nwant the local read's %s", hostedOut, localOut)
				}
			} else {
				if !strings.Contains(hostedErr, "cmd=session list route=api") {
					t.Fatalf("supervisor-hosted stderr = %q, want cmd=session list route=api", hostedErr)
				}
				apiServed["supervisor-hosted"] = hostedOut
			}
			var remoteOut, remoteErr bytes.Buffer
			if code := routeSessionList("", f.state, f.template, remote, "", true, &remoteOut, &remoteErr); code != 0 {
				t.Fatalf("remote routeSessionList = %d; stderr=%s", code, remoteErr.String())
			}
			if !strings.Contains(remoteErr.String(), "route=api") {
				t.Fatalf("remote stderr = %q, want route=api", remoteErr.String())
			}
			apiServed["remote"] = remoteOut.String()

			local := decodeSessionListDoc(t, localOut)
			if _, ok := local["_cache_age_s"]; ok {
				t.Errorf("local document carries _cache_age_s")
			}
			for _, row := range sessionListRows(t, local) {
				for _, k := range sessionListAdditiveKeys {
					if _, ok := row[k]; ok {
						t.Errorf("local row %v carries API-only %s", row["id"], k)
					}
				}
			}
			for name, raw := range apiServed {
				doc := decodeSessionListDoc(t, raw)
				if _, ok := doc["_cache_age_s"].(float64); !ok {
					t.Errorf("%s: _cache_age_s = %#v, want a number", name, doc["_cache_age_s"])
				}
				for _, row := range sessionListRows(t, doc) {
					if _, ok := row["running"].(bool); !ok {
						t.Errorf("%s: row %v running = %#v, want a boolean", name, row["id"], row["running"])
					}
					for _, k := range sessionListRedactedKeys {
						if _, ok := row[k]; ok {
							t.Errorf("%s: row %v carries redacted %s", name, row["id"], k)
						}
					}
				}
				if diff := chartest.JSONShapeDiff(stripRedacted(t, []byte(localOut)), []byte(raw), sessionListAdditiveKeys); diff != "" {
					t.Errorf("%s document differs from the local one: %s\nlocal: %s\n%s: %s", name, diff, localOut, name, raw)
				}
			}
			assertSessionListEquivalenceValues(t, f.name, local, ids)
		})
	}

	for _, f := range []struct{ name, state string }{{name: "text-default"}, {name: "text-all", state: "all"}} {
		t.Run(f.name, func(t *testing.T) {
			local, hosted, remoteOut := sessionListTextRoutes(t, h, remote, f.state)
			assertSessionListTablesMatch(t, "supervisor-hosted", local, hosted)
			assertSessionListTablesMatch(t, "remote", local, remoteOut)
		})
	}
}

// sessionListTextRoutes prints the `gc session list` table for state on the
// local route, the supervisor-hosted route (which the caller has set up) and
// the remote route over remote. The supervisor-hosted route takes the API
// unless state lists closed sessions, which it reads locally.
func sessionListTextRoutes(t *testing.T, h *charHarness, remote *api.Client, state string) (local, hosted, remoteOut string) {
	t.Helper()
	var args []string
	if state != "" {
		args = append(args, "--state", state)
	}
	local, _ = runSessionListCLI(t, h.cityPath, true, args...)
	hosted, hostedErr := runSessionListCLI(t, h.cityPath, false, args...)
	wantRoute := "cmd=session list route=api"
	if session.StateFilterIncludesClosed(state) {
		wantRoute = "cmd=session list route=fallback reason=" + sessionListClosedHistoryReason
	}
	if !strings.Contains(hostedErr, wantRoute) {
		t.Fatalf("supervisor-hosted stderr = %q, want %q", hostedErr, wantRoute)
	}
	var out, errOut bytes.Buffer
	if code := routeSessionList("", state, "", remote, "", false, &out, &errOut); code != 0 {
		t.Fatalf("remote routeSessionList = %d; stderr %s", code, errOut.String())
	}
	return local, hosted, out.String()
}

// assertSessionListEquivalenceValues spot-checks the unified values on the
// local document (the API documents equal it modulo the declared keys).
func assertSessionListEquivalenceValues(t *testing.T, filter string, local map[string]any, ids sessionListEquivalenceSeed) {
	t.Helper()
	rows := sessionListRows(t, local)
	for _, row := range rows {
		created, _ := row["created_at"].(string)
		if !sessionListUTCSecond.MatchString(created) {
			t.Errorf("row %v created_at = %q, want RFC 3339 UTC second precision", row["id"], created)
		}
		want := row["alias"]
		if want == nil {
			want = row["session_name"]
		}
		if row["name"] != want {
			t.Errorf("row %v name = %#v, want alias || session_name = %#v", row["id"], row["name"], want)
		}
	}
	if row := sessionListRowByID(t, local, ids.closed); row != nil {
		if row["state"] != "closed" || row["closed"] != true || row["last_active"] != "" {
			t.Errorf("closed row state/closed/last_active = %#v/%#v/%#v", row["state"], row["closed"], row["last_active"])
		}
	}
	for _, id := range []string{ids.suspended, ids.asleep} {
		if row := sessionListRowByID(t, local, id); row != nil && row["last_active"] != "" {
			t.Errorf("never-active row %s last_active = %#v, want \"\"", id, row["last_active"])
		}
	}
	if row := sessionListRowByID(t, local, ids.active); row != nil {
		for key, want := range map[string]any{
			"last_active": "2026-10-28T12:00:00Z", "last_nudge_delivered_at": "2026-10-28T12:00:00Z",
			"worker_dir": "/w/tree", "agent_name": "worker", "session_key": "key-1",
		} {
			if row[key] != want {
				t.Errorf("active row %s = %#v, want %#v", key, row[key], want)
			}
		}
	}
	if row := sessionListRowByID(t, local, ids.asleep); row != nil && row["rig"] != "omega" {
		t.Errorf("asleep row rig = %#v, want omega", row["rig"])
	}
	switch filter {
	case "all":
		summary, _ := local["summary"].(map[string]any)
		for key, want := range map[string]float64{"total": 5, "active": 1, "suspended": 1, "closed": 1} {
			if summary[key] != want {
				t.Errorf("--state all summary.%s = %#v, want %v", key, summary[key], want)
			}
		}
	case "closed":
		if len(rows) != 1 || rows[0]["id"] != ids.closed {
			t.Errorf("--state closed rows = %v, want exactly %s", rows, ids.closed)
		}
	}
}

// sessionListLanesOver serves the harness city's supervisor mux behind wrap
// and returns a plain client (the supervisor-hosted lane) and a TLS remote
// client (the --city-url lane) over it.
func sessionListLanesOver(t *testing.T, h *charHarness, wrap func(http.Handler) http.Handler) (hosted, remote *api.Client) {
	t.Helper()
	base := api.NewSupervisorMux(&singleCityStateResolver{state: h.cs}, nil, false, "controller", "test", time.Now()).
		WithAnyHostAllowed().
		Handler()
	handler := wrap(base)
	plain := httptest.NewServer(handler)
	t.Cleanup(plain.Close)
	tlsSrv := httptest.NewTLSServer(handler)
	t.Cleanup(tlsSrv.Close)
	remote, err := api.NewRemoteCityScopedClient(tlsSrv.URL, charCityName, api.RemoteOptions{CAFile: writeCapstoneServerCA(t, tlsSrv)})
	if err != nil {
		t.Fatalf("remote client: %v", err)
	}
	return api.NewCityScopedClient(plain.URL, charCityName), remote
}

// sessionListHostedBy makes the city supervisor-hosted, served by c, for
// every `run` in the rest of the test.
func sessionListHostedBy(t *testing.T, c *api.Client) {
	t.Helper()
	origIdentity, origSup, origGrants := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook
	t.Cleanup(func() {
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
		apiRouteSupervisorGrantsHook = origGrants
	})
	apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
		return controllerIdentityReply{PID: 4242, HostingMode: controllerHostingSupervisor}
	}
	apiRouteSupervisorClientHook = func(string) *api.Client { return c }
	apiRouteSupervisorGrantsHook = func() bool { return false }
}

// runSessionListCLIExit runs `gc session list` like runSessionListCLI but
// returns the exit code instead of failing on a non-zero one.
func runSessionListCLIExit(t *testing.T, cityPath string, noAPI bool, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("GC_NO_API", "")
	if noAPI {
		t.Setenv("GC_NO_API", "1")
	}
	clearBuiltinImportWarningCache()
	full := append([]string{"--city", cityPath, "session", "list"}, args...)
	var stdout, stderr bytes.Buffer
	code := run(full, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// sessionListRewriteListBodies wraps base so every GET /sessions answer is
// passed through edit before it reaches the client.
func sessionListRewriteListBodies(base http.Handler, edit func(body map[string]any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/sessions") {
			base.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		base.ServeHTTP(rec, r)
		var body map[string]any
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}
		edit(body)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.Code)
		_ = json.NewEncoder(w).Encode(body)
	})
}

// validateSessionListFailureDoc validates raw against the schema `gc session
// list --json-schema=failure` prints and decodes it.
func validateSessionListFailureDoc(t *testing.T, raw string) map[string]any {
	t.Helper()
	var schemaOut, schemaErr bytes.Buffer
	if code := run([]string{"session", "list", "--json-schema=failure"}, &schemaOut, &schemaErr); code != 0 {
		t.Fatalf("gc session list --json-schema=failure = %d; stderr %s", code, schemaErr.String())
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(&schemaOut)
	if err != nil {
		t.Fatalf("parse failure schema: %v", err)
	}
	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("session/list/failure.schema.json", schemaDoc); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile("session/list/failure.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := compiled.Validate(instance); err != nil {
		t.Fatalf("document does not validate against the failure schema: %v\n%s", err, raw)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestSessionListAPIRouteRefusesAPartialAnswer pins that a partial GET
// /sessions answer fails gc session list as a partial local read does: exit
// 1, the session_list_failed document under --json, the message on stderr
// otherwise, on the supervisor-hosted and remote routes alike.
func TestSessionListAPIRouteRefusesAPartialAnswer(t *testing.T) {
	clearInheritedCityRoutingEnv(t)
	h := newCharCity(t, charCityBasic+"\n[session]\nprovider = \"fake\"\n", func(t *testing.T, store beads.Store) {
		seedSessionListEquivalence(t, store)
	})
	hosted, remote := sessionListLanesOver(t, h, func(base http.Handler) http.Handler {
		return sessionListRewriteListBodies(base, func(body map[string]any) {
			body["partial"] = true
			body["partial_errors"] = []string{"bd list: boom"}
		})
	})
	sessionListHostedBy(t, hosted)

	type result struct {
		code           int
		stdout, stderr string
	}
	runRoute := func(lane string, jsonOut bool) result {
		if lane == "remote" {
			var stdout, stderr bytes.Buffer
			code := routeSessionList("", "", "", remote, "", jsonOut, &stdout, &stderr)
			return result{code, stdout.String(), stderr.String()}
		}
		var args []string
		if jsonOut {
			args = append(args, "--json")
		}
		code, stdout, stderr := runSessionListCLIExit(t, h.cityPath, false, args...)
		return result{code, stdout, stderr}
	}
	for _, lane := range []string{"supervisor-hosted", "remote"} {
		t.Run(lane+"/json", func(t *testing.T) {
			got := runRoute(lane, true)
			if got.code != 1 {
				t.Fatalf("exit = %d, want 1; stdout %q stderr %q", got.code, got.stdout, got.stderr)
			}
			doc := validateSessionListFailureDoc(t, got.stdout)
			if _, ok := doc["sessions"]; ok {
				t.Errorf("failure document carries sessions: %s", got.stdout)
			}
			errObj, _ := doc["error"].(map[string]any)
			if errObj["code"] != "session_list_failed" || errObj["exit_code"] != float64(1) {
				t.Errorf("error = %#v, want code session_list_failed exit_code 1", errObj)
			}
			msg, _ := errObj["message"].(string)
			if !strings.HasPrefix(msg, "gc session list: listing sessions: ") || !strings.Contains(msg, "bd list: boom") {
				t.Errorf("message = %q, want the local read's prefix and the server's error", msg)
			}
			if !strings.Contains(got.stderr, "route=api reason=error") {
				t.Errorf("stderr = %q, want route=api reason=error", got.stderr)
			}
		})
		t.Run(lane+"/text", func(t *testing.T) {
			got := runRoute(lane, false)
			if got.code != 1 {
				t.Fatalf("exit = %d, want 1; stdout %q stderr %q", got.code, got.stdout, got.stderr)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty", got.stdout)
			}
			if !strings.Contains(got.stderr, "route=api reason=error") || !strings.Contains(got.stderr, "gc session list: listing sessions: bd list: boom") {
				t.Errorf("stderr = %q, want route=api reason=error and the listing failure", got.stderr)
			}
		})
	}
}

// TestSessionListClosedHistoryReadFailureFailsTheList pins the local read's
// closed-history failure on the route a supervisor-hosted city's --state all
// takes: a partial or failed listing exits 1 with the session_list_failed
// document under --json and the message on stderr otherwise, never a list
// that silently lacks the sessions the read could not return.
func TestSessionListClosedHistoryReadFailureFailsTheList(t *testing.T) {
	clearInheritedCityRoutingEnv(t)
	h := newCharCity(t, charCityBasic+"\n[session]\nprovider = \"fake\"\n", func(t *testing.T, store beads.Store) {
		seedSessionListEquivalence(t, store)
	})
	// The closed-history route never asks the API; were it asked, this
	// client's refused connection would print another route line.
	sessionListHostedBy(t, api.NewCityScopedClient("http://127.0.0.1:1", charCityName))
	origHistory := listSessionHistory
	t.Cleanup(func() { listSessionHistory = origHistory })

	const prefix = "gc session list: listing sessions including closed history: "
	for _, tc := range []struct {
		name   string
		read   func(front *session.Store) ([]session.Info, error)
		detail string
	}{
		{
			name: "partial",
			read: func(front *session.Store) ([]session.Info, error) {
				infos, err := origHistory(front)
				if err != nil {
					return nil, err
				}
				return infos, &beads.PartialResultError{Op: "bd list", Err: errors.New("skipped 1 corrupt session bead")}
			},
			detail: "skipped 1 corrupt session bead",
		},
		{
			name:   "failed",
			read:   func(*session.Store) ([]session.Info, error) { return nil, errors.New("store unreachable") },
			detail: "store unreachable",
		},
	} {
		listSessionHistory = tc.read
		t.Run(tc.name+"/json", func(t *testing.T) {
			code, stdout, stderr := runSessionListCLIExit(t, h.cityPath, false, "--state", "all", "--json")
			if code != 1 {
				t.Fatalf("exit = %d, want 1; stdout %q stderr %q", code, stdout, stderr)
			}
			if !strings.Contains(stderr, "cmd=session list route=fallback reason="+sessionListClosedHistoryReason) {
				t.Errorf("stderr = %q, want the closed-history fallback", stderr)
			}
			doc := validateSessionListFailureDoc(t, stdout)
			if _, ok := doc["sessions"]; ok {
				t.Errorf("failure document carries sessions: %s", stdout)
			}
			errObj, _ := doc["error"].(map[string]any)
			if errObj["code"] != "session_list_failed" || errObj["exit_code"] != float64(1) {
				t.Errorf("error = %#v, want code session_list_failed exit_code 1", errObj)
			}
			if msg, _ := errObj["message"].(string); !strings.HasPrefix(msg, prefix) || !strings.Contains(msg, tc.detail) {
				t.Errorf("message = %q, want %q and %q", msg, prefix, tc.detail)
			}
		})
		t.Run(tc.name+"/text", func(t *testing.T) {
			code, stdout, stderr := runSessionListCLIExit(t, h.cityPath, false, "--state", "all")
			if code != 1 {
				t.Fatalf("exit = %d, want 1; stdout %q stderr %q", code, stdout, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, "cmd=session list route=fallback reason="+sessionListClosedHistoryReason) ||
				!strings.Contains(stderr, prefix) || !strings.Contains(stderr, tc.detail) {
				t.Errorf("stderr = %q, want the closed-history fallback and %q with %q", stderr, prefix, tc.detail)
			}
		})
	}
}

// sessionListOldServer wraps base as a supervisor that predates agent_name:
// its GET /sessions rows carry none of the fields added since.
func sessionListOldServer(base http.Handler) http.Handler {
	return sessionListRewriteListBodies(base, func(body map[string]any) {
		items, _ := body["items"].([]any)
		for _, item := range items {
			row, _ := item.(map[string]any)
			for _, k := range []string{"agent_name", "transport", "worker_dir", "wake_reasons"} {
				delete(row, k)
			}
		}
	})
}

// sessionListTimeColumns are the text-table columns whose values depend on
// when each route ran; only their "-" versus a value must match.
var sessionListTimeColumns = map[string]bool{"AGE": true, "LAST ACTIVE": true, "LAST NUDGE": true}

// sessionListTableCells splits a `gc session list` table into its header and
// its rows keyed by ID. Columns are separated by two or more spaces.
func sessionListTableCells(t *testing.T, table string) ([]string, map[string][]string, []string) {
	t.Helper()
	sep := regexp.MustCompile(`\s{2,}`)
	lines := strings.Split(strings.TrimRight(table, "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "ID ") {
		t.Fatalf("not a session table:\n%s", table)
	}
	header := sep.Split(strings.TrimSpace(lines[0]), -1)
	rows := map[string][]string{}
	var order []string
	for _, line := range lines[1:] {
		cells := sep.Split(strings.TrimSpace(line), -1)
		if len(cells) != len(header) {
			t.Fatalf("row %q has %d cells, header has %d:\n%s", line, len(cells), len(header), table)
		}
		rows[cells[0]] = cells
		order = append(order, cells[0])
	}
	return header, rows, order
}

// assertSessionListTablesMatch compares two `gc session list` tables cell by
// cell: the header, the row order and every cell exactly, except the time
// columns, where only "-" versus a value must match.
func assertSessionListTablesMatch(t *testing.T, label, want, got string) {
	t.Helper()
	wantHeader, wantRows, wantOrder := sessionListTableCells(t, want)
	gotHeader, gotRows, gotOrder := sessionListTableCells(t, got)
	if strings.Join(wantHeader, "|") != strings.Join(gotHeader, "|") {
		t.Fatalf("%s: header %q, want %q", label, gotHeader, wantHeader)
	}
	if strings.Join(wantOrder, ",") != strings.Join(gotOrder, ",") {
		t.Errorf("%s: rows %v, want %v\nwant:\n%s\ngot:\n%s", label, gotOrder, wantOrder, want, got)
		return
	}
	for _, id := range wantOrder {
		for i, column := range wantHeader {
			w, g := wantRows[id][i], gotRows[id][i]
			if sessionListTimeColumns[column] {
				if (w == "-") != (g == "-") {
					t.Errorf("%s: row %s %s = %q, want %q (set versus unset)", label, id, column, g, w)
				}
				continue
			}
			if w != g {
				t.Errorf("%s: row %s %s = %q, want %q", label, id, column, g, w)
			}
		}
	}
}

// TestSessionListOnASupervisorThatPredatesAgentName pins the CLI's handling
// of a supervisor still running a gc older than agent_name: a local target
// reads the city directly, so it prints what GC_NO_API=1 prints; a remote
// target, which has no local read, prints the server's rows and says what
// they lack. A current server stays on the API route. It lists open
// sessions: a local target never asks the server for closed history.
func TestSessionListOnASupervisorThatPredatesAgentName(t *testing.T) {
	clearInheritedCityRoutingEnv(t)
	h := newCharCity(t, charCityBasic+"\n[session]\nprovider = \"fake\"\n", func(t *testing.T, store beads.Store) {
		seedSessionListEquivalence(t, store)
	})
	oldHosted, oldRemote := sessionListLanesOver(t, h, sessionListOldServer)
	currentHosted, _ := sessionListLanesOver(t, h, func(base http.Handler) http.Handler { return base })

	for _, jsonOut := range []bool{true, false} {
		var args []string
		if jsonOut {
			args = append(args, "--json")
		}
		label := fmt.Sprintf("json=%v", jsonOut)
		want, _ := runSessionListCLI(t, h.cityPath, true, args...)

		sessionListHostedBy(t, oldHosted)
		got, gotErr := runSessionListCLI(t, h.cityPath, false, args...)
		if !strings.Contains(gotErr, "cmd=session list route=fallback reason=session-list-predates-agent-name") {
			t.Errorf("%s old server, supervisor-hosted: stderr = %q, want the predates-agent-name fallback", label, gotErr)
		}
		if jsonOut {
			if got != want {
				t.Errorf("%s old server, supervisor-hosted: stdout = %s\nwant the local read's %s", label, got, want)
			}
		} else {
			assertSessionListTablesMatch(t, label+" old server, supervisor-hosted", want, got)
		}

		var remoteOut, remoteErr bytes.Buffer
		if code := routeSessionList("", "", "", oldRemote, "", jsonOut, &remoteOut, &remoteErr); code != 0 {
			t.Fatalf("%s old server, remote: exit %d; stderr %s", label, code, remoteErr.String())
		}
		if !strings.Contains(remoteErr.String(), "route=api") || !strings.Contains(remoteErr.String(), "predates agent_name") {
			t.Errorf("%s old server, remote: stderr = %q, want route=api and a predates-agent_name notice", label, remoteErr.String())
		}
		if jsonOut {
			doc := decodeSessionListDoc(t, remoteOut.String())
			if rows := sessionListRows(t, doc); len(rows) == 0 {
				t.Errorf("old server, remote: no rows")
			}
			for _, row := range sessionListRows(t, doc) {
				if _, ok := row["agent_name"]; ok {
					t.Errorf("old server, remote: row %v carries agent_name", row["id"])
				}
			}
		}

		sessionListHostedBy(t, currentHosted)
		if _, curErr := runSessionListCLI(t, h.cityPath, false, args...); !strings.Contains(curErr, "cmd=session list route=api") {
			t.Errorf("%s current server: stderr = %q, want route=api", label, curErr)
		}
	}
}

// sessionListTextCity is a city whose agents give the text-equivalence seed
// its wake reasons: a named worker and a pool with a static maximum.
const sessionListTextCity = charCityBasic + `
[session]
provider = "fake"

[[agent]]
name = "worker"
start_command = "true"

[[agent]]
name = "pool"
start_command = "true"
max_active_sessions = 2
`

// TestSessionListTextRouteEquivalence pins the `gc session list` table on
// every route against one seeded city: the API route prints the local
// REASON column, wake reasons included, cell for cell. Only the text route
// asks the server for wake reasons; --json does not. --state all reads the
// supervisor-hosted city locally, so the remote route serves the closed
// row's cell through the API, and the open listing pins the
// supervisor-hosted API route.
func TestSessionListTextRouteEquivalence(t *testing.T) {
	clearInheritedCityRoutingEnv(t)
	ids := map[string]string{}
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	h := newCharCity(t, sessionListTextCity, func(t *testing.T, store beads.Store) {
		seed := func(key string, meta map[string]string) {
			ids[key] = seedSessionListBead(t, store, key, meta)
		}
		seed("attached", map[string]string{"template": "worker", "session_name": "tx-attached", "agent_name": "worker", "state": "active"})
		seed("pool", map[string]string{"template": "pool", "session_name": "tx-pool-1", "agent_name": "pool-1", "state": "active"})
		seed("creating", map[string]string{"template": "worker", "session_name": "tx-creating", "state": "creating", "pending_create_claim": "true"})
		seed("pinned", map[string]string{"template": "worker", "session_name": "tx-pinned", "state": "active", "pin_awake": "true"})
		seed("held", map[string]string{"template": "worker", "session_name": "tx-held", "state": "asleep", "held_until": future, "pin_awake": "true"})
		seed("closed", map[string]string{"template": "worker", "session_name": "tx-closed", "state": "asleep"})
		if err := store.Close(ids["closed"]); err != nil {
			t.Fatalf("store.Close: %v", err)
		}
	})
	fake, ok := h.cs.SessionProvider().(*runtime.Fake)
	if !ok {
		t.Fatalf("harness session provider is %T, want *runtime.Fake", h.cs.SessionProvider())
	}
	origBuild := buildSessionProviderByName
	t.Cleanup(func() { buildSessionProviderByName = origBuild })
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return fake, nil
	}
	for _, name := range []string{"tx-attached", "tx-pool-1", "tx-pinned"} {
		if err := fake.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("fake.Start(%s): %v", name, err)
		}
	}
	fake.SetAttached("tx-attached", true)

	var wakeQueries []string
	var mu sync.Mutex
	hosted, remote := sessionListLanesOver(t, h, func(base http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/sessions") {
				mu.Lock()
				wakeQueries = append(wakeQueries, r.URL.Query().Get("wake_reasons"))
				mu.Unlock()
			}
			base.ServeHTTP(w, r)
		})
	})
	sessionListHostedBy(t, hosted)

	local, hostedOut, remoteOut := sessionListTextRoutes(t, h, remote, "all")
	header, rows, _ := sessionListTableCells(t, local)
	reasonCol := slices.Index(header, "REASON")
	for key, want := range map[string]string{
		"attached": "session,attached",
		"pool":     "session,config",
		"creating": "create",
		"pinned":   "session,pin",
		"held":     "user-hold",
		"closed":   "-",
	} {
		if got := rows[ids[key]][reasonCol]; got != want {
			t.Errorf("local %s row REASON = %q, want %q\n%s", key, got, want, local)
		}
	}
	assertSessionListTablesMatch(t, "supervisor-hosted", local, hostedOut)
	assertSessionListTablesMatch(t, "remote", local, remoteOut)
	localOpen, hostedOpen, remoteOpen := sessionListTextRoutes(t, h, remote, "")
	assertSessionListTablesMatch(t, "supervisor-hosted open", localOpen, hostedOpen)
	assertSessionListTablesMatch(t, "remote open", localOpen, remoteOpen)
	mu.Lock()
	textQueries := slices.Clone(wakeQueries)
	wakeQueries = nil
	mu.Unlock()
	if len(textQueries) == 0 || slices.ContainsFunc(textQueries, func(q string) bool { return q != "true" }) {
		t.Errorf("text route wake_reasons queries = %q, want true on every request", textQueries)
	}

	runSessionListCLI(t, h.cityPath, false, "--json")
	mu.Lock()
	jsonQueries := slices.Clone(wakeQueries)
	mu.Unlock()
	if len(jsonQueries) == 0 || slices.ContainsFunc(jsonQueries, func(q string) bool { return q != "" }) {
		t.Errorf("--json wake_reasons queries = %q, want none", jsonQueries)
	}
}
