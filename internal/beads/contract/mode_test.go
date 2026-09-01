package contract

import "testing"

func TestIsProxiedDoltModeGatesByBackend(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		mode    string
		want    bool
	}{
		{name: "dolt", backend: "dolt", mode: "proxied-server", want: true},
		{name: "legacy empty backend", backend: "", mode: " PROXIED-SERVER ", want: true},
		{name: "doltlite stale marker", backend: "doltlite", mode: "proxied-server"},
		{name: "arbitrary backend stale marker", backend: "postgres", mode: "proxied-server"},
		{name: "direct mode", backend: "dolt", mode: "server"},
		{name: "empty mode", backend: "dolt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsProxiedDoltMode(tt.backend, tt.mode); got != tt.want {
				t.Fatalf("IsProxiedDoltMode(%q, %q) = %v, want %v", tt.backend, tt.mode, got, tt.want)
			}
		})
	}
}

func TestIsDoltBackendTreatsOnlyDoltAndLegacyAsDolt(t *testing.T) {
	for _, tt := range []struct {
		backend string
		want    bool
	}{
		{backend: "", want: true},
		{backend: " DOLT ", want: true},
		{backend: "doltlite", want: false},
		{backend: "postgres", want: false},
	} {
		if got := IsDoltBackend(tt.backend); got != tt.want {
			t.Fatalf("IsDoltBackend(%q) = %v, want %v", tt.backend, got, tt.want)
		}
	}
}
