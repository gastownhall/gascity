package dashboardbff

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newPlaneWithEndpoints builds a plane with all routes registered. New()'s
// registerRoutes wires every endpoint (including the four host-side ones), so
// the helper just constructs the plane.
func newPlaneWithEndpoints(t *testing.T) *Plane {
	t.Helper()
	return New(Deps{})
}

func TestNormalizeGitView(t *testing.T) {
	for _, v := range []string{"recent-main", "recent-all", "today", "this-week"} {
		if got := normalizeGitView(v); got != v {
			t.Errorf("normalizeGitView(%q) = %q, want %q", v, got, v)
		}
	}
	for _, v := range []string{"", "bogus", "../etc", "RECENT-MAIN"} {
		if got := normalizeGitView(v); got != defaultGitView {
			t.Errorf("normalizeGitView(%q) = %q, want default %q", v, got, defaultGitView)
		}
	}
}

func TestParseGitLogSampleLine(t *testing.T) {
	// %H \t %h \t %an \t %aI \t %D \t %s — one commit with refs, one without,
	// plus a malformed line that must be skipped.
	stdout := strings.Join([]string{
		"abc123def\tabc123d\tAda Lovelace\t2026-06-24T10:00:00+00:00\tHEAD -> main, origin/main\tfix: thing",
		"def456abc\tdef456a\tGrace Hopper\t2026-06-23T09:00:00+00:00\t\tdocs: tidy",
		"too\tfew\tfields",
		"",
	}, "\n")

	items := parseGitLog(stdout)
	if len(items) != 2 {
		t.Fatalf("parsed %d commits, want 2 (malformed/empty skipped)", len(items))
	}
	first := items[0]
	if first.Sha != "abc123def" || first.ShortSha != "abc123d" {
		t.Errorf("sha/short = %q/%q", first.Sha, first.ShortSha)
	}
	if first.Author != "Ada Lovelace" || first.Date != "2026-06-24T10:00:00+00:00" {
		t.Errorf("author/date = %q/%q", first.Author, first.Date)
	}
	if first.Subject != "fix: thing" {
		t.Errorf("subject = %q", first.Subject)
	}
	if first.Refs != "HEAD -> main, origin/main" {
		t.Errorf("refs = %q", first.Refs)
	}
	if items[1].Refs != "" {
		t.Errorf("second commit refs = %q, want empty", items[1].Refs)
	}

	// refs must be omitted from the wire shape when empty.
	raw, err := json.Marshal(items[1])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "refs") {
		t.Errorf("empty refs must be omitted: %s", raw)
	}
	// A subject with a tab keeps everything after the 6th field intact.
	multi := parseGitLog("h\ts\ta\td\t\tsubject: with: colons")
	if len(multi) != 1 || multi[0].Subject != "subject: with: colons" {
		t.Errorf("subject splitN handling wrong: %+v", multi)
	}
}

func TestBuildsAbsentLog(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // empty dir: no log, no marker
	p := newPlaneWithEndpoints(t)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/builds", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got deployList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Source != nil {
		t.Errorf("source = %v, want null when log absent", *got.Source)
	}
	if len(got.Items) != 0 {
		t.Errorf("items = %v, want empty", got.Items)
	}
	if got.FailedMarker {
		t.Error("failed_marker should be false with no marker")
	}
	// source must serialize as null, items as [].
	if !strings.Contains(rec.Body.String(), `"source":null`) {
		t.Errorf("source must serialize as null: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("items must serialize as []: %s", rec.Body.String())
	}
}

func TestBuildsPresentLogAndMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logBody := strings.Join([]string{
		"[2026-06-20T08:00:00Z] deploy OK (aaa -> bbb)",
		"[2026-06-21T08:00:00Z] deploying bbb -> ccc",
		"[2026-06-22T08:00:00Z] DEPLOY FAILED at stage: tests",
		"[2026-06-23T08:00:00Z] manual recovery for th-7: poked it",
		"junk line with no timestamp",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(home, ".dev-deploy-log"), []byte(logBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".dev-deploy-FAILED"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := newPlaneWithEndpoints(t)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/builds", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got deployList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Source == nil || !strings.HasSuffix(*got.Source, ".dev-deploy-log") {
		t.Errorf("source = %v, want the log path", got.Source)
	}
	if !got.FailedMarker {
		t.Error("failed_marker should be true with marker present")
	}
	// Newest-first ordering: the recovery line (unknown) is most recent.
	if len(got.Items) != 4 {
		t.Fatalf("parsed %d records, want 4 (junk skipped): %+v", len(got.Items), got.Items)
	}
	if got.Items[0].At != "2026-06-23T08:00:00Z" || got.Items[0].Status != "unknown" {
		t.Errorf("newest record = %+v, want recovery/unknown first", got.Items[0])
	}
	wantStatus := map[string]string{
		"2026-06-22T08:00:00Z": "failed",
		"2026-06-21T08:00:00Z": "in-progress",
		"2026-06-20T08:00:00Z": "ok",
	}
	for _, it := range got.Items {
		if want, ok := wantStatus[it.At]; ok && it.Status != want {
			t.Errorf("record %s status = %q, want %q", it.At, it.Status, want)
		}
	}
}

func TestClientErrorsSingleObject(t *testing.T) {
	p := newPlaneWithEndpoints(t)
	body := `{"component":"sessions","operation":"load","message":"boom"}`
	req := httptest.NewRequest(http.MethodPost, "/api/client-errors", strings.NewReader(body))
	req.Header.Set("X-GC-Request", "dashboard") // POST passes the mutation guard
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 body should be empty, got %q", rec.Body.String())
	}
}

