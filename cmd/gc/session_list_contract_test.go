package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
)

// These tests pin the `gc session list --json` contract: every route prints
// one shape (schemas/session/list/result.schema.json). A read the API served
// adds the optional _cache_age_s and the per-row running, reason, last_output
// and program_status fields; a local read omits them.

// sessionListRendering classifies a decoded `gc session list --json` payload.
// Both renderings carry filters and summary; the API one is identified by
// _cache_age_s. A payload missing filters or summary is a contract violation.
func sessionListRendering(t *testing.T, payload map[string]any) string {
	t.Helper()
	_, hasAge := payload["_cache_age_s"]
	_, hasFilters := payload["filters"]
	_, hasSummary := payload["summary"]
	if !hasFilters || !hasSummary {
		t.Fatalf("payload lacks filters or summary (filters=%v summary=%v): %v", hasFilters, hasSummary, payload)
	}
	if hasAge {
		return "api"
	}
	return "local"
}

// sessionListRows returns the decoded rows of payload, failing when sessions is
// absent, null, or not an array (an empty list must render as []).
func sessionListRows(t *testing.T, payload map[string]any) []map[string]any {
	t.Helper()
	raw, ok := payload["sessions"].([]any)
	if !ok {
		t.Fatalf("sessions = %#v, want an array ([] when empty)", payload["sessions"])
	}
	rows := make([]map[string]any, len(raw))
	for i, r := range raw {
		row, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("sessions[%d] = %#v, want an object", i, r)
		}
		rows[i] = row
	}
	return rows
}

// assertSessionListRendering checks the per-rendering invariants of a
// validated payload.
func assertSessionListRendering(t *testing.T, payload map[string]any, want string, wantEmpty bool) {
	t.Helper()
	if got := sessionListRendering(t, payload); got != want {
		t.Fatalf("rendering = %s, want %s: %v", got, want, payload)
	}
	rows := sessionListRows(t, payload)
	if wantEmpty && len(rows) != 0 {
		t.Fatalf("sessions = %v, want empty", rows)
	}
	if !wantEmpty && len(rows) == 0 {
		t.Fatalf("sessions is empty, want the seeded session")
	}
	if payload["schema_version"] != "1" {
		t.Fatalf("schema_version = %#v, want \"1\"", payload["schema_version"])
	}
	if payload["ok"] != true {
		t.Fatalf("ok = %#v, want true", payload["ok"])
	}
	summary, _ := payload["summary"].(map[string]any)
	if total, _ := summary["total"].(float64); int(total) != len(rows) {
		t.Fatalf("summary.total = %v, want len(sessions) = %d", summary["total"], len(rows))
	}
	for i, row := range rows {
		if _, ok := row["closed"].(bool); !ok {
			t.Fatalf("sessions[%d].closed = %#v, want a boolean", i, row["closed"])
		}
		if _, ok := row["last_active"].(string); !ok {
			t.Fatalf("sessions[%d].last_active = %#v, want a string", i, row["last_active"])
		}
	}
	switch want {
	case "api":
		if _, ok := payload["_cache_age_s"].(float64); !ok {
			t.Fatalf("_cache_age_s = %#v, want a number", payload["_cache_age_s"])
		}
		for i, row := range rows {
			if _, ok := row["running"].(bool); !ok {
				t.Fatalf("sessions[%d].running = %#v, want a boolean", i, row["running"])
			}
		}
	case "local":
		for i, row := range rows {
			for _, key := range []string{"running", "reason", "program_status"} {
				if _, ok := row[key]; ok {
					t.Fatalf("sessions[%d] carries API-only %s on a local read: %v", i, key, row)
				}
			}
		}
	}
}

