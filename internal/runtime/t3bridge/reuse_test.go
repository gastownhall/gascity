package t3bridge

import "testing"

func TestDecideThreadReuse_ExactMatch(t *testing.T) {
	env := baseEnvelope()
	result := DecideThreadReuse(ReuseCheck{
		Desired:       env,
		Stored:        &env,
		ThreadActive:  true,
		ProjectActive: true,
	})
	if result.Decision != ReuseDecisionReuse {
		t.Errorf("expected reuse, got %s (%s)", result.Decision, result.Reason)
	}
}

func TestDecideThreadReuse_ReuseDisabled(t *testing.T) {
	env := baseEnvelope()
	env.Resume.AllowThreadReuse = false
	result := DecideThreadReuse(ReuseCheck{
		Desired:       env,
		Stored:        &env,
		ThreadActive:  true,
		ProjectActive: true,
	})
	if result.Decision != ReuseDecisionRecreate || result.Reason != "reuse-disabled" {
		t.Errorf("expected recreate/reuse-disabled, got %s/%s", result.Decision, result.Reason)
	}
}

func TestDecideThreadReuse_WorkdirMismatch_AlwaysRecreate(t *testing.T) {
	desired := baseEnvelope()
	desired.Resume.AllowRuntimeRebind = true
	stored := baseEnvelope()
	stored.Runtime.WorkDir = "/other/dir"
	result := DecideThreadReuse(ReuseCheck{
		Desired:       desired,
		Stored:        &stored,
		ThreadActive:  true,
		ProjectActive: true,
	})
	if result.Decision != ReuseDecisionRecreate || result.Reason != "workdir-mismatch" {
		t.Errorf("expected recreate/workdir-mismatch, got %s/%s", result.Decision, result.Reason)
	}
}

func TestDecideThreadReuse_ProviderMismatch_RebindAllowed(t *testing.T) {
	desired := baseEnvelope()
	desired.Resume.AllowRuntimeRebind = true
	desired.Runtime.Provider = "codex"
	stored := baseEnvelope()
	result := DecideThreadReuse(ReuseCheck{
		Desired:       desired,
		Stored:        &stored,
		ThreadActive:  true,
		ProjectActive: true,
	})
	if result.Decision != ReuseDecisionRebind || result.Reason != "provider-rebind" {
		t.Errorf("expected rebind/provider-rebind, got %s/%s", result.Decision, result.Reason)
	}
}

func baseEnvelope() StartupEnvelope {
	return StartupEnvelope{
		Version: 1,
		GC: GCSection{
			CityPath:    "/data/projects/gc",
			CityName:    "gc",
			Agent:       "worker-1",
			Template:    "pool/worker",
			SessionName: "gc--worker-1",
		},
		Runtime: RuntimeSection{
			Provider:    "claude",
			Model:       "claude-opus-4-6",
			WorkDir:     "/data/projects/gc",
			RuntimeMode: "headless",
		},
		Resume: ResumeSection{
			Policy:                 "match-or-recreate",
			AllowThreadReuse:       true,
			RequiredThreadProvider: "claude",
		},
	}
}
