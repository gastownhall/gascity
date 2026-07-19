package config

import (
	"strings"
	"testing"
)

func TestValidateDurationsAllValid(t *testing.T) {
	cfg := &City{
		Session: SessionConfig{
			SetupTimeout:       "10s",
			NudgeReadyTimeout:  "10s",
			NudgeRetryInterval: "500ms",
			NudgeLockTimeout:   "30s",
			StartupTimeout:     "60s",
		},
		Daemon: DaemonConfig{
			PatrolInterval:    "30s",
			RestartWindow:     "1h",
			ShutdownTimeout:   "5s",
			DriftDrainTimeout: "2m",
		},
		Agents: []Agent{
			{Name: "mayor", IdleTimeout: "15m"},
			{Name: "worker", DrainTimeout: "5m"},
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for valid config, got: %v", warnings)
	}
}

func TestValidateDurationsEmptyFieldsOK(t *testing.T) {
	cfg := &City{}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for empty config, got: %v", warnings)
	}
}

func TestValidateDurationsBadOrderOverrideCheckTimeout(t *testing.T) {
	bad := "60" // missing unit
	cfg := &City{
		Orders: OrdersConfig{
			Overrides: []OrderOverride{
				{Name: "pr-merge-queue", CheckTimeout: &bad},
			},
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "pr-merge-queue") {
		t.Errorf("warning should mention override name: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "check_timeout") {
		t.Errorf("warning should mention field name: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "60") {
		t.Errorf("warning should mention bad value: %s", warnings[0])
	}
}

func TestValidateDurationsNonPositiveOrderOverrideCheckTimeout(t *testing.T) {
	// A zero/negative override check_timeout parses but silently reverts the
	// condition probe to the 10s default at dispatch, so it must warn.
	zero := "0s"
	cfg := &City{
		Orders: OrdersConfig{
			Overrides: []OrderOverride{
				{Name: "pr-merge-queue", CheckTimeout: &zero},
			},
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "must be a positive duration") {
		t.Errorf("warning should flag non-positive duration: %s", warnings[0])
	}
}

func TestValidateDurationsValidOrderOverrideCheckTimeout(t *testing.T) {
	good := "60s"
	cfg := &City{
		Orders: OrdersConfig{
			Overrides: []OrderOverride{
				{Name: "pr-merge-queue", CheckTimeout: &good},
			},
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for valid override check_timeout, got: %v", warnings)
	}
}

func TestValidateDurationsBadAgentIdleTimeout(t *testing.T) {
	cfg := &City{
		Agents: []Agent{
			{Name: "mayor", IdleTimeout: "5mins"},
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "mayor") {
		t.Errorf("warning should mention agent name: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "idle_timeout") {
		t.Errorf("warning should mention field name: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "5mins") {
		t.Errorf("warning should mention bad value: %s", warnings[0])
	}
}

func TestValidateDurationsBadSessionTimeout(t *testing.T) {
	cfg := &City{
		Session: SessionConfig{SetupTimeout: "ten seconds"},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "[session]") {
		t.Errorf("warning should mention section: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "setup_timeout") {
		t.Errorf("warning should mention field: %s", warnings[0])
	}
}

func TestValidateDurationsBadMailRetentionTTL(t *testing.T) {
	cfg := &City{
		Mail: MailConfig{RetentionTTL: "7d"},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "[mail]") {
		t.Errorf("warning should mention section: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "retention_ttl") {
		t.Errorf("warning should mention field: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "7d") {
		t.Errorf("warning should mention bad value: %s", warnings[0])
	}
}

func TestValidateDurationsBadDaemonFields(t *testing.T) {
	cfg := &City{
		Daemon: DaemonConfig{
			PatrolInterval:                  "30sec",
			RestartWindow:                   "one hour",
			ShutdownTimeout:                 "5 seconds",
			SessionCircuitBreakerWindow:     "ten minutes",
			SessionCircuitBreakerResetAfter: "twenty minutes",
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 5 {
		t.Fatalf("expected 5 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestValidateDurationsBadBeadPolicyDuration(t *testing.T) {
	cfg := &City{
		Beads: BeadsConfig{
			Policies: map[string]BeadPolicyConfig{
				"control": {DeleteAfterClose: "7days"},
			},
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	for _, want := range []string{"[beads.policies.control]", "delete_after_close", "7days"} {
		if !strings.Contains(warnings[0], want) {
			t.Errorf("warning = %q, want substring %q", warnings[0], want)
		}
	}
}

func TestValidateDurationsRejectsUnsafeBeadPolicyDuration(t *testing.T) {
	tests := []string{"-1h", "0s", "1d-48h", "200000d"}
	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			cfg := &City{
				Beads: BeadsConfig{
					Policies: map[string]BeadPolicyConfig{
						"control": {DeleteAfterClose: value},
					},
				},
			}
			warnings := ValidateDurations(cfg, "city.toml")
			if len(warnings) != 1 {
				t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
			}
			for _, want := range []string{"[beads.policies.control]", "delete_after_close", value} {
				if !strings.Contains(warnings[0], want) {
					t.Errorf("warning = %q, want substring %q", warnings[0], want)
				}
			}
		})
	}
}

func TestValidateDurationsBadBeadPolicyStorage(t *testing.T) {
	cfg := &City{
		Beads: BeadsConfig{
			Policies: map[string]BeadPolicyConfig{
				"control": {Storage: "forever-ish"},
			},
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	for _, want := range []string{"[beads.policies.control]", "storage", "forever-ish"} {
		if !strings.Contains(warnings[0], want) {
			t.Errorf("warning = %q, want substring %q", warnings[0], want)
		}
	}
}

func TestValidateDurationsRejectsNonCanonicalBeadPolicyStorage(t *testing.T) {
	tests := []string{"no-history", "EPHEMERAL"}
	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			cfg := &City{
				Beads: BeadsConfig{
					Policies: map[string]BeadPolicyConfig{
						"control": {Storage: value},
					},
				},
			}
			warnings := ValidateDurations(cfg, "city.toml")
			if len(warnings) != 1 {
				t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
			}
			for _, want := range []string{"[beads.policies.control]", "storage", value} {
				if !strings.Contains(warnings[0], want) {
					t.Errorf("warning = %q, want substring %q", warnings[0], want)
				}
			}
		})
	}
}

func TestValidateBeadPolicyStorageCompatibilityAllowsBD104SafePolicies(t *testing.T) {
	cfg := &City{
		Beads: BeadsConfig{
			Policies: map[string]BeadPolicyConfig{
				"session":        {Storage: BeadStorageNoHistory},
				"wait":           {Storage: BeadStorageNoHistory},
				"nudge":          {Storage: BeadStorageNoHistory},
				"order_tracking": {Storage: BeadStorageNoHistory},
				"workflow":       {Storage: BeadStorageHistory},
				"wisp":           {Storage: BeadStorageHistory},
			},
		},
	}
	if err := ValidateBeadPolicyStorageCompatibility(cfg, "city.toml"); err != nil {
		t.Fatalf("ValidateBeadPolicyStorageCompatibility: %v", err)
	}

	cfg.Beads.Policies["session"] = BeadPolicyConfig{Storage: BeadStorageHistory}
	cfg.Beads.Policies["wait"] = BeadPolicyConfig{Storage: BeadStorageHistory}
	cfg.Beads.Policies["nudge"] = BeadPolicyConfig{Storage: BeadStorageHistory}
	cfg.Beads.Policies["order_tracking"] = BeadPolicyConfig{Storage: BeadStorageHistory}
	cfg.Beads.Policies["workflow"] = BeadPolicyConfig{Storage: BeadStorageHistory}
	cfg.Beads.Policies["wisp"] = BeadPolicyConfig{Storage: BeadStorageHistory}
	if err := ValidateBeadPolicyStorageCompatibility(cfg, "city.toml"); err != nil {
		t.Fatalf("ValidateBeadPolicyStorageCompatibility with history overrides: %v", err)
	}
}

func TestValidateBeadPolicyStorageCompatibilityAllowsBD105SafePolicies(t *testing.T) {
	cfg := &City{
		Beads: BeadsConfig{
			BDCompatibility: BeadsBDCompatibility105,
			Policies: map[string]BeadPolicyConfig{
				"session":        {Storage: BeadStorageNoHistory},
				"wait":           {Storage: BeadStorageNoHistory},
				"nudge":          {Storage: BeadStorageNoHistory},
				"order_tracking": {Storage: BeadStorageNoHistory},
				"workflow":       {Storage: BeadStorageNoHistory},
				"wisp":           {Storage: BeadStorageEphemeral},
			},
		},
	}
	if err := ValidateBeadPolicyStorageCompatibility(cfg, "city.toml"); err != nil {
		t.Fatalf("ValidateBeadPolicyStorageCompatibility: %v", err)
	}

	cfg.Beads.Policies["wisp"] = BeadPolicyConfig{Storage: BeadStorageNoHistory}
	if err := ValidateBeadPolicyStorageCompatibility(cfg, "city.toml"); err != nil {
		t.Fatalf("ValidateBeadPolicyStorageCompatibility with no-history wisp override: %v", err)
	}
}

func TestValidateBeadPolicyStorageCompatibilityRejectsBD104UnsafeOverrides(t *testing.T) {
	tests := []struct {
		name    string
		policy  string
		storage string
	}{
		{name: "wisp no-history", policy: "wisp", storage: BeadStorageNoHistory},
		{name: "wisp ephemeral", policy: "wisp", storage: BeadStorageEphemeral},
		{name: "session ephemeral", policy: "session", storage: BeadStorageEphemeral},
		{name: "wait ephemeral", policy: "wait", storage: BeadStorageEphemeral},
		{name: "nudge ephemeral", policy: "nudge", storage: BeadStorageEphemeral},
		{name: "order tracking ephemeral", policy: "order_tracking", storage: BeadStorageEphemeral},
		{name: "workflow no-history", policy: "workflow", storage: BeadStorageNoHistory},
		{name: "workflow ephemeral", policy: "workflow", storage: BeadStorageEphemeral},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &City{
				Beads: BeadsConfig{
					Policies: map[string]BeadPolicyConfig{
						tt.policy: {Storage: tt.storage},
					},
				},
			}
			err := ValidateBeadPolicyStorageCompatibility(cfg, "city.toml")
			if err == nil {
				t.Fatal("ValidateBeadPolicyStorageCompatibility = nil, want error")
			}
			msg := err.Error()
			for _, want := range []string{"city.toml", "[beads.policies." + tt.policy + "]", tt.storage, BeadsBDCompatibility104} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error = %q, want substring %q", msg, want)
				}
			}
		})
	}
}

func TestValidateDurationsBadPoolDrainTimeout(t *testing.T) {
	cfg := &City{
		Agents: []Agent{
			{Name: "worker", Dir: "hw", DrainTimeout: "5min"},
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "hw/worker") {
		t.Errorf("warning should mention qualified name: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "drain_timeout") {
		t.Errorf("warning should mention field: %s", warnings[0])
	}
}

func TestValidateDurationsMultipleIssues(t *testing.T) {
	cfg := &City{
		Session: SessionConfig{NudgeReadyTimeout: "bad1"},
		Daemon:  DaemonConfig{WispGCInterval: "bad2", WispTTL: "bad3"},
		Orders:  OrdersConfig{MaxTimeout: "bad4"},
		Agents: []Agent{
			{Name: "a1", IdleTimeout: "bad5"},
		},
	}
	warnings := ValidateDurations(cfg, "test.toml")
	if len(warnings) != 5 {
		t.Fatalf("expected 5 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestValidateDurationsBadMaintenanceDoltFields(t *testing.T) {
	cfg := &City{
		Maintenance: MaintenanceConfig{
			Dolt: DoltMaintenance{
				Interval:  "one week",
				GCTimeout: "ten minutes",
			},
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d: %v", len(warnings), warnings)
	}
	joined := strings.Join(warnings, "|")
	if !strings.Contains(joined, "[maintenance.dolt]") {
		t.Errorf("warnings should mention section [maintenance.dolt]: %v", warnings)
	}
	if !strings.Contains(joined, "interval") {
		t.Errorf("warnings should mention interval field: %v", warnings)
	}
	if !strings.Contains(joined, "gc_timeout") {
		t.Errorf("warnings should mention gc_timeout field: %v", warnings)
	}
}

func TestValidateDurationsMaintenanceDoltValidOK(t *testing.T) {
	cfg := &City{
		Maintenance: MaintenanceConfig{
			Dolt: DoltMaintenance{
				Enabled:   true,
				Interval:  "168h",
				GCTimeout: "10m",
			},
		},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for valid maintenance.dolt, got: %v", warnings)
	}
}

func TestValidateDurationsIncludesSource(t *testing.T) {
	cfg := &City{
		Session: SessionConfig{SetupTimeout: "invalid"},
	}
	warnings := ValidateDurations(cfg, "/path/to/city.toml")
	if len(warnings) == 0 {
		t.Fatal("expected warning")
	}
	if !strings.Contains(warnings[0], "/path/to/city.toml") {
		t.Errorf("warning should include source path: %s", warnings[0])
	}
}

func TestValidateDurationsBadChatSessionsGracePeriod(t *testing.T) {
	cfg := &City{
		ChatSessions: ChatSessionsConfig{GracePeriod: "bogus"},
	}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "grace_period") {
		t.Errorf("warning should mention field name: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "bogus") {
		t.Errorf("warning should mention bad value: %s", warnings[0])
	}
}
