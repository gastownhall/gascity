package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAgentScriptBDClaimUsesSessionActor(t *testing.T) {
	t.Setenv("GC_SESSION_NAME", "demo/worker-1")

	var calls [][]string
	exec := agentScriptExecutor{
		runCommand: func(name string, args ...string) error {
			calls = append(calls, append([]string{name}, args...))
			return nil
		},
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
	}
	ctx := agentScriptContext{
		bead: agentScriptBead{ID: "ga-123"},
	}

	exitCode, err := exec.runAction(agentScriptAction{"bd_claim": "{bead.id}"}, ctx)
	if err != nil {
		t.Fatalf("runAction: %v", err)
	}
	if exitCode != nil {
		t.Fatalf("exitCode = %v, want nil", *exitCode)
	}
	want := []string{"bd", "update", "ga-123", "--claim", "--actor", "demo/worker-1"}
	if len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Fatalf("calls = %#v, want %#v", calls, [][]string{want})
	}
}

func TestAgentScriptShellEnvIncludesBeadMetadata(t *testing.T) {
	ctx := agentScriptContext{
		bead: agentScriptBead{
			ID:    "ga-123",
			Title: "demo task",
			Metadata: map[string]string{
				"branch":        "polecat/ga-123",
				"review.status": "ready",
			},
		},
		rig:   "demo",
		alias: "demo/worker",
	}

	env, err := agentScriptShellEnv([]string{"PATH=/bin", "GIT_TERMINAL_PROMPT=1"}, ctx)
	if err != nil {
		t.Fatalf("agentScriptShellEnv: %v", err)
	}
	for _, want := range []string{
		"GIT_TERMINAL_PROMPT=0",
		"GC_SCRIPT_BEAD_ID=ga-123",
		"GC_SCRIPT_BEAD_TITLE=demo task",
		"GC_SCRIPT_BEAD_METADATA_BRANCH=polecat/ga-123",
		"GC_SCRIPT_BEAD_METADATA_REVIEW_STATUS=ready",
		"GC_SCRIPT_RIG=demo",
		"GC_SCRIPT_ALIAS=demo/worker",
	} {
		if !slices.Contains(env, want) {
			t.Fatalf("env missing %q in %#v", want, env)
		}
	}
	if slices.Contains(env, "GIT_TERMINAL_PROMPT=1") {
		t.Fatalf("env preserved interactive git prompt setting: %#v", env)
	}
}

func TestAgentScriptShellEnvStripsInheritedRunnerValues(t *testing.T) {
	ctx := agentScriptContext{
		bead: agentScriptBead{
			ID:    "ga-123",
			Title: "demo task",
		},
		rig:   "demo",
		alias: "demo/worker",
	}

	env, err := agentScriptShellEnv([]string{
		"PATH=/bin",
		"GC_SCRIPT_BEAD_ID=stale",
		"GC_SCRIPT_BEAD_TITLE=stale",
		"GC_SCRIPT_BEAD_METADATA_BRANCH=stale",
		"GC_SCRIPT_RIG=stale",
		"GC_SCRIPT_ALIAS=stale",
	}, ctx)
	if err != nil {
		t.Fatalf("agentScriptShellEnv: %v", err)
	}
	for _, want := range []string{
		"PATH=/bin",
		"GC_SCRIPT_BEAD_ID=ga-123",
		"GC_SCRIPT_BEAD_TITLE=demo task",
		"GC_SCRIPT_RIG=demo",
		"GC_SCRIPT_ALIAS=demo/worker",
	} {
		if !slices.Contains(env, want) {
			t.Fatalf("env missing %q in %#v", want, env)
		}
	}
	for _, entry := range env {
		if strings.Contains(entry, "stale") {
			t.Fatalf("env preserved inherited runner value %q in %#v", entry, env)
		}
		if strings.HasPrefix(entry, "GC_SCRIPT_BEAD_METADATA_BRANCH=") {
			t.Fatalf("env preserved stale branch metadata %q in %#v", entry, env)
		}
	}
}

func TestAgentScriptShellEnvRejectsMetadataEnvCollisions(t *testing.T) {
	ctx := agentScriptContext{
		bead: agentScriptBead{
			ID:    "ga-123",
			Title: "demo task",
			Metadata: map[string]string{
				"pr.url": "https://example.invalid/pr/1",
				"pr_url": "https://example.invalid/pr/2",
			},
		},
	}

	_, err := agentScriptShellEnv(nil, ctx)
	if err == nil {
		t.Fatal("agentScriptShellEnv accepted colliding metadata env keys")
	}
	if !strings.Contains(err.Error(), "GC_SCRIPT_BEAD_METADATA_PR_URL") {
		t.Fatalf("error = %q, want colliding env key", err)
	}
}

