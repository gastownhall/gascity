// CLI-side acceptance for the remote worker subset. The tests drive the real
// doBd entry point and assert both the routing decision and the refusal of
// verbs that still require the local store.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newRemoteCityRecorder returns an httptest server that records the requests a
// remote-routed verb makes, and answers every worker route with a claim-shaped
// 200. The recorded method+path is the assertion — not the response body.
func newRemoteCityRecorder(t *testing.T, seen *[]string) *httptest.Server {
	return newEventsTestServer(t, testEventRoutes{
		fallback: func(w http.ResponseWriter, r *http.Request) {
			*seen = append(*seen, r.Method+" "+r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"claimed","bead":{"id":"cr-gdeav.5.3","status":"in_progress","assignee":"worker-local-3-pool"}}`))
		},
	})
}

// remoteCityUnderTest points the CLI's ad-hoc remote tier at srv for the
// duration of one test. It sets the flag globals the same way
// cmd/gc/capstone_integration_test.go:51-53 does — that is the resolution path
// `gc --city-url … --city-name …` takes (targetFromURL, remote_target.go:292-296:
// an ad-hoc target REQUIRES --city-name, and carries only a GC_CITY_URL_TOKEN
// bearer, never a context) — and restores them afterwards so a leaked global
// cannot make an unrelated test resolve remote.
func remoteCityUnderTest(t *testing.T, srv *httptest.Server) {
	t.Helper()
	origCtx, origURL, origName := contextFlag, cityURLFlag, cityNameFlag
	t.Cleanup(func() { contextFlag, cityURLFlag, cityNameFlag = origCtx, origURL, origName })
	contextFlag, cityURLFlag, cityNameFlag = "", srv.URL, "mc"
	t.Setenv("GC_HOME", t.TempDir())
	// The claim leg names its claimant from the environment on the local path
	// (cmd_bd_by_id.go:1296-1300); the remote leg must honor the same identity
	// rather than the transport identity, so the tests set it the way a pool
	// session does.
	t.Setenv("BEADS_ACTOR", "worker-local-3-pool")
}

// TestDoBd_ClaimUnderRemoteTarget_ReachesTheWorkerRoute is the load-bearing RED
// line: `gc bd update <id> --claim` (in-process by-ID arm at
// cmd_bd_by_id.go:187 → graph.Claim :1302) must, given a remote target, travel
// as POST /v0/city/{city}/worker/claim instead of failing the capability gate
// or exec'ing bd.
func TestDoBd_ClaimUnderRemoteTarget_ReachesTheWorkerRoute(t *testing.T) {
	var seen []string
	srv := newRemoteCityRecorder(t, &seen)
	remoteCityUnderTest(t, srv)

	var stdout, stderr bytes.Buffer
	code := doBd([]string{"update", "cr-gdeav.5.3", "--claim"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("remote claim exit = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	want := "POST /v0/city/mc/worker/claim"
	if !strings.Contains(strings.Join(seen, "\n"), want) {
		t.Fatalf("no request reached %s; requests seen: %v", want, seen)
	}
}

// TestDoBd_HeartbeatUnderRemoteTarget_ReachesTheWorkerRoute: today heartbeat is
// a passthrough to bd's own verb (rewriteBdHeartbeatArgs, cmd_bd.go:198-211),
// which is precisely the case an off-host worker cannot use — bd is not running
// where the worker is, and gc holds no lease table for it.
func TestDoBd_HeartbeatUnderRemoteTarget_ReachesTheWorkerRoute(t *testing.T) {
	var seen []string
	srv := newRemoteCityRecorder(t, &seen)
	remoteCityUnderTest(t, srv)

	var stdout, stderr bytes.Buffer
	code := doBd([]string{"heartbeat", "cr-gdeav.5.3"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("remote heartbeat exit = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	want := "POST /v0/city/mc/worker/heartbeat"
	if !strings.Contains(strings.Join(seen, "\n"), want) {
		t.Fatalf("no request reached %s; requests seen: %v", want, seen)
	}
}

// TestDoBd_ReleaseIfCurrentUnderRemoteTarget_UsesDeleteOnTheClaimResource
// keeps the CLI on the same route the edge family already chose (release is
// DELETE /worker/claim, and the local verb already takes the expected assignee
// positionally: parseBdReleaseIfCurrentArgs, cmd_bd.go:600-608).
func TestDoBd_ReleaseIfCurrentUnderRemoteTarget_UsesDeleteOnTheClaimResource(t *testing.T) {
	var seen []string
	srv := newRemoteCityRecorder(t, &seen)
	remoteCityUnderTest(t, srv)

	var stdout, stderr bytes.Buffer
	code := doBd([]string{"release-if-current", "cr-gdeav.5.3", "worker-local-3-pool"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("remote release exit = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	want := "DELETE /v0/city/mc/worker/claim"
	if !strings.Contains(strings.Join(seen, "\n"), want) {
		t.Fatalf("no request reached %s; requests seen: %v", want, seen)
	}
}

// TestDoBd_WorkerSubsetKeepsTheLocalJSONRowSchemaAndExitCode: the startup
// wrapper parses this row and cannot tell a local claim from a remote one, so
// the remote leg must answer with the same shape and the same 0.
func TestDoBd_WorkerSubsetKeepsTheLocalJSONRowSchemaAndExitCode(t *testing.T) {
	var seen []string
	srv := newRemoteCityRecorder(t, &seen)
	remoteCityUnderTest(t, srv)

	var stdout, stderr bytes.Buffer
	code := doBd([]string{"update", "cr-gdeav.5.3", "--claim", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("remote claim exit = %d, want 0; stderr=%q", code, stderr.String())
	}
	row := strings.TrimSpace(stdout.String())
	var parsed map[string]any
	if err := json.Unmarshal([]byte(row), &parsed); err != nil {
		t.Fatalf("remote leg must emit the same JSON row the local leg does; got %q (%v)", row, err)
	}
	for _, key := range []string{"id", "status", "assignee"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("remote claim row missing %q that the local by-ID claim carries; row=%v", key, parsed)
		}
	}
}

func TestParseBdWorkerCloseRefusesUnknownMetadata(t *testing.T) {
	for _, pair := range []string{"gc.routed_to=worker-local-3-pool", "malformed"} {
		if _, err := parseBdWorkerCloseArgs([]string{"mc-7", "--set-metadata", pair}); err == nil {
			t.Fatalf("metadata pair %q was silently discarded", pair)
		}
	}
}

// TestDoBd_NonWorkerVerb_StillRefusesAndNamesTheSubset is the negative half of
// the whitelist at the top of doBd (cmd_bd.go:317): passthrough verbs cannot be
// remote, and the refusal must say which verbs ARE available instead of the
// generic incremental-enablement text (remote_target.go:135-137).
func TestDoBd_NonWorkerVerb_StillRefusesAndNamesTheSubset(t *testing.T) {
	var seen []string
	srv := newRemoteCityRecorder(t, &seen)
	remoteCityUnderTest(t, srv)

	var stdout, stderr bytes.Buffer
	code := doBd([]string{"list"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc bd list under a remote target must refuse, got exit 0; stdout=%q", stdout.String())
	}
	msg := stdout.String() + stderr.String()
	if !strings.Contains(msg, "worker") {
		t.Fatalf("refusal must name the worker subset the remote tier does support; got %q", msg)
	}
	if len(seen) != 0 {
		t.Fatalf("a refused verb must not reach the city at all; requests: %v", seen)
	}
}
