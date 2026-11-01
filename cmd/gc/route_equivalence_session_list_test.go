package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/chartest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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
// API-only additive keys and the local-only redacted keys (#8453).
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
			hostedOut, hostedErr := runSessionListCLI(t, h.cityPath, false, args...)
			if !strings.Contains(hostedErr, "cmd=session list route=api") {
				t.Fatalf("supervisor-hosted stderr = %q, want cmd=session list route=api", hostedErr)
			}
			var remoteOut, remoteErr bytes.Buffer
			if code := routeSessionList("", f.state, f.template, remote, "", true, &remoteOut, &remoteErr); code != 0 {
				t.Fatalf("remote routeSessionList = %d; stderr=%s", code, remoteErr.String())
			}
			if !strings.Contains(remoteErr.String(), "route=api") {
				t.Fatalf("remote stderr = %q, want route=api", remoteErr.String())
			}

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
			for name, raw := range map[string]string{"supervisor-hosted": hostedOut, "remote": remoteOut.String()} {
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

	t.Run("text-header", func(t *testing.T) {
		localOut, _ := runSessionListCLI(t, h.cityPath, true, "--state", "all")
		hostedOut, _ := runSessionListCLI(t, h.cityPath, false, "--state", "all")
		localHeader, _, _ := strings.Cut(localOut, "\n")
		hostedHeader, _, _ := strings.Cut(hostedOut, "\n")
		if strings.Join(strings.Fields(localHeader), " ") != strings.Join(strings.Fields(hostedHeader), " ") {
			t.Errorf("text headers differ:\nlocal:  %q\nhosted: %q", localHeader, hostedHeader)
		}
	})
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