// TestSessionListJSONRouteConformance runs the real `gc session list --json`
// command through each route the ladder can take and checks that every route
// prints the one shape, plus the API-only fields exactly when the API served
// the read, that each payload validates against the schema, and that the
// route audit line names the route taken. The decision
// table itself is TestSessionListAPIClientRouting's; this test pins what each
// decision prints.
func TestSessionListJSONRouteConformance(t *testing.T) {
	h := newContractHarness(t)
	h.createAgentSession(t, "route conformance")

	// A second city with an [api] port and an empty store: the [api]-port rows
	// read their API data from the harness and their local data from here.
	apiCity := t.TempDir()
	if err := bootstrapScopedFileProviderCityFS(fsys.OSFS{}, apiCity); err != nil {
		t.Fatal(err)
	}
	writeSchema2RigCity(t, apiCity, contractCityName,
		"[workspace]\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[api]\nport = 9443\n", "")

	// The supervisor-managed client reaches the harness's real SupervisorMux
	// over the in-process transport.
	supervisorClient := api.NewCityScopedClientWithHTTPClient("http://contract.invalid", contractCityName,
		&http.Client{Transport: h.transport})
	// The standalone client is built from the [api] port and carries no
	// transport seam, so point the default transport at the same mux.
	origTransport := http.DefaultTransport
	http.DefaultTransport = h.transport
	origIdentity, origSup, origGrants := apiRouteControllerIdentityHook, apiRouteSupervisorClientHook, apiRouteSupervisorGrantsHook
	t.Cleanup(func() {
		http.DefaultTransport = origTransport
		apiRouteControllerIdentityHook = origIdentity
		apiRouteSupervisorClientHook = origSup
		apiRouteSupervisorGrantsHook = origGrants
	})

	const (
		toAPI   = "route=api"
		noMatch = "--template=no-such-template"
	)
	cases := []struct {
		name        string
		city        string
		escapeHatch bool
		pid         int
		mode        controllerHostingMode
		supervisor  bool // the supervisor client resolves to supervisorClient; on the escape-hatch rows it proves the route, not a nil client, kept the read off the API
		grants      bool
		args        []string
		rendering   string
		empty       bool
		route       string
	}{
		{name: "supervisor-hosted", city: h.cityPath, supervisor: true, pid: 4242, mode: controllerHostingSupervisor, rendering: "api", route: toAPI},
		{name: "supervisor-hosted-requiring-grants", city: h.cityPath, pid: 4242, mode: controllerHostingSupervisor, grants: true, rendering: "local", route: "route=fallback reason=grant-required"},
		{name: "supervisor-hosted-with-api-port", city: apiCity, supervisor: true, pid: 4242, mode: controllerHostingSupervisor, rendering: "api", route: toAPI},
		{name: "supervisor-hosted-supervisor-unavailable", city: h.cityPath, pid: 4242, mode: controllerHostingSupervisor, rendering: "local", route: "route=fallback reason=controller-down"},
		{name: "standalone-with-api-port", city: apiCity, pid: 4242, mode: controllerHostingStandalone, rendering: "api", route: toAPI},
		{name: "unknown-hosting-with-api-port", city: apiCity, pid: 4242, mode: controllerHostingUnknown, rendering: "api", route: toAPI},
		{name: "no-controller-supervisor-reachable", city: h.cityPath, supervisor: true, rendering: "api", route: toAPI},
		{name: "no-controller-supervisor-unreachable", city: h.cityPath, rendering: "local", route: "route=fallback reason=controller-down"},
		{name: "escape-hatch", city: h.cityPath, escapeHatch: true, supervisor: true, pid: 4242, mode: controllerHostingSupervisor, rendering: "local", route: "route=fallback reason=escape-hatch"},
		{name: "api-empty-list", city: h.cityPath, supervisor: true, args: []string{noMatch}, rendering: "api", empty: true, route: toAPI},
		{name: "local-empty-list", city: h.cityPath, escapeHatch: true, supervisor: true, pid: 4242, mode: controllerHostingSupervisor, args: []string{noMatch}, rendering: "local", empty: true, route: "route=fallback reason=escape-hatch"},
		{name: "supervisor-hosted-closed-history", city: h.cityPath, supervisor: true, pid: 4242, mode: controllerHostingSupervisor, args: []string{"--state=all"}, rendering: "local", route: "route=fallback reason=" + sessionListClosedHistoryReason},
		{name: "standalone-with-api-port-closed-history", city: apiCity, pid: 4242, mode: controllerHostingStandalone, args: []string{"--state=closed"}, rendering: "local", empty: true, route: "route=fallback reason=" + sessionListClosedHistoryReason},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_DEBUG", "1")
			t.Setenv("GC_NO_API", "")
			if tc.escapeHatch {
				t.Setenv("GC_NO_API", "1")
			}
			apiRouteControllerIdentityHook = func(string) controllerIdentityReply {
				if tc.escapeHatch {
					t.Errorf("controller identity probed under the GC_NO_API escape hatch")
				}
				return controllerIdentityReply{PID: tc.pid, HostingMode: tc.mode}
			}
			apiRouteSupervisorClientHook = func(string) *api.Client {
				if tc.supervisor {
					return supervisorClient
				}
				return nil
			}
			apiRouteSupervisorGrantsHook = func() bool { return tc.grants }

			args := append([]string{"--city", tc.city, "session", "list", "--json"}, tc.args...)
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 0 {
				t.Fatalf("run(%v) = %d, stderr:\n%s", args, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.route) {
				t.Fatalf("stderr = %q, want the route audit line to contain %q", stderr.String(), tc.route)
			}
			// Every route prints one single-line JSON document validated against
			// the one schema.
			validateJSONAgainstBuiltinSchema(t, []string{"session", "list"}, stdout.Bytes())
			var payload map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
				t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout.String())
			}
			assertSessionListRendering(t, payload, tc.rendering, tc.empty)
		})
	}
}

