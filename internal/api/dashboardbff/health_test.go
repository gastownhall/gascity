package dashboardbff

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
)

func TestCurrentSystemHealthLoadSamplerOutlivesRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	_, _ = currentSystemHealthWithSamplers(ctx, healthSamplers{
		loadAverage: func() (*load.AvgStat, error) {
			called = true
			return &load.AvgStat{}, nil
		},
		virtualMemory: func(context.Context) (*mem.VirtualMemoryStat, error) {
			return &mem.VirtualMemoryStat{Total: 1024, Available: 512}, nil
		},
		hostUptime: func(context.Context) (uint64, error) { return 60, nil },
		processRSS: func(context.Context, int32) (uint64, error) { return 256, nil },
	})

	if !called {
		t.Fatal("context-independent load sampler was not called after request cancellation")
	}
}

func TestCurrentSystemHealthKeepsIndependentMetricsWhenOneSamplerFails(t *testing.T) {
	tests := []struct {
		name         string
		failedMetric string
		breakSampler func(*healthSamplers)
	}{
		{
			name:         "load",
			failedMetric: "host.load",
			breakSampler: func(s *healthSamplers) {
				s.loadAverage = func() (*load.AvgStat, error) {
					return nil, errors.New("load source offline")
				}
			},
		},
		{
			name:         "memory",
			failedMetric: "host.memory",
			breakSampler: func(s *healthSamplers) {
				s.virtualMemory = func(context.Context) (*mem.VirtualMemoryStat, error) {
					return nil, errors.New("memory source offline")
				}
			},
		},
		{
			name:         "uptime",
			failedMetric: "host.uptime",
			breakSampler: func(s *healthSamplers) {
				s.hostUptime = func(context.Context) (uint64, error) {
					return 0, errors.New("uptime source offline")
				}
			},
		},
		{
			name:         "rss",
			failedMetric: "admin.rss",
			breakSampler: func(s *healthSamplers) {
				s.processRSS = func(context.Context, int32) (uint64, error) {
					return 0, errors.New("process source offline")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			samplers := healthyHealthSamplers()
			tt.breakSampler(&samplers)
			snapshot, err := currentSystemHealthWithSamplers(context.Background(), samplers)
			if err != nil {
				t.Fatalf("currentSystemHealthWithSamplers: %v", err)
			}

			metrics := []struct {
				name   string
				status healthMetricStatus
				reason healthMetricUnavailableReason
			}{
				{name: "host.load", status: snapshot.Host.Load.Status, reason: snapshot.Host.Load.Reason},
				{name: "host.memory", status: snapshot.Host.Memory.Status, reason: snapshot.Host.Memory.Reason},
				{name: "host.uptime", status: snapshot.Host.Uptime.Status, reason: snapshot.Host.Uptime.Reason},
				{name: "admin.rss", status: snapshot.Admin.RSS.Status, reason: snapshot.Admin.RSS.Reason},
			}
			for _, metric := range metrics {
				if metric.name == tt.failedMetric {
					if metric.status != healthMetricUnavailable || metric.reason != healthMetricSampleFailed {
						t.Errorf("%s = (%q, %q), want unavailable/sample_failed", metric.name, metric.status, metric.reason)
					}
					continue
				}
				if metric.status != healthMetricAvailable {
					t.Errorf("%s status = %q, want available", metric.name, metric.status)
				}
			}
		})
	}
}

func healthyHealthSamplers() healthSamplers {
	return healthSamplers{
		loadAverage: func() (*load.AvgStat, error) {
			return &load.AvgStat{Load1: 0.5, Load5: 0.25, Load15: 0.125}, nil
		},
		virtualMemory: func(context.Context) (*mem.VirtualMemoryStat, error) {
			return &mem.VirtualMemoryStat{Total: 4096, Available: 1024}, nil
		},
		hostUptime: func(context.Context) (uint64, error) { return 90, nil },
		processRSS: func(context.Context, int32) (uint64, error) { return 2048, nil },
	}
}

func TestCurrentSystemHealthClassifiesInvalidAndOverflowSamplesIndependently(t *testing.T) {
	snapshot, err := currentSystemHealthWithSamplers(context.Background(), healthSamplers{
		loadAverage: func() (*load.AvgStat, error) {
			return &load.AvgStat{Load1: 0.5, Load5: 0.25, Load15: 0.125}, nil
		},
		virtualMemory: func(context.Context) (*mem.VirtualMemoryStat, error) {
			return &mem.VirtualMemoryStat{Total: 0, Available: 0}, nil
		},
		hostUptime: func(context.Context) (uint64, error) { return 0, nil },
		processRSS: func(context.Context, int32) (uint64, error) { return uint64(1) << 63, nil },
	})
	if err != nil {
		t.Fatalf("currentSystemHealthWithSamplers: %v", err)
	}

	if snapshot.Host.Load.Status != healthMetricAvailable {
		t.Errorf("host.load status = %q, want available", snapshot.Host.Load.Status)
	}
	if snapshot.Host.Memory.Reason != healthMetricInvalidSample {
		t.Errorf("host.memory reason = %q, want %q", snapshot.Host.Memory.Reason, healthMetricInvalidSample)
	}
	if snapshot.Host.Uptime.Reason != healthMetricInvalidSample {
		t.Errorf("host.uptime reason = %q, want %q", snapshot.Host.Uptime.Reason, healthMetricInvalidSample)
	}
	if snapshot.Admin.RSS.Reason != healthMetricValueOverflow {
		t.Errorf("admin.rss reason = %q, want %q", snapshot.Admin.RSS.Reason, healthMetricValueOverflow)
	}
}

func TestHealthMetricJSONUsesDiscriminatedAvailableAndUnavailableShapes(t *testing.T) {
	availableJSON, err := json.Marshal(availableHealthMetric(int64(42)))
	if err != nil {
		t.Fatalf("marshal available metric: %v", err)
	}
	if got := string(availableJSON); got != `{"status":"available","value":42}` {
		t.Errorf("available metric JSON = %s", got)
	}

	unavailableJSON, err := json.Marshal(unavailableHealthMetric[int64](healthMetricSampleFailed))
	if err != nil {
		t.Fatalf("marshal unavailable metric: %v", err)
	}
	if got := string(unavailableJSON); got != `{"status":"unavailable","reason":"sample_failed"}` {
		t.Errorf("unavailable metric JSON = %s", got)
	}
}

func TestHealthSystemReturnsUnavailableWhenSamplingFails(t *testing.T) {
	p := New(Deps{})
	p.healthSnapshot = func(context.Context) (systemHealth, error) {
		return systemHealth{}, errors.New("host metrics unavailable")
	}

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health/system", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var got apiErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if got.Error != "system health unavailable" {
		t.Errorf("error = %q, want %q", got.Error, "system health unavailable")
	}
}

