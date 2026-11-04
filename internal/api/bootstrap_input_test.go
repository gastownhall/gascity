package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

func bootstrapProviderHTTPState(t *testing.T, family string, opaque bool) *fakeState {
	t.Helper()
	fs := newSessionFakeState(t)
	base := config.BasePrefixBuiltin + family
	spec := config.ProviderSpec{Base: &base}
	binary := family
	if opaque {
		spec.Command = "opaque-" + family
		spec.ResumeCommand = "opaque-" + family + " resume {{.SessionKey}}"
		binary = spec.Command
	}
	fs.cfg.Providers = map[string]config.ProviderSpec{"alias": spec}
	fs.cfg.Workspace.Provider = "alias"
	fs.cfg.Agents = nil
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, binary), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return fs
}

func TestProviderHTTPFreshBuiltinOptionsReachOnlyFreshRuntimeStart(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		for _, compat := range []bool{false, true} {
			name := family + "/huma"
			if compat {
				name = family + "/compat"
			}
			t.Run(name, func(t *testing.T) {
				fs := bootstrapProviderHTTPState(t, family, false)
				model := "gpt-6.1-sol"
				if family == "claude" {
					model = "claude-opus-5-5"
				}
				body := `{"kind":"provider","name":"alias","title":"Fresh builtin","options":{"model":"` + model + `","effort":"medium","permission_mode":"unrestricted"}}`
				srv := New(fs)
				rec := httptest.NewRecorder()
				if compat {
					srv.handleSessionCreate(rec, newPostRequest("/v0/sessions", strings.NewReader(body)))
				} else {
					newTestCityHandlerWith(t, fs, srv).ServeHTTP(rec, newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body)))
				}
				var sid string
				if compat {
					if rec.Code != http.StatusCreated {
						t.Fatalf("compat status %d body %s", rec.Code, rec.Body.String())
					}
					var created sessionResponse
					if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
						t.Fatal(err)
					}
					sid = created.ID
				} else {
					if rec.Code != http.StatusAccepted {
						t.Fatalf("Huma status %d body %s", rec.Code, rec.Body.String())
					}
					accepted := decodeAsyncAccepted(t, rec.Body)
					success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
					if success == nil {
						t.Fatalf("create failed: %+v", failure)
					}
					sid = success.Session.ID
				}
				row, err := fs.cityBeadStore.Get(sid)
				if err != nil {
					t.Fatal(err)
				}
				started := fs.sp.LastStartConfig(row.Metadata["session_name"])
				if started == nil || started.BootstrapInputOrigin != runtime.BootstrapInputFreshBuiltin || started.ProviderName != family {
					t.Fatalf("fresh API start did not receive typed family/provenance: %+v", started)
				}
				if started.PromptSuffix != "" || started.Nudge != "" {
					t.Fatal("fresh bare provider unexpectedly gained startup input")
				}
				for _, value := range []string{model, "medium"} {
					if !strings.Contains(started.Command, value) {
						t.Fatalf("actual option %q absent from %q", value, started.Command)
					}
				}
				if started.Env[runtime.RuntimeEpochEnv] != "1" || started.Env[runtime.ContinuationEpochEnv] != "1" {
					t.Fatal("fresh API start is not the initial incarnation")
				}
			})
		}
	}
}

func TestProviderHTTPWrappedCommandNeverGetsFreshBuiltinCredit(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		t.Run(family, func(t *testing.T) {
			fs := bootstrapProviderHTTPState(t, family, true)
			srv := New(fs)
			rec := httptest.NewRecorder()
			newTestCityHandlerWith(t, fs, srv).ServeHTTP(rec, newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"alias","title":"Opaque command"}`)))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
			}
			accepted := decodeAsyncAccepted(t, rec.Body)
			success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
			if success == nil {
				t.Fatalf("create failed: %+v", failure)
			}
			row, err := fs.cityBeadStore.Get(success.Session.ID)
			if err != nil {
				t.Fatal(err)
			}
			started := fs.sp.LastStartConfig(row.Metadata["session_name"])
			if started == nil || started.BootstrapInputOrigin != "" {
				t.Fatalf("opaque API launch gained first-input authority: %+v", started)
			}
		})
	}
}

func TestFreshProviderInputConfigRefusesStartupInputOrProtocol(t *testing.T) {
	base := config.BasePrefixBuiltin + "codex"
	city := &config.City{Providers: map[string]config.ProviderSpec{"alias": {Base: &base}}}
	resolved, err := config.ResolveProvider(&config.Agent{Provider: "alias"}, &city.Workspace, city.Providers, func(s string) (string, error) { return "/qualified/bin/" + s, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"prompt", "nudge", "setup", "setup script", "live", "protocol"} {
		t.Run(kind, func(t *testing.T) {
			cfg := worker.ResolvedSessionConfig{}
			cfg.Runtime.Hints.BootstrapInputOrigin = runtime.BootstrapInputFreshBuiltin
			switch kind {
			case "prompt":
				cfg.Runtime.Hints.PromptSuffix = "startup task"
			case "nudge":
				cfg.Runtime.Hints.Nudge = "startup task"
			case "setup":
				cfg.Runtime.Hints.SessionSetup = []string{"opaque startup"}
			case "setup script":
				cfg.Runtime.Hints.SessionSetupScript = "opaque-startup.sh"
			case "live":
				cfg.Runtime.Hints.SessionLive = []string{"opaque live"}
			case "protocol":
				cfg.Transport = config.SessionTransportACP
			}
			got := freshProviderInputConfig(city, resolved, cfg)
			if got.Runtime.Hints.BootstrapInputOrigin != "" {
				t.Fatalf("unsupported startup path gained credit: %s", kind)
			}
		})
	}
}