// fillAllFields sets every exported field of the struct v points to a non-zero
// value derived from its JSON name, and fails when a field type is not
// supported, so a new field cannot silently stay empty in the sync test.
func fillAllFields(t *testing.T, v any) {
	t.Helper()
	rv := reflect.ValueOf(v).Elem()
	stamp := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
		name := rv.Type().Field(i).Name
		tag, _, _ := strings.Cut(rv.Type().Field(i).Tag.Get("json"), ",")
		switch {
		case f.Kind() == reflect.String:
			f.SetString("x-" + tag)
		case f.Kind() == reflect.Bool:
			f.SetBool(true)
		case f.Type() == reflect.TypeOf((*bool)(nil)):
			yes := true
			f.Set(reflect.ValueOf(&yes))
		case f.Type() == reflect.TypeOf(time.Time{}):
			f.Set(reflect.ValueOf(stamp))
		case f.Type() == reflect.TypeOf([]string(nil)):
			f.Set(reflect.ValueOf([]string{"x-" + tag}))
		case f.Type() == reflect.TypeOf((*time.Time)(nil)):
			f.Set(reflect.ValueOf(&stamp))
		case f.Type() == reflect.TypeOf((*api.SessionProgramStatus)(nil)):
			progress := 40
			f.Set(reflect.ValueOf(&api.SessionProgramStatus{
				State: "blocked", Kind: "permission", App: "x-app", Progress: &progress,
				Since: stamp, QuietSince: &stamp, WorkingChildren: 1,
			}))
		default:
			t.Fatalf("fillAllFields: %s.%s has unsupported type %s", rv.Type(), name, f.Type())
		}
		if f.IsZero() {
			t.Fatalf("fillAllFields: %s.%s is still zero", rv.Type(), name)
		}
	}
}

