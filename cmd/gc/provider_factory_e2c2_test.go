package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

const e2c2ProviderConstructionFailure = "constructing session provider: injected provider failure"

func TestE2c2ProviderConstructionFailuresReturnThroughRun(t *testing.T) {
	if cityPath, sessionID, markerPath, ok := e2c2ProviderFailureHelperArgs(os.Args); ok {
		runE2c2ProviderFailureHelper(t, cityPath, sessionID, markerPath)
		return
	}

	cityPath, sessionID := writeE2c2ProviderFailureCity(t)
	markerPath := filepath.Join(t.TempDir(), "returned-through-run")
	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestE2c2ProviderConstructionFailuresReturnThroughRun$",
		"--",
		"e2c2-provider-failure-helper",
		cityPath,
		sessionID,
		markerPath,
	)
	cmd.Dir = cityPath
	cmd.Env = e2c2ProviderFailureChildEnv(
		"GC_ALIAS=worker",
		"GC_AGENT=frontend/worker",
		"GC_BEADS=file",
		"GC_BEADS_SCOPE_ROOT=",
		"GC_BOOTSTRAP=skip",
		"GC_CITY="+cityPath,
		"GC_CITY_PATH="+cityPath,
		"GC_CEILING_DIRECTORIES="+filepath.Dir(cityPath),
		"GC_DOLT=skip",
		"GC_HOME="+filepath.Join(filepath.Dir(cityPath), "gc-home"),
		"GC_SESSION=broken",
		"GC_SESSION_ID="+sessionID,
		"GC_SESSION_NAME=test-city--frontend--worker",
		"GC_TMUX_SESSION=test-city--frontend--worker",
	)
	var processStdout, processStderr bytes.Buffer
	cmd.Stdout = &processStdout
	cmd.Stderr = &processStderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("provider failure helper did not return through run: %v; stdout=%q stderr=%q", err, processStdout.String(), processStderr.String())
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("run-return marker missing: %v", err)
	}
	if got, want := string(marker), "returned\n"; got != want {
		t.Fatalf("run-return marker = %q, want %q", got, want)
	}
}

func e2c2ProviderFailureHelperArgs(args []string) (string, string, string, bool) {
	for index, arg := range args {
		if arg == "--" && index+5 == len(args) && args[index+1] == "e2c2-provider-failure-helper" {
			return args[index+2], args[index+3], args[index+4], true
		}
	}
	return "", "", "", false
}

