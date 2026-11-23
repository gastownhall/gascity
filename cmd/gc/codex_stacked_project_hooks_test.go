package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Fixtures for #5872: codex loads $CODEX_HOME/hooks.json (else
// ~/.codex/hooks.json) AND every project-level .codex/hooks.json and runs both,
// so a project copy gc stages on top of a user layer that already carries gc's
// hooks fires every gc hook twice.

type stackedHookCmd struct{ event, command string }

const stackedHookPathPrefix = `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && `

// stackedCodexGenerations returns the gc-managed codex hook commands of the
// three shipped generations of the core overlay the report names: a3558786
// (current), 46f61e93 (no gc hook run --timeout guard) and bbd0d76e (no
// --hook-format codex either).
func stackedCodexGenerations() map[string][]stackedHookCmd {
	return map[string][]stackedHookCmd{
		"a3558786": {
			{"SessionStart", stackedHookPathPrefix + `GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart "${GC_BIN:-gc}" prime --hook --hook-format codex`},
			{"PreCompact", stackedHookPathPrefix + `"${GC_BIN:-gc}" handoff --auto --hook-format codex "context cycle"`},
			{"UserPromptSubmit", stackedHookPathPrefix + `"${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format codex`},
			{"UserPromptSubmit", stackedHookPathPrefix + `"${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject --hook-format codex`},
		},
		"46f61e93": {
			{"SessionStart", stackedHookPathPrefix + `"${GC_BIN:-gc}" prime --hook --hook-format codex`},
			{"PreCompact", stackedHookPathPrefix + `"${GC_BIN:-gc}" handoff --auto --hook-format codex "context cycle"`},
			{"UserPromptSubmit", stackedHookPathPrefix + `"${GC_BIN:-gc}" nudge drain --inject --hook-format codex`},
			{"UserPromptSubmit", stackedHookPathPrefix + `"${GC_BIN:-gc}" mail check --inject --hook-format codex`},
		},
		"bbd0d76e": {
			{"SessionStart", `export PATH="$HOME/go/bin:$HOME/.local/bin:$PATH" && gc prime --hook`},
			{"PreCompact", `export PATH="$HOME/go/bin:$HOME/.local/bin:$PATH" && gc handoff "context cycle"`},
			{"UserPromptSubmit", `export PATH="$HOME/go/bin:$HOME/.local/bin:$PATH" && gc nudge drain --inject`},
			{"UserPromptSubmit", `export PATH="$HOME/go/bin:$HOME/.local/bin:$PATH" && gc mail check --inject`},
		},
	}
}

var stackedUserAuthoredHook = stackedHookCmd{"UserPromptSubmit", "my-own-prompt-hook"}

// stackedHooksDoc builds a codex hooks document with one group per command.
func stackedHooksDoc(t *testing.T, cmds ...stackedHookCmd) []byte {
	t.Helper()
	events := map[string]any{}
	for _, c := range cmds {
		group := map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": c.command}}}
		existing, _ := events[c.event].([]any)
		events[c.event] = append(existing, group)
	}
	data, err := json.Marshal(map[string]any{"hooks": events})
	if err != nil {
		t.Fatalf("marshal hooks doc: %v", err)
	}
	return data
}

func writeStackedTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// isolateCodexUserHome points HOME at a fresh directory, clears CODEX_HOME and
// returns the user-level hooks path codex would load.
func isolateCodexUserHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	return filepath.Join(home, ".codex", "hooks.json")
}

// coveringUserHooks is a user-level hooks file carrying every gc hook the core
// overlay stages, in the given generation, plus one user-authored hook.
func coveringUserHooks(t *testing.T, generation string) []byte {
	t.Helper()
	cmds, ok := stackedCodexGenerations()[generation]
	if !ok {
		t.Fatalf("unknown generation %q", generation)
	}
	return stackedHooksDoc(t, append(append([]stackedHookCmd(nil), cmds...), stackedUserAuthoredHook)...)
}

// stackedTickWorkHook runs one reconcile tick and returns the codex agent's
// project-level hooks path.
func stackedTickWorkHook(t *testing.T, cityPath string, cfg *config.City, stderr io.Writer) string {
	t.Helper()
	result := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, stderr)
	_, tp := onlyDesiredSession(t, result.State)
	return filepath.Join(tp.WorkDir, ".codex", "hooks.json")
}

func assertNoStackedProjectHooks(t *testing.T, path, when string) {
	t.Helper()
	if data, err := os.ReadFile(path); err == nil {
		t.Fatalf("%s: project-level %s present although the user layer already loads every gc hook (codex would fire each twice):\n%s", when, path, data)
	} else if !os.IsNotExist(err) {
		t.Fatalf("%s: stat %s: %v", when, path, err)
	}
}