// jsonKeys marshals v and returns the sorted top-level keys of the object.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// structJSONNames returns the sorted JSON names of every field of the struct
// type of v.
func structJSONNames(v any) []string {
	rt := reflect.TypeOf(v)
	names := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// sessionListSchema loads the built-in result schema, fails when it is not
// one closed shape, and returns the row item schema and the envelope schema.
func sessionListSchema(t *testing.T) (items, envelope map[string]any) {
	t.Helper()
	raw, err := readBuiltinSchema([]string{"session", "list"}, jsonSchemaResultRole)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["oneOf"]; ok {
		t.Fatalf("schema has a oneOf; every route must print one shape")
	}
	if doc["additionalProperties"] != false {
		t.Fatalf("schema envelope must be closed (additionalProperties false)")
	}
	props, _ := doc["properties"].(map[string]any)
	sessions, _ := props["sessions"].(map[string]any)
	items, _ = sessions["items"].(map[string]any)
	if items == nil {
		t.Fatalf("schema has no sessions.items")
	}
	return items, doc
}

func schemaRowProperties(row map[string]any) []string {
	props, _ := row["properties"].(map[string]any)
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// TestSessionListRowStructsMatchSchema keeps the one envelope and row struct
// in step with the one schema shape: a document with every field populated
// validates, the schema declares exactly the fields the structs can emit, a
// minimal row carries every required property, and each route's row builder
// fills only its own fields.
func TestSessionListRowStructsMatchSchema(t *testing.T) {
	items, envelope := sessionListSchema(t)

	t.Run("envelope", func(t *testing.T) {
		want := append(structJSONNames(sessionListJSON{}), "ok")
		sort.Strings(want)
		if got := schemaRowProperties(envelope); !reflect.DeepEqual(got, want) {
			t.Fatalf("schema envelope properties = %v, want %v", got, want)
		}
		var required []string
		for _, r := range envelope["required"].([]any) {
			required = append(required, r.(string))
		}
		sort.Strings(required)
		if want := []string{"filters", "ok", "schema_version", "sessions", "summary"}; !reflect.DeepEqual(required, want) {
			t.Fatalf("schema envelope required = %v, want %v", required, want)
		}
	})

	t.Run("row", func(t *testing.T) {
		var row sessionListJSONRow
		fillAllFields(t, &row)
		row.LastActive = "2026-10-28T12:00:00Z"
		var out, errOut bytes.Buffer
		age := 1.5
		if code := writeSessionListJSON([]sessionListJSONRow{row}, "all", "x", &age, &out, &errOut); code != 0 {
			t.Fatalf("writeSessionListJSON = %d: %s", code, errOut.String())
		}
		validateJSONAgainstBuiltinSchema(t, []string{"session", "list"}, out.Bytes())
		assertRowSchemaSync(t, items, row, sessionListJSONRow{})
		var minimal bytes.Buffer
		if code := writeSessionListJSON([]sessionListJSONRow{{}}, "", "", nil, &minimal, &errOut); code != 0 {
			t.Fatalf("writeSessionListJSON(minimal) = %d", code)
		}
		if !strings.Contains(minimal.String(), `"last_active":""`) {
			t.Fatalf("a zero row must print last_active \"\": %s", minimal.String())
		}
	})

	t.Run("api-row", func(t *testing.T) {
		var view api.SessionListView
		fillAllFields(t, &view)
		rows := sessionListJSONRowsFromAPI([]api.SessionListView{view})
		if len(rows) != 1 {
			t.Fatalf("rows = %v", rows)
		}
		r := rows[0]
		if r.Command != "" || r.SessionKey != "" || r.ResumeFlag != "" || r.ResumeStyle != "" || r.ResumeCommand != "" {
			t.Fatalf("an API row carries a redacted field: %+v", r)
		}
		if r.Running == nil || !*r.Running || r.Reason == "" || r.LastOutput == "" || r.ProgramStatus == nil {
			t.Fatalf("an API row must carry the server's running, reason, last_output and program_status: %+v", r)
		}
		if empty := sessionListJSONRowsFromAPI(nil); empty == nil {
			t.Fatalf("sessionListJSONRowsFromAPI(nil) must be non-nil")
		}
	})

	t.Run("local-row", func(t *testing.T) {
		info := session.Info{
			ID: "gc-1", Template: "worker", State: session.StateActive, Command: "x", SessionKey: "k",
			CreatedAt: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC),
		}
		r := sessionListJSONRows([]session.Info{info})[0]
		if r.Running != nil || r.Reason != "" || r.LastOutput != "" || r.ProgramStatus != nil {
			t.Fatalf("a local row carries an API-only field: %+v", r)
		}
		if r.Command != "x" || r.SessionKey != "k" {
			t.Fatalf("a local row must carry command and session_key: %+v", r)
		}
	})
}

// assertRowSchemaSync checks one row struct against its schema variant: the
// populated row emits every struct field, the schema declares exactly the
// struct's fields, and the zero row emits every required property.
func assertRowSchemaSync(t *testing.T, items map[string]any, full, minimal any) {
	t.Helper()
	if got, want := jsonKeys(t, full), structJSONNames(full); !reflect.DeepEqual(got, want) {
		t.Fatalf("populated row emits %v, struct declares %v", got, want)
	}
	if got, want := schemaRowProperties(items), structJSONNames(full); !reflect.DeepEqual(got, want) {
		t.Fatalf("schema row properties = %v, struct fields = %v", got, want)
	}
	emitted := map[string]bool{}
	for _, k := range jsonKeys(t, minimal) {
		emitted[k] = true
	}
	required, _ := items["required"].([]any)
	if len(required) == 0 {
		t.Fatalf("schema row declares no required properties")
	}
	for _, r := range required {
		if !emitted[r.(string)] {
			t.Errorf("schema requires %q but a minimal %T row does not emit it", r, minimal)
		}
	}
}
