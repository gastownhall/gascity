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
// publishes, and returns the decoded payload and which of the two renderings
// it is: "api" iff _cache_age_s is present, "local" iff filters and summary
// are, never both.
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
	switch {
	case hasAge && !hasFilters && !hasSummary:
		return payload, "api"
	case !hasAge && hasFilters && hasSummary:
		return payload, "local"
	}
	t.Fatalf("gc session list --json output is not exactly one rendering (_cache_age_s=%v filters=%v summary=%v)\n%s", hasAge, hasFilters, hasSummary, out)
	return nil, ""
}

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
		out, err := c.GC("session", "list", "--json")
		if err != nil {
			t.Fatalf("gc session list --json: %v\n%s", err, out)
		}
		// A supervisor-hosted city lists locally, so plain output is exactly
		// the local rendering.
		payload, rendering := validateSessionListJSON(t, c, out)
		if rendering != "local" {
			t.Fatalf("plain gc session list --json rendering = %s, want local\n%s", rendering, out)
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
	})

	t.Run("List_JSON_LoopbackCityURL", func(t *testing.T) {
		// A remote target is served by the supervisor API, so it prints the
		// API rendering: _cache_age_s, and running on every row.
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
		rows, _ := payload["sessions"].([]any)
		if len(rows) == 0 {
			t.Fatalf("no sessions in the API rendering, want the default mayor session\n%s", out)
		}
		for i, r := range rows {
			row, _ := r.(map[string]any)
			if _, ok := row["running"].(bool); !ok {
				t.Errorf("sessions[%d].running = %#v, want a boolean\n%s", i, row["running"], out)
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
