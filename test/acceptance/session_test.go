//go:build acceptance_a

// Session command acceptance tests.
//
// These exercise gc session subcommands as a black box. Session
// management is fundamental to the agent lifecycle. Most mutating
// operations need a running controller, so Tier A tests focus on
// list, prune, and error paths for each subcommand.
package acceptance_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// validateSessionListJSON validates out, the stdout of a
// `gc session list --json` run, against the result schema the binary itself
// publishes, and returns the decoded payload and which route served it. Every
// route prints filters and summary; an API-served read adds _cache_age_s
// ("api"), a local read omits it ("local").
func validateSessionListJSON(t *testing.T, c *helpers.City, out string) (map[string]any, string) {
	t.Helper()
	schemaOut, err := c.GC("session", "list", "--json-schema=result")
	if err != nil {
		t.Fatalf("gc session list --json-schema=result: %v\n%s", err, schemaOut)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(schemaOut)))
	if err != nil {
		t.Fatalf("parse published schema: %v\n%s", err, schemaOut)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(out)))
	if err != nil {
		t.Fatalf("gc session list --json output is not JSON: %v\n%s", err, out)
	}
	compiler := jsonschema.NewCompiler()
	const schemaURL = "gc/session/list/result.schema.json"
	if err := compiler.AddResource(schemaURL, schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		t.Fatalf("compile published schema: %v", err)
	}
	if err := compiled.Validate(instance); err != nil {
		t.Fatalf("gc session list --json output does not validate against its schema: %v\n%s", err, out)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode gc session list --json output: %v\n%s", err, out)
	}
	_, hasAge := payload["_cache_age_s"]
	_, hasFilters := payload["filters"]
	_, hasSummary := payload["summary"]
	if !hasFilters || !hasSummary {
		t.Fatalf("gc session list --json output lacks filters or summary (filters=%v summary=%v)\n%s", hasFilters, hasSummary, out)
	}
	if hasAge {
		return payload, "api"
	}
	return payload, "local"
}

// sessionListPayloadRows returns the rows of a decoded session list payload.
func sessionListPayloadRows(t *testing.T, payload map[string]any, out string) []map[string]any {
	t.Helper()
	raw, ok := payload["sessions"].([]any)
	if !ok {
		t.Fatalf("sessions = %#v, want an array\n%s", payload["sessions"], out)
	}
	rows := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		row, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("session row %#v is not an object\n%s", r, out)
		}
		rows = append(rows, row)
	}
	return rows
}

// sessionListAPIOnlyKeys and sessionListRedactedKeys are the keys only one
// route prints: the API adds its own observations, and redacts the local-only
// command, session key and resume fields.
var (
	sessionListAPIOnlyKeys  = []string{"running", "reason", "last_output", "program_status"}
	sessionListRedactedKeys = []string{"command", "session_key", "resume_flag", "resume_style", "resume_command"}
)

