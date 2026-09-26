package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/gastownhall/gascity/internal/runtime"
)

// preGCBinCodexHooks is the managed Codex hooks document a gc built before
// #6579 wrote into every workdir: PATH-prepend wrapper, bare `gc`, bound to the
// city with --city. Its canonical form hashes to 371bc5c5 for the
// /home/jaword/projects/gc-management city — the "stored" side of every
// config-drift the fleet logged after the #6579 deploy (ga-ln5ull).
const preGCBinCodexHooks = `{
  "hooks": {
    "PreCompact": [
      {
        "hooks": [
          {
            "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc --city '__CITY__' handoff --auto --hook-format codex \"context cycle\"",
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
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc --city '__CITY__' hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format codex",
            "type": "command"
          },
          {
            "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc --city '__CITY__' hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject --hook-format codex",
            "type": "command"
          }
        ],
        "matcher": ""
      }
    ]
  }
}
`

// probedCoreFingerprint returns the CoreFingerprint contribution of the
// content-probed workdir hook files, computed the way resolveTemplate does it
// (stageHookFiles). Only CopyFiles varies in this test, so equal fingerprints
// here mean equal started_config_hash / current-hash values in the reconciler.
func probedCoreFingerprint(cityDir, workDir string) (string, string) {
	copyFiles := stageHookFiles(nil, cityDir, workDir, []string{"codex"})
	hash := ""
	for _, cf := range copyFiles {
		if strings.HasSuffix(cf.RelDst, ".codex/hooks.json") {
			hash = cf.ContentHash
		}
	}
	return runtime.CoreFingerprint(runtime.Config{CopyFiles: copyFiles}), hash
}

// TestSessionStartStagingKeepsProbedHookFingerprint reproduces ga-ln5ull: a
// session start rewrites a content-probed CopyFiles file AFTER the fingerprint
// it commits as started_config_hash, so the first drift check after the start
// sees a different hash and restarts the session it just woke.
//
// Sequence, as production runs it:
//  1. Wake tick: prepareTemplateResolution runs hooks.Install on the home
//     workdir, then resolveTemplate probes .codex/hooks.json (stageHookFiles).
//     buildPreparedStart turns that probe into coreHash; CommitStartedPatch
//     stores it as started_config_hash.
//  2. Session start: the provider stages every PackOverlayDir into the SAME
//     workdir through the non-skipping StageProviderOverlayDir
//     (tmux stageStartFiles; runtime.StageSessionWorkDir for subprocess/acp).
//     The bundled core overlay is merged into .codex/hooks.json.
//  3. Next tick: hooks.Install settles the merged document, the probe runs
//     again, and the reconciler compares it with started_config_hash.
//
// When the workdir already holds the document the current overlay produces,
// 1 and 3 agree. When it holds a document an older gc wrote (here: the
// pre-#6579 shape), the merge in 2 turns it into a different, stable document
// (both SessionStart entries survive; PreCompact/UserPromptSubmit take the new
// wrapper), and 1 and 3 disagree once — one spurious config-drift restart per
// workdir, on its first start after a gc deploy changes the bundled overlay.
func TestSessionStartStagingKeepsProbedHookFingerprint(t *testing.T) {
	overlaySrc := seedCodexOverlay(t)
	startStaging := func(t *testing.T, workDir string) {
		t.Helper()
		cfg := runtime.Config{
			WorkDir:           workDir,
			ProviderName:      "codex",
			InstallAgentHooks: []string{"claude", "codex"},
			PackOverlayDirs:   []string{overlaySrc},
		}
		if err := runtime.StageSessionWorkDirWithWarnings(cfg, nil); err != nil {
			t.Fatalf("session-start staging: %v", err)
		}
	}
	wakeThenNextTick := func(t *testing.T, cityDir, workDir string) (stored, current, storedHook, currentHook string) {
		t.Helper()
		installCodex(t, cityDir, workDir) // wake tick: prepareTemplateResolution
		stored, storedHook = probedCoreFingerprint(cityDir, workDir)
		startStaging(t, workDir)
		installCodex(t, cityDir, workDir) // next tick
		current, currentHook = probedCoreFingerprint(cityDir, workDir)
		return stored, current, storedHook, currentHook
	}

	t.Run("workdir written by an older gc", func(t *testing.T) {
		cityDir := t.TempDir()
		workDir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(workDir, ".codex"), 0o755); err != nil {
			t.Fatal(err)
		}
		legacy := strings.ReplaceAll(preGCBinCodexHooks, "__CITY__", cityDir)
		if err := os.WriteFile(filepath.Join(workDir, ".codex", "hooks.json"), []byte(legacy), 0o644); err != nil {
			t.Fatal(err)
		}
		stored, current, storedHook, currentHook := wakeThenNextTick(t, cityDir, workDir)
		if stored != current {
			t.Errorf("session start changed the probed .codex/hooks.json after its fingerprint was taken:\n"+
				"  started_config_hash=%s (hooks.json %s)\n  next-tick hash      =%s (hooks.json %s)\n"+
				"the reconciler reports this as config drift and restarts the session it just woke",
				stored, storedHook[:8], current, currentHook[:8])
		}
		// Whatever the fix, the restart that follows must not drift again.
		stored2, current2, _, _ := wakeThenNextTick(t, cityDir, workDir)
		if stored2 != current2 {
			t.Errorf("second start drifted too: %s -> %s", stored2, current2)
		}
	})

	t.Run("workdir already current", func(t *testing.T) {
		cityDir := t.TempDir()
		workDir := t.TempDir()
		stored, current, _, _ := wakeThenNextTick(t, cityDir, workDir)
		if stored != current {
			t.Errorf("fresh workdir drifted across a start: %s -> %s", stored, current)
		}
	})
}