func e2c2ProviderFailureChildEnv(extra ...string) []string {
	base := sanitizedBaseEnv(extra...)
	env := make([]string, 0, len(base)+1)
	for _, entry := range base {
		if strings.HasPrefix(entry, "OTEL_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "OTEL_SDK_DISABLED=true")
}

func writeE2c2ProviderFailureCity(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("create rig path: %v", err)
	}
	cityTOML := `[workspace]

[beads]
provider = "file"

[[rigs]]
name = "frontend"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	writeCatalogFile(t, cityPath, ".gc/site.toml", fmt.Sprintf(`workspace_name = "test-city"

[[rig]]
name = "frontend"
path = %q
`, rigPath))
	writeBuiltinImportsFixture(t, cityPath, "core")
	writeCatalogFile(t, cityPath, "agents/worker/agent.toml", "dir = \"frontend\"\n")

	store, err := openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	created, err := store.Create(beads.Bead{
		Title:  "mutation provider failure target",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"alias":        "worker",
			"agent_name":   "frontend/worker",
			"template":     "frontend/worker",
			"session_name": "test-city--frontend--worker",
			"state":        "awake",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return cityPath, created.ID
}

func runE2c2ProviderFailureHelper(t *testing.T, cityPath, sessionID, markerPath string) {
	t.Helper()
	defer func() {
		if err := os.WriteFile(markerPath, []byte("returned\n"), 0o600); err != nil {
			t.Errorf("write run-return marker: %v", err)
		}
	}()

	t.Setenv("GC_ALIAS", "worker")
	t.Setenv("GC_AGENT", "frontend/worker")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_BOOTSTRAP", "skip")
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)
	t.Setenv("GC_CEILING_DIRECTORIES", filepath.Dir(cityPath))
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_SESSION", "broken")
	t.Setenv("GC_SESSION_ID", sessionID)
	t.Setenv("GC_SESSION_NAME", "test-city--frontend--worker")
	t.Setenv("GC_TMUX_SESSION", "test-city--frontend--worker")

	oldBuild := buildSessionProviderByName
	providerBuilds := 0
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		providerBuilds++
		return nil, errors.New("injected provider failure")
	}
	defer func() { buildSessionProviderByName = oldBuild }()

	const genericJSONFailure = "{\"schema_version\":\"1\",\"ok\":false,\"error\":{\"code\":\"command_failed\",\"message\":\"command failed; see stderr for diagnostics\",\"exit_code\":1}}\n"
	const handoffFailure = "gc handoff: " + e2c2ProviderConstructionFailure + "\n"
	assertE2c2RunResult(t, []string{"--city", cityPath, "handoff", "context cycle"}, 1, "", handoffFailure)
	assertE2c2ProviderBuilds(t, providerBuilds, 1, "self handoff text")
	assertE2c2RunResult(t, []string{"--city", cityPath, "handoff", "context cycle", "--json"}, 1, genericJSONFailure, handoffFailure)
	assertE2c2ProviderBuilds(t, providerBuilds, 2, "self handoff JSON")
	assertE2c2RunResult(t, []string{"--city", cityPath, "handoff", "context cycle", "--target", "worker"}, 1, "", handoffFailure)
	assertE2c2ProviderBuilds(t, providerBuilds, 3, "remote handoff text")
	assertE2c2RunResult(t, []string{"--city", cityPath, "handoff", "context cycle", "--target", "worker", "--json"}, 1, genericJSONFailure, handoffFailure)
	assertE2c2ProviderBuilds(t, providerBuilds, 4, "remote handoff JSON")
	assertE2c2NoBeadsOfType(t, cityPath, "message", "handoff provider failures")

	target, err := resolveNudgeTarget(sessionID)
	if err != nil {
		t.Fatalf("resolve nudge target: %v", err)
	}
	const pollFailure = "gc nudge poll: " + e2c2ProviderConstructionFailure + "\n"
	assertE2c2RunResult(t, []string{"--city", cityPath, "nudge", "poll", sessionID, "--session", target.sessionName, "--interval", "1ms", "--quiescence", "0s"}, 1, "", pollFailure)
	assertE2c2ProviderBuilds(t, providerBuilds, 5, "nudge poll")
	if _, err := os.Stat(nudgePollerPIDPath(cityPath, target.sessionName, target.pollerKey())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nudge poll provider failure left poller PID behind: %v", err)
	}

	const sessionNudgeFailure = "gc session nudge: " + e2c2ProviderConstructionFailure + "\n"
	assertE2c2RunResult(t, []string{"--city", cityPath, "session", "nudge", sessionID, "check deploy status", "--delivery", "queue"}, 1, "", sessionNudgeFailure)
	assertE2c2ProviderBuilds(t, providerBuilds, 6, "session nudge text")
	assertE2c2RunResult(t, []string{"--city", cityPath, "session", "nudge", sessionID, "check deploy status", "--delivery", "queue", "--json"}, 1, genericJSONFailure, sessionNudgeFailure)
	assertE2c2ProviderBuilds(t, providerBuilds, 7, "session nudge JSON")
	assertE2c2NoQueuedNudges(t, target, "session nudge provider failures")

	if err := sendMailNotify(target, "human"); err == nil || err.Error() != e2c2ProviderConstructionFailure {
		t.Fatalf("sendMailNotify provider failure = %v, want %q", err, e2c2ProviderConstructionFailure)
	}
	assertE2c2ProviderBuilds(t, providerBuilds, 8, "mail notify")
	assertE2c2NoQueuedNudges(t, target, "mail notify provider failure")

	t.Setenv("GC_MAIL", "fake")
	const mailNotifyFailure = "gc mail send: nudge failed: " + e2c2ProviderConstructionFailure + "\n"
	assertE2c2RunResult(t, []string{"--city", cityPath, "mail", "send", "worker", "provider failure notice", "--notify", "--from", "human"}, 0, "Sent message fake-1 to worker\n", mailNotifyFailure)
	assertE2c2ProviderBuilds(t, providerBuilds, 9, "mail notify text command")
	assertE2c2MailNotifyJSONResult(t, []string{"--city", cityPath, "mail", "send", "worker", "provider failure notice", "--notify", "--from", "human", "--json"}, mailNotifyFailure)
	assertE2c2ProviderBuilds(t, providerBuilds, 10, "mail notify JSON command")
	assertE2c2NoQueuedNudges(t, target, "mail notify command provider failures")

	const slingFailureMessage = "gc sling: " + e2c2ProviderConstructionFailure
	assertE2c2RunResult(t, []string{"--city", cityPath, "sling", "frontend/worker", "provider failure task"}, 1, "", slingFailureMessage+"\n")
	assertE2c2ProviderBuilds(t, providerBuilds, 11, "sling text")
	const slingJSONFailure = "{\n  \"schema_version\": \"1\",\n  \"ok\": false,\n  \"error\": {\n    \"code\": \"session_provider_failed\",\n    \"message\": \"" + slingFailureMessage + "\",\n    \"exit_code\": 1\n  }\n}\n"
	const slingJSONDiagnostic = "{\"schema_version\":\"1\",\"level\":\"error\",\"code\":\"session_provider_failed\",\"message\":\"" + slingFailureMessage + "\",\"exit_code\":1}\n"
	assertE2c2RunResult(t, []string{"--city", cityPath, "sling", "frontend/worker", "provider failure task", "--json"}, 1, slingJSONFailure, slingJSONDiagnostic)
	assertE2c2ProviderBuilds(t, providerBuilds, 12, "sling JSON")
	assertE2c2NoBeadsOfType(t, cityPath, "task", "sling provider failures")
}

func assertE2c2RunResult(t *testing.T, args []string, wantCode int, wantStdout, wantStderr string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != wantCode {
		t.Fatalf("run(%v) = %d, want %d; stdout=%q stderr=%q", args, code, wantCode, stdout.String(), stderr.String())
	}
	if got := stdout.String(); got != wantStdout {
		t.Fatalf("run(%v) stdout = %q, want %q", args, got, wantStdout)
	}
	if got := stderr.String(); got != wantStderr {
		t.Fatalf("run(%v) stderr = %q, want %q", args, got, wantStderr)
	}
}

func assertE2c2ProviderBuilds(t *testing.T, got, want int, operation string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s provider builds = %d, want %d cumulative", operation, got, want)
	}
}

func assertE2c2MailNotifyJSONResult(t *testing.T, args []string, wantStderr string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("run(%v) = %d, want 0; stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
	}
	if got := stderr.String(); got != wantStderr {
		t.Fatalf("run(%v) stderr = %q, want %q", args, got, wantStderr)
	}
	var result mailActionResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("run(%v) stdout is not a mail JSON result: %v; stdout=%q", args, err, stdout.String())
	}
	if result.SchemaVersion != "1" || !result.OK || result.Command != "mail.send" || result.Action != "send" || result.ID != "fake-1" || result.Notified {
		t.Fatalf("run(%v) JSON result = %#v, want successful unnotified fake-1 mail send", args, result)
	}
	if result.Count == nil || *result.Count != 1 || result.Message == nil || result.Message.ID != "fake-1" || len(result.Messages) != 1 || result.Messages[0].ID != "fake-1" {
		t.Fatalf("run(%v) JSON message summary = %#v, want one fake-1 message", args, result)
	}
}

func assertE2c2NoBeadsOfType(t *testing.T, cityPath, beadType, operation string) {
	t.Helper()
	store, err := openScopeLocalFileStore(cityPath)
	if err != nil {
		t.Fatalf("open city store after %s: %v", operation, err)
	}
	items, err := store.List(beads.ListQuery{Type: beadType, Status: "open", TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("list %s beads after %s: %v", beadType, operation, err)
	}
	if len(items) != 0 {
		t.Fatalf("%s created %d %s bead(s), want 0", operation, len(items), beadType)
	}
}

func assertE2c2NoQueuedNudges(t *testing.T, target nudgeTarget, operation string) {
	t.Helper()
	pending, inFlight, dead, err := listQueuedNudgesForTarget(target.cityPath, target, time.Now())
	if err != nil {
		t.Fatalf("list queued nudges after %s: %v", operation, err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("%s left queued nudges: pending=%d in_flight=%d dead=%d", operation, len(pending), len(inFlight), len(dead))
	}
}