// supervisorLoopbackURL returns the base URL of the acceptance supervisor,
// read from the supervisor.toml the test environment wrote.
func supervisorLoopbackURL(t *testing.T) string {
	t.Helper()
	var cfg struct {
		Supervisor struct {
			Port int    `toml:"port"`
			Bind string `toml:"bind"`
		} `toml:"supervisor"`
	}
	path := filepath.Join(testEnv.Get("GC_HOME"), "supervisor.toml")
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if cfg.Supervisor.Port == 0 {
		t.Fatalf("%s has no supervisor port", path)
	}
	bind := cfg.Supervisor.Bind
	if bind == "" {
		bind = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%s", bind, strconv.Itoa(cfg.Supervisor.Port))
}

func TestSessionErrors(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitNoStart("claude")

	t.Run("NoSubcommand", func(t *testing.T) {
		out, err := c.GC("session")
		if err == nil {
			t.Fatal("expected error for bare 'gc session', got success")
		}
		if !strings.Contains(out, "missing subcommand") {
			t.Errorf("expected 'missing subcommand' message, got:\n%s", out)
		}
	})

	t.Run("UnknownSubcommand", func(t *testing.T) {
		out, err := c.GC("session", "explode")
		if err == nil {
			t.Fatal("expected error for unknown subcommand, got success")
		}
		if !strings.Contains(out, "unknown subcommand") {
			t.Errorf("expected 'unknown subcommand' message, got:\n%s", out)
		}
	})

	t.Run("New_NonexistentTemplate", func(t *testing.T) {
		_, err := c.GC("session", "new", "nonexistent-template-xyz")
		if err == nil {
			t.Fatal("expected error for nonexistent template, got success")
		}
	})

	t.Run("New_MissingTemplate", func(t *testing.T) {
		// cobra.ExactArgs(1) or custom validation handles this.
		_, err := c.GC("session", "new")
		if err == nil {
			t.Fatal("expected error for missing template, got success")
		}
	})

	t.Run("Close_Nonexistent", func(t *testing.T) {
		_, err := c.GC("session", "close", "nonexistent-session-xyz")
		if err == nil {
			t.Fatal("expected error for nonexistent session, got success")
		}
	})

	t.Run("Kill_Nonexistent", func(t *testing.T) {
		_, err := c.GC("session", "kill", "nonexistent-session-xyz")
		if err == nil {
			t.Fatal("expected error for nonexistent session, got success")
		}
	})

	t.Run("Wake_Nonexistent", func(t *testing.T) {
		_, err := c.GC("session", "wake", "nonexistent-session-xyz")
		if err == nil {
			t.Fatal("expected error for nonexistent session, got success")
		}
	})

	t.Run("Peek_Nonexistent", func(t *testing.T) {
		_, err := c.GC("session", "peek", "nonexistent-session-xyz")
		if err == nil {
			t.Fatal("expected error for nonexistent session, got success")
		}
	})

	t.Run("Rename_Nonexistent", func(t *testing.T) {
		_, err := c.GC("session", "rename", "nonexistent-session", "new-title")
		if err == nil {
			t.Fatal("expected error for nonexistent session, got success")
		}
	})
}

func TestSessionDefaultNamedSession(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	// The default named session (mayor) is created asynchronously by the
	// reconciler. Poll until it appears before running subtests to avoid a
	// race on slow or CPU-saturated runners.
	if !c.WaitForCondition(func() bool {
		out, err := c.GC("session", "list")
		return err == nil && strings.Contains(out, "mayor")
	}, 30*time.Second) {
		out, _ := c.GC("session", "list")
		t.Fatalf("timed out waiting for default named session (mayor) to appear:\n%s", out)
	}

	t.Run("List_DefaultNamedSession", func(t *testing.T) {
		out, err := c.GC("session", "list")
		if err != nil {
			t.Fatalf("gc session list: %v\n%s", err, out)
		}
		if strings.Contains(out, "No sessions found") {
			t.Errorf("expected default named session on fresh city, got:\n%s", out)
		}
		if !strings.Contains(out, "mayor") {
			t.Errorf("expected default named session in list, got:\n%s", out)
		}
		if !strings.Contains(out, string(session.StateCreating)) &&
			!strings.Contains(out, string(session.StateActive)) &&
			!strings.Contains(out, string(session.StateAwake)) &&
			!strings.Contains(out, string(session.StateAsleep)) {
			t.Errorf("expected materialized default named session state in list, got:\n%s", out)
		}
	})

	t.Run("List_JSON_DefaultNamedSession", func(t *testing.T) {
		// A supervisor-hosted city lists through the supervisor API, so plain
		// output is the API-served document. Poll for it: while the
		// supervisor's session cache primes, the read falls back locally.
		debugEnv := c.Env.Clone().With("GC_DEBUG", "1")
		var out, stderr string
		var payload map[string]any
		served := c.WaitForCondition(func() bool {
			var err error
			out, stderr, err = helpers.RunGCStreams(debugEnv, c.Dir, "session", "list", "--json")
			if err != nil {
				return false
			}
			var rendering string
			payload, rendering = validateSessionListJSON(t, c, out)
			return rendering == "api"
		}, 30*time.Second)
		if !served {
			t.Fatalf("plain gc session list --json never read through the supervisor API\nstdout:\n%s\nstderr:\n%s", out, stderr)
		}
		var got struct {
			Sessions []struct {
				Template string        `json:"template"`
				State    session.State `json:"state"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("gc session list --json output is not a session list envelope: %v\n%s", err, out)
		}
		if payload["schema_version"] != "1" || payload["ok"] != true {
			t.Errorf("schema_version/ok = %v/%v, want \"1\"/true\n%s", payload["schema_version"], payload["ok"], out)
		}
		var mayorSeen bool
		for _, sess := range got.Sessions {
			if sess.Template == "mayor" {
				mayorSeen = true
			}
		}
		if !mayorSeen {
			t.Fatalf("default mayor named session missing\n%s", out)
		}
		for _, sess := range got.Sessions {
			switch sess.State {
			case session.StateCreating, session.StateActive, session.StateAwake, session.StateAsleep:
			default:
				t.Errorf("session %q state = %q, want materialized lifecycle state\n%s", sess.Template, sess.State, out)
			}
		}
		for i, row := range sessionListPayloadRows(t, payload, out) {
			if _, ok := row["running"].(bool); !ok {
				t.Errorf("sessions[%d].running = %#v, want a boolean\n%s", i, row["running"], out)
			}
			if _, ok := row["closed"].(bool); !ok {
				t.Errorf("sessions[%d].closed = %#v, want a boolean\n%s", i, row["closed"], out)
			}
		}
	})

	t.Run("List_JSON_LoopbackCityURL", func(t *testing.T) {
		// A remote target is served by the supervisor API: the one document
		// plus _cache_age_s, and running on every row.
		out, err := c.GC("--city-url", supervisorLoopbackURL(t), "--city-name", filepath.Base(c.Dir),
			"session", "list", "--state", "open", "--json")
		if err != nil {
			t.Fatalf("gc --city-url session list --json: %v\n%s", err, out)
		}
		payload, rendering := validateSessionListJSON(t, c, out)
		if rendering != "api" {
			t.Fatalf("--city-url gc session list --json rendering = %s, want api\n%s", rendering, out)
		}
		if _, ok := payload["_cache_age_s"].(float64); !ok {
			t.Fatalf("_cache_age_s = %#v, want a number\n%s", payload["_cache_age_s"], out)
		}
		filters, _ := payload["filters"].(map[string]any)
		if filters["state"] != "open" {
			t.Errorf("filters = %#v, want state open\n%s", payload["filters"], out)
		}
		rows := sessionListPayloadRows(t, payload, out)
		if len(rows) == 0 {
			t.Fatalf("no sessions in the API-served document, want the default mayor session\n%s", out)
		}
		summary, _ := payload["summary"].(map[string]any)
		if total, _ := summary["total"].(float64); int(total) != len(rows) {
			t.Errorf("summary.total = %#v, want %d\n%s", summary["total"], len(rows), out)
		}
		for i, row := range rows {
			if _, ok := row["running"].(bool); !ok {
				t.Errorf("sessions[%d].running = %#v, want a boolean\n%s", i, row["running"], out)
			}
			if _, ok := row["closed"].(bool); !ok {
				t.Errorf("sessions[%d].closed = %#v, want a boolean\n%s", i, row["closed"], out)
			}
			if _, ok := row["name"].(string); !ok {
				t.Errorf("sessions[%d].name = %#v, want a string\n%s", i, row["name"], out)
			}
			if _, ok := row["last_active"].(string); !ok {
				t.Errorf("sessions[%d].last_active = %#v, want a string\n%s", i, row["last_active"], out)
			}
		}
	})

	t.Run("List_JSON_SameShapeOnEveryRoute", func(t *testing.T) {
		// The plain (supervisor API), GC_NO_API (local) and --city-url (remote)
		// reads print one shape: equal row key sets once the API-only and
		// redacted keys are set aside, and equal identity values per session.
		plain, _, err := helpers.RunGCStreams(c.Env, c.Dir, "session", "list", "--json")
		if err != nil {
			t.Fatalf("gc session list --json: %v\n%s", err, plain)
		}
		local, _, err := helpers.RunGCStreams(c.Env.Clone().With("GC_NO_API", "1"), c.Dir, "session", "list", "--json")
		if err != nil {
			t.Fatalf("GC_NO_API=1 gc session list --json: %v\n%s", err, local)
		}
		remote, _, err := helpers.RunGCStreams(c.Env, c.Dir, "--city-url", supervisorLoopbackURL(t), "--city-name", filepath.Base(c.Dir),
			"session", "list", "--json")
		if err != nil {
			t.Fatalf("gc --city-url session list --json: %v\n%s", err, remote)
		}
		localPayload, localRendering := validateSessionListJSON(t, c, local)
		if localRendering != "local" {
			t.Fatalf("GC_NO_API=1 rendering = %s, want local\n%s", localRendering, local)
		}
		localRows := sessionListRowsByID(t, localPayload, local)
		for _, row := range localRows {
			if _, ok := row["running"]; ok {
				t.Errorf("local row %v carries running\n%s", row["id"], local)
			}
		}
		for name, out := range map[string]string{"plain": plain, "remote": remote} {
			payload, _ := validateSessionListJSON(t, c, out)
			for id, row := range sessionListRowsByID(t, payload, out) {
				localRow, ok := localRows[id]
				if !ok {
					continue // a session that appeared between the reads
				}
				if got, want := sessionListComparableKeys(row), sessionListComparableKeys(localRow); got != want {
					t.Errorf("%s row %s keys = %s, local keys = %s", name, id, got, want)
				}
				for _, key := range []string{"id", "template", "name", "session_name", "alias", "agent_name", "rig", "closed", "created_at", "title", "work_dir"} {
					if fmt.Sprint(row[key]) != fmt.Sprint(localRow[key]) {
						t.Errorf("%s row %s %s = %#v, local = %#v", name, id, key, row[key], localRow[key])
					}
				}
			}
		}
	})

	t.Run("Config_JSON_NoDefaultControlDispatcherNamedSession", func(t *testing.T) {
		out, err := c.GC("config", "show", "--json")
		if err != nil {
			t.Fatalf("gc config show --json: %v\n%s", err, out)
		}
		var got struct {
			Config struct {
				NamedSessions []struct {
					Name     string
					Template string
					Mode     string
				}
			}
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("gc config show --json output is not a config envelope: %v\n%s", err, out)
		}
		// The control dispatcher serves via demand-scaling of the core-pack
		// agent template (openControlDispatcherDemand), so gc init no longer
		// injects a redundant on_demand named session for it -- that only
		// produced a confusing "backing template not found ... disabled"
		// warning on upgraded cities. Assert it is NOT auto-created.
		for _, sess := range got.Config.NamedSessions {
			if sess.Name == config.ControlDispatcherAgentName {
				t.Fatalf("gc init should not create a control-dispatcher named session (redundant with demand-scaling); found %+v\n%s", sess, out)
			}
		}
	})

	t.Run("Prune_NoClosedSessions", func(t *testing.T) {
		out, err := c.GC("session", "prune")
		if err != nil {
			t.Fatalf("gc session prune: %v\n%s", err, out)
		}
		if !strings.Contains(out, "No sessions to prune") {
			t.Errorf("expected 'No sessions to prune' with only default named session, got:\n%s", out)
		}
	})
}

// sessionListRowsByID indexes a session list payload's rows by id.
func sessionListRowsByID(t *testing.T, payload map[string]any, out string) map[string]map[string]any {
	t.Helper()
	byID := map[string]map[string]any{}
	for _, row := range sessionListPayloadRows(t, payload, out) {
		id, _ := row["id"].(string)
		byID[id] = row
	}
	return byID
}

// sessionListComparableKeys returns a row's sorted key set minus the keys
// only one route prints and the observer-dependent provider/transport, which
// the route-equivalence contract does not cover.
func sessionListComparableKeys(row map[string]any) string {
	skip := map[string]bool{"provider": true, "transport": true}
	for _, k := range sessionListAPIOnlyKeys {
		skip[k] = true
	}
	for _, k := range sessionListRedactedKeys {
		skip[k] = true
	}
	var keys []string
	for k := range row {
		if !skip[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
