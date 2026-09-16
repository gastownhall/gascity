package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// remoteCity is a read-only stand-in for a hosted city. It serves exactly the
// reads a `gc sling --dry-run` pre-flight is allowed to make — the bead, the
// city config, the status snapshot — and records every mutating request so a
// test can assert a dry run wrote nothing. The bead is served from a byte
// buffer that only a mutating handler could change, so "gc.routed_to came back
// byte-identical" is a property of the fixture, not a claim the test makes.
type remoteCity struct {
	t   *testing.T
	mu  sync.Mutex
	rid string

	beadJSON  string
	cfgJSON   string
	path      string
	mutations []string
}

func newRemoteCity(t *testing.T, beadJSON, cfgJSON, cityPath string) (*remoteCity, *httptest.Server) {
	t.Helper()
	rc := &remoteCity{t: t, beadJSON: beadJSON, cfgJSON: cfgJSON, path: cityPath}
	srv := httptest.NewServer(http.HandlerFunc(rc.serve))
	t.Cleanup(srv.Close)
	return rc, srv
}

func (rc *remoteCity) serve(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		body, _ := io.ReadAll(r.Body)
		rc.mutations = append(rc.mutations, r.Method+" "+r.URL.Path+" "+string(body))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"mutation_refused","title":"read-only fixture"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/config"):
		_, _ = io.WriteString(w, rc.cfgJSON)
	case strings.HasSuffix(r.URL.Path, "/status"):
		_, _ = w.Write([]byte(`{"city":"mc","path":"` + rc.path + `"}`))
	case strings.Contains(r.URL.Path, "/beads/"):
		if strings.Contains(r.URL.Path, "MISSING") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"type":"not_found","title":"bead not found"}`)
			return
		}
		_, _ = io.WriteString(w, rc.beadJSON)
	default:
		rc.t.Errorf("unexpected read: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (rc *remoteCity) routedTo() string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var b struct {
		Metadata *map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(rc.beadJSON), &b); err != nil {
		rc.t.Fatalf("fixture bead json: %v", err)
	}
	if b.Metadata == nil {
		return ""
	}
	return (*b.Metadata)[beadmeta.RoutedToMetadataKey]
}

func (rc *remoteCity) beadBytes() string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.beadJSON
}

func (rc *remoteCity) countMutations() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.mutations)
}

// remoteBeadJSON builds one bead wire row with a fixed gc.routed_to so the
// byte-identity assertion has a stable subject.
func remoteBeadJSON(id, issueType, status, routedTo string) string {
	meta := map[string]string{beadmeta.RoutedToMetadataKey: routedTo}
	if routedTo == "" {
		meta = map[string]string{"gc.unrelated": "keep"}
	}
	body, err := json.Marshal(map[string]any{
		"id": id, "title": "a bead", "status": status, "issue_type": issueType,
		"created_at": "2026-09-16T00:00:00Z", "metadata": meta,
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// remoteCfgJSON is the GET /v0/config projection: rigs carry the name/path/prefix
// the bead-store projection needs, agents carry the name/dir/scope the
// reachability predicate needs.
func remoteCfgJSON(workspacePrefix string, rigs, agents string) string {
	return fmt.Sprintf(`{"workspace":{"name":"mc","prefix":%q},%s%s"rigs":%s,"agents":%s}`,
		workspacePrefix, `"patches":{"agent_count":1,"rig_count":1,"provider_count":0},`,
		rigs, agents)
}

// --dry-run against a remote city pre-flights the route and writes nothing.
// RED: today cmd/gc/sling_remote.go refuses --dry-run outright (exit 1), so the
// pre-flight never runs and nothing is printed.
func TestCmdSlingRemote_DryRunPreflightsAndWritesNothing(t *testing.T) {
	rigs := `[{"name":"alpha","path":"/cities/mc/rigs/alpha","prefix":"al"}]`
	agents := `[{"name":"mechanic","dir":"/cities/mc/rigs/alpha","scope":"rig"}]`
	rc, srv := newRemoteCity(t, remoteBeadJSON("al-7", "task", "open", "worker-2-pool"),
		remoteCfgJSON("mc", rigs, agents), "/cities/mc")

	before := rc.beadBytes()
	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL),
		[]string{"mechanic", "al-7"}, false, false, false, "", nil, "",
		false, false, false, "", false, false, true /*dryRun*/, "", "", false, &out, &errb)

	if code != 0 {
		t.Fatalf("dry-run exit = %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	got := out.String()
	for _, want := range []string{"dry-run", "al-7", "mechanic"} {
		if !strings.Contains(got, want) {
			t.Errorf("preview missing %q: %q", want, got)
		}
	}
	// The class the operator is routing is named, so a convoy/epic mis-target is
	// visible in the preview rather than discovered at the real sling.
	if !strings.Contains(got, "task") {
		t.Errorf("preview does not name the bead class: %q", got)
	}
	// The resolved remote target is echoed, exactly as the real remote sling does:
	// an operator must be able to see which control plane was pre-flighted.
	if !strings.Contains(errb.String(), "target:") || !strings.Contains(errb.String(), "mc @") {
		t.Errorf("dry-run did not echo the resolved target: %q", errb.String())
	}
	if n := rc.countMutations(); n != 0 {
		t.Errorf("dry-run issued %d mutating request(s) — a pre-flight must write nothing", n)
	}
	if after := rc.beadBytes(); after != before {
		t.Errorf("remote bead changed on a dry run:\n before=%s\n after=%s", before, after)
	}
	if rt := rc.routedTo(); rt != "worker-2-pool" {
		t.Errorf("%s = %q, want the untouched %q", beadmeta.RoutedToMetadataKey, rt, "worker-2-pool")
	}
}

// The check that actually matters: a cross-store route silently wedges pools
// (tr-6s7yx), so the remote pre-flight must run the SAME agreement predicate the
// local path runs and print the same refusal.
// RED: today --dry-run is refused before any agreement check can happen.
func TestCmdSlingRemote_DryRunRefusesCrossStoreRoute(t *testing.T) {
	rigs := `[{"name":"alpha","path":"/cities/mc/rigs/alpha","prefix":"al"},` +
		`{"name":"beta","path":"/cities/mc/rigs/beta","prefix":"bt"}]`
	// The bead lives in rig alpha; the target agent belongs to rig beta.
	agents := `[{"name":"worker","dir":"/cities/mc/rigs/beta","scope":"rig"}]`
	rc, srv := newRemoteCity(t, remoteBeadJSON("al-7", "task", "open", ""),
		remoteCfgJSON("mc", rigs, agents), "/cities/mc")

	before := rc.beadBytes()
	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL),
		[]string{"worker", "al-7"}, false, false, false, "", nil, "",
		false, false, false, "", false, false, true /*dryRun*/, "", "", false, &out, &errb)

	if code != 1 {
		t.Fatalf("cross-store dry-run exit = %d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	// The refusal is the domain's own message, not a second one invented here.
	combined := out.String() + errb.String()
	for _, want := range []string{"refusing cross-store route", "al-7", "worker", "tr-6s7yx"} {
		if !strings.Contains(combined, want) {
			t.Errorf("cross-store refusal missing %q: %q", want, combined)
		}
	}
	if n := rc.countMutations(); n != 0 {
		t.Errorf("dry-run issued %d mutating request(s)", n)
	}
	if after := rc.beadBytes(); after != before {
		t.Errorf("remote bead changed: %s -> %s", before, after)
	}
}

// A city-scoped target is cross-store eligible by design (vp-kvp), so the
// pre-flight must NOT refuse it — the same exemption the local guard applies.
func TestCmdSlingRemote_DryRunAllowsCityScopedTarget(t *testing.T) {
	rigs := `[{"name":"alpha","path":"/cities/mc/rigs/alpha","prefix":"al"}]`
	agents := `[{"name":"mayor","dir":"","scope":"city"}]`
	rc, srv := newRemoteCity(t, remoteBeadJSON("al-7", "task", "open", ""),
		remoteCfgJSON("mc", rigs, agents), "/cities/mc")

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL),
		[]string{"mayor", "al-7"}, false, false, false, "", nil, "",
		false, false, false, "", false, false, true /*dryRun*/, "", "", false, &out, &errb)
	if code != 0 {
		t.Fatalf("city-scoped dry-run exit = %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if n := rc.countMutations(); n != 0 {
		t.Errorf("dry-run issued %d mutating request(s)", n)
	}
}

// A target the hosted city does not configure is the first failure an operator
// pre-flights for: the real sling would route it into a lane that never claims it.
func TestCmdSlingRemote_DryRunRefusesUnknownTarget(t *testing.T) {
	rigs := `[{"name":"alpha","path":"/cities/mc/rigs/alpha","prefix":"al"}]`
	agents := `[{"name":"worker","dir":"/cities/mc/rigs/alpha","scope":"rig"}]`
	rc, srv := newRemoteCity(t, remoteBeadJSON("al-7", "task", "open", ""),
		remoteCfgJSON("mc", rigs, agents), "/cities/mc")

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL),
		[]string{"nope", "al-7"}, false, false, false, "", nil, "",
		false, false, false, "", false, false, true /*dryRun*/, "", "", false, &out, &errb)
	if code != 1 {
		t.Fatalf("unknown-target dry-run exit = %d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String()+errb.String(), "nope") {
		t.Errorf("refusal does not name the target: %q", out.String()+errb.String())
	}
	if n := rc.countMutations(); n != 0 {
		t.Errorf("dry-run issued %d mutating request(s)", n)
	}
}

// A bead the hosted city does not have must be refused, not previewed as a route.
func TestCmdSlingRemote_DryRunRefusesMissingBead(t *testing.T) {
	rigs := `[{"name":"alpha","path":"/cities/mc/rigs/alpha","prefix":"al"}]`
	agents := `[{"name":"worker","dir":"/cities/mc/rigs/alpha","scope":"rig"}]`
	rc, srv := newRemoteCity(t, remoteBeadJSON("al-7", "task", "open", ""),
		remoteCfgJSON("mc", rigs, agents), "/cities/mc")

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL),
		[]string{"worker", "al-MISSING"}, false, false, false, "", nil, "",
		false, false, false, "", false, false, true /*dryRun*/, "", "", false, &out, &errb)
	if code != 1 {
		t.Fatalf("missing-bead dry-run exit = %d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if n := rc.countMutations(); n != 0 {
		t.Errorf("dry-run issued %d mutating request(s)", n)
	}
}

// --json stays machine-readable for automation repointed at a remote city.
func TestCmdSlingRemote_DryRunJSON(t *testing.T) {
	rigs := `[{"name":"alpha","path":"/cities/mc/rigs/alpha","prefix":"al"}]`
	agents := `[{"name":"worker","dir":"/cities/mc/rigs/alpha","scope":"rig"}]`
	rc, srv := newRemoteCity(t, remoteBeadJSON("al-7", "task", "open", ""),
		remoteCfgJSON("mc", rigs, agents), "/cities/mc")

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL),
		[]string{"worker", "al-7"}, false, false, false, "", nil, "",
		false, false, false, "", false, false, true /*dryRun*/, "", "", true /*json*/, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &payload); err != nil {
		t.Fatalf("not json: %v (%q)", err, out.String())
	}
	if payload["dry_run"] != true {
		t.Errorf("dry_run = %v, want true (the local sling --json contract): %q", payload["dry_run"], out.String())
	}
	if payload["target"] != "worker" || payload["bead_id"] != "al-7" {
		t.Errorf("payload lost target/bead: %q", out.String())
	}
	if n := rc.countMutations(); n != 0 {
		t.Errorf("dry-run issued %d mutating request(s)", n)
	}
}

// A real (non-dry) sling is NOT widened by this change: it still forwards the
// mutation to the hosted city with the same body. (The fixture refuses writes,
// so a forwarded write surfaces here as a refused mutation rather than a 200.)
func TestCmdSlingRemote_RealSlingStillForwardsTheWrite(t *testing.T) {
	rigs := `[{"name":"alpha","path":"/cities/mc/rigs/alpha","prefix":"al"}]`
	agents := `[{"name":"worker","dir":"/cities/mc/rigs/alpha","scope":"rig"}]`
	rc, srv := newRemoteCity(t, remoteBeadJSON("al-7", "task", "open", ""),
		remoteCfgJSON("mc", rigs, agents), "/cities/mc")

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL),
		[]string{"worker", "al-7"}, false, false, false, "", []string{"pr=42"}, "",
		false, false, false, "", false, false, false /*NOT dryRun*/, "", "", false, &out, &errb)
	if code != 0 {
		t.Fatalf("real sling exit = %d, want 0; stderr=%q", code, errb.String())
	}
	// The write path is still a write path: one POST to /sling, body unchanged.
	// (The read-only fixture answers it 500, so the client reports the failure;
	// what this pins is that the mutation was ATTEMPTED with the same route.)
	if n := rc.countMutations(); n != 1 {
		t.Fatalf("real sling issued %d mutating request(s), want the POST /sling to still be forwarded", n)
	}
	if !strings.Contains(rc.mutations[0], "POST") || !strings.Contains(rc.mutations[0], "/sling") {
		t.Errorf("mutation was not the sling POST: %q", rc.mutations[0])
	}
	for _, want := range []string{`"target":"worker"`, `"bead":"al-7"`, `"pr":"42"`} {
		if !strings.Contains(rc.mutations[0], want) {
			t.Errorf("forwarded body lost %q: %q", want, rc.mutations[0])
		}
	}
}

// Widening --dry-run must not widen any other mode: the forms that need local
// state stay refused, and they stay refused BEFORE any request is made, so a
// dry run never reaches the hosted city for them either.
func TestCmdSlingRemote_DryRunStillRefusesLocalOnlyForms(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		formul bool
		onFor  string
		stdin  bool
		want   string
	}{
		{"stdin", []string{"mayor"}, false, "", true, "stdin"},
		{"one-arg", []string{"al-7"}, false, "", false, "explicit target"},
		{"inline-text", []string{"mayor", "write a readme"}, false, "", false, "inline text"},
		{"nudge", []string{"mayor", "al-7"}, false, "", false, "not supported"},
		{"on", []string{"mayor", "al-7"}, false, "mol-do-work", false, "not supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No server may be contacted at all for a refused form.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("refused form reached the wire: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer srv.Close()
			var out, errb bytes.Buffer
			code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL),
				tc.args, tc.formul, tc.name == "nudge", false, "", nil, "",
				false, false, false, tc.onFor, false, tc.stdin, true /*dryRun*/, "", "", false, &out, &errb)
			if code != 1 {
				t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
			}
			if !strings.Contains(out.String()+errb.String(), tc.want) {
				t.Errorf("refusal missing %q: %q", tc.want, out.String()+errb.String())
			}
		})
	}
}

// A formula launch has no source bead, so the store agreement is not a fact
// about it; the preview must say so rather than print an unverified agreement.
func TestCmdSlingRemote_DryRunFormulaStatesStoreNotDetermined(t *testing.T) {
	rigs := `[{"name":"alpha","path":"/cities/mc/rigs/alpha","prefix":"al"}]`
	agents := `[{"name":"worker","dir":"/cities/mc/rigs/alpha","scope":"rig"}]`
	rc, srv := newRemoteCity(t, remoteBeadJSON("al-7", "task", "open", ""),
		remoteCfgJSON("mc", rigs, agents), "/cities/mc")

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL),
		[]string{"worker", "mol-review"}, true /*isFormula*/, false, false, "", nil, "",
		false, false, false, "", false, false, true /*dryRun*/, "", "", false, &out, &errb)
	if code != 0 {
		t.Fatalf("formula dry-run exit = %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	combined := out.String() + errb.String()
	if !strings.Contains(combined, "mol-review") {
		t.Errorf("formula not named in preview: %q", combined)
	}
	if strings.Contains(combined, "would route") && !strings.Contains(combined, "not determined") {
		t.Errorf("formula preview asserted an agreement it never checked: %q", combined)
	}
	if n := rc.countMutations(); n != 0 {
		t.Errorf("dry-run issued %d mutating request(s)", n)
	}
}