func TestCodexProjectHooksRetiredWhenUserLayerCovers(t *testing.T) {
	t.Run("not staged across two ticks", func(t *testing.T) {
		userHooks := isolateCodexUserHome(t)
		writeStackedTestFile(t, userHooks, coveringUserHooks(t, "a3558786"))
		cityPath, cfg := codexHookStagingCity(t)
		for tick := 1; tick <= 2; tick++ {
			workHook := stackedTickWorkHook(t, cityPath, cfg, io.Discard)
			assertNoStackedProjectHooks(t, workHook, fmt.Sprintf("tick %d", tick))
		}
	})

	for name, cmds := range stackedCodexGenerations() {
		t.Run("user layer generation "+name+" covers", func(t *testing.T) {
			userHooks := isolateCodexUserHome(t)
			writeStackedTestFile(t, userHooks, coveringUserHooks(t, name))
			cityPath, cfg := codexHookStagingCity(t)
			workHook := stackedTickWorkHook(t, cityPath, cfg, io.Discard)
			assertNoStackedProjectHooks(t, workHook, "tick 1")
		})

		t.Run("pre-existing all-managed copy of generation "+name+" is deleted", func(t *testing.T) {
			userHooks := isolateCodexUserHome(t)
			writeStackedTestFile(t, userHooks, coveringUserHooks(t, "a3558786"))
			cityPath, cfg := codexHookStagingCity(t)
			workHook := stackedTickWorkHook(t, cityPath, cfg, io.Discard)
			writeStackedTestFile(t, workHook, stackedHooksDoc(t, cmds...))
			var stderr strings.Builder
			for tick := 1; tick <= 2; tick++ {
				workHook = stackedTickWorkHook(t, cityPath, cfg, &stderr)
				assertNoStackedProjectHooks(t, workHook, "tick after seeding")
			}
			if !strings.Contains(stderr.String(), workHook) || !strings.Contains(stderr.String(), userHooks) {
				t.Errorf("retirement not reported naming %s and %s; stderr:\n%s", workHook, userHooks, stderr.String())
			}
		})
	}

	t.Run("pre-existing mixed copy keeps only user entries", func(t *testing.T) {
		userHooks := isolateCodexUserHome(t)
		writeStackedTestFile(t, userHooks, coveringUserHooks(t, "a3558786"))
		cityPath, cfg := codexHookStagingCity(t)
		workHook := stackedTickWorkHook(t, cityPath, cfg, io.Discard)
		projectOwn := stackedHookCmd{"Stop", "project-stop-hook"}
		seeded := append(append([]stackedHookCmd(nil), stackedCodexGenerations()["46f61e93"]...), projectOwn)
		writeStackedTestFile(t, workHook, stackedHooksDoc(t, seeded...))
		want, err := overlay.CanonicalJSON(stackedHooksDoc(t, projectOwn))
		if err != nil {
			t.Fatalf("canonical: %v", err)
		}
		for tick := 1; tick <= 2; tick++ {
			workHook = stackedTickWorkHook(t, cityPath, cfg, io.Discard)
			got, err := os.ReadFile(workHook)
			if err != nil {
				t.Fatalf("tick %d: user-authored project hooks lost: %v", tick, err)
			}
			if string(got) != string(want) {
				t.Fatalf("tick %d: project hooks =\n%s\nwant only the user-authored entries:\n%s", tick, got, want)
			}
		}
	})

	t.Run("CODEX_HOME governs over HOME", func(t *testing.T) {
		homeHooks := isolateCodexUserHome(t)
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		writeStackedTestFile(t, filepath.Join(codexHome, "hooks.json"), coveringUserHooks(t, "a3558786"))
		cityPath, cfg := codexHookStagingCity(t)
		workHook := stackedTickWorkHook(t, cityPath, cfg, io.Discard)
		assertNoStackedProjectHooks(t, workHook, "covering $CODEX_HOME/hooks.json")

		// A covering ~/.codex/hooks.json is not what codex loads when
		// CODEX_HOME names an empty home, so the fallback stays.
		writeStackedTestFile(t, homeHooks, coveringUserHooks(t, "a3558786"))
		if err := os.Remove(filepath.Join(codexHome, "hooks.json")); err != nil {
			t.Fatalf("remove CODEX_HOME hooks: %v", err)
		}
		workHook = stackedTickWorkHook(t, cityPath, cfg, io.Discard)
		if _, err := os.Stat(workHook); err != nil {
			t.Fatalf("fallback project hooks not staged although $CODEX_HOME/hooks.json is absent: %v", err)
		}
	})
}