// readCodexSessionStartEntries returns the raw SessionStart hook entries
// (each a `{"hooks":[{"command":...,"type":"command"}],"matcher":...}`
// object) from a codex hooks.json document, as untyped JSON values — the
// same representation normalizeCodexManagedHookEntries operates on.
func readCodexSessionStartEntries(t *testing.T, path string) []any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	hooksMap, ok := doc["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	entries, _ := hooksMap["SessionStart"].([]any)
	return entries
}

// TestSessionStartTickCollapsesOldAndNewShapeToOne is a regression test for
// the coupled defect in ga-d9y4nr / ga-sf1dpe: normalizeCodexManagedHookEntries
// dedupes SessionStart entries by exact byte-identity only, so an old-shape
// (PATH-prepend, bare `gc`) and a new-shape (post-#6579) managed entry for the
// SAME event — each independently recognized by codexHookValueHasManagedCommand
// as "the managed SessionStart hook" — never collapse into one. A workdir that
// starts pre-#6579 and gets its first post-#6579 start ends up with the stale
// entry #6579 was meant to retire running forever alongside the new one, and
// `gc prime --hook` running twice on every future start.
//
// "Current desired shape" is derived from production code itself (a fresh
// install into an empty workdir), not hardcoded, so this test does not need
// updating if the bundled wrapper shape changes again.
func TestSessionStartTickCollapsesOldAndNewShapeToOne(t *testing.T) {
	cityDir := t.TempDir()

	// Ground truth: what a single managed SessionStart entry looks like today.
	freshWork := t.TempDir()
	installCodex(t, cityDir, freshWork)
	freshEntries := readCodexSessionStartEntries(t, filepath.Join(freshWork, ".codex", "hooks.json"))
	if len(freshEntries) != 1 {
		t.Fatalf("fresh install produced %d SessionStart entries, want exactly 1: %v", len(freshEntries), freshEntries)
	}
	currentShape := freshEntries[0]

	// Old-shape entry, as a gc built before #6579 would have left it.
	legacy := strings.ReplaceAll(preGCBinCodexHooks, "__CITY__", cityDir)
	var legacyDoc map[string]any
	if err := json.Unmarshal([]byte(legacy), &legacyDoc); err != nil {
		t.Fatalf("unmarshal legacy fixture: %v", err)
	}
	legacyEntries := legacyDoc["hooks"].(map[string]any)["SessionStart"].([]any)
	if len(legacyEntries) != 1 {
		t.Fatalf("legacy fixture has %d SessionStart entries, want exactly 1", len(legacyEntries))
	}
	oldShape := legacyEntries[0]

	// Simulate the state a start's post-fingerprint merge produces: both the
	// untouched old-shape entry and the newly-merged current-shape entry
	// present for the same event, before the next tick runs.
	mixedWork := t.TempDir()
	mixedDoc := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{oldShape, currentShape},
		},
	}
	mixedData, err := json.Marshal(mixedDoc)
	if err != nil {
		t.Fatalf("marshal mixed doc: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(mixedWork, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mixedWork, ".codex", "hooks.json"), mixedData, 0o644); err != nil {
		t.Fatal(err)
	}

	installCodex(t, cityDir, mixedWork) // the tick

	gotEntries := readCodexSessionStartEntries(t, filepath.Join(mixedWork, ".codex", "hooks.json"))
	if len(gotEntries) != 1 {
		t.Fatalf("after tick: %d SessionStart entries, want exactly 1 (old-shape and new-shape must collapse): %v", len(gotEntries), gotEntries)
	}
	gotCanon, err := overlay.MarshalCanonicalJSON(gotEntries[0])
	if err != nil {
		t.Fatalf("canonicalize got entry: %v", err)
	}
	wantCanon, err := overlay.MarshalCanonicalJSON(currentShape)
	if err != nil {
		t.Fatalf("canonicalize want entry: %v", err)
	}
	if string(gotCanon) != string(wantCanon) {
		t.Errorf("collapsed SessionStart entry = %s, want current desired shape %s", gotCanon, wantCanon)
	}
}
