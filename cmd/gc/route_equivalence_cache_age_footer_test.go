package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// The cache-age footer is a diagnostic on stderr, so it never breaks the
// local and API routes' byte-equal stdout, however old the server's cache is.
// The clock is frozen 75 s past the city cache's last fresh data, past twice
// its 30 s re-scan interval: the API route prints the footer on stderr, the
// local route, which reads no cache, prints none, and stdout is identical.
// Order history is pinned separately: TestOrderHistoryRouteEquivalence holds
// its stdout equal and TestRouteOrderHistory_CacheAgeFooterOnStderr puts its
// footer on stderr.
func TestCacheAgeFooterLeavesRouteStdoutEqual(t *testing.T) {
	clearInheritedBeadsEnv(t)
	var beadID string
	h := newCharCity(t, cacheAgeCity, func(t *testing.T, store beads.Store) {
		b, err := store.Create(beads.Bead{Title: "footer bead", Type: "task", Labels: []string{"route-eq"}})
		if err != nil {
			t.Fatalf("seed bead: %v", err)
		}
		beadID = b.ID
	})
	cache := cityCacheOf(t, h.cs)
	t.Cleanup(api.SetLivenessClockForTest(&clock.Fake{Time: cache.Stats().LastFreshAt.Add(75 * time.Second)}))
	client := registryReadsAPIClient(t, h)

	for _, tc := range []struct {
		cmd   string
		route func(c *api.Client, nilReason string, stdout, stderr io.Writer) int
	}{
		{"beads list", func(c *api.Client, nilReason string, stdout, stderr io.Writer) int {
			return routeBeadsList(h.cityPath, c, nilReason, "text", beadFilters{}, stdout, stderr)
		}},
		{"beads show", func(c *api.Client, nilReason string, stdout, stderr io.Writer) int {
			return routeBeadsShow(h.cityPath, c, nilReason, beadID, "text", stdout, stderr)
		}},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			run := func(c *api.Client, nilReason, route string) (stdout, stderr string) {
				t.Helper()
				clearBuiltinImportWarningCache()
				var out, errOut bytes.Buffer
				if code := tc.route(c, nilReason, &out, &errOut); code != 0 {
					t.Fatalf("%s via %s: exit %d: %s", tc.cmd, route, code, errOut.String())
				}
				if want := "cmd=" + tc.cmd + " route=" + route; !strings.Contains(errOut.String(), want) {
					t.Fatalf("%s: stderr missing %q:\n%s", tc.cmd, want, errOut.String())
				}
				return out.String(), errOut.String()
			}
			apiOut, apiErr := run(client, "", "api")
			localOut, localErr := run(nil, "controller-down", "fallback")

			if apiOut != localOut {
				t.Fatalf("%s stdout differs between routes:\nlocal:\n%s\napi:\n%s", tc.cmd, localOut, apiOut)
			}
			if strings.TrimSpace(localOut) == "" {
				t.Fatalf("%s printed nothing; the fixture did not seed what it claims", tc.cmd)
			}
			if !strings.Contains(apiErr, "(cache age: ") || !strings.Contains(apiErr, "30s re-scan interval") {
				t.Errorf("%s: the API route's stderr lacks the cache-age footer:\n%s", tc.cmd, apiErr)
			}
			if strings.Contains(localErr, "(cache age: ") {
				t.Errorf("%s: the local route printed a cache-age footer:\n%s", tc.cmd, localErr)
			}
		})
	}
}
