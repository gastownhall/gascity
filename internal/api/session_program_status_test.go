package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

const apiProgramStatusEnvKey = "PROGRAM_STATUS_TEST_FLAG"

var apiProgramStatusArm = runtime.ProgramStatusArm{Capture: true, App: "agent"}

func apiProgramStatusBinding() config.ProgramStatusBinding {
	return config.ProgramStatusBinding{
		Protocol: config.ProgramStatusProtocolOSC7501,
		App:      "agent",
		Env:      map[string]string{apiProgramStatusEnvKey: "1"},
	}
}

func TestSessionProgramStatusDerivation(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Name:          "stub",
		Env:           map[string]string{"PROVIDER": "p", apiProgramStatusEnvKey: "provider"},
		ProgramStatus: apiProgramStatusBinding(),
	}
	arm, env := sessionProgramStatus(runtime.ProgramStatusOn, resolved, "")
	if arm != apiProgramStatusArm {
		t.Errorf("arm = %+v, want %+v", arm, apiProgramStatusArm)
	}
	if env["PROVIDER"] != "p" || env[apiProgramStatusEnvKey] != "1" {
		t.Errorf("env = %v, want provider env followed by the program-status env", env)
	}
	if resolved.Env[apiProgramStatusEnvKey] != "provider" {
		t.Error("sessionProgramStatus mutated the resolved provider env")
	}
	for name, tc := range map[string]struct {
		mode      runtime.ProgramStatusMode
		transport string
	}{
		"off": {runtime.ProgramStatusOff, ""},
		"acp": {runtime.ProgramStatusOn, config.SessionTransportACP},
	} {
		arm, env := sessionProgramStatus(tc.mode, resolved, tc.transport)
		if arm != (runtime.ProgramStatusArm{}) {
			t.Errorf("%s: arm = %+v, want disarmed", name, arm)
		}
		if env[apiProgramStatusEnvKey] != "provider" {
			t.Errorf("%s: env = %v, want the provider env untouched", name, env)
		}
	}
}

// TestEveryRuntimeConfigSiteAppliesProgramStatusArm pins the internal/api
// runtime.Config construction sites: create (resolvedSessionConfigForProvider
// and the named-session create, both through sessionCreateHints) and resume
// (buildSessionResume and resolveWorkerSessionRuntime, through
// sessionResumeHints).
func TestEveryRuntimeConfigSiteAppliesProgramStatusArm(t *testing.T) {
	resolved := &config.ResolvedProvider{Name: "stub", Command: "/bin/echo", ProgramStatus: apiProgramStatusBinding()}

	t.Run("create", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			mode      runtime.ProgramStatusMode
			transport string
			wantArm   runtime.ProgramStatusArm
		}{
			{"on", runtime.ProgramStatusOn, "", apiProgramStatusArm},
			{"shadow", runtime.ProgramStatusShadow, "", apiProgramStatusArm},
			{"off", runtime.ProgramStatusOff, "", runtime.ProgramStatusArm{}},
			{"acp", runtime.ProgramStatusOn, config.SessionTransportACP, runtime.ProgramStatusArm{}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cfg, err := resolvedSessionConfigForProvider(
					"/tmp/test-city", nil, tc.mode,
					"worker", "", "worker", "worker", tc.transport, nil,
					resolved, "", "/tmp/work", nil,
				)
				if err != nil {
					t.Fatalf("resolvedSessionConfigForProvider: %v", err)
				}
				if got := cfg.Runtime.Hints.ProgramStatus; got != tc.wantArm {
					t.Errorf("Hints.ProgramStatus = %+v, want %+v", got, tc.wantArm)
				}
				wantEnv := tc.wantArm.Capture
				if _, ok := cfg.Runtime.SessionEnv[apiProgramStatusEnvKey]; ok != wantEnv {
					t.Errorf("SessionEnv has program-status env = %v, want %v", ok, wantEnv)
				}
				if _, ok := cfg.Runtime.Hints.Env[apiProgramStatusEnvKey]; ok != wantEnv {
					t.Errorf("Hints.Env has program-status env = %v, want %v", ok, wantEnv)
				}
			})
		}
	})

	t.Run("hints", func(t *testing.T) {
		if got := sessionCreateHints(resolved, nil, nil, apiProgramStatusArm).ProgramStatus; got != apiProgramStatusArm {
			t.Errorf("sessionCreateHints ProgramStatus = %+v", got)
		}
		if got := sessionResumeHints(resolved, "", nil, nil, false, apiProgramStatusArm).ProgramStatus; got != apiProgramStatusArm {
			t.Errorf("sessionResumeHints ProgramStatus = %+v", got)
		}
	})

	newServer := func(t *testing.T, mode string) *Server {
		t.Helper()
		fs := newSessionFakeState(t)
		fs.cfg = &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Session:   config.SessionConfig{ProgramStatus: mode},
			Agents:    []config.Agent{{Name: "worker", Provider: "stub"}},
			Providers: map[string]config.ProviderSpec{"stub": {
				Command:       "/bin/echo",
				PathCheck:     "true",
				ProgramStatus: apiProgramStatusBinding(),
			}},
		}
		return New(fs)
	}
	info := session.Info{ID: "gc-1", Template: "worker", Command: "/bin/echo", Provider: "stub", WorkDir: "/tmp/workdir"}

	for _, tc := range []struct {
		mode    string
		wantArm runtime.ProgramStatusArm
	}{
		{"on", apiProgramStatusArm},
		{"off", runtime.ProgramStatusArm{}},
	} {
		t.Run("buildSessionResume "+tc.mode, func(t *testing.T) {
			_, hints, err := newServer(t, tc.mode).buildSessionResume(info)
			if err != nil {
				t.Fatalf("buildSessionResume: %v", err)
			}
			if hints.ProgramStatus != tc.wantArm {
				t.Errorf("ProgramStatus = %+v, want %+v", hints.ProgramStatus, tc.wantArm)
			}
			if _, ok := hints.Env[apiProgramStatusEnvKey]; ok != tc.wantArm.Capture {
				t.Errorf("Env has program-status env = %v, want %v", ok, tc.wantArm.Capture)
			}
		})
		t.Run("resolveWorkerSessionRuntime "+tc.mode, func(t *testing.T) {
			rt, err := newServer(t, tc.mode).resolveWorkerSessionRuntime(info)
			if err != nil {
				t.Fatalf("resolveWorkerSessionRuntime: %v", err)
			}
			if rt == nil {
				t.Fatal("resolveWorkerSessionRuntime() = nil")
			}
			if rt.Hints.ProgramStatus != tc.wantArm {
				t.Errorf("ProgramStatus = %+v, want %+v", rt.Hints.ProgramStatus, tc.wantArm)
			}
			if _, ok := rt.SessionEnv[apiProgramStatusEnvKey]; ok != tc.wantArm.Capture {
				t.Errorf("SessionEnv has program-status env = %v, want %v", ok, tc.wantArm.Capture)
			}
		})
	}
}