func TestAgentScriptValidatesHookBeadBeforeActions(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "script.yaml")
	if err := os.WriteFile(scriptPath, []byte(`
schema_version: v1
turns:
  - when:
      hook: has_work
    do:
      - log: "work {bead.id}"
`), 0o644); err != nil {
		t.Fatalf("writing script: %v", err)
	}

	tests := []struct {
		name string
		bead agentScriptBead
		want string
	}{
		{
			name: "unsafe id",
			bead: agentScriptBead{ID: "../ga-123"},
			want: "characters unsafe",
		},
		{
			name: "metadata env collision",
			bead: agentScriptBead{
				ID: "ga-123",
				Metadata: map[string]string{
					"pr.url": "https://example.invalid/one",
					"pr_url": "https://example.invalid/two",
				},
			},
			want: "GC_SCRIPT_BEAD_METADATA_PR_URL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runAgentScriptWithRuntime(scriptPath, &stdout, &stderr,
				agentScriptExecutor{stdout: &stdout, stderr: &stderr},
				func(io.Writer) (agentScriptBead, bool, error) {
					return tt.bead, true, nil
				})
			if code == 0 {
				t.Fatalf("runAgentScriptWithRuntime exit = 0, want failure")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want no action output", stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.want)
			}
		})
	}
}

func TestAgentScriptBeadUnmarshalJSONKeepsNonStringMetadata(t *testing.T) {
	var bead agentScriptBead
	if err := json.Unmarshal([]byte(`{"id":"ga-123","metadata":{"count":42,"flag":true,"label":"x"}}`), &bead); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	want := map[string]string{
		"count": "42",
		"flag":  "true",
		"label": "x",
	}
	if !reflect.DeepEqual(bead.Metadata, want) {
		t.Fatalf("metadata = %#v, want %#v", bead.Metadata, want)
	}
}

func TestAgentScriptRejectsUnknownSchemaVersion(t *testing.T) {
	err := validateAgentScript(agentScriptDocument{SchemaVersion: "v2"})
	if err == nil {
		t.Fatal("validateAgentScript accepted unknown schema version")
	}
}

func TestAgentScriptRejectsSetupBeadPlaceholders(t *testing.T) {
	err := validateAgentScript(agentScriptDocument{
		SchemaVersion: "v1",
		Setup: []agentScriptAction{
			{"log": "setup for {bead.id}"},
		},
		Turns: []agentScriptTurn{
			{When: map[string]any{"hook": "empty"}},
		},
	})
	if err == nil {
		t.Fatal("validateAgentScript accepted setup bead placeholder")
	}
	if !strings.Contains(err.Error(), "cannot reference bead context placeholder {bead.id}") {
		t.Fatalf("error = %q, want setup bead placeholder rejection", err)
	}
}

func TestAgentScriptRejectsSetupShellAddressPlaceholders(t *testing.T) {
	for _, command := range []string{"echo {rig}", "echo {alias}"} {
		t.Run(command, func(t *testing.T) {
			err := validateAgentScript(agentScriptDocument{
				SchemaVersion: "v1",
				Setup: []agentScriptAction{
					{"shell": command},
				},
				Turns: []agentScriptTurn{
					{When: map[string]any{"hook": "empty"}},
				},
			})
			if err == nil {
				t.Fatal("validateAgentScript accepted setup shell address placeholder")
			}
			if !strings.Contains(err.Error(), "setup action 0 shell cannot interpolate") {
				t.Fatalf("error = %q, want setup shell placeholder rejection", err)
			}
		})
	}
}