func TestClientErrorsArray(t *testing.T) {
	p := newPlaneWithEndpoints(t)
	body := `[{"component":"a","operation":"b","message":"c"},{"component":"d","operation":"e","message":"f"}]`
	req := httptest.NewRequest(http.MethodPost, "/api/client-errors", strings.NewReader(body))
	req.Header.Set("X-GC-Request", "dashboard")
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 for array body", rec.Code)
	}
}

func TestClientErrorsInvalidJSON(t *testing.T) {
	p := newPlaneWithEndpoints(t)
	req := httptest.NewRequest(http.MethodPost, "/api/client-errors", strings.NewReader("not json"))
	req.Header.Set("X-GC-Request", "dashboard")
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for malformed body", rec.Code)
	}
}

func TestClientErrorField(t *testing.T) {
	// Control bytes and CSI sequences (SGR and otherwise) are stripped whole and
	// whitespace is collapsed, so untrusted browser input cannot inject into the
	// log line.
	if got := clientErrorField("a\x07 \x1b[2K b\tc\n"); got != "a b c" {
		t.Errorf("clientErrorField sanitization = %q, want %q", got, "a b c")
	}
	// Over-long input is capped.
	long := strings.Repeat("x", maxClientErrorField+50)
	if got := clientErrorField(long); len(got) != maxClientErrorField {
		t.Errorf("clientErrorField len = %d, want %d", len(got), maxClientErrorField)
	}
}

func TestHealthSystemReturnsNestedJSON(t *testing.T) {
	p := newPlaneWithEndpoints(t)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health/system", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got systemHealth
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Admin.Pid != os.Getpid() {
		t.Errorf("admin.pid = %d, want %d", got.Admin.Pid, os.Getpid())
	}
	if got.Admin.NodeVersion == "" {
		t.Error("admin.node_version must be populated (go runtime version)")
	}
	if got.Host.CPUCount < 1 {
		t.Errorf("host.cpu_count = %d, want >= 1", got.Host.CPUCount)
	}
	// The nested objects must be present on the wire, snake_case keys intact.
	js := rec.Body.String()
	for _, key := range []string{`"admin"`, `"host"`, `"heap_used_bytes"`, `"load_avg_1"`, `"total_mem_bytes"`} {
		if !strings.Contains(js, key) {
			t.Errorf("system health JSON missing %s: %s", key, js)
		}
	}
}

func TestHealthLocalToolsReturnsThreeKeys(t *testing.T) {
	p := newPlaneWithEndpoints(t)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health/local-tools", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got localToolVersions
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Each tool reports a status of available or unavailable, regardless of
	// whether the binary exists on the test host.
	for name, tv := range map[string]localToolVersion{"dolt": got.Dolt, "beads": got.Beads, "gc": got.GC} {
		if tv.Status != "available" && tv.Status != "unavailable" {
			t.Errorf("%s status = %q, want available|unavailable", name, tv.Status)
		}
		if tv.Status == "available" && tv.Version == "" {
			t.Errorf("%s available but version empty", name)
		}
		if tv.Status == "unavailable" && tv.Reason == "" {
			t.Errorf("%s unavailable but reason empty", name)
		}
	}
	js := rec.Body.String()
	for _, key := range []string{`"dolt"`, `"beads"`, `"gc"`, `"status"`} {
		if !strings.Contains(js, key) {
			t.Errorf("local-tools JSON missing %s: %s", key, js)
		}
	}
}

func TestUnavailableShapeOmitsVersionFields(t *testing.T) {
	raw, err := json.Marshal(unavailable("nope"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(raw)
	if !strings.Contains(js, `"status":"unavailable"`) || !strings.Contains(js, `"reason":"nope"`) {
		t.Errorf("unavailable shape wrong: %s", js)
	}
	if strings.Contains(js, "version") || strings.Contains(js, "source") {
		t.Errorf("unavailable must omit version/source: %s", js)
	}
}

func TestAvailableShapeOmitsReason(t *testing.T) {
	raw, err := json.Marshal(localToolVersion{Status: "available", Version: "1.2.3", Source: "/usr/bin/dolt"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "reason") {
		t.Errorf("available must omit reason: %s", raw)
	}
}
