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

const (
	stackedHookPathPrefix        = `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && `
	stackedHookPrependPathPrefix = `export PATH="$HOME/go/bin:$HOME/.local/bin:$PATH" && `
)

// stackedCurrentGeneration names the generation gc ships today.
const stackedCurrentGeneration = "8f8d64b5"

// stackedCodexGenerations returns the gc-managed codex hook commands of three
// shipped generations of
// internal/bootstrap/packs/core/overlay/per-provider/codex/.codex/hooks.json,
// keyed by the md5 prefix of that file as the report names them, each command
// copied verbatim from the file at the commit named
// (`git show <commit>:<path> | md5sum`):
//
//   - 8f8d64b5, 392bd91343 (#6579): what gc ships today, "${GC_BIN:-gc}"
//     behind the appending PATH prefix.
//   - a3558786, 1b123157ba (#3457), the file gc 1.4.1 ships: bare gc behind
//     the prepending PATH prefix, with --hook-format codex and the gc hook run
//     timeout guard.
//   - 46f61e93, 7979946cd1 (#1739): bare gc behind the prepending PATH prefix,
//     without the timeout guard.
func stackedCodexGenerations() map[string][]stackedHookCmd {
	return map[string][]stackedHookCmd{
		stackedCurrentGeneration: {
			{"SessionStart", stackedHookPathPrefix + `GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart "${GC_BIN:-gc}" prime --hook --hook-format codex`},
			{"PreCompact", stackedHookPathPrefix + `"${GC_BIN:-gc}" handoff --auto --hook-format codex "context cycle"`},
			{"UserPromptSubmit", stackedHookPathPrefix + `"${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format codex`},
			{"UserPromptSubmit", stackedHookPathPrefix + `"${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject --hook-format codex`},
		},
		"a3558786": {
			{"SessionStart", stackedHookPrependPathPrefix + `GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc prime --hook --hook-format codex`},
			{"PreCompact", stackedHookPrependPathPrefix + `gc handoff --auto --hook-format codex "context cycle"`},
			{"UserPromptSubmit", stackedHookPrependPathPrefix + `gc hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format codex`},
			{"UserPromptSubmit", stackedHookPrependPathPrefix + `gc hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject --hook-format codex`},
		},
		"46f61e93": {
			{"SessionStart", stackedHookPrependPathPrefix + `GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc prime --hook --hook-format codex`},
			{"PreCompact", stackedHookPrependPathPrefix + `gc handoff --auto --hook-format codex "context cycle"`},
			{"UserPromptSubmit", stackedHookPrependPathPrefix + `gc nudge drain --inject --hook-format codex`},
			{"UserPromptSubmit", stackedHookPrependPathPrefix + `gc mail check --inject --hook-format codex`},
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

// stackedHookGroup is one group of a codex hooks document: its matcher and
// the commands it runs.
type stackedHookGroup struct {
	matcher  string
	commands []string
}

// stackedHookGroupsDoc builds a codex hooks document from its groups by event.
func stackedHookGroupsDoc(t *testing.T, events map[string][]stackedHookGroup) []byte {
	t.Helper()
	out := make(map[string]any, len(events))
	for event, groups := range events {
		encoded := make([]any, 0, len(groups))
		for _, g := range groups {
			entries := make([]any, 0, len(g.commands))
			for _, c := range g.commands {
				entries = append(entries, map[string]any{"type": "command", "command": c})
			}
			encoded = append(encoded, map[string]any{"matcher": g.matcher, "hooks": entries})
		}
		out[event] = encoded
	}
	data, err := json.Marshal(map[string]any{"hooks": out})
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
// overlay stages, in the given generation, plus one user-authored hook. Only
// the current generation (stackedCurrentGeneration) covers the overlay; an
// older one is stale.
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

// stackedTickObserve runs one reconcile tick and returns the codex agent's
// project-level hooks path and core fingerprint.
func stackedTickObserve(t *testing.T, cityPath string, cfg *config.City, stderr io.Writer) (string, string) {
	t.Helper()
	result := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, stderr)
	_, tp := onlyDesiredSession(t, result.State)
	return filepath.Join(tp.WorkDir, ".codex", "hooks.json"), runtime.CoreFingerprint(templateParamsToConfig(tp))
}

// withStackedGroupMatcher returns doc with matcher set on every group under event.
func withStackedGroupMatcher(t *testing.T, doc []byte, event, matcher string) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(doc, &root); err != nil {
		t.Fatalf("decode hooks doc: %v", err)
	}
	groups, _ := root["hooks"].(map[string]any)[event].([]any)
	if len(groups) == 0 {
		t.Fatalf("hooks doc has no %s groups", event)
	}
	for _, g := range groups {
		g.(map[string]any)["matcher"] = matcher
	}
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("encode hooks doc: %v", err)
	}
	return out
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
		writeStackedTestFile(t, userHooks, coveringUserHooks(t, stackedCurrentGeneration))
		cityPath, cfg := codexHookStagingCity(t)
		for tick := 1; tick <= 2; tick++ {
			workHook := stackedTickWorkHook(t, cityPath, cfg, io.Discard)
			assertNoStackedProjectHooks(t, workHook, fmt.Sprintf("tick %d", tick))
		}
	})

	for name, cmds := range stackedCodexGenerations() {
		t.Run("pre-existing all-managed copy of generation "+name+" is deleted", func(t *testing.T) {
			userHooks := isolateCodexUserHome(t)
			writeStackedTestFile(t, userHooks, coveringUserHooks(t, stackedCurrentGeneration))
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
		writeStackedTestFile(t, userHooks, coveringUserHooks(t, stackedCurrentGeneration))
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

	t.Run("pre-existing copy keeps gc hooks the user layer does not run", func(t *testing.T) {
		userHooks := isolateCodexUserHome(t)
		writeStackedTestFile(t, userHooks, coveringUserHooks(t, stackedCurrentGeneration))
		cityPath, cfg := codexHookStagingCity(t)
		workHook := stackedTickWorkHook(t, cityPath, cfg, io.Discard)
		current := stackedCodexGenerations()[stackedCurrentGeneration]
		prime, handoff, nudge, mail := current[0].command, current[1].command, current[2].command, current[3].command
		// The user layer runs prime only under matcher "" and no gc hook bound
		// to another city, so prime under "startup" and mail check bound to
		// /elsewhere stay.
		otherCityMail := strings.Replace(mail, `"${GC_BIN:-gc}" `, `"${GC_BIN:-gc}" --city /elsewhere `, 1)
		writeStackedTestFile(t, workHook, stackedHookGroupsDoc(t, map[string][]stackedHookGroup{
			"SessionStart":     {{"", []string{prime}}, {"startup", []string{prime}}},
			"PreCompact":       {{"", []string{handoff}}},
			"UserPromptSubmit": {{"", []string{nudge, mail, otherCityMail}}},
		}))
		want, err := overlay.CanonicalJSON(stackedHookGroupsDoc(t, map[string][]stackedHookGroup{
			"SessionStart":     {{"startup", []string{prime}}},
			"UserPromptSubmit": {{"", []string{otherCityMail}}},
		}))
		if err != nil {
			t.Fatalf("canonical: %v", err)
		}
		for tick := 1; tick <= 2; tick++ {
			workHook = stackedTickWorkHook(t, cityPath, cfg, io.Discard)
			got, err := os.ReadFile(workHook)
			if err != nil {
				t.Fatalf("tick %d: gc hooks the user layer does not run lost: %v", tick, err)
			}
			if string(got) != string(want) {
				t.Fatalf("tick %d: project hooks =\n%s\nwant only the gc hooks the user layer does not run:\n%s", tick, got, want)
			}
		}
	})

	t.Run("CODEX_HOME governs over HOME", func(t *testing.T) {
		homeHooks := isolateCodexUserHome(t)
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		writeStackedTestFile(t, filepath.Join(codexHome, "hooks.json"), coveringUserHooks(t, stackedCurrentGeneration))
		cityPath, cfg := codexHookStagingCity(t)
		workHook := stackedTickWorkHook(t, cityPath, cfg, io.Discard)
		assertNoStackedProjectHooks(t, workHook, "covering $CODEX_HOME/hooks.json")

		// A covering ~/.codex/hooks.json is not what codex loads when
		// CODEX_HOME names an empty home, so the fallback stays.
		writeStackedTestFile(t, homeHooks, coveringUserHooks(t, stackedCurrentGeneration))
		if err := os.Remove(filepath.Join(codexHome, "hooks.json")); err != nil {
			t.Fatalf("remove CODEX_HOME hooks: %v", err)
		}
		workHook = stackedTickWorkHook(t, cityPath, cfg, io.Discard)
		if _, err := os.Stat(workHook); err != nil {
			t.Fatalf("fallback project hooks not staged although $CODEX_HOME/hooks.json is absent: %v", err)
		}
	})

	t.Run("CODEX_HOME from the selected upstream governs", func(t *testing.T) {
		isolateCodexUserHome(t)
		codexHome := t.TempDir()
		writeStackedTestFile(t, filepath.Join(codexHome, "hooks.json"), coveringUserHooks(t, stackedCurrentGeneration))
		cityPath, cfg := codexHookStagingCity(t)
		cfg.Upstreams = map[string]config.UpstreamSpec{"alt": {Env: map[string]string{"CODEX_HOME": codexHome}}}
		cfg.Agents[0].Upstream = "alt"
		workHook := stackedTickWorkHook(t, cityPath, cfg, io.Discard)
		assertNoStackedProjectHooks(t, workHook, "covering hooks in the upstream's CODEX_HOME")
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
	writeStackedTestFile(t, userHooks, coveringUserHooks(t, stackedCurrentGeneration))
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
// copy, byte-identical to what a city with no user hooks file stages and
// stable across ticks, whenever the user layer the session loads does not run
// every gc hook that copy would, alike and in the current form. It reports a
// user file it cannot parse and names a user file whose gc hooks are older.
func TestCodexProjectHooksFallbackWhenUserLayerDoesNotCover(t *testing.T) {
	current := stackedCodexGenerations()[stackedCurrentGeneration]
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
			name: "user SessionStart under a narrower matcher",
			user: func(t *testing.T) []byte {
				return withStackedGroupMatcher(t, coveringUserHooks(t, stackedCurrentGeneration), "SessionStart", "startup")
			},
		},
		{name: "user layer generation a3558786 (gc 1.4.1) runs bare gc behind the prepending PATH prefix", user: func(t *testing.T) []byte { return coveringUserHooks(t, "a3558786") }, warnPath: true},
		{name: "user layer generation 46f61e93 also has no hook run timeout guard", user: func(t *testing.T) []byte { return coveringUserHooks(t, "46f61e93") }, warnPath: true},
		{
			name: "CODEX_HOME is the work dir's .codex",
			mutate: func(_ *testing.T, cityPath string, cfg *config.City) {
				cfg.Agents[0].Env = map[string]string{"CODEX_HOME": filepath.Join(cityPath, "worker", ".codex")}
			},
		},
		{
			name: "HOME is the work dir",
			mutate: func(_ *testing.T, cityPath string, cfg *config.City) {
				cfg.Agents[0].Env = map[string]string{"HOME": filepath.Join(cityPath, "worker")}
			},
		},
		{
			name: "selected upstream relocates CODEX_HOME to a home with no hooks file",
			user: func(t *testing.T) []byte { return coveringUserHooks(t, stackedCurrentGeneration) },
			mutate: func(t *testing.T, _ string, cfg *config.City) {
				cfg.Upstreams = map[string]config.UpstreamSpec{"alt": {Env: map[string]string{"CODEX_HOME": t.TempDir()}}}
				cfg.Agents[0].Upstream = "alt"
			},
		},
		{
			name: "pack overlay contributes a non-gc hook",
			user: func(t *testing.T) []byte { return coveringUserHooks(t, stackedCurrentGeneration) },
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
			workHook, fingerprint := stackedTickObserve(t, cityPath, cfg, &stderr)
			got, err := os.ReadFile(workHook)
			if err != nil {
				t.Fatalf("fallback project hooks not staged: %v", err)
			}
			if string(got) != string(baseline) {
				t.Fatalf("fallback project hooks =\n%s\nwant the no-user-file staging:\n%s", got, baseline)
			}
			if tc.warnPath && !strings.Contains(stderr.String(), userHooks) {
				t.Fatalf("user hooks not reported naming %s; stderr:\n%s", userHooks, stderr.String())
			}

			// The fallback holds: the next tick neither retires nor rewrites
			// it, and the fingerprint the session started with stands.
			workHook, nextFingerprint := stackedTickObserve(t, cityPath, cfg, &stderr)
			again, err := os.ReadFile(workHook)
			if err != nil {
				t.Fatalf("fallback project hooks gone on the next tick: %v; stderr:\n%s", err, stderr.String())
			}
			if string(again) != string(baseline) {
				t.Fatalf("fallback project hooks on the next tick =\n%s\nwant the no-user-file staging:\n%s", again, baseline)
			}
			if nextFingerprint != fingerprint {
				t.Fatalf("core fingerprint moved on the next tick: %s, then %s", fingerprint, nextFingerprint)
			}
		})
	}
}

// TestCodexStackedHooksStaleUserLayerHintedOnce names a user hooks file whose
// gc hooks are an older generation once per process, not on every tick, while
// gc keeps staging its project copy.
func TestCodexStackedHooksStaleUserLayerHintedOnce(t *testing.T) {
	userHooks := isolateCodexUserHome(t)
	writeStackedTestFile(t, userHooks, coveringUserHooks(t, "46f61e93"))
	cityPath, cfg := codexHookStagingCity(t)

	var first strings.Builder
	workHook := stackedTickWorkHook(t, cityPath, cfg, &first)
	if _, err := os.Stat(workHook); err != nil {
		t.Fatalf("project hooks not staged for a stale user layer: %v", err)
	}
	if !strings.Contains(first.String(), userHooks) || !strings.Contains(first.String(), "older form") {
		t.Fatalf("stale user hooks not named on the first tick; stderr:\n%s", first.String())
	}
	var second strings.Builder
	stackedTickWorkHook(t, cityPath, cfg, &second)
	if strings.Contains(second.String(), userHooks) {
		t.Fatalf("stale user hooks named again on the second tick; stderr:\n%s", second.String())
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

			writeStackedTestFile(t, userHooks, coveringUserHooks(t, stackedCurrentGeneration))
			got, _ := observe(t, cityPath, cfg, tc.acpRouting)
			if got != baseline {
				t.Fatalf("covering user hooks changed staging:\n got %+v\nwant %+v (no user file)", got, baseline)
			}
		})
	}
}