func TestAgentScriptRejectsSetupWorkActions(t *testing.T) {
	for _, tt := range []struct {
		name   string
		action agentScriptAction
	}{
		{name: "bd_claim", action: agentScriptAction{"bd_claim": "ga-123"}},
		{name: "bd_update", action: agentScriptAction{"bd_update": map[string]any{"id": "ga-123", "status": "closed"}}},
		{name: "mail_send", action: agentScriptAction{"mail_send": map[string]any{"to": "demo/worker", "subject": "setup"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAgentScript(agentScriptDocument{
				SchemaVersion: "v1",
				Setup: []agentScriptAction{
					tt.action,
				},
				Turns: []agentScriptTurn{
					{When: map[string]any{"hook": "empty"}},
				},
			})
			if err == nil {
				t.Fatal("validateAgentScript accepted setup work action")
			}
			if !strings.Contains(err.Error(), "setup action 0 cannot use") {
				t.Fatalf("error = %q, want setup work action rejection", err)
			}
		})
	}
}

func TestAgentScriptRejectsInvalidTurnWhen(t *testing.T) {
	tests := []struct {
		name string
		when map[string]any
	}{
		{name: "extra key", when: map[string]any{"hook": "empty", "priority": "P0"}},
		{name: "non string hook", when: map[string]any{"hook": 1}},
		{name: "unknown hook", when: map[string]any{"hook": "later"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAgentScript(agentScriptDocument{
				SchemaVersion: "v1",
				Turns: []agentScriptTurn{
					{When: tt.when},
				},
			})
			if err == nil {
				t.Fatal("validateAgentScript accepted invalid when")
			}
		})
	}
}

func TestFindAgentScriptTurnNoMatchAndFirstMatch(t *testing.T) {
	script := agentScriptDocument{
		Turns: []agentScriptTurn{
			{When: map[string]any{"hook": "has_work"}, Do: []agentScriptAction{{"log": "first"}}},
			{When: map[string]any{"hook": "has_work"}, Do: []agentScriptAction{{"log": "second"}}},
		},
	}
	turn, ok := findAgentScriptTurn(script, "has_work")
	if !ok {
		t.Fatal("findAgentScriptTurn did not find has_work")
	}
	if got := turn.Do[0]["log"]; got != "first" {
		t.Fatalf("first matching turn log = %v, want first", got)
	}
	if _, ok := findAgentScriptTurn(script, "empty"); ok {
		t.Fatal("findAgentScriptTurn found a non-existent empty turn")
	}
}

func TestAgentScriptRunActionRejectsShellPlaceholders(t *testing.T) {
	exec := agentScriptExecutor{
		runShell: func(_ string, _ []string) error {
			t.Fatal("runShell called for unsafe shell action")
			return nil
		},
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
	}
	ctx := agentScriptContext{
		bead: agentScriptBead{
			ID:    "ga-123",
			Title: "demo",
			Metadata: map[string]string{
				"branch": "polecat/ga-123",
			},
		},
		rig:   "demo",
		alias: "demo/worker",
	}

	for _, command := range []string{
		"echo {bead.id}",
		"echo {bead.title}",
		"echo {bead.metadata.branch}",
		"echo {rig}",
		"echo {alias}",
	} {
		t.Run(command, func(t *testing.T) {
			_, err := exec.runAction(agentScriptAction{"shell": command}, ctx)
			if err == nil {
				t.Fatal("runAction accepted unsafe shell placeholder")
			}
			if !strings.Contains(err.Error(), "shell actions must read bead, rig, and alias data from GC_SCRIPT_") {
				t.Fatalf("error = %q, want GC_SCRIPT_ guidance", err)
			}
		})
	}
}

func TestSubstituteAgentScriptStringIsSinglePassDeterministic(t *testing.T) {
	ctx := agentScriptContext{
		bead: agentScriptBead{
			ID:    "ga-123",
			Title: "demo",
			Metadata: map[string]string{
				"a": "x {bead.metadata.b} y",
				"b": "B",
			},
		},
		rig:   "demo",
		alias: "demo/worker",
	}

	got, err := substituteAgentScriptString("{bead.metadata.a}", ctx)
	if err != nil {
		t.Fatalf("substituteAgentScriptString: %v", err)
	}
	if got != "x {bead.metadata.b} y" {
		t.Fatalf("substituteAgentScriptString = %q, want single-pass metadata value", got)
	}
}

func TestSubstituteAgentScriptStringRejectsEmptyRequiredAddressPlaceholders(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
		ctx   agentScriptContext
		want  string
	}{
		{
			name:  "rig",
			value: "{rig}/lifecycle.refinery",
			want:  "{rig}",
		},
		{
			name:  "alias",
			value: "claimed by {alias}",
			ctx:   agentScriptContext{rig: "demo"},
			want:  "{alias}",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := substituteAgentScriptString(tt.value, tt.ctx)
			if err == nil {
				t.Fatal("substituteAgentScriptString succeeded, want missing placeholder error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func TestAgentScriptRunActionBuildsBDUpdateArgs(t *testing.T) {
	var calls [][]string
	exec := agentScriptExecutor{
		runCommand: func(name string, args ...string) error {
			calls = append(calls, append([]string{name}, args...))
			return nil
		},
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
	}
	ctx := agentScriptContext{
		bead: agentScriptBead{
			ID: "ga-123",
			Metadata: map[string]string{
				"branch": "polecat/ga-123",
			},
		},
		rig: "demo",
	}

	_, err := exec.runAction(agentScriptAction{"bd_update": map[string]any{
		"id":       "{bead.id}",
		"assignee": "{rig}/lifecycle.refinery",
		"status":   "closed",
		"notes":    "merged {bead.metadata.branch}",
		"metadata": map[string]any{
			"zeta":  "last",
			"alpha": "first",
		},
	}}, ctx)
	if err != nil {
		t.Fatalf("runAction: %v", err)
	}
	want := []string{
		"bd", "update", "ga-123",
		"--assignee", "demo/lifecycle.refinery",
		"--status", "closed",
		"--notes", "merged polecat/ga-123",
		"--set-metadata", "alpha=first",
		"--set-metadata", "zeta=last",
	}
	if len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Fatalf("calls = %#v, want %#v", calls, [][]string{want})
	}
}

func TestAgentScriptRejectsMissingMetadataPlaceholder(t *testing.T) {
	var calls [][]string
	exec := agentScriptExecutor{
		runCommand: func(name string, args ...string) error {
			calls = append(calls, append([]string{name}, args...))
			return nil
		},
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
	}
	ctx := agentScriptContext{
		bead: agentScriptBead{
			ID:       "ga-123",
			Metadata: map[string]string{},
		},
	}

	_, err := exec.runAction(agentScriptAction{"bd_update": map[string]any{
		"id":    "{bead.id}",
		"notes": "merged {bead.metadata.branch}",
	}}, ctx)
	if err == nil {
		t.Fatal("runAction succeeded, want missing metadata placeholder error")
	}
	if !strings.Contains(err.Error(), `metadata key "branch" is missing`) {
		t.Fatalf("error = %q, want missing branch metadata", err.Error())
	}
	if len(calls) != 0 {
		t.Fatalf("runCommand called before placeholder validation failed: %#v", calls)
	}
}

func TestAgentScriptRejectsNullYAMLValues(t *testing.T) {
	tests := []struct {
		name   string
		action string
		want   string
	}{
		{
			name: "assignee",
			action: `
      - bd_update:
          id: "{bead.id}"
          assignee: null
`,
			want: `field "assignee" cannot be null`,
		},
		{
			name: "metadata",
			action: `
      - bd_update:
          id: "{bead.id}"
          metadata:
            state: null
`,
			want: `metadata "state" cannot be null`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			scriptPath := filepath.Join(dir, "script.yaml")
			script := `
schema_version: v1
turns:
  - when:
      hook: has_work
    do:
` + tt.action
			if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
				t.Fatalf("writing script: %v", err)
			}

			var stdout, stderr bytes.Buffer
			exec := agentScriptExecutor{
				stdout: &stdout,
				stderr: &stderr,
				runCommand: func(name string, args ...string) error {
					t.Fatalf("runCommand called for null YAML value: %s %#v", name, args)
					return nil
				},
			}
			code := runAgentScriptWithRuntime(scriptPath, &stdout, &stderr, exec,
				func(io.Writer) (agentScriptBead, bool, error) {
					return agentScriptBead{ID: "ga-123"}, true, nil
				})
			if code == 0 {
				t.Fatalf("runAgentScriptWithRuntime exit = 0, want failure")
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.want)
			}
		})
	}
}

