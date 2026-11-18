package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
	"github.com/gastownhall/gascity/internal/session"
)

func statusScript(t *testing.T, answers ...string) string {
	answer := `echo '{"state":"idle","incarnation":"run"}'`
	if len(answers) > 0 {
		answer = answers[0]
	}
	return writeRPPScript(t, `case "$1" in
 protocol) echo '{"version":0,"capabilities":["program-status","observe.v1","report-attachment"]}' ;;
 program-status) `+answer+` ;;
 inventory) echo '{"complete":true,"sessions":[{"name":"mc-a","state":"running","object":{"id":"box","created":"t"}}]}' ;;
 *) exit 2 ;;
 esac`)
}

func TestPublishProgramStatusModeReachesExec(t *testing.T) {
	p := sessionexec.NewSeamBackedForCity(statusScript(t), "test", t.TempDir())
	for _, root := range []runtime.Provider{p, hybrid.New(p, runtime.NewFake(), func(string) bool { return false })} {
		for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusOn, runtime.ProgramStatusShadow, runtime.ProgramStatusOff, runtime.ProgramStatusOn} {
			publishProgramStatusModeTo(root, mode)
			ps, ok := runtime.ProgramStatusFor(root, "mc-a")
			if !ok {
				t.Error("seam-backed exec lacks ProgramStatusProvider")
				return
			}
			got, err := ps.ProgramStatus("mc-a")
			if err != nil || got.Mode != mode || (mode != runtime.ProgramStatusOff && !got.Valid()) {
				t.Errorf("mode %s: %+v %v", mode, got, err)
			}
		}
	}
}

