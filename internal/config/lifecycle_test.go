package config

import (
	"testing"
	"time"
)

func TestLifecycleDefaults(t *testing.T) {
	var lc Lifecycle

	// Zero-value Lifecycle should default to immediate drain.
	agent := Agent{Lifecycle: lc}
	if got := agent.EffectiveDrainPolicy(); got != DrainPolicyImmediate {
		t.Errorf("EffectiveDrainPolicy() = %q, want %q", got, DrainPolicyImmediate)
	}
	if got := agent.GraceTimeoutDuration(); got != 0 {
		t.Errorf("GraceTimeoutDuration() = %v, want 0", got)
	}
	if got := lc.EffectiveIdleSignal(); got != IdleSignalBeadActivity {
		t.Errorf("EffectiveIdleSignal() = %q, want %q", got, IdleSignalBeadActivity)
	}
}

func TestEffectiveDrainPolicy(t *testing.T) {
	tests := []struct {
		name   string
		policy string
		want   string
	}{
		{"empty defaults to immediate", "", DrainPolicyImmediate},
		{"explicit immediate", DrainPolicyImmediate, DrainPolicyImmediate},
		{"defer_until_idle", DrainPolicyDeferUntilIdle, DrainPolicyDeferUntilIdle},
		{"unknown defaults to immediate", "bogus", DrainPolicyImmediate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := Agent{Lifecycle: Lifecycle{DrainPolicy: tt.policy}}
			if got := agent.EffectiveDrainPolicy(); got != tt.want {
				t.Errorf("EffectiveDrainPolicy() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGraceTimeoutDuration(t *testing.T) {
	tests := []struct {
		name    string
		timeout string
		want    time.Duration
	}{
		{"empty", "", 0},
		{"5m", "5m", 5 * time.Minute},
		{"30s", "30s", 30 * time.Second},
		{"1h", "1h", time.Hour},
		{"invalid", "bogus", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := Agent{Lifecycle: Lifecycle{GraceTimeout: tt.timeout}}
			if got := agent.GraceTimeoutDuration(); got != tt.want {
				t.Errorf("GraceTimeoutDuration() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEffectiveIdleSignal(t *testing.T) {
	tests := []struct {
		name   string
		signal string
		want   string
	}{
		{"empty defaults to bead_activity", "", IdleSignalBeadActivity},
		{"explicit bead_activity", IdleSignalBeadActivity, IdleSignalBeadActivity},
		{"custom value preserved", "custom_signal", "custom_signal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lc := Lifecycle{IdleSignal: tt.signal}
			if got := lc.EffectiveIdleSignal(); got != tt.want {
				t.Errorf("EffectiveIdleSignal() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyLifecyclePatch(t *testing.T) {
	strVal := func(s string) *string { return &s }

	t.Run("full patch", func(t *testing.T) {
		dst := Lifecycle{}
		src := &LifecyclePatch{
			DrainPolicy:  strVal("defer_until_idle"),
			IdleSignal:   strVal("bead_activity"),
			GraceTimeout: strVal("10m"),
		}
		applyLifecyclePatch(&dst, src)
		if dst.DrainPolicy != "defer_until_idle" {
			t.Errorf("DrainPolicy = %q, want %q", dst.DrainPolicy, "defer_until_idle")
		}
		if dst.IdleSignal != "bead_activity" {
			t.Errorf("IdleSignal = %q, want %q", dst.IdleSignal, "bead_activity")
		}
		if dst.GraceTimeout != "10m" {
			t.Errorf("GraceTimeout = %q, want %q", dst.GraceTimeout, "10m")
		}
	})

	t.Run("partial patch preserves existing", func(t *testing.T) {
		dst := Lifecycle{
			DrainPolicy:  "defer_until_idle",
			IdleSignal:   "bead_activity",
			GraceTimeout: "5m",
		}
		src := &LifecyclePatch{
			GraceTimeout: strVal("10m"),
		}
		applyLifecyclePatch(&dst, src)
		if dst.DrainPolicy != "defer_until_idle" {
			t.Errorf("DrainPolicy = %q, want %q (should be preserved)", dst.DrainPolicy, "defer_until_idle")
		}
		if dst.GraceTimeout != "10m" {
			t.Errorf("GraceTimeout = %q, want %q", dst.GraceTimeout, "10m")
		}
	})

	t.Run("nil fields are no-ops", func(t *testing.T) {
		dst := Lifecycle{DrainPolicy: "immediate"}
		src := &LifecyclePatch{}
		applyLifecyclePatch(&dst, src)
		if dst.DrainPolicy != "immediate" {
			t.Errorf("DrainPolicy = %q, want %q", dst.DrainPolicy, "immediate")
		}
	})
}

func TestLifecycleTOMLParsing(t *testing.T) {
	input := `
[[agent]]
name = "mayor"
[agent.lifecycle]
drain_policy = "defer_until_idle"
idle_signal = "bead_activity"
grace_timeout = "5m"
`
	cfg, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(cfg.Agents))
	}
	a := cfg.Agents[0]
	if a.Lifecycle.DrainPolicy != "defer_until_idle" {
		t.Errorf("DrainPolicy = %q, want %q", a.Lifecycle.DrainPolicy, "defer_until_idle")
	}
	if a.Lifecycle.IdleSignal != "bead_activity" {
		t.Errorf("IdleSignal = %q, want %q", a.Lifecycle.IdleSignal, "bead_activity")
	}
	if a.Lifecycle.GraceTimeout != "5m" {
		t.Errorf("GraceTimeout = %q, want %q", a.Lifecycle.GraceTimeout, "5m")
	}
}

func TestLifecycleTOMLOmitted(t *testing.T) {
	input := `
[[agent]]
name = "worker"
`
	cfg, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := cfg.Agents[0]
	if a.Lifecycle.DrainPolicy != "" {
		t.Errorf("DrainPolicy = %q, want empty", a.Lifecycle.DrainPolicy)
	}
	if a.EffectiveDrainPolicy() != DrainPolicyImmediate {
		t.Errorf("EffectiveDrainPolicy() = %q, want %q", a.EffectiveDrainPolicy(), DrainPolicyImmediate)
	}
}
