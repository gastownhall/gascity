package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
	"github.com/gastownhall/gascity/internal/session"
)

// These tests reach program status only through interfaces that existed before
// exec implemented them, so a missing implementation fails an assertion.
func execDoctorProvider(t *testing.T, state string) runtime.Provider {
	t.Helper()
	script := writeRPPScript(t, `case "$1" in
 protocol) echo '{"version":0,"capabilities":["program-status","observe.v1","report-attachment"]}' ;;
 program-status) printf '{"state":"%s","incarnation":"run"}\n' '`+state+`' ;;
 inventory) echo '{"complete":true,"sessions":[{"name":"exec","state":"running","object":{"id":"box","created":"t"}}]}' ;;
 *) exit 2 ;;
 esac`)
	p := sessionexec.NewSeamBackedForCity(script, "test", t.TempDir())
	publishProgramStatusModeTo(p, runtime.ProgramStatusOn)
	return p
}

func execDoctorReading(t *testing.T, p runtime.Provider, state string) runtime.ProgramStatus {
	t.Helper()
	ps, ok := runtime.ProgramStatusFor(p, "exec")
	if !ok {
		t.Fatal("real seam-backed exec lacks ProgramStatusProvider")
	}
	got, err := ps.ProgramStatus("exec")
	validity := runtime.ProgramStatusValid
	if state == "" {
		validity = runtime.ProgramStatusSilent
	}
	if err != nil || got.Mode != runtime.ProgramStatusOn || got.Validity != validity || got.State != runtime.ProgramState(state) {
		t.Fatalf("real exec reading: %+v %v, want on %s %q", got, err, validity, state)
	}
	return got
}

func TestProgramCaptureCheckOverRealExecProvider(t *testing.T) {
	for _, state := range []string{"idle", ""} {
		for _, declared := range []bool{false, true} {
			t.Run(fmt.Sprintf("state=%s/declared=%v", state, declared), func(t *testing.T) {
				p := execDoctorProvider(t, state)
				reading := execDoctorReading(t, p, state)
				cfg := &config.City{
					Session:   config.SessionConfig{ProgramStatus: "on"},
					Providers: map[string]config.ProviderSpec{"p": {Command: "sh", PromptMode: "none"}},
					Agents:    []config.Agent{{Name: "runner", Provider: "p"}},
				}
				if declared {
					spec := cfg.Providers["p"]
					spec.ProgramStatus = programStatusTestBinding()
					cfg.Providers["p"] = spec
				}
				store := beads.NewMemStore()
				if _, err := store.Create(beads.Bead{Title: "runner", Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{"template": "runner", "session_name": "exec", "state": "active"}}); err != nil {
					t.Fatal(err)
				}
				st, err := readProgramCaptureState(cfg, func() (*sessionBeadSnapshot, error) { return loadSessionBeadSnapshot(store) }, p, nil)
				if err != nil {
					t.Fatal(err)
				}
				if len(st.Sessions) != 1 || st.Sessions[0].Name != "exec" || st.Sessions[0].ReadErr != nil || st.Sessions[0].Reading != reading {
					t.Fatalf("doctor did not consume the fixture exec reading: %+v", st.Sessions)
				}
				result := doctor.NewProgramCaptureCheck(func() (doctor.ProgramCaptureState, error) { return st, nil }).Run(&doctor.CheckContext{})
				if result.Status != doctor.StatusOK || !strings.Contains(result.Message, "; 1 session(s) reading program status from their runtime") {
					t.Errorf("runtime-reading summary: %+v", result)
				}
				for _, detail := range result.Details {
					for _, invented := range []string{"armed", "tap liveness", "companion", "relay"} {
						if strings.Contains(detail, invented) {
							t.Errorf("invented exec capture finding: %s", detail)
						}
					}
				}
			})
		}
	}
}

func TestHealOverRealExecProvider(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%v", mixed), func(t *testing.T) {
			p := execDoctorProvider(t, "idle")
			reading := execDoctorReading(t, p, "idle")
			programs := map[string]runtime.ProgramStatus{"exec": reading}
			calls := 0
			wants := func() (map[string]runtime.ProgramStatusArm, error) {
				calls++
				return map[string]runtime.ProgramStatusArm{"exec": {Capture: true}, "tmux": {Capture: true}}, nil
			}
			var f *runtime.Fake
			if mixed {
				var ff runtime.FullFake
				f, ff = healFake(t, map[string]runtime.ProgramPaneRow{"tmux": paneRow("%1", "", false)})
				p = hybrid.New(p, ff, func(name string) bool { return name == "tmux" })
				programs["tmux"] = runtime.ProgramStatus{Validity: runtime.ProgramStatusNotArmed}
			}
			healProgramCaptureReadings(p, programs, wants)
			if !mixed {
				if calls != 0 {
					t.Errorf("exec-only heal read the store %d times", calls)
				}
			} else if got := healedNames(f); calls != 1 || len(got) != 1 || got[0] != "tmux" {
				t.Errorf("mixed heal: store reads=%d healed=%v, want only tmux", calls, got)
			}
		})
	}
}