// TestLocalToolVersionsMemoized verifies the MEDIUM finding fix: repeated calls
// within the TTL reuse the cached snapshot instead of re-probing. The cache is
// seeded by a real probe, then overwritten with a sentinel and given a future
// expiry; the next call must return the sentinel (proving no re-probe), and a
// past expiry must force a re-probe (sentinel replaced).
func TestLocalToolVersionsMemoized(t *testing.T) {
	p := New(Deps{})
	ctx := context.Background()

	_ = p.localToolVersions(ctx) // prime the cache
	c := p.localTools

	sentinel := localToolVersions{Dolt: localToolVersion{Status: "available", Version: "sentinel"}}
	c.mu.Lock()
	c.val = sentinel
	c.expires = time.Now().Add(time.Hour)
	c.mu.Unlock()

	if got := p.localToolVersions(ctx); got.Dolt.Version != "sentinel" {
		t.Errorf("cached call re-probed: dolt version = %q, want sentinel", got.Dolt.Version)
	}

	// Expire the entry: the next call must re-probe and overwrite the sentinel.
	c.mu.Lock()
	c.expires = time.Now().Add(-time.Minute)
	c.mu.Unlock()
	if got := p.localToolVersions(ctx); got.Dolt.Version == "sentinel" {
		t.Error("expired cache was not re-probed: still returning sentinel")
	}
}

// TestLocalToolsCachePerPlane confirms each Plane gets its own cache entry, so
// one Plane's snapshot never leaks into another's.
func TestLocalToolsCachePerPlane(t *testing.T) {
	p1, p2 := New(Deps{}), New(Deps{})
	if p1.localTools == p2.localTools {
		t.Error("distinct planes share a localToolsCache")
	}
	if p1.localTools == nil {
		t.Error("plane localTools cache not initialized")
	}
}

// TestUnavailableSanitizesReason verifies the NIT fix: subprocess/error text in
// an unavailable reason is run through sanitizeTerminalOutput before it reaches
// the wire, stripping control and escape bytes.
func TestUnavailableSanitizesReason(t *testing.T) {
	tv := unavailable("boom\x07 \x1b]0;title\x07here\x00")
	if tv.Status != "unavailable" {
		t.Fatalf("status = %q, want unavailable", tv.Status)
	}
	if strings.ContainsAny(tv.Reason, "\x07\x00\x1b") {
		t.Errorf("reason not sanitized: %q", tv.Reason)
	}
	if !strings.Contains(tv.Reason, "boom") || !strings.Contains(tv.Reason, "here") {
		t.Errorf("sanitizer dropped legible text: %q", tv.Reason)
	}
}
