package clock

import (
	"testing"
	"time"
)

func TestBackstopIsTheIdentityUnlessATestAsksForASpeedup(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"", 30 * time.Second},
		{"1", 30 * time.Second},
		{"6", 5 * time.Second},
		{" 6 ", 5 * time.Second},
		{"30", time.Second},
		{"31", 30 * time.Second},
		{"0", 30 * time.Second},
		{"-6", 30 * time.Second},
		{"1.5", 30 * time.Second},
		{"fast", 30 * time.Second},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv(BackstopSpeedupEnv, tc.raw)
			if got := Backstop(30 * time.Second); got != tc.want {
				t.Errorf("%s=%q: Backstop(30s) = %s, want %s", BackstopSpeedupEnv, tc.raw, got, tc.want)
			}
		})
	}
}

func TestBackstopLeavesANonPositiveDurationAlone(t *testing.T) {
	t.Setenv(BackstopSpeedupEnv, "6")
	for _, d := range []time.Duration{0, -time.Second} {
		if got := Backstop(d); got != d {
			t.Errorf("Backstop(%s) = %s, want it unchanged", d, got)
		}
	}
}
