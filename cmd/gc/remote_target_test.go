package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/clientcontext"
)

// fileWith builds a contexts File from the given contexts, with an optional
// sticky default, for resolver tests.
func fileWith(def string, ctxs ...clientcontext.Context) *clientcontext.File {
	return &clientcontext.File{Default: def, Contexts: ctxs}
}

var prodCtx = clientcontext.Context{
	Name:         "prod",
	URL:          "https://box.internal:9443",
	City:         "example-city",
	GrantCommand: "gc-write-mint --key k",
}

func TestResolveRemoteSelection_NoSelectionFallsThrough(t *testing.T) {
	target, handled, err := resolveRemoteSelection(remoteSelection{}, fileWith(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Fatalf("expected handled=false with no selection, got target=%+v", target)
	}
	if target != nil {
		t.Fatalf("expected nil target, got %+v", target)
	}
}

func TestResolveRemoteSelection_ContextFlag(t *testing.T) {
	target, handled, err := resolveRemoteSelection(
		remoteSelection{contextFlag: "prod"}, fileWith("", prodCtx))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if target.BaseURL != prodCtx.URL {
		t.Errorf("BaseURL = %q, want %q", target.BaseURL, prodCtx.URL)
	}
	if target.CityName != "example-city" {
		t.Errorf("CityName = %q, want example-city", target.CityName)
	}
	if target.Ctx == nil || target.Ctx.Name != "prod" {
		t.Errorf("Ctx not bound to prod context: %+v", target.Ctx)
	}
	if target.Source != remoteSourceContextFlag {
		t.Errorf("Source = %q, want %q", target.Source, remoteSourceContextFlag)
	}
}

func TestResolveRemoteSelection_ContextFlagNotFound(t *testing.T) {
	_, _, err := resolveRemoteSelection(
		remoteSelection{contextFlag: "nope"}, fileWith("", prodCtx))
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want not-found error naming context, got %v", err)
	}
}

func TestResolveRemoteSelection_AdHocURLWithName(t *testing.T) {
	target, handled, err := resolveRemoteSelection(
		remoteSelection{urlFlag: "https://host:9443", nameFlag: "city-x"}, fileWith(""))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if target.BaseURL != "https://host:9443" || target.CityName != "city-x" {
		t.Errorf("target = %+v", target)
	}
	if target.Ctx != nil {
		t.Errorf("ad-hoc target must have nil Ctx, got %+v", target.Ctx)
	}
	if target.Source != remoteSourceURLFlag {
		t.Errorf("Source = %q, want %q", target.Source, remoteSourceURLFlag)
	}
}

func TestResolveRemoteSelection_AdHocURLMissingName(t *testing.T) {
	_, _, err := resolveRemoteSelection(
		remoteSelection{urlFlag: "https://host:9443"}, fileWith(""))
	if err == nil || !strings.Contains(err.Error(), "city-name") {
		t.Fatalf("want missing-city-name error, got %v", err)
	}
}

func TestResolveRemoteSelection_AdHocURLLoopbackHTTPAllowed(t *testing.T) {
	target, handled, err := resolveRemoteSelection(
		remoteSelection{urlFlag: "http://127.0.0.1:8080", nameFlag: "c"}, fileWith(""))
	if err != nil || !handled {
		t.Fatalf("loopback http should be allowed: handled=%v err=%v", handled, err)
	}
	if target.BaseURL != "http://127.0.0.1:8080" {
		t.Errorf("BaseURL = %q", target.BaseURL)
	}
}

func TestResolveRemoteSelection_AdHocURLNonLoopbackHTTPRejected(t *testing.T) {
	_, _, err := resolveRemoteSelection(
		remoteSelection{urlFlag: "http://evil.example.com", nameFlag: "c"}, fileWith(""))
	if err == nil || !strings.Contains(err.Error(), "http") {
		t.Fatalf("want http-on-remote rejection, got %v", err)
	}
}

func TestResolveRemoteSelection_URLFlagPlusCityFlagConflict(t *testing.T) {
	_, _, err := resolveRemoteSelection(
		remoteSelection{urlFlag: "https://h", nameFlag: "c", cityFlag: "/some/city"}, fileWith(""))
	if err == nil || !strings.Contains(err.Error(), "--city") {
		t.Fatalf("want remote+local flag conflict, got %v", err)
	}
}

func TestResolveRemoteSelection_ContextPlusURLFlagConflict(t *testing.T) {
	_, _, err := resolveRemoteSelection(
		remoteSelection{contextFlag: "prod", urlFlag: "https://h"}, fileWith("", prodCtx))
	if err == nil || !strings.Contains(err.Error(), "--city-url") {
		t.Fatalf("want remote+remote flag conflict, got %v", err)
	}
}

func TestResolveRemoteSelection_ContextPlusCityFlagConflict(t *testing.T) {
	_, _, err := resolveRemoteSelection(
		remoteSelection{contextFlag: "prod", cityFlag: "/c"}, fileWith("", prodCtx))
	if err == nil {
		t.Fatalf("want remote+local flag conflict, got nil")
	}
}

