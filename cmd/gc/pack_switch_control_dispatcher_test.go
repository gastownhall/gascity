package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/gastownhall/gascity/internal/runtime"
)

// resolvedControlDispatcher is one control-dispatcher agent as the controller
// resolves it for a session start.
type resolvedControlDispatcher struct {
	agent  config.Agent
	prompt string
	start  runtime.Config
}

// resolveControlDispatchers loads the city and resolves every
// control-dispatcher agent through the controller's template path, keyed by
// qualified name.
func resolveControlDispatchers(t *testing.T, cityPath string) map[string]resolvedControlDispatcher {
	t.Helper()
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("loading %s: %v", cityPath, err)
	}
	params := newReadOnlyAgentBuildParams(loadedCityName(cfg, cityPath), cityPath, cfg,
		func(name string) (string, error) { return "/usr/bin/" + name, nil }, testBeaconTime, io.Discard)
	resolved := make(map[string]resolvedControlDispatcher)
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Name != config.ControlDispatcherAgentName {
			continue
		}
		qualified := a.QualifiedName()
		if _, dup := resolved[qualified]; dup {
			t.Fatalf("city resolves two control-dispatchers for %q", qualified)
		}
		tp, err := resolveTemplate(params, a, qualified, nil)
		if err != nil {
			t.Fatalf("resolveTemplate(%s): %v", qualified, err)
		}
		resolved[qualified] = resolvedControlDispatcher{
			agent:  *a,
			prompt: tp.Prompt,
			start:  templateParamsToConfig(tp),
		}
	}
	return resolved
}

// TestPackSwitchControlDispatchersStayDeterministicAndStable pins AC-7 of
// issue #1689: a city initialized from the minimal template, given one rig,
// and then switched to the gastown template at the same path with the same
// rig resolves exactly one city and one rig core.control-dispatcher, each the
// deterministic convoy control process (no startup prompt, no nudge, no
// instruction to run gc prime) following its own qualified name, with the
// same core fingerprint as before the switch, so the controller keeps both
// sessions instead of restarting them.
func TestPackSwitchControlDispatchersStayDeterministicAndStable(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	cityPath := filepath.Join(t.TempDir(), "city")
	rigPath := filepath.Join(t.TempDir(), "myrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}
	addRig := func(t *testing.T) {
		t.Helper()
		var out, errBuf bytes.Buffer
		if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &out, &errBuf); code != 0 {
			t.Fatalf("doRigAdd = %d; stderr: %s", code, errBuf.String())
		}
	}

	// Phase 1: minimal template, then the rig.
	var out, errBuf bytes.Buffer
	wiz := wizardConfig{configName: "minimal", defaultProvider: "codex", providers: []string{"codex"}}
	if code := doInit(fsys.OSFS{}, cityPath, wiz, "", &out, &errBuf, false); code != 0 {
		t.Fatalf("doInit(minimal) = %d; stderr: %s", code, errBuf.String())
	}
	addRig(t)
	before := resolveControlDispatchers(t, cityPath)

	// Phase 2: switch to the gastown template at the same path, same rig.
	// `gc init --from` refuses an initialized city, so the switch is the
	// template copy it would have made: overlay the shipped gastown example
	// onto the existing city, then re-register the same rig (the example's
	// city.toml carries no rigs table of its own).
	src := gastownExamplePath(t)
	errBuf.Reset()
	if err := overlay.CopyDirWithSkip(src, cityPath, initFromSkipForSourceFS(fsys.OSFS{}, src), &errBuf); err != nil {
		t.Fatalf("copying the gastown template over the city: %v\nstderr: %s", err, errBuf.String())
	}
	addRig(t)

	switched, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("loading the switched city: %v", err)
	}
	if !slices.ContainsFunc(switched.Agents, func(a config.Agent) bool { return a.QualifiedName() == "gastown.mayor" }) {
		t.Fatal("precondition: the switched city does not compose the gastown pack (no gastown.mayor agent)")
	}
	after := resolveControlDispatchers(t, cityPath)

	want := []string{"core.control-dispatcher", "myrig/core.control-dispatcher"}
	for label, resolved := range map[string]map[string]resolvedControlDispatcher{"before the switch": before, "after the switch": after} {
		var got []string
		for name := range resolved {
			got = append(got, name)
		}
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Fatalf("control dispatchers %s = %q, want exactly %q", label, got, want)
		}
	}
	for _, name := range want {
		d := after[name]
		if !config.IsDeterministicControlDispatcher(&d.agent) {
			t.Errorf("%s is not the deterministic control dispatcher after the switch (provider %q, start_command %q)", name, d.agent.Provider, d.agent.StartCommand)
		}
		if d.prompt != "" || d.start.PromptSuffix != "" || d.start.Nudge != "" {
			t.Errorf("%s gets a startup prompt or nudge after the switch: prompt %q, prompt suffix %q, nudge %q", name, d.prompt, d.start.PromptSuffix, d.start.Nudge)
		}
		for _, text := range []string{d.prompt, d.start.PromptSuffix, d.start.Nudge} {
			if strings.Contains(text, "gc prime") {
				t.Errorf("%s is told to run gc prime after the switch: %q", name, text)
			}
		}
		if !slices.Equal(d.start.ProcessNames, []string{"gc"}) {
			t.Errorf("%s process_names = %q, want [gc]", name, d.start.ProcessNames)
		}
		if follow := "convoy control --serve --follow " + name; !strings.Contains(d.start.Command, follow) {
			t.Errorf("%s start command does not run %q:\n%s", name, follow, d.start.Command)
		}
		if got, prev := runtime.CoreFingerprint(d.start), runtime.CoreFingerprint(before[name].start); got != prev {
			t.Errorf("%s core fingerprint changed across the switch: %s -> %s", name, prev, got)
		}
		if drift := runtime.CoreFingerprintDriftFields(runtime.CoreFingerprintBreakdown(before[name].start), d.start); len(drift) != 0 {
			t.Errorf("%s drifts across the switch on %q, want no drift fields", name, drift)
		}
	}
}