func TestAgentScriptRunActionBuildsMailSendArgs(t *testing.T) {
	tests := []struct {
		name string
		arg  map[string]any
		want []string
	}{
		{
			name: "all",
			arg: map[string]any{
				"subject": "drained",
				"body":    "no work",
			},
			want: []string{"gc", "mail", "send", "--all", "-s", "drained", "-m", "no work"},
		},
		{
			name: "explicit target",
			arg: map[string]any{
				"to":      "{rig}/lifecycle.refinery",
				"subject": "ready {bead.id}",
				"body":    "branch {bead.metadata.branch}",
			},
			want: []string{"gc", "mail", "send", "--to", "demo/lifecycle.refinery", "-s", "ready ga-123", "-m", "branch polecat/ga-123"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls [][]string
			exec := agentScriptExecutor{
				runCommand: func(name string, args ...string) error {
					calls = append(calls, append([]string{name}, args...))
					return nil
				},
				stdout: &bytes.Buffer{},
				stderr: &bytes.Buffer{},
			}
			ctx := agentScriptContext{
				bead: agentScriptBead{
					ID: "ga-123",
					Metadata: map[string]string{
						"branch": "polecat/ga-123",
					},
				},
				rig: "demo",
			}

			_, err := exec.runAction(agentScriptAction{"mail_send": tt.arg}, ctx)
			if err != nil {
				t.Fatalf("runAction: %v", err)
			}
			if len(calls) != 1 || !slices.Equal(calls[0], tt.want) {
				t.Fatalf("calls = %#v, want %#v", calls, [][]string{tt.want})
			}
		})
	}
}

