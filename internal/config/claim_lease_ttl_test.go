package config

import (
	"strings"
	"testing"
	"time"
)

// The claim-lease TTL is the single configured value both sides of lease
// keeping derive from: bd's claim leases expire this long after each
// heartbeat, and the controller's renewal cadence is computed from it
// (gas-76r). The default matches bd's 5-minute claim-lease TTL.

func TestDaemonClaimLeaseTTLDefault(t *testing.T) {
	d := DaemonConfig{}
	if got := d.ClaimLeaseTTLDuration(); got != 5*time.Minute {
		t.Errorf("ClaimLeaseTTLDuration() = %v, want 5m", got)
	}
}

func TestDaemonClaimLeaseTTLCustom(t *testing.T) {
	d := DaemonConfig{ClaimLeaseTTL: "2m"}
	if got := d.ClaimLeaseTTLDuration(); got != 2*time.Minute {
		t.Errorf("ClaimLeaseTTLDuration() = %v, want 2m", got)
	}
}

func TestDaemonClaimLeaseTTLInvalid(t *testing.T) {
	d := DaemonConfig{ClaimLeaseTTL: "not-a-duration"}
	if got := d.ClaimLeaseTTLDuration(); got != 5*time.Minute {
		t.Errorf("ClaimLeaseTTLDuration() = %v, want 5m fallback", got)
	}
}

func TestValidateDurationsBadClaimLeaseTTL(t *testing.T) {
	cfg := &City{Daemon: DaemonConfig{ClaimLeaseTTL: "5mins"}}
	warnings := ValidateDurations(cfg, "city.toml")
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "claim_lease_ttl") {
		t.Errorf("warning %q does not name claim_lease_ttl", warnings[0])
	}
}
