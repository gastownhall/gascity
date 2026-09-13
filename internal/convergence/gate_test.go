package convergence

import (
	"testing"
	"time"
)

func TestParseGateConfig(t *testing.T) {
	tests := []struct {
		name    string
		meta    map[string]string
		wantCfg GateConfig
		wantErr bool
	}{
		{
			name: "manual mode",
			meta: map[string]string{
				FieldGateMode: GateModeManual,
			},
			wantCfg: GateConfig{
				Mode:          GateModeManual,
				Timeout:       DefaultGateTimeout,
				TimeoutAction: TimeoutActionIterate,
			},
		},
		{
			name: "condition mode with all fields",
			meta: map[string]string{
				FieldGateMode:          GateModeCondition,
				FieldGateCondition:     "/path/to/check.sh",
				FieldGateTimeout:       "30s",
				FieldGateTimeoutAction: TimeoutActionRetry,
			},
			wantCfg: GateConfig{
				Mode:          GateModeCondition,
				Condition:     "/path/to/check.sh",
				Timeout:       30 * time.Second,
				TimeoutAction: TimeoutActionRetry,
			},
		},
		{
			name: "hybrid mode",
			meta: map[string]string{
				FieldGateMode:      GateModeHybrid,
				FieldGateCondition: "/path/to/gate.sh",
			},
			wantCfg: GateConfig{
				Mode:          GateModeHybrid,
				Condition:     "/path/to/gate.sh",
				Timeout:       DefaultGateTimeout,
				TimeoutAction: TimeoutActionIterate,
			},
		},
		{
			name: "defaults when mode is empty",
			meta: map[string]string{},
			wantCfg: GateConfig{
				Mode:          GateModeManual,
				Timeout:       DefaultGateTimeout,
				TimeoutAction: TimeoutActionIterate,
			},
		},
		{
			name: "defaults from nil map",
			meta: nil,
			wantCfg: GateConfig{
				Mode:          GateModeManual,
				Timeout:       DefaultGateTimeout,
				TimeoutAction: TimeoutActionIterate,
			},
		},
		{
			name: "invalid mode",
			meta: map[string]string{
				FieldGateMode: "auto",
			},
			wantErr: true,
		},
		{
			name: "invalid timeout",
			meta: map[string]string{
				FieldGateMode:    GateModeCondition,
				FieldGateTimeout: "not-a-duration",
			},
			wantErr: true,
		},
		{
			name: "negative timeout",
			meta: map[string]string{
				FieldGateMode:    GateModeCondition,
				FieldGateTimeout: "-5s",
			},
			wantErr: true,
		},
		{
			name: "invalid timeout action",
			meta: map[string]string{
				FieldGateMode:          GateModeCondition,
				FieldGateTimeoutAction: "explode",
			},
			wantErr: true,
		},
		{
			name: "all timeout actions are valid",
			meta: map[string]string{
				FieldGateMode:          GateModeCondition,
				FieldGateTimeoutAction: TimeoutActionTerminate,
			},
			wantCfg: GateConfig{
				Mode:          GateModeCondition,
				Timeout:       DefaultGateTimeout,
				TimeoutAction: TimeoutActionTerminate,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParseGateConfig(tt.meta)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Mode != tt.wantCfg.Mode {
				t.Errorf("Mode = %q, want %q", cfg.Mode, tt.wantCfg.Mode)
			}
			if cfg.Condition != tt.wantCfg.Condition {
				t.Errorf("Condition = %q, want %q", cfg.Condition, tt.wantCfg.Condition)
			}
			if cfg.Timeout != tt.wantCfg.Timeout {
				t.Errorf("Timeout = %v, want %v", cfg.Timeout, tt.wantCfg.Timeout)
			}
			if cfg.TimeoutAction != tt.wantCfg.TimeoutAction {
				t.Errorf("TimeoutAction = %q, want %q", cfg.TimeoutAction, tt.wantCfg.TimeoutAction)
			}
		})
	}
}

func TestNeedsConditionExecution(t *testing.T) {
	tests := []struct {
		name string
		cfg  GateConfig
		want bool
	}{
		{
			name: "manual mode always false",
			cfg:  GateConfig{Mode: GateModeManual},
			want: false,
		},
		{
			name: "manual mode with condition still false",
			cfg:  GateConfig{Mode: GateModeManual, Condition: "/some/script"},
			want: false,
		},
		{
			name: "condition mode with condition",
			cfg:  GateConfig{Mode: GateModeCondition, Condition: "/path/to/check.sh"},
			want: true,
		},
		{
			name: "condition mode without condition",
			cfg:  GateConfig{Mode: GateModeCondition},
			want: false,
		},
		{
			name: "hybrid mode with condition",
			cfg:  GateConfig{Mode: GateModeHybrid, Condition: "/path/to/gate.sh"},
			want: true,
		},
		{
			name: "hybrid mode without condition",
			cfg:  GateConfig{Mode: GateModeHybrid},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.NeedsConditionExecution()
			if got != tt.want {
				t.Errorf("NeedsConditionExecution() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultGateTimeoutIs5Minutes(t *testing.T) {
	if DefaultGateTimeout != 5*time.Minute {
		t.Errorf("DefaultGateTimeout = %v, want 5m", DefaultGateTimeout)
	}
}

func TestGateManualResult(t *testing.T) {
	r := GateManualResult()
	if r.Outcome != GatePass {
		t.Errorf("Outcome = %q, want %q", r.Outcome, GatePass)
	}
	if r.ExitCode != nil {
		t.Errorf("ExitCode = %v, want nil", r.ExitCode)
	}
	if r.RetryCount != 0 {
		t.Errorf("RetryCount = %d, want 0", r.RetryCount)
	}
	if r.Stdout != "" {
		t.Errorf("Stdout = %q, want empty", r.Stdout)
	}
	if r.Stderr != "" {
		t.Errorf("Stderr = %q, want empty", r.Stderr)
	}
	if r.Duration != 0 {
		t.Errorf("Duration = %v, want 0", r.Duration)
	}
	if r.Truncated {
		t.Error("Truncated = true, want false")
	}
}
