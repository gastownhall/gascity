package dashboardbff

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestCurrentSystemHealthHostMetrics verifies the platform host/process metric
// seam returns live values on the host it runs on, instead of the procfs-only
// zeros the pre-split readers produced off Linux. It runs on Linux and Darwin,
// the two platforms with a real readPlatformMetrics implementation; the
// zero-fallback build (health_other.go) has no live source to assert against.
func TestCurrentSystemHealthHostMetrics(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("no live host-metric reader on %s", runtime.GOOS)
	}
	h := currentSystemHealth()

	if h.Host.TotalMemBytes <= 0 {
		t.Errorf("Host.TotalMemBytes = %d, want > 0", h.Host.TotalMemBytes)
	}
	if h.Host.UptimeSec <= 0 {
		t.Errorf("Host.UptimeSec = %d, want > 0", h.Host.UptimeSec)
	}
	if h.Host.LoadAvg1 < 0 {
		t.Errorf("Host.LoadAvg1 = %v, want >= 0", h.Host.LoadAvg1)
	}
	if h.Host.FreeMemBytes < 0 {
		t.Errorf("Host.FreeMemBytes = %d, want >= 0", h.Host.FreeMemBytes)
	}
	if h.Host.CPUCount <= 0 {
		t.Errorf("Host.CPUCount = %d, want > 0", h.Host.CPUCount)
	}
	if h.Admin.RssBytes <= 0 {
		t.Errorf("Admin.RssBytes = %d, want > 0", h.Admin.RssBytes)
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