func TestAgentScriptRunActionValidationFailures(t *testing.T) {
	exec := agentScriptExecutor{
		runCommand: func(string, ...string) error { return nil },
		runShell:   func(string, []string) error { return nil },
		stdout:     &bytes.Buffer{},
		stderr:     &bytes.Buffer{},
	}

	for name, action := range map[string]agentScriptAction{
		"empty":       {},
		"multi-key":   {"log": "hello", "exit": 0},
		"unknown":     {"bogus": "hello"},
		"bad-shell":   {"shell": 42},
		"bad-bd-meta": {"bd_update": map[string]any{"id": "ga-123", "metadata": "bad"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := exec.runAction(action, agentScriptContext{}); err == nil {
				t.Fatal("runAction succeeded, want error")
			}
		})
	}
}

func TestAgentScriptRunActionSleepMS(t *testing.T) {
	exec := agentScriptExecutor{
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
	}
	start := time.Now()
	exitCode, err := exec.runAction(agentScriptAction{"sleep_ms": 1}, agentScriptContext{})
	if err != nil {
		t.Fatalf("runAction: %v", err)
	}
	if exitCode != nil {
		t.Fatalf("exitCode = %v, want nil", *exitCode)
	}
	if elapsed := time.Since(start); elapsed < time.Millisecond {
		t.Fatalf("sleep_ms returned after %s, want at least 1ms", elapsed)
	}
}

func TestAgentScriptCommandPreservesScriptExitCode(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "script.yaml")
	if err := os.WriteFile(scriptPath, []byte(`
schema_version: v1
setup:
  - exit: 5
turns:
  - when:
      hook: empty
    do:
      - log: unreachable
`), 0o644); err != nil {
		t.Fatalf("writing script: %v", err)
	}

	var stdout, stderr bytes.Buffer
	got := run([]string{"agent-script", "--script", scriptPath}, &stdout, &stderr)
	if got != 5 {
		t.Fatalf("run(agent-script exit 5) = %d, want 5; stdout=%q stderr=%q", got, stdout.String(), stderr.String())
	}
}