// TestCodexStackedHooksSharedWorkDirStable covers a covered codex agent
// sharing its work dir with a codex agent whose sessions do not read the host
// filesystem (an acp agent), which still needs gc's project copy. The copy is
// staged on the first tick and then left alone: no tick retires what another
// agent's staging restores, so the file and both agents' fingerprints hold.
func TestCodexStackedHooksSharedWorkDirStable(t *testing.T) {
	userHooks := isolateCodexUserHome(t)
	writeStackedTestFile(t, userHooks, coveringUserHooks(t, stackedCurrentGeneration))
	cityPath, cfg := codexHookStagingCity(t)
	spec := cfg.Providers["codex"]
	spec.SupportsACP = boolPtr(true)
	cfg.Providers["codex"] = spec
	cfg.Agents = append(cfg.Agents, config.Agent{
		Name:              "acp-operator",
		Provider:          "codex",
		Session:           "acp",
		MaxActiveSessions: intPtr(1),
		ScaleCheck:        "echo 1",
		WorkDir:           "worker",
	})
	workHook := filepath.Join(cityPath, "worker", ".codex", "hooks.json")

	type snapshot struct {
		content string
		modTime time.Time
		core    map[string]string
	}
	tick := func(n int) snapshot {
		t.Helper()
		var stderr strings.Builder
		result := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, transportFake{runtime.NewFake()}, nil, &stderr)
		if len(result.State) != 2 {
			t.Fatalf("tick %d: desired state size = %d, want 2; stderr:\n%s", n, len(result.State), stderr.String())
		}
		snap := snapshot{core: make(map[string]string, len(result.State))}
		for name, tp := range result.State {
			if got := filepath.Clean(tp.WorkDir); got != filepath.Dir(filepath.Dir(workHook)) {
				t.Fatalf("tick %d: session %q work dir = %s, want the shared %s", n, name, got, filepath.Dir(filepath.Dir(workHook)))
			}
			snap.core[name] = runtime.CoreFingerprint(templateParamsToConfig(tp))
		}
		info, err := os.Stat(workHook)
		if err != nil {
			t.Fatalf("tick %d: shared project hooks not staged although the acp agent needs them: %v", n, err)
		}
		data, err := os.ReadFile(workHook)
		if err != nil {
			t.Fatalf("tick %d: read %s: %v", n, workHook, err)
		}
		snap.content, snap.modTime = string(data), info.ModTime()
		return snap
	}

	first := tick(1)
	for n := 2; n <= 3; n++ {
		got := tick(n)
		if got.content != first.content {
			t.Fatalf("tick %d: shared project hooks changed:\n%s\nwant (tick 1):\n%s", n, got.content, first.content)
		}
		if !got.modTime.Equal(first.modTime) {
			t.Fatalf("tick %d: shared project hooks rewritten: mtime %s, tick 1 %s", n, got.modTime, first.modTime)
		}
		for name, fp := range first.core {
			if got.core[name] != fp {
				t.Fatalf("tick %d: session %q core fingerprint moved: %s, tick 1 %s", n, name, got.core[name], fp)
			}
		}
	}
}
