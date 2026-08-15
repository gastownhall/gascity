package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/runtime"
)

// seedCodexOverlay writes the real embedded core codex hooks overlay into a
// temp overlay source dir (per-provider/codex/.codex/hooks.json) so staging and
// hooks.Install operate on the same bytes the reconciler uses in production.
func seedCodexOverlay(t *testing.T) string {
	t.Helper()
	data, err := core.PackFS.ReadFile("overlay/per-provider/codex/.codex/hooks.json")
	if err != nil {
		t.Fatalf("read embedded codex hooks overlay: %v", err)
	}
	src := t.TempDir()
	dstDir := filepath.Join(src, "per-provider", "codex", ".codex")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatalf("mkdir codex overlay: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "hooks.json"), data, 0o644); err != nil {
		t.Fatalf("write codex overlay: %v", err)
	}
	return src
}

// codexSessionStartMatchers returns the "matcher" value of every SessionStart
// hook entry in a codex hooks.json document.
func codexSessionStartMatchers(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	matchers := make([]string, 0, len(doc.Hooks["SessionStart"]))
	for _, e := range doc.Hooks["SessionStart"] {
		matchers = append(matchers, e.Matcher)
	}
	return matchers
}

// driftedCodexHooks is a live hybrid captured from a drifted Codex
// agent (see #3866 / #3808): the reconciler's bound `matcher:"startup"`
// SessionStart entry coexisting with the overlay's pre-#3866 unbound
// `matcher:""` `gc prime` entry, plus the unbound PreCompact/UserPromptSubmit
// entries. `gc doctor` flags this as codex-hooks-drift ("needs upgrade")
// forever because the two writers keep re-seeding disagreeing matchers.
const driftedCodexHooks = `{
  "hooks": {
    "PreCompact": [
      {
        "hooks": [
          {
            "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc handoff --auto --hook-format codex \"context cycle\"",
            "type": "command"
          }
        ],
        "matcher": ""
      }
    ],
    "SessionStart": [
      {
        "hooks": [
          {
            "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc --city '__CITY__' prime --hook --hook-format codex",
            "type": "command"
          }
        ],
        "matcher": "startup"
      },
      {
        "hooks": [
          {
            "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc prime --hook --hook-format codex",
            "type": "command"
          }
        ],
        "matcher": ""
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format codex",
            "type": "command"
          },
          {
            "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject --hook-format codex",
            "type": "command"
          }
        ],
        "matcher": ""
      }
    ]
  }
}`

// seedDriftedHybrid writes the live hybrid fixture into workDir/.codex/hooks.json
// with its bound SessionStart entry pinned to cityDir, reproducing the drifted
// starting state a reconcile tick must converge.
func seedDriftedHybrid(t *testing.T, cityDir, workDir string) {
	t.Helper()
	dir := filepath.Join(workDir, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	body := strings.ReplaceAll(driftedCodexHooks, "__CITY__", cityDir)
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("seed drifted hybrid: %v", err)
	}
}

// stageCodex runs the staging half of the build_desired_state home-dir tick.
// skipMergeable selects the fixed (skip) vs legacy (no-skip) path.
func stageCodex(t *testing.T, overlaySrc, workDir string, skipMergeable bool) {
	t.Helper()
	var err error
	if skipMergeable {
		err = runtime.StageProviderOverlayDirSkippingMergeable(overlaySrc, workDir, []string{"codex"}, nil)
	} else {
		err = runtime.StageProviderOverlayDir(overlaySrc, workDir, []string{"codex"}, nil)
	}
	if err != nil {
		t.Fatalf("stage codex overlay (skip=%v): %v", skipMergeable, err)
	}
}

// installCodex runs the hooks.Install half of the tick on the same workDir.
func installCodex(t *testing.T, cityDir, workDir string) {
	t.Helper()
	if err := hooks.Install(fsys.OSFS{}, cityDir, workDir, []string{"codex"}); err != nil {
		t.Fatalf("hooks.Install codex: %v", err)
	}
}