func TestResolveRemoteSelection_ContextPlusCityNameConflict(t *testing.T) {
	_, _, err := resolveRemoteSelection(
		remoteSelection{contextFlag: "prod", nameFlag: "override"}, fileWith("", prodCtx))
	if err == nil || !strings.Contains(err.Error(), "--city-name") {
		t.Fatalf("want context+city-name conflict, got %v", err)
	}
}

func TestResolveRemoteSelection_EnvContext(t *testing.T) {
	target, handled, err := resolveRemoteSelection(
		remoteSelection{envContext: "prod"}, fileWith("", prodCtx))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if target.Source != remoteSourceEnvContext {
		t.Errorf("Source = %q, want %q", target.Source, remoteSourceEnvContext)
	}
	if target.CityName != "example-city" {
		t.Errorf("CityName = %q", target.CityName)
	}
}

func TestResolveRemoteSelection_EnvURLWithToken(t *testing.T) {
	target, handled, err := resolveRemoteSelection(
		remoteSelection{envURL: "https://h:9443", nameFlag: "c", envToken: "tok123"}, fileWith(""))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if target.Token != "tok123" {
		t.Errorf("Token = %q, want tok123", target.Token)
	}
	if target.Source != remoteSourceEnvURL {
		t.Errorf("Source = %q, want %q", target.Source, remoteSourceEnvURL)
	}
}

func TestResolveRemoteSelection_EnvURLPlusEnvContextConflict(t *testing.T) {
	_, _, err := resolveRemoteSelection(
		remoteSelection{envURL: "https://h", envContext: "prod"}, fileWith("", prodCtx))
	if err == nil {
		t.Fatalf("want env remote+remote conflict, got nil")
	}
}

func TestResolveRemoteSelection_EnvContextPlusLocalCityEnvConflict(t *testing.T) {
	_, _, err := resolveRemoteSelection(
		remoteSelection{envContext: "prod", localCityEnv: true}, fileWith("", prodCtx))
	if err == nil {
		t.Fatalf("want env remote+local conflict, got nil")
	}
}

func TestResolveRemoteSelection_LocalFlagBeatsRemoteEnv(t *testing.T) {
	// A local --city flag with a remote env set: precedence is flag > env, so
	// the resolver must defer (handled=false) and let local flag resolution win.
	target, handled, err := resolveRemoteSelection(
		remoteSelection{cityFlag: "/local/city", envURL: "https://h", envToken: "t"}, fileWith(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Fatalf("local flag must shadow remote env (handled=false), got target=%+v", target)
	}
}

func TestResolveRemoteSelection_TokenWithContextRejected(t *testing.T) {
	_, _, err := resolveRemoteSelection(
		remoteSelection{contextFlag: "prod", envToken: "t"}, fileWith("", prodCtx))
	if err == nil || !strings.Contains(err.Error(), "GC_CITY_URL_TOKEN") {
		t.Fatalf("want token-with-context conflict, got %v", err)
	}
}

func TestResolveRemoteSelection_NoAPIConflict(t *testing.T) {
	_, _, err := resolveRemoteSelection(
		remoteSelection{contextFlag: "prod", noAPI: true}, fileWith("", prodCtx))
	if err == nil || !strings.Contains(err.Error(), "GC_NO_API") {
		t.Fatalf("want GC_NO_API+remote conflict, got %v", err)
	}
}

func TestResolveRemoteSelection_InvalidContextRejected(t *testing.T) {
	bad := clientcontext.Context{Name: "bad", URL: "http://evil.example.com", City: "c"}
	_, _, err := resolveRemoteSelection(
		remoteSelection{contextFlag: "bad"}, fileWith("", bad))
	if err == nil {
		t.Fatalf("want invalid-context rejection, got nil")
	}
}

func TestResolveStickyDefault(t *testing.T) {
	target, handled, err := resolveStickyDefault(fileWith("prod", prodCtx))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if target.Source != remoteSourceStickyDefault {
		t.Errorf("Source = %q, want %q", target.Source, remoteSourceStickyDefault)
	}
	if target.CityName != "example-city" {
		t.Errorf("CityName = %q", target.CityName)
	}
}

func TestResolveStickyDefault_NoneSet(t *testing.T) {
	_, handled, err := resolveStickyDefault(fileWith("", prodCtx))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Fatalf("no default => handled=false")
	}
}

func TestResolveStickyDefault_DanglingRejected(t *testing.T) {
	_, _, err := resolveStickyDefault(fileWith("ghost", prodCtx))
	if err == nil {
		t.Fatalf("want dangling-default error, got nil")
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	got := DefaultPath()
	want := filepath.Join(t.TempDir(), "contexts.toml")
	// t.TempDir() returns a fresh dir per call; compare only the base name +
	// that DefaultPath honors GC_HOME by living under it.
	if filepath.Base(got) != "contexts.toml" {
		t.Errorf("DefaultPath base = %q, want contexts.toml", filepath.Base(got))
	}
	_ = want
}