func TestAgentScriptRunsSetupHookAndTurn(t *testing.T) {
	t.Setenv("GC_RIG", "demo")
	t.Setenv("GC_ALIAS", "demo/worker")
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "script.yaml")
	if err := os.WriteFile(scriptPath, []byte(`
schema_version: v1
setup:
  - log: "setup {rig}"
turns:
  - when:
      hook: has_work
    do:
      - bd_claim: "{bead.id}"
      - shell: "printf safe"
      - bd_update:
          id: "{bead.id}"
          status: closed
          metadata:
            branch: "{bead.metadata.branch}"
      - exit: 7
`), 0o644); err != nil {
		t.Fatalf("writing script: %v", err)
	}

	var stdout, stderr bytes.Buffer
	var commands [][]string
	var shells []string
	exec := agentScriptExecutor{
		stdout: &stdout,
		stderr: &stderr,
		runCommand: func(name string, args ...string) error {
			commands = append(commands, append([]string{name}, args...))
			return nil
		},
		runShell: func(command string, env []string) error {
			shells = append(shells, command)
			if !slices.Contains(env, "GC_SCRIPT_BEAD_METADATA_BRANCH=polecat/ga-123") {
				t.Fatalf("shell env missing branch metadata: %#v", env)
			}
			return nil
		},
	}

	code := runAgentScriptWithRuntime(scriptPath, &stdout, &stderr, exec,
		func(io.Writer) (agentScriptBead, bool, error) {
			return agentScriptBead{
				ID:    "ga-123",
				Title: "demo task",
				Metadata: map[string]string{
					"branch": "polecat/ga-123",
				},
			}, true, nil
		})
	if code != 7 {
		t.Fatalf("runAgentScriptWithRuntime exit = %d, want 7; stderr=%q", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "setup demo" {
		t.Fatalf("stdout = %q, want setup log", got)
	}
	wantCommands := [][]string{
		{"bd", "update", "ga-123", "--claim", "--actor", "demo/worker"},
		{"bd", "update", "ga-123", "--status", "closed", "--set-metadata", "branch=polecat/ga-123"},
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", commands, wantCommands)
	}
	if !reflect.DeepEqual(shells, []string{"printf safe"}) {
		t.Fatalf("shells = %#v, want safe shell", shells)
	}
}

func TestAgentScriptRunsShippedLifecyclePolecatScript(t *testing.T) {
	t.Setenv("GC_RIG", "demo")
	t.Setenv("GC_ALIAS", "demo/lifecycle.polecat")
	t.Setenv("GC_SESSION_NAME", "demo/lifecycle.polecat")

	scriptPath := filepath.Join("..", "..", "examples", "lifecycle", "packs", "lifecycle", "assets", "scripts", "lifecycle-polecat-claim-handoff.yaml")

	var stdout, stderr bytes.Buffer
	var commands [][]string
	var shells []string
	exec := agentScriptExecutor{
		stdout: &stdout,
		stderr: &stderr,
		runCommand: func(name string, args ...string) error {
			commands = append(commands, append([]string{name}, args...))
			return nil
		},
		runShell: func(command string, env []string) error {
			isTurnShell := len(shells) >= 4
			shells = append(shells, command)
			wants := []string{
				"GIT_TERMINAL_PROMPT=0",
				"GC_SCRIPT_RIG=demo",
				"GC_SCRIPT_ALIAS=demo/lifecycle.polecat",
			}
			if isTurnShell {
				wants = append(wants, "GC_SCRIPT_BEAD_ID=ga-123")
			}
			for _, want := range wants {
				if !slices.Contains(env, want) {
					t.Fatalf("shell env missing %q in %#v", want, env)
				}
			}
			return nil
		},
	}

	code := runAgentScriptWithRuntime(scriptPath, &stdout, &stderr, exec,
		func(io.Writer) (agentScriptBead, bool, error) {
			return agentScriptBead{ID: "ga-123", Title: "Demo work"}, true, nil
		})
	if code != 0 {
		t.Fatalf("runAgentScriptWithRuntime exit = %d, want 0; stderr=%q", code, stderr.String())
	}

	wantCommands := [][]string{
		{"bd", "update", "ga-123", "--claim", "--actor", "demo/lifecycle.polecat"},
		{"gc", "mail", "send", "--to", "demo/lifecycle.refinery", "-s", "CLAIMED: Demo work (ga-123)", "-m", "scripted polecat demo/lifecycle.polecat claimed ga-123; branch polecat/ga-123."},
		{"bd", "update", "ga-123", "--assignee", "demo/lifecycle.refinery", "--notes", "scripted polecat: implemented ga-123, handed off to refinery"},
		{"gc", "mail", "send", "--to", "demo/lifecycle.refinery", "-s", "READY FOR MERGE: polecat/ga-123 (ga-123)", "-m", "scripted polecat demo/lifecycle.polecat pushed polecat/ga-123 for demo/lifecycle.refinery."},
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", commands, wantCommands)
	}
	if len(shells) != 6 {
		t.Fatalf("shell count = %d, want 6: %#v", len(shells), shells)
	}
	if !strings.Contains(strings.Join(shells[:4], "\n"), "GITHUB_TOKEN") ||
		!strings.Contains(strings.Join(shells[:4], "\n"), "github.com") ||
		!strings.Contains(strings.Join(shells[:4], "\n"), "$HOME/.netrc") ||
		!strings.Contains(strings.Join(shells[:4], "\n"), "chmod 600") {
		t.Fatalf("setup shells do not install github token netrc bridge: %#v", shells[:4])
	}
	if !strings.Contains(strings.Join(shells, "\n"), "bd update \"$BEAD_ID\" --set-metadata branch=\"$BRANCH\" --set-metadata branch_head=\"$HEAD_SHA\"") {
		t.Fatalf("shells do not record branch head before handoff: %#v", shells)
	}
}

func TestAgentScriptRunsShippedLifecycleRefineryScript(t *testing.T) {
	t.Setenv("GC_RIG", "demo")
	t.Setenv("GC_ALIAS", "demo/lifecycle.refinery")
	t.Setenv("GC_SESSION_NAME", "demo/lifecycle.refinery")

	scriptPath := filepath.Join("..", "..", "examples", "lifecycle", "packs", "lifecycle", "assets", "scripts", "lifecycle-refinery-merge.yaml")

	var stdout, stderr bytes.Buffer
	var commands [][]string
	var shells []string
	var actionOrder []string
	exec := agentScriptExecutor{
		stdout: &stdout,
		stderr: &stderr,
		runCommand: func(name string, args ...string) error {
			command := append([]string{name}, args...)
			commands = append(commands, command)
			switch {
			case name == "bd":
				actionOrder = append(actionOrder, "cmd:bd_update")
			case name == "gc" && len(args) >= 2 && args[0] == "mail" && args[1] == "send":
				actionOrder = append(actionOrder, "cmd:mail_send")
			default:
				actionOrder = append(actionOrder, "cmd:"+name)
			}
			return nil
		},
		runShell: func(command string, env []string) error {
			isTurnShell := len(shells) >= 2
			shells = append(shells, command)
			switch {
			case strings.Contains(command, "git merge --no-edit"):
				actionOrder = append(actionOrder, "shell:merge")
			case strings.Contains(command, "git push origin HEAD:main"):
				actionOrder = append(actionOrder, "shell:push")
			case strings.Contains(command, "git push origin --delete"):
				actionOrder = append(actionOrder, "shell:cleanup")
			default:
				actionOrder = append(actionOrder, "shell:setup")
			}
			wants := []string{
				"GIT_TERMINAL_PROMPT=0",
				"GC_SCRIPT_RIG=demo",
				"GC_SCRIPT_ALIAS=demo/lifecycle.refinery",
			}
			if isTurnShell {
				wants = append(wants,
					"GC_SCRIPT_BEAD_ID=ga-123",
					"GC_SCRIPT_BEAD_METADATA_BRANCH=polecat/ga-123",
					"GC_SCRIPT_BEAD_METADATA_BRANCH_HEAD=0123456789abcdef",
				)
			}
			for _, want := range wants {
				if !slices.Contains(env, want) {
					t.Fatalf("shell env missing %q in %#v", want, env)
				}
			}
			return nil
		},
	}

	code := runAgentScriptWithRuntime(scriptPath, &stdout, &stderr, exec,
		func(io.Writer) (agentScriptBead, bool, error) {
			return agentScriptBead{
				ID:    "ga-123",
				Title: "Demo work",
				Metadata: map[string]string{
					"branch":      "polecat/ga-123",
					"branch_head": "0123456789abcdef",
				},
			}, true, nil
		})
	if code != 0 {
		t.Fatalf("runAgentScriptWithRuntime exit = %d, want 0; stderr=%q", code, stderr.String())
	}

	wantCommands := [][]string{
		{"gc", "mail", "send", "--to", "demo/lifecycle.polecat", "-s", "MERGING: polecat/ga-123 (ga-123)", "-m", "scripted refinery demo/lifecycle.refinery is merging polecat/ga-123."},
		{"bd", "update", "ga-123", "--status", "closed", "--notes", "scripted refinery: merged polecat/ga-123 into main", "--set-metadata", "merge_result=merged"},
		{"gc", "mail", "send", "--to", "demo/lifecycle.polecat", "-s", "MERGED: polecat/ga-123 (ga-123)", "-m", "scripted refinery demo/lifecycle.refinery merged polecat/ga-123 into main."},
	}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", commands, wantCommands)
	}
	if len(shells) != 5 {
		t.Fatalf("shell count = %d, want 5: %#v", len(shells), shells)
	}
	if !strings.Contains(strings.Join(shells[:2], "\n"), "GITHUB_TOKEN") ||
		!strings.Contains(strings.Join(shells[:2], "\n"), "github.com") ||
		!strings.Contains(strings.Join(shells[:2], "\n"), "$HOME/.netrc") ||
		!strings.Contains(strings.Join(shells[:2], "\n"), "chmod 600") {
		t.Fatalf("setup shells do not install github token netrc bridge: %#v", shells[:2])
	}
	wantOrder := []string{
		"shell:setup",
		"shell:setup",
		"cmd:mail_send",
		"shell:merge",
		"shell:push",
		"cmd:bd_update",
		"shell:cleanup",
		"cmd:mail_send",
	}
	if !reflect.DeepEqual(actionOrder, wantOrder) {
		t.Fatalf("action order = %#v, want %#v", actionOrder, wantOrder)
	}
}