// stackedTick runs one reconcile tick and returns the codex agent's session
// name and desired template params.
func stackedTick(t *testing.T, cityPath string, cfg *config.City) (string, TemplateParams) {
	t.Helper()
	result := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	return onlyDesiredSession(t, result.State)
}

// TestCodexStackedHooksSessionStartKeepsFingerprint covers session start for a
// covered codex agent: session-start staging (subprocess, acp, herdr and host
// exec through runtime.StageSessionWorkDirWithWarnings; tmux through
// stageStartFiles, covered in its own package) must not write the project
// copy the tick withholds, and the next tick must compute the fingerprint the
// session started with.
func TestCodexStackedHooksSessionStartKeepsFingerprint(t *testing.T) {
	userHooks := isolateCodexUserHome(t)
	writeStackedTestFile(t, userHooks, coveringUserHooks(t, "a3558786"))
	cityPath, cfg := codexHookStagingCity(t)

	sessionName, firstTP := stackedTick(t, cityPath, cfg)
	startedCfg := templateParamsToConfig(firstTP)
	startedHash := runtime.CoreFingerprint(startedCfg)
	if err := runtime.StageSessionWorkDirWithWarnings(startedCfg, io.Discard); err != nil {
		t.Fatalf("StageSessionWorkDirWithWarnings: %v", err)
	}
	assertNoStackedProjectHooks(t, filepath.Join(startedCfg.WorkDir, ".codex", "hooks.json"), "after session start")

	result := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	secondTP, ok := result.State[sessionName]
	if !ok {
		t.Fatalf("second desired state missing %q", sessionName)
	}
	currentCfg := templateParamsToConfig(secondTP)
	if got := runtime.CoreFingerprint(currentCfg); got != startedHash {
		t.Fatalf("core fingerprint moved after session start: started=%s current=%s drifted=%v",
			startedHash, got, runtime.CoreFingerprintDriftFields(runtime.CoreFingerprintBreakdown(startedCfg), currentCfg))
	}
}

// TestCodexProjectHooksFallbackWhenUserLayerDoesNotCover keeps gc's project
// copy, byte-identical to what a city with no user hooks file stages, whenever
// the user layer does not provide every gc hook that copy would carry, and
// reports a user file it cannot parse.
func TestCodexProjectHooksFallbackWhenUserLayerDoesNotCover(t *testing.T) {
	current := stackedCodexGenerations()["a3558786"]
	withoutEvent := func(event string) []stackedHookCmd {
		var out []stackedHookCmd
		for _, c := range current {
			if c.event != event {
				out = append(out, c)
			}
		}
		return out
	}
	boundElsewhere := func() []stackedHookCmd {
		out := make([]stackedHookCmd, 0, len(current))
		for _, c := range current {
			out = append(out, stackedHookCmd{c.event, strings.Replace(c.command, `"${GC_BIN:-gc}" `, `"${GC_BIN:-gc}" --city /elsewhere `, 1)})
		}
		return out
	}

	cases := []struct {
		name string
		// user is the user-level hooks file content; nil leaves it absent.
		user func(t *testing.T) []byte
		// mutate adjusts the city before either tick.
		mutate func(t *testing.T, cityPath string, cfg *config.City)
		// warnPath reports that stderr must name the user hooks file.
		warnPath bool
	}{
		{name: "user file absent"},
		{name: "user file unparseable", user: func(*testing.T) []byte { return []byte("{") }, warnPath: true},
		{name: "user layer lacks PreCompact", user: func(t *testing.T) []byte { return stackedHooksDoc(t, withoutEvent("PreCompact")...) }},
		{name: "user hooks bound to another city", user: func(t *testing.T) []byte { return stackedHooksDoc(t, boundElsewhere()...) }},
		{
			name: "pack overlay contributes a non-gc hook",
			user: func(t *testing.T) []byte { return coveringUserHooks(t, "a3558786") },
			mutate: func(t *testing.T, cityPath string, cfg *config.City) {
				extra := filepath.Join(cityPath, "packs", "extra", "overlay")
				writeStackedTestFile(t, filepath.Join(extra, "per-provider", "codex", ".codex", "hooks.json"),
					stackedHooksDoc(t, stackedHookCmd{"Stop", "pack-stop-hook"}))
				cfg.PackOverlayDirs = append(cfg.PackOverlayDirs, extra)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			userHooks := isolateCodexUserHome(t)
			cityPath, cfg := codexHookStagingCity(t)
			if tc.mutate != nil {
				tc.mutate(t, cityPath, cfg)
			}
			workHook := stackedTickWorkHook(t, cityPath, cfg, io.Discard)
			baseline, err := os.ReadFile(workHook)
			if err != nil {
				t.Fatalf("baseline with no user hooks file: project hooks not staged: %v", err)
			}
			if err := os.Remove(workHook); err != nil {
				t.Fatalf("remove baseline project hooks: %v", err)
			}

			if tc.user != nil {
				writeStackedTestFile(t, userHooks, tc.user(t))
			}
			var stderr strings.Builder
			workHook = stackedTickWorkHook(t, cityPath, cfg, &stderr)
			got, err := os.ReadFile(workHook)
			if err != nil {
				t.Fatalf("fallback project hooks not staged: %v", err)
			}
			if string(got) != string(baseline) {
				t.Fatalf("fallback project hooks =\n%s\nwant the no-user-file staging:\n%s", got, baseline)
			}
			if tc.warnPath && !strings.Contains(stderr.String(), userHooks) {
				t.Fatalf("unparseable user hooks not reported naming %s; stderr:\n%s", userHooks, stderr.String())
			}
		})
	}
}

