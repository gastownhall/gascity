package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/citylayout"
)

type fakeInitializer struct {
	scaffoldReq    cityinit.InitRequest
	scaffoldResult *cityinit.InitResult
	scaffoldErr    error

	unregisterReq    cityinit.UnregisterRequest
	unregisterResult *cityinit.UnregisterResult
	unregisterErr    error
}

func (f *fakeInitializer) Init(context.Context, cityinit.InitRequest) (*cityinit.InitResult, error) {
	return nil, errors.New("Init should not be called by supervisor tests")
}

func (f *fakeInitializer) Scaffold(_ context.Context, req cityinit.InitRequest) (*cityinit.InitResult, error) {
	f.scaffoldReq = req
	if f.scaffoldErr != nil {
		return nil, f.scaffoldErr
	}
	return f.scaffoldResult, nil
}

func (f *fakeInitializer) Unregister(_ context.Context, req cityinit.UnregisterRequest) (*cityinit.UnregisterResult, error) {
	f.unregisterReq = req
	if f.unregisterErr != nil {
		return nil, f.unregisterErr
	}
	return f.unregisterResult, nil
}

func newTestSupervisorMuxWithInitializer(t *testing.T, init cityInitializer) *SupervisorMux {
	t.Helper()
	return NewSupervisorMux(&fakeCityResolver{cities: map[string]*fakeState{}}, init, false, "test", time.Now())
}

func TestSupervisorCityCreateConflictsWhenTargetAlreadyInitialized(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{
			name: "scaffold_present",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				for _, path := range []string{
					filepath.Join(dir, citylayout.RuntimeRoot),
					filepath.Join(dir, citylayout.RuntimeRoot, "cache"),
					filepath.Join(dir, citylayout.RuntimeRoot, "runtime"),
					filepath.Join(dir, citylayout.RuntimeRoot, "system"),
				} {
					if err := os.MkdirAll(path, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(dir, citylayout.RuntimeRoot, "events.jsonl"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "city")
			tc.setup(t, dir)

			sm := newTestSupervisorMux(t, map[string]*fakeState{})
			req := httptest.NewRequest(http.MethodPost, "/v0/city", strings.NewReader(`{"dir":"`+dir+`","provider":"claude"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-GC-Request", "test")
			rec := httptest.NewRecorder()

			sm.ServeHTTP(rec, req)

			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusConflict, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "already initialized") {
				t.Fatalf("body = %q, want already initialized detail", rec.Body.String())
			}
		})
	}
}

func TestSupervisorCityCreateScaffoldsViaInitializer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cityPath := filepath.Join(home, "mc-city")
	init := &fakeInitializer{
		scaffoldResult: &cityinit.InitResult{
			CityName:      "mc-city",
			CityPath:      cityPath,
			ProviderUsed:  "codex",
			ReloadWarning: "reload failed",
		},
	}
	sm := newTestSupervisorMuxWithInitializer(t, init)

	req := httptest.NewRequest(http.MethodPost, "/v0/city", strings.NewReader(`{
		"dir":"mc-city",
		"provider":"codex",
		"bootstrap_profile":"single-host-compat"
	}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "test")
	rec := httptest.NewRecorder()

	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if init.scaffoldReq.Dir != cityPath {
		t.Fatalf("Scaffold Dir = %q, want %q", init.scaffoldReq.Dir, cityPath)
	}
	if init.scaffoldReq.Provider != "codex" || init.scaffoldReq.BootstrapProfile != "single-host-compat" {
		t.Fatalf("Scaffold request = %+v, want codex + single-host-compat", init.scaffoldReq)
	}
	if !init.scaffoldReq.SkipProviderReadiness {
		t.Fatal("Scaffold request should skip provider readiness for API callers")
	}
	if body := rec.Body.String(); !strings.Contains(body, `"name":"mc-city"`) || !strings.Contains(body, `"path":"`+cityPath+`"`) {
		t.Fatalf("body = %s, want name and path", body)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"reload_warning":"reload failed"`) {
		t.Fatalf("body = %s, want reload warning", body)
	}
}

func TestSupervisorCityCreateMapsInitializerErrors(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "mc-city")
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "already initialized", err: cityinit.ErrAlreadyInitialized, want: http.StatusConflict},
		{name: "invalid directory", err: cityinit.ErrInvalidDirectory, want: http.StatusUnprocessableEntity},
		{name: "invalid provider", err: cityinit.ErrInvalidProvider, want: http.StatusUnprocessableEntity},
		{name: "invalid bootstrap", err: cityinit.ErrInvalidBootstrapProfile, want: http.StatusUnprocessableEntity},
		{name: "generic", err: errors.New("boom"), want: http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			init := &fakeInitializer{scaffoldErr: tc.err}
			sm := newTestSupervisorMuxWithInitializer(t, init)
			req := httptest.NewRequest(http.MethodPost, "/v0/city", strings.NewReader(`{"dir":"`+cityPath+`","provider":"codex"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-GC-Request", "test")
			rec := httptest.NewRecorder()

			sm.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestSupervisorCityCreateWithoutInitializerReturns501(t *testing.T) {
	sm := newTestSupervisorMux(t, map[string]*fakeState{})
	cityPath := filepath.Join(t.TempDir(), "mc-city")
	req := httptest.NewRequest(http.MethodPost, "/v0/city", strings.NewReader(`{"dir":"`+cityPath+`","provider":"codex"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "test")
	rec := httptest.NewRecorder()

	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusNotImplemented, rec.Body.String())
	}
}

func TestSupervisorCityUnregisterUsesInitializer(t *testing.T) {
	init := &fakeInitializer{
		unregisterResult: &cityinit.UnregisterResult{
			CityName:      "mc-city",
			CityPath:      "/tmp/mc-city",
			ReloadWarning: "reload failed",
		},
	}
	sm := newTestSupervisorMuxWithInitializer(t, init)
	req := httptest.NewRequest(http.MethodPost, "/v0/city/mc-city/unregister", nil)
	req.Header.Set("X-GC-Request", "test")
	rec := httptest.NewRecorder()

	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if init.unregisterReq.CityName != "mc-city" {
		t.Fatalf("Unregister CityName = %q, want mc-city", init.unregisterReq.CityName)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"name":"mc-city"`) || !strings.Contains(body, `"path":"/tmp/mc-city"`) {
		t.Fatalf("body = %s, want name and path", body)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"reload_warning":"reload failed"`) {
		t.Fatalf("body = %s, want reload warning", body)
	}
}

func TestSupervisorCityUnregisterMapsNotRegistered(t *testing.T) {
	init := &fakeInitializer{unregisterErr: cityinit.ErrNotRegistered}
	sm := newTestSupervisorMuxWithInitializer(t, init)
	req := httptest.NewRequest(http.MethodPost, "/v0/city/missing/unregister", nil)
	req.Header.Set("X-GC-Request", "test")
	rec := httptest.NewRecorder()

	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestCityDirAlreadyInitializedAllowsConfigOnlyBootstrap(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, citylayout.CityConfigFile), []byte("[workspace]\nname = \"alpha\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if cityDirAlreadyInitialized(dir) {
		t.Fatal("config-only city should be left for gc init bootstrap")
	}
}
