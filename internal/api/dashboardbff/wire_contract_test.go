package dashboardbff

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The hand-typed Go /api structs are coupled to the SPA's response decoders in
// frontend/src/api/client.ts only by "// must match" comments. These tests are
// the cross-boundary guard: they assert each endpoint's JSON satisfies exactly
// the field+type contract the matching decoder enforces, so a Go struct that
// drifts from the TS decoder fails here instead of at runtime in the browser.
// (The complementary half — running these shapes through the real TS decoders —
// is a frontend Vitest follow-up.)

func wireGet(t *testing.T, p *Plane, path string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200 (body %s)", path, rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("GET %s: decode: %v", path, err)
	}
	return m
}

func mustString(t *testing.T, m map[string]any, field string) {
	t.Helper()
	if _, ok := m[field].(string); !ok {
		t.Errorf("field %q must be a string, got %T", field, m[field])
	}
}

func mustBool(t *testing.T, m map[string]any, field string) {
	t.Helper()
	if _, ok := m[field].(bool); !ok {
		t.Errorf("field %q must be a bool, got %T", field, m[field])
	}
}

func mustArray(t *testing.T, m map[string]any, field string) {
	t.Helper()
	if _, ok := m[field].([]any); !ok {
		t.Errorf("field %q must be an array (never null), got %T", field, m[field])
	}
}

func mustObject(t *testing.T, m map[string]any, field string) {
	t.Helper()
	if _, ok := m[field].(map[string]any); !ok {
		t.Errorf("field %q must be an object, got %T", field, m[field])
	}
}

func mustStringOrNull(t *testing.T, m map[string]any, field string) {
	t.Helper()
	if v, present := m[field]; !present {
		t.Errorf("field %q must be present (string|null)", field)
	} else if _, isStr := v.(string); v != nil && !isStr {
		t.Errorf("field %q must be string or null, got %T", field, v)
	}
}

func contractPlane() *Plane {
	return New(Deps{Resolver: mapResolver{"alpha": "/srv/alpha"}})
}

func TestWireContractConfig(t *testing.T) {
	m := wireGet(t, contractPlane(), "/api/city/alpha/config")
	for _, f := range []string{"cityName", "cityRoot", "operatorAlias", "operatorWireAlias", "decisionLabel"} {
		mustString(t, m, f)
	}
	mustBool(t, m, "useFixtures")
	mustBool(t, m, "readOnly")
	mustArray(t, m, "enabledModules") // explicit [] for core-only, never null
	mustStringOrNull(t, m, "defaultView")
}

func TestWireContractHealth(t *testing.T) {
	m := wireGet(t, contractPlane(), "/api/health")
	mustBool(t, m, "ok")
	mustString(t, m, "ts")
}

func TestWireContractSystemHealth(t *testing.T) {
	m := wireGet(t, contractPlane(), "/api/health/system")
	mustObject(t, m, "admin")
	mustObject(t, m, "host")
}

func TestWireContractLocalTools(t *testing.T) {
	m := wireGet(t, contractPlane(), "/api/health/local-tools")
	for _, tool := range []string{"dolt", "beads", "gc"} {
		mustObject(t, m, tool)
		mustString(t, m[tool].(map[string]any), "status")
	}
}

func TestWireContractGitCommits(t *testing.T) {
	m := wireGet(t, contractPlane(), "/api/git/commits?view=recent-main")
	mustString(t, m, "view")
	mustArray(t, m, "items")
}

func TestWireContractBuilds(t *testing.T) {
	m := wireGet(t, contractPlane(), "/api/builds")
	mustArray(t, m, "items")
	mustStringOrNull(t, m, "source")
	mustBool(t, m, "failed_marker")
}

func TestWireContractSupervisorStatus(t *testing.T) {
	// No Start()/SupervisorBaseURL -> deterministic not-sampled-yet shape.
	m := wireGet(t, contractPlane(), "/api/city/alpha/supervisor-status")
	mustBool(t, m, "available")
	if _, present := m["status"]; !present {
		t.Error("supervisor-status must always carry a status field (object|null)")
	}
	if m["available"] == false {
		mustString(t, m, "reason")
	}
}

func TestWireContractDoltTrend(t *testing.T) {
	m := wireGet(t, contractPlane(), "/api/city/alpha/dolt-noms/trend")
	mustBool(t, m, "available")
	mustArray(t, m, "samples")
}

func TestWireContractRigStoreHealth(t *testing.T) {
	m := wireGet(t, contractPlane(), "/api/city/alpha/rig-store-health")
	mustBool(t, m, "available")
	mustArray(t, m, "rigs")
}

func TestWireContractRunDiff(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/city/alpha/runs/gc-run-1/diff",
		strings.NewReader(`{"executionPath":{"kind":"unavailable","reason":"missing_cwd_and_rig_root"}}`))
	req.Header.Set("X-GC-Request", "dashboard")
	contractPlane().Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run-diff: status %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("run-diff decode: %v", err)
	}
	mustString(t, m, "kind")
	mustObject(t, m, "rootPath")
	mustObject(t, m, "comparison")
	mustArray(t, m, "status")
	mustArray(t, m, "changedFiles")
	mustString(t, m, "patch")
	mustBool(t, m, "truncated")
}

// TestSanitizeTerminalOutputStripsCSIAndControls is the regression guard for the
// broadened CSI grammar: SGR, intermediate-byte, and private CSI sequences are
// all removed whole, leaving no introducer/param/intermediate residue, and no
// ESC/control or bidi bytes survive.
func TestSanitizeTerminalOutputStripsCSIAndControls(t *testing.T) {
	in := "a\x1b[31mred\x1b[0m b\x1b[1$rc\x1b[>0cd\x07\u202ee"
	got := sanitizeTerminalOutput(in)
	for _, bad := range []string{"\x1b", "[31m", "[1$r", "[>0c", "$", ">", "\x07", "\u202e"} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitized output retains %q: %q", bad, got)
		}
	}
	if got != "ared bcde" { // the literal space in " b" survives; only escapes/controls are stripped
		t.Errorf("sanitized = %q, want %q", got, "ared bcde")
	}
}
