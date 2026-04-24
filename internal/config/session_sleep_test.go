package config

import (
	"strings"
	"testing"
)

func TestParseSleepAfterIdle(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		d, off, err := ParseSleepAfterIdle("")
		if err != nil {
			t.Fatalf("ParseSleepAfterIdle(empty): %v", err)
		}
		if d != 0 || off {
			t.Fatalf("got duration=%v off=%v, want 0 false", d, off)
		}
	})

	t.Run("off", func(t *testing.T) {
		d, off, err := ParseSleepAfterIdle(" OFF ")
		if err != nil {
			t.Fatalf("ParseSleepAfterIdle(off): %v", err)
		}
		if d != 0 || !off {
			t.Fatalf("got duration=%v off=%v, want 0 true", d, off)
		}
	})

	t.Run("duration", func(t *testing.T) {
		d, off, err := ParseSleepAfterIdle("45s")
		if err != nil {
			t.Fatalf("ParseSleepAfterIdle(duration): %v", err)
		}
		if d.Seconds() != 45 || off {
			t.Fatalf("got duration=%v off=%v, want 45s false", d, off)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		if _, _, err := ParseSleepAfterIdle("bad"); err == nil {
			t.Fatal("expected invalid value to fail")
		}
	})
}

func TestNormalizeSessionSleepFieldsSetsAgentSource(t *testing.T) {
	cfg := &City{
		SessionSleep: SessionSleepConfig{
			InteractiveResume: " OFF ",
		},
		Rigs: []Rig{{
			Name: "rig-a",
			SessionSleep: SessionSleepConfig{
				NonInteractive: " 30s ",
			},
		}},
		Agents: []Agent{{
			Name:           "worker",
			SleepAfterIdle: " OFF ",
		}},
	}

	NormalizeSessionSleepFields(cfg)

	if cfg.SessionSleep.InteractiveResume != SessionSleepOff {
		t.Fatalf("workspace interactive_resume = %q, want %q", cfg.SessionSleep.InteractiveResume, SessionSleepOff)
	}
	if cfg.Rigs[0].SessionSleep.NonInteractive != "30s" {
		t.Fatalf("rig noninteractive = %q, want 30s", cfg.Rigs[0].SessionSleep.NonInteractive)
	}
	if cfg.Agents[0].SleepAfterIdle != SessionSleepOff {
		t.Fatalf("agent sleep_after_idle = %q, want %q", cfg.Agents[0].SleepAfterIdle, SessionSleepOff)
	}
	if cfg.Agents[0].SleepAfterIdleSource != "agent" {
		t.Fatalf("agent source = %q, want agent", cfg.Agents[0].SleepAfterIdleSource)
	}
}

func TestApplyAgentPatchFieldsSleepAfterIdleSetsSource(t *testing.T) {
	raw := "45s"
	agent := &Agent{Name: "worker"}
	patch := &AgentPatch{SleepAfterIdle: &raw}

	applyAgentPatchFields(agent, patch)

	if agent.SleepAfterIdle != "45s" {
		t.Fatalf("SleepAfterIdle = %q, want 45s", agent.SleepAfterIdle)
	}
	if agent.SleepAfterIdleSource != "agent_patch" {
		t.Fatalf("SleepAfterIdleSource = %q, want agent_patch", agent.SleepAfterIdleSource)
	}
}

func TestApplyAgentOverrideSleepAfterIdleSetsSource(t *testing.T) {
	raw := "0s"
	agent := &Agent{Name: "worker"}
	override := &AgentOverride{SleepAfterIdle: &raw}

	applyAgentOverride(agent, override)

	if agent.SleepAfterIdle != "0s" {
		t.Fatalf("SleepAfterIdle = %q, want 0s", agent.SleepAfterIdle)
	}
	if agent.SleepAfterIdleSource != "rig_override" {
		t.Fatalf("SleepAfterIdleSource = %q, want rig_override", agent.SleepAfterIdleSource)
	}
}

func TestValidateDurationsSleepAfterIdle(t *testing.T) {
	cfg := &City{
		SessionSleep: SessionSleepConfig{
			InteractiveResume: SessionSleepOff,
			InteractiveFresh:  "bad1",
		},
		Rigs: []Rig{{
			Name: "rig-a",
			SessionSleep: SessionSleepConfig{
				NonInteractive: "bad2",
			},
		}},
		Agents: []Agent{
			{Name: "worker", SleepAfterIdle: SessionSleepOff},
			{Name: "helper", SleepAfterIdle: "bad3"},
		},
	}

	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 3 {
		t.Fatalf("warnings = %d, want 3: %v", len(warnings), warnings)
	}
	for _, warning := range warnings {
		if strings.Contains(warning, `= "off"`) && strings.Contains(warning, "not a valid duration") {
			t.Fatalf("unexpected warning for explicit off value: %s", warning)
		}
	}
}

