package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	gcapi "github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/clientcontext"
)

// G7: the shared stream-status classifier decides reconnect/reauth/permanent
// identically for the city and supervisor streams.
func TestClassifyStreamStatus(t *testing.T) {
	cases := []struct {
		status    int
		retry     string
		reconnect bool
		reauth    bool
		delay     time.Duration
	}{
		{http.StatusServiceUnavailable, "", true, false, 0},                // 503 → reconnect (backoff)
		{http.StatusServiceUnavailable, "5", true, false, 5 * time.Second}, // 503 + Retry-After
		{http.StatusTooManyRequests, "12", true, false, 12 * time.Second},  // 429 + Retry-After
		{http.StatusUnauthorized, "", false, true, 0},                      // 401 → reauth
		{http.StatusForbidden, "", false, false, 0},                        // 403 → permanent
		{http.StatusNotFound, "5", false, false, 0},                        // 404 → permanent (Retry-After ignored)
		{http.StatusMisdirectedRequest, "", false, false, 0},               // 421 → permanent
		{http.StatusBadGateway, "", false, false, 0},                       // 502 → permanent
	}
	for _, c := range cases {
		got := classifyStreamStatus(c.status, c.retry)
		if got.reconnect != c.reconnect || got.reauth != c.reauth || got.delay != c.delay {
			t.Errorf("classifyStreamStatus(%d, %q) = %+v, want {reconnect:%v reauth:%v delay:%v}",
				c.status, c.retry, got, c.reconnect, c.reauth, c.delay)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("30"); d != 30*time.Second {
		t.Errorf("30 -> %v", d)
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Errorf("empty -> %v", d)
	}
	if d := parseRetryAfter("Wed, 21 Oct 2026 07:28:00 GMT"); d != 0 {
		t.Errorf("http-date -> %v (want 0; delta-seconds only)", d)
	}
	if d := parseRetryAfter("-5"); d != 0 {
		t.Errorf("negative -> %v", d)
	}
	// A hostile Retry-After is capped.
	if d := parseRetryAfter("100000"); d != streamReconnectMax*4 {
		t.Errorf("huge -> %v, want cap %v", d, streamReconnectMax*4)
	}
}

// --api and a remote flag (--city-url/--context) both select a remote city and
// share the flag tier, so combining them is a loud conflict (gate G3).
func TestResolveEventsScope_ApiPlusRemoteFlagConflict(t *testing.T) {
	prev := contextFlag
	contextFlag = "prod"
	t.Cleanup(func() { contextFlag = prev })

	if _, err := resolveEventsScope("https://remote:9443"); err == nil ||
		!strings.Contains(err.Error(), "cannot combine --api") {
		t.Fatalf("want --api + --context conflict, got %v", err)
	}
}

// The core G3 property: a remote events scope (an explicit --api that is not the
// local supervisor) must never read the local .gc/events.jsonl on a 404 — that
// would be the local-disk fallback the design forbids.
func TestShouldUseLocalCityEventsFallback_RemoteScopeNeverReadsJsonl(t *testing.T) {
	scope := eventsAPIScope{cityPath: "/some/local/city", explicitAPI: true, localSupervisorAPI: false}
	notFound := &eventsAPIError{statusCode: http.StatusNotFound, detail: "city \"mc\" not found"}
	if shouldUseLocalCityEventsFallback(scope, notFound) {
		t.Fatal("a remote events scope must NOT fall back to .gc/events.jsonl on 404")
	}
}

// gc events under a remote --context now streams from the hosted city
// (previously refused with "does not support a remote city"). The scope carries
// a pre-built authenticated genclient (bearer/TLS/401-remint, backed by the
// no-timeout stream client) and the context's city name, so
// `gc --context prod events --follow` works as the runbook documents.
func TestResolveEventsScope_RemoteContextStreams(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := doContextAdd(clientcontext.Context{Name: "prod", URL: "https://box:9443", City: "mc"}, &out, &errb); code != 0 {
		t.Fatalf("seed context: %q", errb.String())
	}
	prev := contextFlag
	contextFlag = "prod"
	t.Cleanup(func() { contextFlag = prev })

	scope, err := resolveEventsScope("")
	if err != nil {
		t.Fatalf("gc events under a remote context should now resolve, got %v", err)
	}
	if scope.gen == nil {
		t.Fatal("remote events scope must carry an authenticated genclient")
	}
	if scope.cityName != "mc" {
		t.Fatalf("cityName = %q, want mc", scope.cityName)
	}
}

// TestDoEventsRotate_RefusesRemote proves rotate (a mutation) is refused under a
// remote --context target rather than routed through the read-only events client
// (which would 401 on a hardened city). The read events subcommands still stream.
func TestDoEventsRotate_RefusesRemote(t *testing.T) {
	gen, err := gcapi.NewRemoteEventsClient("https://box:9443", gcapi.RemoteOptions{})
	if err != nil {
		t.Fatalf("NewRemoteEventsClient: %v", err)
	}
	scope := eventsAPIScope{apiURL: "https://box:9443", cityName: "mc", gen: gen}
	var out, errb bytes.Buffer
	if code := doEventsRotate(scope, false, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "not supported for a remote city") {
		t.Fatalf("stderr = %q, want remote-rotate refusal", errb.String())
	}
}