// TestCodexHooksConvergeWithSkipStaging is the dual-writer reproduce+fix test.
//
// The build_desired_state home-dir tick is staging followed by hooks.Install on
// the SAME dir. Starting from the live drifted hybrid, hooks.Install converges
// the document to a single bound SessionStart entry — but the LEGACY staging
// path re-merges the overlay's unbound `matcher:""` entry back in on the very
// next tick, so the on-disk document a fresh `gc doctor`/session-start reads
// right after staging is perpetually drifted ([startup, ""]). That oscillation
// is why codex-hooks-drift never clears without --fix.
//
// The observation point that matters is therefore the post-staging state. With
// the skip path staging no longer touches the mergeable file, so the converged
// [startup] document is stable at every point in the cycle.
func TestCodexHooksConvergeWithSkipStaging(t *testing.T) {
	overlaySrc := seedCodexOverlay(t)
	cityDir := t.TempDir()

	assertSingleBound := func(t *testing.T, workDir, when string) {
		t.Helper()
		hooksPath := filepath.Join(workDir, ".codex", "hooks.json")
		matchers := codexSessionStartMatchers(t, hooksPath)
		data, _ := os.ReadFile(hooksPath)
		if len(matchers) != 1 || matchers[0] != "startup" {
			t.Fatalf("%s: SessionStart matchers = %v, want exactly [startup] (converged, bound)\n%s", when, matchers, data)
		}
		if !strings.Contains(string(data), "--city") {
			t.Fatalf("%s: converged SessionStart not bound to city root (missing --city)\n%s", when, data)
		}
		if strings.Contains(string(data), "gc prime --hook") {
			t.Fatalf("%s: unbound `gc prime` SessionStart entry still present (drift)\n%s", when, data)
		}
	}

	// assertManagedEventsIntact guards the dual-writer regression surface. Because
	// the home-dir staging path now skips the ENTIRE .codex/hooks.json, hooks.Install
	// must remain the sole, COMPLETE writer: the converged document has to keep the
	// managed PreCompact (context-cycle handoff) and UserPromptSubmit (mail check +
	// nudge drain) hooks, not just SessionStart. A future change to the installer's
	// fresh-write/upgrade path that dropped either event would otherwise slip past
	// assertSingleBound, which only inspects SessionStart.
	assertManagedEventsIntact := func(t *testing.T, workDir, when string) {
		t.Helper()
		hooksPath := filepath.Join(workDir, ".codex", "hooks.json")
		data, err := os.ReadFile(hooksPath)
		if err != nil {
			t.Fatalf("%s: read %s: %v", when, hooksPath, err)
		}
		var doc struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("%s: unmarshal %s: %v", when, hooksPath, err)
		}
		commandsFor := func(event string) string {
			var b strings.Builder
			for _, e := range doc.Hooks[event] {
				for _, h := range e.Hooks {
					b.WriteString(h.Command)
					b.WriteByte('\n')
				}
			}
			return b.String()
		}
		if !strings.Contains(commandsFor("PreCompact"), "handoff") {
			t.Fatalf("%s: converged doc dropped the managed PreCompact handoff hook\n%s", when, data)
		}
		prompt := commandsFor("UserPromptSubmit")
		if !strings.Contains(prompt, "mail check") || !strings.Contains(prompt, "nudge drain") {
			t.Fatalf("%s: converged doc dropped managed UserPromptSubmit hooks (want mail check + nudge drain)\n%s", when, data)
		}
	}

	// Fixed path: seed the drifted hybrid, then run stage → install → stage.
	// The file must be converged and bound at EVERY observation point, including
	// the post-staging states where the legacy path re-drifts.
	fixedWork := t.TempDir()
	seedDriftedHybrid(t, cityDir, fixedWork)
	stageCodex(t, overlaySrc, fixedWork, true)
	installCodex(t, cityDir, fixedWork)
	assertSingleBound(t, fixedWork, "fixed after install")
	assertManagedEventsIntact(t, fixedWork, "fixed after install")
	stageCodex(t, overlaySrc, fixedWork, true)
	assertSingleBound(t, fixedWork, "fixed after re-stage")
	assertManagedEventsIntact(t, fixedWork, "fixed after re-stage")

	// Legacy path: identical sequence with non-skip staging. After the trailing
	// staging step the unbound overlay entry is merged back in, re-creating the
	// hybrid a reconcile tick can never settle. This is the drift the fix removes.
	legacyWork := t.TempDir()
	seedDriftedHybrid(t, cityDir, legacyWork)
	stageCodex(t, overlaySrc, legacyWork, false)
	installCodex(t, cityDir, legacyWork)
	stageCodex(t, overlaySrc, legacyWork, false)
	legacyMatchers := codexSessionStartMatchers(t, filepath.Join(legacyWork, ".codex", "hooks.json"))
	if len(legacyMatchers) <= 1 {
		t.Fatalf("expected legacy non-skip staging to re-drift the hybrid (>1 SessionStart entry) after re-staging, got %v; if this no longer reproduces, the dual-write may have been fixed elsewhere — re-verify the skip is still required", legacyMatchers)
	}
}