func TestValidateSemanticsWarnsWhenIdleTimeoutAndSleepAfterIdleSet(t *testing.T) {
	cfg := &City{
		Agents: []Agent{{
			Name:           "worker",
			IdleTimeout:    "5m",
			SleepAfterIdle: "30s",
		}},
	}

	warnings := ValidateSemantics(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("warnings = %d, want 1: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "idle_timeout") || !strings.Contains(warnings[0], "sleep_after_idle") {
		t.Fatalf("warning = %q, want idle_timeout and sleep_after_idle", warnings[0])
	}
}

func TestResolveSessionSleepPolicy(t *testing.T) {
	trueVal := true
	falseVal := false

	cfg := &City{
		SessionSleep: SessionSleepConfig{
			InteractiveResume: "60s",
			InteractiveFresh:  SessionSleepOff,
			NonInteractive:    "30s",
		},
		Rigs: []Rig{{
			Name: "payments",
			SessionSleep: SessionSleepConfig{
				InteractiveResume: "5m",
			},
		}},
	}

	t.Run("agent explicit wins", func(t *testing.T) {
		agent := &Agent{Name: "mayor", SleepAfterIdle: "10s", SleepAfterIdleSource: SessionSleepSourceAgentPatch}
		got := ResolveSessionSleepPolicy(cfg, agent)
		if got.Class != SessionSleepInteractiveResume || got.Value != "10s" || got.Source != SessionSleepSourceAgentPatch {
			t.Fatalf("ResolveSessionSleepPolicy() = %+v", got)
		}
	})

	t.Run("rig default wins by class", func(t *testing.T) {
		agent := &Agent{Name: "mayor", Dir: "payments"}
		got := ResolveSessionSleepPolicy(cfg, agent)
		if got.Class != SessionSleepInteractiveResume || got.Value != "5m" || got.Source != SessionSleepSourceRigDefault {
			t.Fatalf("ResolveSessionSleepPolicy() = %+v", got)
		}
	})

	t.Run("workspace default applies to noninteractive", func(t *testing.T) {
		agent := &Agent{Name: "worker", Attach: &falseVal}
		got := ResolveSessionSleepPolicy(cfg, agent)
		if got.Class != SessionSleepNonInteractive || got.Value != "30s" || got.Source != SessionSleepSourceWorkspaceDefault {
			t.Fatalf("ResolveSessionSleepPolicy() = %+v", got)
		}
	})

	t.Run("fresh class inherits workspace fresh", func(t *testing.T) {
		agent := &Agent{Name: "polecat", WakeMode: "fresh", Attach: &trueVal}
		got := ResolveSessionSleepPolicy(cfg, agent)
		if got.Class != SessionSleepInteractiveFresh || got.Value != SessionSleepOff || got.Source != SessionSleepSourceWorkspaceDefault {
			t.Fatalf("ResolveSessionSleepPolicy() = %+v", got)
		}
	})

	t.Run("legacy off when unset", func(t *testing.T) {
		got := ResolveSessionSleepPolicy(&City{}, &Agent{Name: "worker"})
		if got.Value != SessionSleepOff || got.Source != SessionSleepSourceLegacyOff {
			t.Fatalf("ResolveSessionSleepPolicy() = %+v", got)
		}
	})
}

func TestValidateNamedSessions_RejectsAlwaysWithSleepAfterIdle(t *testing.T) {
	cfg := &City{
		Workspace: Workspace{Name: "test-city"},
		Agents: []Agent{{
			Name:           "deacon",
			SleepAfterIdle: "30s",
		}},
		NamedSessions: []NamedSession{{
			Template: "deacon",
			Mode:     "always",
		}},
	}

	if err := ValidateNamedSessions(cfg); err == nil {
		t.Fatal("ValidateNamedSessions() = nil, want error")
	} else if !strings.Contains(err.Error(), "sleep_after_idle") {
		t.Fatalf("ValidateNamedSessions() error = %v, want mention of sleep_after_idle", err)
	}
}

func TestValidateNamedSessions_RejectsAliasSessionNameCollision(t *testing.T) {
	cfg := &City{
		Workspace: Workspace{
			Name:            "test-city",
			SessionTemplate: "sess-{{.Name}}",
		},
		Agents: []Agent{
			{Name: "mayor"},
			{Name: "sess-mayor"},
		},
		NamedSessions: []NamedSession{
			{Template: "mayor"},
			{Template: "sess-mayor"},
		},
	}

	if err := ValidateNamedSessions(cfg); err == nil {
		t.Fatal("ValidateNamedSessions() = nil, want collision error")
	} else if !strings.Contains(err.Error(), "collides with deterministic session_name") {
		t.Fatalf("ValidateNamedSessions() error = %v, want session_name collision", err)
	}
}

func TestValidateNamedSessions_RejectsQualifiedAliasName(t *testing.T) {
	cfg := &City{
		Workspace: Workspace{Name: "test-city"},
		Agents: []Agent{{
			Name: "mayor",
		}},
		NamedSessions: []NamedSession{{
			Name:     "gs.witness",
			Template: "mayor",
		}},
	}

	if err := ValidateNamedSessions(cfg); err == nil {
		t.Fatal("ValidateNamedSessions() = nil, want error")
	} else if !strings.Contains(err.Error(), `name "gs.witness"`) {
		t.Fatalf("ValidateNamedSessions() error = %v, want qualified alias rejection", err)
	}
}

func TestValidateNamedSessions_UsesResolvedWorkspaceName(t *testing.T) {
	cfg := &City{
		ResolvedWorkspaceName: "test-city",
		Workspace: Workspace{
			SessionTemplate: "{{.City}}--{{.Name}}",
		},
		Agents: []Agent{
			{Name: "mayor"},
			{Name: "test-city--mayor"},
		},
		NamedSessions: []NamedSession{
			{Template: "mayor"},
			{Template: "test-city--mayor"},
		},
	}

	if err := ValidateNamedSessions(cfg); err == nil {
		t.Fatal("ValidateNamedSessions() = nil, want collision error")
	} else if !strings.Contains(err.Error(), "collides with deterministic session_name") {
		t.Fatalf("ValidateNamedSessions() error = %v, want session_name collision", err)
	}
}