func TestAgentScriptHookBeadAllowsWarningsWithReadyWork(t *testing.T) {
	var stderr bytes.Buffer
	bead, hasWork, err := agentScriptHookBeadWithRunner(&stderr,
		func(_ []string, _ bool, _ string, stdout, hookStderr io.Writer) int {
			_, _ = hookStderr.Write([]byte("gc hook: deprecated config warning\n"))
			_, _ = stdout.Write([]byte(`[{"id":"ga-123","title":"demo","metadata":{"branch":"feature"}}]`))
			return 0
		})
	if err != nil {
		t.Fatalf("agentScriptHookBeadWithRunner: %v", err)
	}
	if !hasWork {
		t.Fatal("agentScriptHookBeadWithRunner reported no work")
	}
	if bead.ID != "ga-123" || bead.Metadata["branch"] != "feature" {
		t.Fatalf("bead = %#v, want id ga-123 with branch metadata", bead)
	}
	if got := stderr.String(); got != "gc hook: deprecated config warning\n" {
		t.Fatalf("stderr = %q, want forwarded hook warning", got)
	}
}

func TestAgentScriptHookBeadTreatsEmptyHookWarningAsNoWork(t *testing.T) {
	var stderr bytes.Buffer
	bead, hasWork, err := agentScriptHookBeadWithRunner(&stderr,
		func(_ []string, _ bool, _ string, _ io.Writer, hookStderr io.Writer) int {
			_, _ = hookStderr.Write([]byte("gc hook: deprecated config warning\n"))
			return 1
		})
	if err != nil {
		t.Fatalf("agentScriptHookBeadWithRunner: %v", err)
	}
	if hasWork {
		t.Fatalf("agentScriptHookBeadWithRunner returned work %#v, want no work", bead)
	}
	if got := stderr.String(); got != "gc hook: deprecated config warning\n" {
		t.Fatalf("stderr = %q, want forwarded hook warning", got)
	}
}

