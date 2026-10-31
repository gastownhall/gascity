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
)

// These tests pin the `gc session list --json` contract: two renderings (the
// API envelope and the local result) share one schema
// (schemas/session/list/result.schema.json), and the route a caller takes
// decides which one it gets.

// sessionListRendering classifies a decoded `gc session list --json` payload.
// The API rendering is identified by _cache_age_s, the local one by filters and
// summary; a payload carrying both, or neither, is a contract violation.
func sessionListRendering(t *testing.T, payload map[string]any) string {
	t.Helper()
	_, hasAge := payload["_cache_age_s"]
	_, hasFilters := payload["filters"]
	_, hasSummary := payload["summary"]
	switch {
	case hasAge && !hasFilters && !hasSummary:
		return "api"
	case !hasAge && hasFilters && hasSummary:
		return "local"
	}
	t.Fatalf("payload is not exactly one rendering (_cache_age_s=%v filters=%v summary=%v): %v", hasAge, hasFilters, hasSummary, payload)
	return ""
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
		summary, _ := payload["summary"].(map[string]any)
		if total, _ := summary["total"].(float64); int(total) != len(rows) {
			t.Fatalf("summary.total = %v, want len(sessions) = %d", summary["total"], len(rows))
		}
	}
}

// TestSessionListJSONRouteConformance runs the real `gc session list --json`
// command through each route the ladder can take and checks that the route
// decides the rendering, that each rendering validates against the shared
// schema, and that the route audit line names the route taken. The decision
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
		supervisor  bool // the supervisor client resolves to supervisorClient; on a row that must stay local it proves the route, not a nil client, kept it off the API
		grants      bool
		args        []string
		rendering   string
		empty       bool
		route       string
	}{
		{name: "supervisor-hosted", city: h.cityPath, supervisor: true, pid: 4242, mode: controllerHostingSupervisor, rendering: "local", route: "route=fallback reason=supervisor-hosted"},
		{name: "supervisor-hosted-requiring-grants", city: h.cityPath, supervisor: true, pid: 4242, mode: controllerHostingSupervisor, grants: true, rendering: "local", route: "route=fallback reason=supervisor-hosted"},
		{name: "supervisor-hosted-with-api-port", city: apiCity, supervisor: true, pid: 4242, mode: controllerHostingSupervisor, rendering: "local", empty: true, route: "route=fallback reason=supervisor-hosted"},
		{name: "standalone-with-api-port", city: apiCity, pid: 4242, mode: controllerHostingStandalone, rendering: "api", route: toAPI},
		{name: "unknown-hosting-with-api-port", city: apiCity, pid: 4242, mode: controllerHostingUnknown, rendering: "api", route: toAPI},
		{name: "no-controller-supervisor-reachable", city: h.cityPath, supervisor: true, rendering: "api", route: toAPI},
		{name: "no-controller-supervisor-unreachable", city: h.cityPath, rendering: "local", route: "route=fallback reason=controller-down"},
		{name: "escape-hatch", city: h.cityPath, escapeHatch: true, supervisor: true, pid: 4242, mode: controllerHostingSupervisor, rendering: "local", route: "route=fallback reason=escape-hatch"},
		{name: "api-empty-list", city: h.cityPath, supervisor: true, args: []string{noMatch}, rendering: "api", empty: true, route: toAPI},
		{name: "local-empty-list", city: h.cityPath, supervisor: true, pid: 4242, mode: controllerHostingSupervisor, args: []string{noMatch}, rendering: "local", empty: true, route: "route=fallback reason=supervisor-hosted"},
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
			// The API rendering is indented over several lines, the local one is a
			// single line; both are one JSON document validated against the one
			// schema.
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
		case f.Type() == reflect.TypeOf(time.Time{}):
			f.Set(reflect.ValueOf(stamp))
		case f.Type() == reflect.TypeOf((*time.Time)(nil)):
			f.Set(reflect.ValueOf(&stamp))
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

// sessionListSchemaRowVariants loads the built-in result schema and returns,
// for each oneOf variant, the row item schema keyed by rendering ("api" when
// the variant requires _cache_age_s, "local" otherwise).
func sessionListSchemaRowVariants(t *testing.T) map[string]map[string]any {
	t.Helper()
	raw, err := readBuiltinSchema([]string{"session", "list"}, jsonSchemaResultRole)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	defs, _ := doc["$defs"].(map[string]any)
	branches, _ := doc["oneOf"].([]any)
	if len(branches) != 2 {
		t.Fatalf("schema oneOf has %d branches, want 2 (api and local renderings)", len(branches))
	}
	out := map[string]map[string]any{}
	for _, b := range branches {
		branch, _ := b.(map[string]any)
		if ref, ok := branch["$ref"].(string); ok {
			branch, _ = defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		}
		kind := "local"
		for _, r := range branch["required"].([]any) {
			if r == "_cache_age_s" {
				kind = "api"
			}
		}
		props, _ := branch["properties"].(map[string]any)
		sessions, _ := props["sessions"].(map[string]any)
		items, _ := sessions["items"].(map[string]any)
		if items == nil {
			t.Fatalf("variant %s has no sessions.items schema", kind)
		}
		out[kind] = items
	}
	if out["api"] == nil || out["local"] == nil {
		t.Fatalf("schema variants = %v, want one api and one local", out)
	}
	return out
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

// TestSessionListRowStructsMatchSchema keeps the two row structs and their
// schema variants in step: a row with every field populated validates, the
// schema declares exactly the fields the struct can emit, and a minimal row
// carries every property the schema requires.
func TestSessionListRowStructsMatchSchema(t *testing.T) {
	variants := sessionListSchemaRowVariants(t)

	t.Run("local", func(t *testing.T) {
		var row sessionListJSONRow
		fillAllFields(t, &row)
		var out bytes.Buffer
		result := sessionListJSON{SchemaVersion: "1", Sessions: []sessionListJSONRow{row}, Summary: sessionListSummary{Total: 1}}
		if err := writeCLIJSONLine(&out, result); err != nil {
			t.Fatal(err)
		}
		validateJSONAgainstBuiltinSchema(t, []string{"session", "list"}, out.Bytes())
		assertRowSchemaSync(t, variants["local"], row, sessionListJSONRow{})
	})

	t.Run("api", func(t *testing.T) {
		var row api.SessionView
		fillAllFields(t, &row)
		var out bytes.Buffer
		if code := renderSessionListFromAPI(api.CachedRead[[]api.SessionView]{Body: []api.SessionView{row}, AgeSeconds: 1.5}, true, &out); code != 0 {
			t.Fatalf("renderSessionListFromAPI = %d", code)
		}
		validateJSONAgainstBuiltinSchema(t, []string{"session", "list"}, out.Bytes())
		assertRowSchemaSync(t, variants["api"], row, api.SessionView{})
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