func TestRuntimeRegistryExecCarriesProgramStatusMode(t *testing.T) {
	script := statusScript(t)
	cfg := &config.City{Runtimes: map[string]config.DiscoveredRuntime{"statuspack": {Name: "statuspack", Command: script}}}
	reg, err := runtimeRegistryForCity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, selection := range []string{"exec:" + script, "statuspack"} {
		p, err := reg.New(selection, config.SessionConfig{ProgramStatus: "shadow"}, "test", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		ps, ok := runtime.ProgramStatusFor(p, "s")
		if !ok {
			t.Fatalf("%s lacks status provider", selection)
		}
		got, err := ps.ProgramStatus("s")
		if err != nil || !got.Valid() || got.Mode != runtime.ProgramStatusShadow {
			t.Errorf("%s: %+v %v", selection, got, err)
		}
	}
}

func TestInventoryLaneReceivesExecProgramStatus(t *testing.T) {
	p := sessionexec.NewSeamBacked(statusScript(t))
	if setter, ok := p.(interface {
		SetProgramStatusMode(runtime.ProgramStatusMode)
	}); ok {
		setter.SetProgramStatusMode(runtime.ProgramStatusShadow)
	} else {
		t.Error("missing mode setter")
	}
	listing := listInventoryBackends(p, "exec")
	var backends []BackendPass
	for _, leaf := range listing.leaves {
		backends = append(backends, classifyInventoryBackend(leaf))
	}
	lane := &runtimeInventoryLane{clock: &clock.Fake{Time: obsTestEpoch}, interval: time.Minute}
	_, _, programs, errs := lane.enrich(context.Background(), listing.leaves, backends)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if got := programs["mc-a"]; !got.Valid() || got.State != runtime.ProgramIdle || got.Mode != runtime.ProgramStatusShadow {
		t.Errorf("inventory lane received %+v, want shadow idle", got)
	}
}

func TestInventoryPassHealthUnchangedByStatusFailure(t *testing.T) {
	for _, answer := range []string{`exit 66`, `exit 2`, `echo malformed`, `exec sleep 30`} {
		p := sessionexec.NewSeamBacked(statusScript(t, answer))
		if setter, ok := p.(interface {
			SetProgramStatusMode(runtime.ProgramStatusMode)
		}); ok {
			setter.SetProgramStatusMode(runtime.ProgramStatusShadow)
		}
		listing := listInventoryBackends(p, "exec")
		if len(listing.leaves) != 1 {
			t.Fatalf("leaves: %+v", listing.leaves)
		}
		backend := classifyInventoryBackend(listing.leaves[0])
		if backend.Outcome != OutcomeComplete || !backend.Attested || backend.Err != nil {
			t.Errorf("%s changed backend health: %+v", answer, backend)
		}
		lane := &runtimeInventoryLane{clock: &clock.Fake{Time: obsTestEpoch}, interval: time.Minute}
		_, _, programs, errs := lane.enrich(t.Context(), listing.leaves, []BackendPass{backend})
		reading, found := programs["mc-a"]
		if !found || len(errs) != 0 || reading.Validity != runtime.ProgramStatusUnread {
			t.Errorf("%s: programs %+v errors %v", answer, programs, errs)
		}
	}
}

type nonCapturingStatusLeaf struct {
	runtime.Provider
	reading runtime.ProgramStatus
	err     error
}

func (p nonCapturingStatusLeaf) ProgramStatus(string) (runtime.ProgramStatus, error) {
	return p.reading, p.err
}

func TestProgramCaptureCheckIgnoresNonCapturingLeaf(t *testing.T) {
	for _, valid := range []runtime.ProgramStatusValidity{runtime.ProgramStatusValid, runtime.ProgramStatusSilent, runtime.ProgramStatusUnread} {
		for _, declared := range []bool{false, true} {
			f := runtime.NewFake()
			if err := f.Start(t.Context(), "s", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			cfg := &config.City{Session: config.SessionConfig{ProgramStatus: "on"}, Providers: map[string]config.ProviderSpec{"p": {Command: "sh", PromptMode: "none"}}, Agents: []config.Agent{{Name: "runner", Provider: "p"}}}
			if declared {
				spec := cfg.Providers["p"]
				spec.ProgramStatus = programStatusTestBinding()
				cfg.Providers["p"] = spec
			}
			store := beads.NewMemStore()
			if _, err := store.Create(beads.Bead{Title: "runner", Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{"template": "runner", "session_name": "s", "state": "active"}}); err != nil {
				t.Fatal(err)
			}
			reading := runtime.ProgramStatus{Mode: runtime.ProgramStatusOn, Validity: valid}
			if valid == runtime.ProgramStatusValid {
				reading.State = runtime.ProgramIdle
			}
			leaf := nonCapturingStatusLeaf{Provider: f, reading: reading}
			if valid == runtime.ProgramStatusUnread {
				leaf.err = runtime.ErrRuntimeUnavailable
			}
			st, err := readProgramCaptureState(cfg, func() (*sessionBeadSnapshot, error) { return loadSessionBeadSnapshot(store) }, leaf, nil)
			if err != nil {
				t.Fatal(err)
			}
			result := doctor.NewProgramCaptureCheck(func() (doctor.ProgramCaptureState, error) { return st, nil }).Run(&doctor.CheckContext{})
			if result.Status != doctor.StatusOK {
				t.Errorf("declared=%v validity=%s: %+v", declared, valid, result)
			}
			for _, detail := range result.Details {
				if strings.Contains(detail, "armed but") || strings.Contains(detail, "tap liveness") {
					t.Errorf("invented capture finding: %s", detail)
				}
			}
		}
	}
}

func TestHealSkipsLeavesWithoutHealer(t *testing.T) {
	plain := nonCapturingStatusLeaf{Provider: runtime.NewFake()}
	calls := 0
	wants := func() (map[string]runtime.ProgramStatusArm, error) {
		calls++
		return map[string]runtime.ProgramStatusArm{"exec": {}, "tmux": {Capture: true}}, nil
	}
	programs := map[string]runtime.ProgramStatus{"exec": {Validity: runtime.ProgramStatusValid}}
	healProgramCaptureReadings(plain, programs, wants)
	if calls != 0 {
		t.Error("non-capturing leaf caused a store read")
	}
	f, ff := healFake(t, map[string]runtime.ProgramPaneRow{"tmux": paneRow("%1", "", false)})
	composite := hybrid.New(plain, ff, func(name string) bool { return name == "tmux" })
	programs["tmux"] = runtime.ProgramStatus{Validity: runtime.ProgramStatusNotArmed}
	healProgramCaptureReadings(composite, programs, wants)
	if got := healedNames(f); len(got) != 1 || got[0] != "tmux" {
		t.Errorf("healed %v", got)
	}
}
