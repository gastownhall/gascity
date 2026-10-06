package dashboardbff

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type mapResolver map[string]string

func (m mapResolver) CityPath(name string) (string, bool) {
	p, ok := m[name]
	return p, ok
}

func TestRuntimeConfigDefaultsAreNeutral(t *testing.T) {
	p := New(Deps{ReadOnly: true})
	cfg := p.runtimeConfigFor("alpha", "/srv/alpha")

	if cfg.CityName != "alpha" || cfg.CityRoot != "/srv/alpha" {
		t.Errorf("city = %q/%q, want alpha//srv/alpha", cfg.CityName, cfg.CityRoot)
	}
	if !cfg.ReadOnly {
		t.Error("readOnly should mirror Deps.ReadOnly")
	}
	if cfg.OperatorAlias != "operator" || cfg.OperatorWireAlias != "human" {
		t.Errorf("operator defaults = %q/%q, want operator/human", cfg.OperatorAlias, cfg.OperatorWireAlias)
	}
	if cfg.DecisionLabel != "needs/operator" {
		t.Errorf("decisionLabel = %q, want needs/operator", cfg.DecisionLabel)
	}
	if cfg.EnabledModules == nil || len(cfg.EnabledModules) != 0 {
		t.Errorf("enabledModules = %v, want explicit empty slice (core-only)", cfg.EnabledModules)
	}
	if cfg.DefaultView != nil {
		t.Errorf("defaultView = %v, want nil", cfg.DefaultView)
	}

	// The wire shape must serialize enabledModules as [] (never null),
	// defaultView as null, and must omit the dropped maintainer field — the
	// frontend's decodeRuntimeConfig depends on exactly this.
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(raw)
	if !strings.Contains(js, `"enabledModules":[]`) {
		t.Errorf("enabledModules must serialize as []: %s", js)
	}
	if !strings.Contains(js, `"defaultView":null`) {
		t.Errorf("defaultView must serialize as null: %s", js)
	}
	if strings.Contains(js, "maintainer") {
		t.Errorf("maintainer must be absent: %s", js)
	}
}

func TestRuntimeConfigHonorsOperatorOverrides(t *testing.T) {
	p := New(Deps{OperatorAlias: "alice", DefaultView: "activity", EnabledModules: []string{}})
	cfg := p.runtimeConfigFor("c", "/p")
	if cfg.OperatorAlias != "alice" {
		t.Errorf("operatorAlias = %q, want alice", cfg.OperatorAlias)
	}
	if cfg.DecisionLabel != "needs/alice" {
		t.Errorf("decisionLabel = %q, want needs/alice (tracks alias)", cfg.DecisionLabel)
	}
	if cfg.DefaultView == nil || *cfg.DefaultView != "activity" {
		t.Errorf("defaultView = %v, want activity", cfg.DefaultView)
	}
}

func TestConfigEndpointServesKnownCity(t *testing.T) {
	p := New(Deps{Resolver: mapResolver{"alpha": "/srv/alpha"}})
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/alpha/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got runtimeConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CityRoot != "/srv/alpha" {
		t.Errorf("cityRoot = %q, want /srv/alpha (from resolver)", got.CityRoot)
	}
}

func TestConfigEndpoint404UnknownCity(t *testing.T) {
	p := New(Deps{Resolver: mapResolver{}})
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/ghost/config", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown city", rec.Code)
	}
}

func TestGuardBlocksMutationWithoutCSRFHeader(t *testing.T) {
	p := New(Deps{Resolver: mapResolver{"alpha": "/p"}})
	rec := httptest.NewRecorder()
	// Any mutation method without X-GC-Request is refused before routing.
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/city/alpha/config", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing X-GC-Request)", rec.Code)
	}
}

func TestGuardAllowsMutationWithCSRFHeader(t *testing.T) {
	p := New(Deps{Resolver: mapResolver{"alpha": "/p"}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/city/alpha/config", nil)
	req.Header.Set("X-GC-Request", "dashboard")
	p.Handler().ServeHTTP(rec, req)
	// Guard passes; the GET-only /config route then rejects POST with 405.
	if rec.Code == http.StatusForbidden {
		t.Fatalf("guard rejected a request carrying X-GC-Request")
	}
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 (guard passed, method not allowed on /config)", rec.Code)
	}
}

func TestGuardReadOnlyRefusesMutation(t *testing.T) {
	p := New(Deps{Resolver: mapResolver{"alpha": "/p"}, ReadOnly: true})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/city/alpha/config", nil)
	req.Header.Set("X-GC-Request", "dashboard")
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 (read-only refuses mutation even with CSRF header)", rec.Code)
	}
}
