//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// gcSeatHookLatencyBudget is the wall time the fastest timed call of every
// seat hook must stay under: the goal is every hook a fresh agent seat runs
// under 1 s (ga-ylpbsy.6), as gcBdLatencyBudget is for gc bd.
const gcSeatHookLatencyBudget = time.Second

// gcSeatHookLatencyRuns is how many timed calls each hook gets, after one
// untimed warm-up call.
const gcSeatHookLatencyRuns = 7

// gcSeatHookRig is the rig the seat's work lives in, so the step reminder reads
// a rig store as a rig-scoped seat's does.
const gcSeatHookRig = "seat-rig"

// gcSeatHookShape is one hook as a provider runs it: argv, the extra hook
// environment, and the hook input on stdin.
type gcSeatHookShape struct {
	name  string
	args  []string
	env   map[string]string
	stdin []byte
	// stdout is a substring each call's stdout must carry, so a call that was
	// fast because it did nothing cannot pass.
	stdout string
	// apiRoute requires each call to report, under GC_DEBUG, that the
	// supervisor answered its step reminder.
	apiRoute bool
}

// TestGcSeatHookLatencyBudget guards the per-call cost of the three hooks every
// agent seat runs: `gc prime --hook` at SessionStart, and `gc nudge drain
// --inject` and `gc mail check --inject` on every prompt, each as the provider
// overlays run them. Real gc and the pinned bd run against a fresh city whose
// supervisor holds its stores open, with one bound rig and one live named
// session on the subprocess provider, from that session's identity. The bd on
// PATH is the pinned bd itself (newIsolatedCommandEnvDirectBD), not the
// harness shim, which costs every bd call a 120 MB binary's start-up that no
// production seat pays.
//
// As TestGcBdLatencyBudget, it catches gross regressions only: a fixed cost
// that pushes a hook's fastest call past the budget, the order of a store open
// with its preflight or several repeated config compositions. It also requires
// the step reminder of every timed prime and drain call to come from the
// supervisor (GET /work/active-step): reading it in process opens the rig
// store, which on a large city is most of each hook.
func TestGcSeatHookLatencyBudget(t *testing.T) {
	requireDoltIntegration(t)

	env := newIsolatedCommandEnvDirectBD(t)
	root, err := os.MkdirTemp("/tmp", "gsh-*")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	cityName := uniqueCityName()
	cityDir := filepath.Join(root, "c")
	rigDir := filepath.Join(root, "r")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	registerIntegrationDoltSQLServerCleanup(t, cityDir)
	registerIntegrationDoltSQLServerCleanup(t, rigDir)
	configPath := filepath.Join(root, "seat-hooks.toml")
	config := fmt.Sprintf(`[workspace]
name = %q

[beads]
provider = "bd"

[session]
provider = "subprocess"

[daemon]
patrol_interval = "100ms"

[[rigs]]
name = %q
path = %q
prefix = "sr"

[[agent]]
name = "seat"
start_command = "sleep 3600"
dir = %q

[[named_session]]
template = "seat"
mode = "always"
dir = %q
`, cityName, gcSeatHookRig, rigDir, gcSeatHookRig, gcSeatHookRig)
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatalf("write city config: %v", err)
	}
	if out, err := runGCDoltWithEnv(env, "", "init", "--skip-provider-readiness", "--file", configPath, cityDir); err != nil {
		t.Fatalf("gc init: %v\noutput: %s", err, out)
	}
	registerCityCommandEnv(cityDir, env)
	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCDoltWithEnv(env, "", "stop", cityDir)                //nolint:errcheck // best-effort cleanup
		runGCDoltWithEnv(env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best-effort cleanup
	})
	template := gcSeatHookRig + "/seat"
	waitForActiveSessionTargets(t, cityDir, []string{template}, 60*time.Second)
	seat := gcSeatHookSession(t, cityDir, template)

	// The seat's identity, as the session lifecycle projects it into the
	// provider's environment.
	seatEnv := env
	for k, v := range map[string]string{
		"GC_CITY":         cityDir,
		"GC_CITY_PATH":    cityDir,
		"GC_SESSION_ID":   seat.ID,
		"GC_SESSION_NAME": seat.SessionName,
		"GC_AGENT":        seat.ID,
		"GC_TEMPLATE":     template,
		"GC_RIG":          gcSeatHookRig,
		"GC_RIG_ROOT":     rigDir,
		"GC_DIR":          rigDir,
		"GC_DEBUG":        "1",
	} {
		seatEnv = replaceEnv(seatEnv, k, v)
	}
	transcript := filepath.Join(root, "transcript.jsonl")
	usage := `{"type":"assistant","message":{"model":"claude-opus-5-5","usage":{"input_tokens":1000,"cache_read_input_tokens":20000,"cache_creation_input_tokens":1000}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(usage), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	prompt, _ := json.Marshal(map[string]string{"hook_event_name": "UserPromptSubmit", "transcript_path": transcript, "prompt": "hello"})
	hookRun := []string{"hook", "run", "--timeout", "15s", "--timeout-exit-code", "0", "--"}
	shapes := []gcSeatHookShape{
		{
			name: "prime --hook (SessionStart)",
			args: []string{"prime", "--hook", "--hook-format", "codex"},
			env: map[string]string{
				"GC_MANAGED_SESSION_HOOK":     "1",
				"GC_HOOK_EVENT_NAME":          "SessionStart",
				"GC_STARTUP_PROMPT_DELIVERED": "1",
			},
			stdin:    []byte(`{"hook_event_name":"SessionStart","source":"startup"}` + "\n"),
			stdout:   seat.ID,
			apiRoute: true,
		},
		// The transcript's usage sample is what a seat's later prompts carry;
		// it takes the drain's full path, which resolves the target.
		{name: "hook run -- nudge drain --inject (UserPromptSubmit)", args: append(append([]string(nil), hookRun...), "nudge", "drain", "--inject"), stdin: prompt, stdout: "Current time:", apiRoute: true},
		{name: "hook run -- mail check --inject (UserPromptSubmit)", args: append(append([]string(nil), hookRun...), "mail", "check", "--inject"), stdin: prompt},
	}

	samples := make([][]time.Duration, len(shapes))
	for round := 0; round <= gcSeatHookLatencyRuns; round++ {
		for i, shape := range shapes {
			elapsed := gcSeatHookTimedCall(t, rigDir, seatEnv, shape)
			if round > 0 {
				samples[i] = append(samples[i], elapsed)
			}
		}
	}

	fmt.Fprint(os.Stderr, gcSeatHookLatencyTable(shapes, samples)) //nolint:errcheck // best-effort report
	for i, shape := range shapes {
		sorted := append([]time.Duration(nil), samples[i]...)
		sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })
		if sorted[0] >= gcSeatHookLatencyBudget {
			t.Errorf("gc %s: fastest of %d calls took %s, budget %s", shape.name, len(sorted), sorted[0].Round(time.Millisecond), gcSeatHookLatencyBudget)
		}
	}
}

// gcSeatHookSessionRow is the part of `gc session list --json` the test needs.
type gcSeatHookSessionRow struct {
	ID          string `json:"id"`
	Template    string `json:"template"`
	SessionName string `json:"session_name"`
	Closed      bool   `json:"closed"`
}

// gcSeatHookSession returns the live session of template.
func gcSeatHookSession(t *testing.T, cityDir, template string) gcSeatHookSessionRow {
	t.Helper()
	out, err := gc(cityDir, "session", "list", "--json", "--template", template)
	if err != nil {
		t.Fatalf("gc session list: %v\n%s", err, out)
	}
	var list struct {
		Sessions []gcSeatHookSessionRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &list); err != nil {
		t.Fatalf("decode session list: %v\n%s", err, out)
	}
	for _, s := range list.Sessions {
		if !s.Closed && s.Template == template && s.ID != "" && s.SessionName != "" {
			return s
		}
	}
	t.Fatalf("no live %s session in:\n%s", template, out)
	return gcSeatHookSessionRow{}
}

// gcSeatHookTimedCall runs one hook as its provider does and returns its wall
// time, failing on a non-zero exit, on stdout that lacks the shape's marker,
// or, for a shape that requires it, on a step reminder the supervisor did not
// answer.
func gcSeatHookTimedCall(t *testing.T, dir string, env []string, shape gcSeatHookShape) time.Duration {
	t.Helper()
	for k, v := range shape.env {
		env = replaceEnv(env, k, v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), integrationGCCommandTimeout)
	defer cancel()
	cmd := buildCommand(ctx, dir, env, gcBinary, shape.args...)
	cmd.Stdin = bytes.NewReader(shape.stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("gc %s: %v\nstdout: %s\nstderr: %s", shape.name, err, stdout.String(), stderr.String())
	}
	if shape.stdout != "" && !strings.Contains(stdout.String(), shape.stdout) {
		t.Fatalf("gc %s: stdout does not carry %q:\n%s\nstderr: %s", shape.name, shape.stdout, stdout.String(), stderr.String())
	}
	if shape.apiRoute && !strings.Contains(stderr.String(), "cmd=step-reminder route=api") {
		t.Fatalf("gc %s: the supervisor did not answer the step reminder:\n%s", shape.name, stderr.String())
	}
	return elapsed
}

func gcSeatHookLatencyTable(shapes []gcSeatHookShape, samples [][]time.Duration) string {
	var b strings.Builder
	b.WriteString("\nseat hook latency (fastest / median / slowest of the timed calls):\n")
	for i, shape := range shapes {
		sorted := append([]time.Duration(nil), samples[i]...)
		sort.Slice(sorted, func(a, c int) bool { return sorted[a] < sorted[c] })
		fmt.Fprintf(&b, "  %-52s %6dms %6dms %6dms\n", shape.name, sorted[0].Milliseconds(), sorted[len(sorted)/2].Milliseconds(), sorted[len(sorted)-1].Milliseconds())
	}
	return b.String()
}