// TestMaterializeProviderOverlays_SkipsMergeableCodexHook guards the production
// caller wiring: materializeProviderOverlaysBeforeFingerprint (the
// staging-only half of prepareTemplateResolution) must skip the reconciler-owned
// mergeable .codex/hooks.json while still staging non-mergeable overlay
// siblings. This is the observation point where skip vs non-skip staging
// diverge — the trailing hooks.Install converges either way, so only the
// staging-only state distinguishes a reverted caller wiring.
func TestMaterializeProviderOverlays_SkipsMergeableCodexHook(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	overlayDir := filepath.Join(cityDir, "packs", "myrig", "overlay")
	codexOverlay := filepath.Join(overlayDir, "per-provider", "codex", ".codex")
	if err := os.MkdirAll(codexOverlay, 0o755); err != nil {
		t.Fatalf("MkdirAll(overlay): %v", err)
	}
	if err := os.WriteFile(filepath.Join(codexOverlay, "hooks.json"), []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("write codex hooks overlay: %v", err)
	}
	sibling := filepath.Join(overlayDir, "per-provider", "codex", "AGENTS.codex.md")
	if err := os.WriteFile(sibling, []byte("codex"), 0o644); err != nil {
		t.Fatalf("write codex sibling overlay: %v", err)
	}

	codexBase := "builtin:codex"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "polecat",
			Provider: "codex",
			Scope:    "rig",
			Dir:      "myrig",
		}},
		Providers: map[string]config.ProviderSpec{
			// Explicit command + resume_command so resolution does not depend on
			// a real codex binary on PATH; base still yields the codex family.
			"codex": {Base: &codexBase, Command: "/bin/echo", ResumeCommand: "/bin/echo resume {{.SessionKey}}"},
		},
		Rigs:           []config.Rig{{Name: "myrig", Path: rigDir}},
		RigOverlayDirs: map[string][]string{"myrig": {overlayDir}},
	}

	bp := newAgentBuildParams("test-city", cityDir, cfg, runtime.NewFake(), time.Now().UTC(), nil, io.Discard)
	cfgAgent := &cfg.Agents[0]
	resolved, err := config.ResolveProvider(cfgAgent, bp.workspace, bp.providers, bp.lookPath)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	workDir, err := resolveConfiguredWorkDir(bp.cityPath, bp.cityName, "myrig/polecat", cfgAgent, bp.rigs)
	if err != nil {
		t.Fatalf("resolveConfiguredWorkDir: %v", err)
	}
	rigName := sessionSetupContextForAgent(bp.cityPath, bp.cityName, "myrig/polecat", cfgAgent, bp.rigs).Rig

	// Staging only — hooks.Install is a separate step in prepareTemplateResolution.
	materializeProviderOverlaysBeforeFingerprint(bp, cfgAgent, resolved, "myrig/polecat", rigName, workDir, io.Discard)

	if _, err := os.Stat(filepath.Join(workDir, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("build_desired_state staging wrote reconciler-owned .codex/hooks.json (err=%v); caller must use the skip variant so hooks.Install is sole writer", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "AGENTS.codex.md")); err != nil {
		t.Fatalf("non-mergeable codex overlay sibling not staged: %v", err)
	}
}
