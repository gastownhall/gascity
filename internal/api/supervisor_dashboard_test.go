package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/api/dashboardbff"
	"github.com/gastownhall/gascity/internal/api/dashboardspa"
)

type dashTestResolver struct{ cities []api.CityInfo }

func (r dashTestResolver) ListCities() []api.CityInfo { return r.cities }
func (r dashTestResolver) CityState(string) api.State { return nil }

type dashCityPaths map[string]string

func (m dashCityPaths) CityPath(n string) (string, bool) { p, ok := m[n]; return p, ok }

func (m dashCityPaths) Cities() []dashboardbff.CityRef {
	refs := make([]dashboardbff.CityRef, 0, len(m))
	for name, path := range m {
		refs = append(refs, dashboardbff.CityRef{Name: name, Path: path})
	}
	return refs
}

// TestSupervisorHostsDashboardSameOrigin proves the headline outcome: one
// listener serves the SPA, the typed /v0 API, and the host-side /api plane,
// with the reserved API prefixes never falling through to the SPA shell.
func TestSupervisorHostsDashboardSameOrigin(t *testing.T) {
	mux := api.NewSupervisorMux(
		dashTestResolver{cities: []api.CityInfo{{Name: "alpha", Path: "/srv/alpha", Running: true}}},
		nil, false, "vtest", "btest", time.Now(),
	)
	spa, err := dashboardspa.NewStaticHandler()
	if err != nil {
		t.Fatalf("NewStaticHandler: %v", err)
	}
	plane := dashboardbff.New(dashboardbff.Deps{Resolver: dashCityPaths{"alpha": "/srv/alpha"}})
	mux.WithAPIPlane(plane.Handler()).WithStaticHandler(spa)

	srv := httptest.NewServer(mux.Handler())
	defer srv.Close()
	c := srv.Client()

	t.Run("SPA at root", func(t *testing.T) {
		assertGetOK(t, c, srv.URL+"/", `id="root"`)
	})
	t.Run("SPA fallback for client route", func(t *testing.T) {
		assertGetOK(t, c, srv.URL+"/city/alpha/agents", `id="root"`)
	})
	t.Run("typed /v0 still served", func(t *testing.T) {
		assertGetOK(t, c, srv.URL+"/v0/cities", "alpha")
	})
	t.Run("host /api config served from resolver path", func(t *testing.T) {
		assertGetOK(t, c, srv.URL+"/api/city/alpha/config", "/srv/alpha")
	})
	t.Run("unknown city config 404", func(t *testing.T) {
		assertStatus(t, c, http.MethodGet, srv.URL+"/api/city/ghost/config", http.StatusNotFound)
	})
	t.Run("unknown /v0 path 404 not SPA", func(t *testing.T) {
		assertStatus(t, c, http.MethodGet, srv.URL+"/v0/does-not-exist", http.StatusNotFound)
	})
	t.Run("api mutation without CSRF header refused", func(t *testing.T) {
		assertStatus(t, c, http.MethodPost, srv.URL+"/api/city/alpha/config", http.StatusForbidden)
	})
}

func assertGetOK(t *testing.T, c *http.Client, url, wantBodySubstr string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200 (body: %s)", url, resp.StatusCode, truncate(body))
	}
	if wantBodySubstr != "" && !strings.Contains(string(body), wantBodySubstr) {
		t.Errorf("GET %s: body missing %q (got: %s)", url, wantBodySubstr, truncate(body))
	}
}

func assertStatus(t *testing.T, c *http.Client, method, url string, wantStatus int) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != wantStatus {
		t.Errorf("%s %s: status = %d, want %d", method, url, resp.StatusCode, wantStatus)
	}
}

func truncate(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}
