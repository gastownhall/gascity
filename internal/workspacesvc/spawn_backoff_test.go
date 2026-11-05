package workspacesvc

import (
	"math"
	"testing"
	"time"
)

// TestSpawnBackoffDelay pins the schedule every proxy_process respawn loop
// shares: the base window for the first failure, doubling per further failure,
// clamped at proxyProcessRestartBackoffMax, with no overflow however large the
// count (ga-ydr5p3).
func TestSpawnBackoffDelay(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{math.MinInt32, time.Second},
		{-1, time.Second},
		{0, time.Second},
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 32 * time.Second},
		{7, 64 * time.Second},
		{8, 128 * time.Second},
		{9, 256 * time.Second},
		{10, 300 * time.Second},
		{11, 300 * time.Second},
		{100, 300 * time.Second},
		{math.MaxInt32, 300 * time.Second},
		{math.MaxInt, 300 * time.Second},
	}
	for _, tc := range cases {
		if got := spawnBackoffDelay(tc.failures); got != tc.want {
			t.Errorf("spawnBackoffDelay(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
	if proxyProcessRestartBackoffMax != 5*time.Minute {
		t.Errorf("proxyProcessRestartBackoffMax = %v, want 5m", proxyProcessRestartBackoffMax)
	}
}
