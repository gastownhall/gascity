package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestFleetStartBreakerEnabledByDefault(t *testing.T) {
	d := DaemonConfig{}
	if !d.FleetStartBreakerEnabled() {
		t.Error("FleetStartBreakerEnabled() = false, want true (default)")
	}
}

func TestFleetStartBreakerKillSwitch(t *testing.T) {
	var c City
	if _, err := toml.NewDecoder(strings.NewReader("[daemon]\nfleet_start_breaker = false\n")).Decode(&c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Daemon.FleetStartBreakerEnabled() {
		t.Error("FleetStartBreakerEnabled() = true with fleet_start_breaker = false, want false")
	}
	v := true
	if !(&DaemonConfig{FleetStartBreaker: &v}).FleetStartBreakerEnabled() {
		t.Error("FleetStartBreakerEnabled() = false with explicit true, want true")
	}
}