// TestCodexStackedHooksGuardsKeepTodaysStaging pins the cases the stacked
// project-hook decision must leave alone even when a covering user hooks file
// exists: runtimes whose sessions do not read the controller host's
// filesystem (k8s, hybrid, an acp agent), an installer that owns the file
// (install_agent_hooks naming codex) and a provider whose hook files do not
// stack (claude). Each stages and fingerprints exactly as with no user file.
// These pass on the pre-change code by design: they guard the decision's
// scope, not a regression it fixes.
func TestCodexStackedHooksGuardsKeepTodaysStaging(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(cfg *config.City)
		// acpRouting runs the tick on a session provider that routes ACP.
		acpRouting bool
	}{
		{name: "k8s session provider", mutate: func(cfg *config.City) { cfg.Session.Provider = "k8s" }},
		{name: "hybrid session provider", mutate: func(cfg *config.City) { cfg.Session.Provider = "hybrid" }},
		{name: "acp agent", mutate: func(cfg *config.City) {
			spec := cfg.Providers["codex"]
			spec.SupportsACP = boolPtr(true)
			cfg.Providers["codex"] = spec
			cfg.Agents[0].Session = "acp"
		}, acpRouting: true},
		{name: "install_agent_hooks names codex", mutate: func(cfg *config.City) { cfg.Agents[0].InstallAgentHooks = []string{"codex"} }},
		{name: "claude agent", mutate: func(cfg *config.City) {
			cfg.Providers["claude"] = config.ProviderSpec{Command: "echo", PromptMode: "none"}
			cfg.Agents[0].Provider = "claude"
		}},
	}
	type outcome struct {
		present bool
		content string
		core    string
	}
	observe := func(t *testing.T, cityPath string, cfg *config.City, acpRouting bool) (outcome, string) {
		t.Helper()
		var sp runtime.Provider = runtime.NewFake()
		if acpRouting {
			sp = transportFake{runtime.NewFake()}
		}
		var stderr strings.Builder
		result := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, sp, nil, &stderr)
		if len(result.State) != 1 {
			t.Fatalf("desired state size = %d, want 1; stderr:\n%s", len(result.State), stderr.String())
		}
		_, tp := onlyDesiredSession(t, result.State)
		workHook := filepath.Join(tp.WorkDir, ".codex", "hooks.json")
		got := outcome{core: runtime.CoreFingerprint(templateParamsToConfig(tp))}
		data, err := os.ReadFile(workHook)
		switch {
		case err == nil:
			got.present, got.content = true, string(data)
		case !os.IsNotExist(err):
			t.Fatalf("read %s: %v", workHook, err)
		}
		return got, workHook
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			userHooks := isolateCodexUserHome(t)
			cityPath, cfg := codexHookStagingCity(t)
			tc.mutate(cfg)
			baseline, workHook := observe(t, cityPath, cfg, tc.acpRouting)
			if err := os.Remove(workHook); err != nil && !os.IsNotExist(err) {
				t.Fatalf("remove baseline project hooks: %v", err)
			}

			writeStackedTestFile(t, userHooks, coveringUserHooks(t, "a3558786"))
			got, _ := observe(t, cityPath, cfg, tc.acpRouting)
			if got != baseline {
				t.Fatalf("covering user hooks changed staging:\n got %+v\nwant %+v (no user file)", got, baseline)
			}
		})
	}
}