func TestAgentScriptHookBeadReportsHookFailureWithEmptyOutput(t *testing.T) {
	var stderr bytes.Buffer
	_, hasWork, err := agentScriptHookBeadWithRunner(&stderr,
		func(_ []string, _ bool, _ string, _ io.Writer, hookStderr io.Writer) int {
			_, _ = hookStderr.Write([]byte("gc hook: config failed\n"))
			return 1
		})
	if err == nil {
		t.Fatal("agentScriptHookBeadWithRunner succeeded, want hook failure")
	}
	if hasWork {
		t.Fatal("agentScriptHookBeadWithRunner reported work on hook failure")
	}
	if !strings.Contains(err.Error(), "gc hook failed") {
		t.Fatalf("error = %q, want hook failure", err.Error())
	}
	if got := stderr.String(); got != "gc hook: config failed\n" {
		t.Fatalf("stderr = %q, want forwarded hook error", got)
	}
}

func TestAgentScriptHookBeadTreatsEmptyHookAsNoWork(t *testing.T) {
	var stderr bytes.Buffer
	bead, hasWork, err := agentScriptHookBeadWithRunner(&stderr,
		func(_ []string, _ bool, _ string, _ io.Writer, _ io.Writer) int {
			return 1
		})
	if err != nil {
		t.Fatalf("agentScriptHookBeadWithRunner: %v", err)
	}
	if hasWork {
		t.Fatalf("agentScriptHookBeadWithRunner returned work %#v, want no work", bead)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestAgentScriptHookBeadReportsArrayAndObjectDecodeErrors(t *testing.T) {
	var stderr bytes.Buffer
	_, _, err := agentScriptHookBeadWithRunner(&stderr,
		func(_ []string, _ bool, _ string, stdout, _ io.Writer) int {
			_, _ = stdout.Write([]byte(`{"id":`))
			return 0
		})
	if err == nil {
		t.Fatal("agentScriptHookBeadWithRunner accepted malformed hook output")
	}
	joined := errors.Join(err)
	if !strings.Contains(joined.Error(), "decoding hook output") {
		t.Fatalf("error = %q, want decode context", joined.Error())
	}
}
