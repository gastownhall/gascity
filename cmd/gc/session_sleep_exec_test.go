package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/exec"
)

// writeHandshakePack writes an exec pack script whose protocol op prints
// handshake and which answers every other op as unknown.
func writeHandshakePack(t *testing.T, handshake string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pack")
	body := "#!/bin/sh\ncase \"$1\" in\n  protocol) printf '%s' '" + handshake + "' ;;\n  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// An exec pack declaring report-activity, report-attachment and proc.exec
// resolves an interactive sleep policy as requested; a pack declaring only an
// activity clock keeps today's off/interactive_capability_insufficient.
// Kills: a policy resolver that never sees the exec pack's full capability
// through the production seam-backed provider.
func TestResolveSessionSleepPolicyExecPackCapability(t *testing.T) {
	cfg := &config.City{
		SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s"},
		Agents:       []config.Agent{{Name: "worker"}},
	}
	session := makeBead("b1", map[string]string{
		"template":     "worker",
		"session_name": "worker",
	})
	tests := []struct {
		name           string
		handshake      string
		wantCapability runtime.SessionSleepCapability
		wantEffective  string
		wantReason     string
	}{
		{
			name:           "idle boundary declared",
			handshake:      `{"version":0,"capabilities":["proc.exec","report-activity","report-attachment"]}`,
			wantCapability: runtime.SessionSleepCapabilityFull,
			wantEffective:  "60s",
		},
		{
			name:           "activity only",
			handshake:      `{"version":0,"capabilities":["report-activity"]}`,
			wantCapability: runtime.SessionSleepCapabilityTimedOnly,
			wantEffective:  config.SessionSleepOff,
			wantReason:     "interactive_capability_insufficient",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := exec.NewSeamBacked(writeHandshakePack(t, tt.handshake))
			policy := resolveSessionSleepPolicyInfo(seedSessionInfo(session), cfg, sp)
			if policy.Class != config.SessionSleepInteractiveResume {
				t.Fatalf("Class = %q, want %q", policy.Class, config.SessionSleepInteractiveResume)
			}
			if policy.Capability != tt.wantCapability || policy.Effective != tt.wantEffective || policy.AdjustmentReason != tt.wantReason {
				t.Fatalf("capability/effective/reason = %q/%q/%q, want %q/%q/%q",
					policy.Capability, policy.Effective, policy.AdjustmentReason,
					tt.wantCapability, tt.wantEffective, tt.wantReason)
			}
		})
	}
}
