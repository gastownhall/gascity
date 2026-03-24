package t3bridge

import (
	"encoding/json"
	"testing"
)

func TestBuildStartupEnvelope_DisablesReuseForNamedAgents(t *testing.T) {
	raw, err := BuildStartupEnvelope(Intent{
		AgentKind: AgentKindNamed,
		GC: GCSection{
			CityPath:    "/data/projects/gc",
			CityName:    "gc",
			Agent:       "mayor",
			Template:    "mayor",
			SessionName: "mayor",
		},
		Runtime: RuntimeSection{
			Provider:         "claude",
			Model:            "claude-opus-4-6",
			SessionTransport: "exec:/data/projects/gc/scripts/gc-session-t3",
			RuntimeMode:      "full-access",
			InteractionMode:  "default",
			WorkDir:          "/data/projects/gc",
		},
		Startup: StartupSection{
			StartupPrompt: "prompt",
		},
		RequiredProvider: "claude",
		RequiredModel:    "claude-opus-4-6",
	})
	if err != nil {
		t.Fatalf("BuildStartupEnvelope: %v", err)
	}

	var got StartupEnvelope
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Resume.AllowThreadReuse {
		t.Fatal("Resume.AllowThreadReuse = true, want false for named agent")
	}
}

func TestBuildStartupEnvelope_AllowsReuseForResumablePoolAgents(t *testing.T) {
	raw, err := BuildStartupEnvelope(Intent{
		AgentKind: AgentKindPool,
		WakeMode:  "resume",
		GC: GCSection{
			CityPath:    "/data/projects/gc",
			CityName:    "gc",
			Agent:       "t3code/codex",
			Template:    "t3code/codex",
			SessionName: "t3code--codex-1",
		},
		Runtime: RuntimeSection{
			Provider:         "codex",
			Model:            "gpt-5-codex",
			SessionTransport: "exec:/data/projects/gc/scripts/gc-session-t3",
			RuntimeMode:      "full-access",
			InteractionMode:  "default",
			WorkDir:          "/data/projects/t3code",
		},
		Startup: StartupSection{
			StartupPrompt: "prompt",
		},
		RequiredProvider: "codex",
		RequiredModel:    "gpt-5-codex",
	})
	if err != nil {
		t.Fatalf("BuildStartupEnvelope: %v", err)
	}

	var got StartupEnvelope
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Resume.AllowThreadReuse {
		t.Fatal("Resume.AllowThreadReuse = false, want true for pool agent with wake_mode=resume")
	}
}

func TestDecideThreadReuse(t *testing.T) {
	desired := StartupEnvelope{
		GC: GCSection{Agent: "t3code/codex", Template: "t3code/codex"},
		Runtime: RuntimeSection{
			Provider: "codex",
			Model:    "gpt-5-codex",
			WorkDir:  "/data/projects/t3code",
		},
		Resume: ResumeSection{AllowThreadReuse: true},
	}
	stored := desired

	got := DecideThreadReuse(ReuseCheck{
		Desired:       desired,
		Stored:        &stored,
		ThreadActive:  true,
		ProjectActive: true,
	})
	if got.Decision != ReuseDecisionReuse {
		t.Fatalf("Decision = %q, want %q", got.Decision, ReuseDecisionReuse)
	}

	desired.Resume.AllowThreadReuse = false
	got = DecideThreadReuse(ReuseCheck{
		Desired:       desired,
		Stored:        &stored,
		ThreadActive:  true,
		ProjectActive: true,
	})
	if got.Decision != ReuseDecisionRecreate || got.Reason != "reuse-disabled" {
		t.Fatalf("got = %#v, want recreate/reuse-disabled", got)
	}
}
