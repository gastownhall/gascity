//go:build darwin

package dashboardbff

import (
	"context"
	"testing"
)

// TestCurrentSystemHealthSamplesDarwinMetrics exercises the real gopsutil
// Darwin implementations in macOS CI. These metrics are supported on Darwin;
// returning an unavailable state here would make the supported sampler regress
// silently while platform-neutral injected tests continued to pass.
func TestCurrentSystemHealthSamplesDarwinMetrics(t *testing.T) {
	health, err := currentSystemHealth(context.Background())
	if err != nil {
		t.Fatalf("currentSystemHealth: %v", err)
	}
	if health.Host.Load.Status != healthMetricAvailable {
		t.Errorf("host.load = %#v, want available on Darwin", health.Host.Load)
	}
	if health.Host.Memory.Status != healthMetricAvailable {
		t.Errorf("host.memory = %#v, want available on Darwin", health.Host.Memory)
	}
	if health.Host.Uptime.Status != healthMetricAvailable {
		t.Errorf("host.uptime = %#v, want available on Darwin", health.Host.Uptime)
	}
	if health.Admin.RSS.Status != healthMetricAvailable {
		t.Errorf("admin.rss = %#v, want available on Darwin", health.Admin.RSS)
	}
}
