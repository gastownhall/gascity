package main

import (
	"bytes"
	"testing"
)

// The failure mode this guards against is doctor stopping a managed dolt server
// it did not start. Every "false" row below is a server someone else owns.
func TestDoctorShouldStopManagedDolt(t *testing.T) {
	tests := []struct {
		name       string
		cityIsLive bool
		wasRunning bool
		nowRunning bool
		want       bool
	}{
		{
			name:       "doctor started it on a stopped city: stop it",
			cityIsLive: false,
			wasRunning: false,
			nowRunning: true,
			want:       true,
		},
		{
			name:       "already running before doctor: leave it",
			cityIsLive: false,
			wasRunning: true,
			nowRunning: true,
			want:       false,
		},
		{
			name:       "never started: nothing to stop",
			cityIsLive: false,
			wasRunning: false,
			nowRunning: false,
			want:       false,
		},
		{
			name:       "live city owns its dolt: never stop",
			cityIsLive: true,
			wasRunning: false,
			nowRunning: true,
			want:       false,
		},
		{
			name:       "live city, already running: never stop",
			cityIsLive: true,
			wasRunning: true,
			nowRunning: true,
			want:       false,
		},
		{
			name:       "live city takes precedence over a raced probe",
			cityIsLive: true,
			wasRunning: false,
			nowRunning: false,
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := doctorShouldStopManagedDolt(tc.cityIsLive, tc.wasRunning, tc.nowRunning)
			if got != tc.want {
				t.Fatalf("doctorShouldStopManagedDolt(live=%v, was=%v, now=%v) = %v, want %v",
					tc.cityIsLive, tc.wasRunning, tc.nowRunning, got, tc.want)
			}
		})
	}
}

// A live city must never arm the guard, so no probe result can lead to a stop.
func TestNewDoctorManagedDoltGuardLiveCityIsDisarmed(t *testing.T) {
	guard := newDoctorManagedDoltGuard(t.TempDir(), true)
	if guard.armed {
		t.Fatal("guard armed for a live city; doctor must never stop a running city's dolt")
	}
}

// No city path means no trustworthy "before" picture: fail closed.
func TestNewDoctorManagedDoltGuardEmptyCityPathIsDisarmed(t *testing.T) {
	for _, cityPath := range []string{"", "   "} {
		guard := newDoctorManagedDoltGuard(cityPath, false)
		if guard.armed {
			t.Fatalf("guard armed for cityPath %q; expected fail-closed", cityPath)
		}
	}
}

// Regression: a stopped city publishes no dolt port until the server starts, so
// an implementation that resolved the port only at snapshot time disarmed
// itself in precisely the case this guard exists for — and the server leaked.
// A stopped city with nothing published must arm, with wasRunning false.
func TestNewDoctorManagedDoltGuardStoppedCityWithNoPublishedPortArms(t *testing.T) {
	guard := newDoctorManagedDoltGuard(t.TempDir(), false)
	if !guard.armed {
		t.Fatal("guard disarmed for a stopped city with no published port; " +
			"a server started by doctor's own checks would leak")
	}
	if guard.wasRunning {
		t.Fatal("wasRunning true with no published port; nothing can be running")
	}
}

// release on a disarmed guard must be inert — no panic, no output, no stop.
func TestReleaseDisarmedGuardIsInert(t *testing.T) {
	var stderr bytes.Buffer
	guard := newDoctorManagedDoltGuard(t.TempDir(), true)
	guard.release(&stderr)
	if stderr.Len() != 0 {
		t.Fatalf("disarmed guard wrote to stderr: %q", stderr.String())
	}
}

// A nil guard must be safe: doDoctor defers release unconditionally.
func TestReleaseNilGuardIsSafe(t *testing.T) {
	var stderr bytes.Buffer
	var guard *doctorManagedDoltGuard
	guard.release(&stderr)
	if stderr.Len() != 0 {
		t.Fatalf("nil guard wrote to stderr: %q", stderr.String())
	}
}
