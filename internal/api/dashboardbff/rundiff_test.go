package dashboardbff

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// initGitRepo creates a minimal git repo at dir and returns dir. It is used by
// the run-diff allowlist tests, which need a real repo under the resolved city
// path so an in-city execution path is accepted end-to-end.
func initGitRepo(t *testing.T, dir string) string {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// postRunDiff issues a guarded POST to the run-diff endpoint. The plane's
// mutation guard requires X-GC-Request, so every call carries it.
func postRunDiff(t *testing.T, p *Plane, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "dashboard")
	req.Header.Set("Content-Type", "application/json")
	p.Handler().ServeHTTP(rec, req)
	return rec
}

func TestRunDiffUnknownCity404(t *testing.T) {
	p := New(Deps{Resolver: mapResolver{}})
	rec := postRunDiff(t, p, "/api/city/ghost/runs/gc-run-1/diff",
		`{"executionPath":{"kind":"known","path":"/tmp/repo"}}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown city", rec.Code)
	}
}

func TestRunDiffInvalidRunID400(t *testing.T) {
	p := New(Deps{Resolver: mapResolver{"alpha": "/srv/alpha"}})
	rec := postRunDiff(t, p, "/api/city/alpha/runs/bad%20id/diff",
		`{"executionPath":{"kind":"known","path":"/tmp/repo"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for invalid run id", rec.Code)
	}
}

// An execution path that fails the run-cwd shape gate (a relative path here)
// must surface as a 400, not a 500. The cwd is validated inside the exec
// methods, which return an *execError; the handler maps that to a client error.
func TestRunDiffInvalidCwd400(t *testing.T) {
	p := New(Deps{Resolver: mapResolver{"alpha": "/srv/alpha"}})
	rec := postRunDiff(t, p, "/api/city/alpha/runs/gc-run-1/diff",
		`{"executionPath":{"kind":"known","path":"relative/not/absolute"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for invalid (relative) cwd", rec.Code)
	}
}

// An absolute execution path OUTSIDE the resolved city dir and not in
// RunCwdAllowedRoots is rejected: the effective allowlist is {RunCwdAllowedRoots
// + cityPath}, which is never empty, so a path under neither root fails the
// run-cwd gate and surfaces as a 400. This is the MEDIUM security fix.
func TestRunDiffPathOutsideCityRejected(t *testing.T) {
	cityDir := initGitRepo(t, t.TempDir())
	outside := t.TempDir() // a sibling temp dir, not under the city and not allowlisted
	p := New(Deps{Resolver: mapResolver{"alpha": cityDir}})

	rec := postRunDiff(t, p, "/api/city/alpha/runs/gc-run-1/diff",
		`{"executionPath":{"kind":"known","path":`+jsonString(outside)+`}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a path outside the city dir", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid execution path") {
		t.Errorf("body = %s, want an invalid-execution-path error", rec.Body.String())
	}
}

// An execution path UNDER the resolved city dir is accepted by default (the city
// path is always part of the effective allowlist), so a real git repo there
// yields a 200 diff without any RunCwdAllowedRoots configured.
func TestRunDiffPathUnderCityAccepted(t *testing.T) {
	cityDir := initGitRepo(t, t.TempDir())
	p := New(Deps{Resolver: mapResolver{"alpha": cityDir}})

	rec := postRunDiff(t, p, "/api/city/alpha/runs/gc-run-1/diff",
		`{"executionPath":{"kind":"known","path":`+jsonString(cityDir)+`}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s), want 200 for an in-city git repo", rec.Code, rec.Body.String())
	}
	var got runDiffResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// A real (empty) repo resolves a known root rather than a path/allowlist
	// rejection; the exact kind ("ok"/"not_git") depends on git, but it must not
	// be a rejection.
	if got.RootPath.Kind == "unavailable" && got.RootPath.Reason == "path_unknown" {
		t.Errorf("in-city repo treated as path_unknown: %+v", got)
	}
}

// A path under an explicitly configured RunCwdAllowedRoots (but outside the city
// dir) is still accepted, confirming the city path is appended to, not replacing,
// the configured roots.
func TestRunDiffPathUnderConfiguredRootAccepted(t *testing.T) {
	cityDir := initGitRepo(t, t.TempDir())
	otherRoot := initGitRepo(t, t.TempDir())
	p := New(Deps{
		Resolver:           mapResolver{"alpha": cityDir},
		RunCwdAllowedRoots: []string{otherRoot},
	})

	rec := postRunDiff(t, p, "/api/city/alpha/runs/gc-run-1/diff",
		`{"executionPath":{"kind":"known","path":`+jsonString(otherRoot)+`}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s), want 200 for a path under a configured root", rec.Code, rec.Body.String())
	}
}

// jsonString quotes s as a JSON string literal for inline request bodies.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A missing/empty execution path is a malformed body, rejected at parse time.
func TestRunDiffMissingExecutionPath400(t *testing.T) {
	p := New(Deps{Resolver: mapResolver{"alpha": "/srv/alpha"}})
	rec := postRunDiff(t, p, "/api/city/alpha/runs/gc-run-1/diff",
		`{"executionPath":{"kind":"known","path":"   "}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty execution path", rec.Code)
	}
}

// An "unavailable" execution path is a well-formed request: it yields a
// path_unknown diff (no git is run, no cwd is needed), serialized with the wire
// shape the SPA validates.
func TestRunDiffUnavailablePathYieldsPathUnknown(t *testing.T) {
	p := New(Deps{Resolver: mapResolver{"alpha": "/srv/alpha"}})
	rec := postRunDiff(t, p, "/api/city/alpha/runs/gc-run-1/diff",
		`{"executionPath":{"kind":"unavailable","reason":"missing_cwd_and_rig_root"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got runDiffResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != "path_unknown" {
		t.Errorf("kind = %q, want path_unknown", got.Kind)
	}
	if got.RootPath.Kind != "unavailable" || got.RootPath.Reason != "path_unknown" {
		t.Errorf("rootPath = %+v, want unavailable/path_unknown", got.RootPath)
	}
	if got.Comparison.Kind != "unavailable" || got.Comparison.Reason != "path_unknown" {
		t.Errorf("comparison = %+v, want unavailable/path_unknown", got.Comparison)
	}
	// The SPA's decodeRunDiff requires status/changedFiles to be arrays (never
	// null) and patch to be a string; verify the wire JSON keeps that shape.
	js := rec.Body.String()
	if !strings.Contains(js, `"status":[]`) {
		t.Errorf("status must serialize as []: %s", js)
	}
	if !strings.Contains(js, `"changedFiles":[]`) {
		t.Errorf("changedFiles must serialize as []: %s", js)
	}
	if !strings.Contains(js, `"patch":""`) {
		t.Errorf("patch must serialize as empty string: %s", js)
	}
	// The error field is absent on a non-error variant.
	if strings.Contains(js, `"error"`) {
		t.Errorf("error field must be absent on path_unknown: %s", js)
	}
}

func TestRunDiffInvalidScopeQuery400(t *testing.T) {
	p := New(Deps{Resolver: mapResolver{"alpha": "/srv/alpha"}})
	cases := []struct{ name, query string }{
		{"bad scope kind", "?scope_kind=bogus&scope_ref=racoon"},
		{"kind without ref", "?scope_kind=city"},
		{"ref without kind", "?scope_ref=racoon"},
		{"bad scope ref", "?scope_kind=city&scope_ref=" + "%20space"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postRunDiff(t, p, "/api/city/alpha/runs/gc-run-1/diff"+c.query,
				`{"executionPath":{"kind":"unavailable","reason":"missing_cwd_and_rig_root"}}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %s", rec.Code, c.name)
			}
		})
	}
}

// A valid scope query is accepted (the cwd still comes from the body, so an
// unavailable path yields a 200 path_unknown diff regardless of scope).
func TestRunDiffValidScopeQueryAccepted(t *testing.T) {
	p := New(Deps{Resolver: mapResolver{"alpha": "/srv/alpha"}})
	rec := postRunDiff(t, p,
		"/api/city/alpha/runs/gc-run-1/diff?scope_kind=city&scope_ref=racoon-city",
		`{"executionPath":{"kind":"unavailable","reason":"missing_cwd_and_rig_root"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for valid scope query", rec.Code)
	}
}

func TestExecutionPathResolve(t *testing.T) {
	cases := []struct {
		name    string
		in      runExecutionPath
		wantCwd string
		wantErr bool
	}{
		{"known", runExecutionPath{Kind: "known", Path: "/tmp/repo"}, "/tmp/repo", false},
		{"known empty path", runExecutionPath{Kind: "known", Path: "  "}, "", true},
		{"unavailable ok", runExecutionPath{Kind: "unavailable", Reason: "missing_cwd_and_rig_root"}, "", false},
		{"unavailable bad reason", runExecutionPath{Kind: "unavailable", Reason: "nope"}, "", true},
		{"bad kind", runExecutionPath{Kind: "weird"}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd, errMsg := c.in.resolve()
			if (errMsg != "") != c.wantErr {
				t.Fatalf("err = %q, wantErr = %v", errMsg, c.wantErr)
			}
			if cwd != c.wantCwd {
				t.Errorf("cwd = %q, want %q", cwd, c.wantCwd)
			}
		})
	}
}

func TestClassifyRunDiffFile(t *testing.T) {
	cases := map[string]string{
		"src/app.ts":               "code",
		"internal/api/plane.go":    "code",
		"a/internal/api/plane.go":  "code", // a/ prefix stripped before classify
		"main.rs":                  "code",
		"styles/app.scss":          "code",
		"src/app.test.ts":          "test",
		"src/app.spec.tsx":         "test",
		"pkg/test/helper.go":       "test", // /test/ wins over .go code ext
		"frontend/tests/run.tsx":   "test",
		"README.md":                "docs",
		"guide.mdx":                "docs",
		"pkg/docs/api.go":          "docs", // /docs/ wins over .go code ext
		"docs/architecture/api.go": "code", // leading "docs/" is not "/docs/"; .go => code
		"config.json":              "config",
		"pack.toml":                "config",
		"deploy.yaml":              "config",
		"ci.yml":                   "config",
		"vite.config.ts":           "config", // .config.ts wins over code ext
		"package.json":             "config",
		"web/package.json":         "config",
		"LICENSE":                  "other",
		"bin/run":                  "other",
		"assets/logo.png":          "other",
	}
	for path, want := range cases {
		if got := classifyRunDiffFile(path); got != want {
			t.Errorf("classifyRunDiffFile(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestNormalizeGitDiffPath(t *testing.T) {
	cases := map[string]string{
		"a/src/app.ts":        "src/app.ts",
		"b/src/app.ts":        "src/app.ts",
		`"a/path with space"`: "path with space",
		"plain/path.go":       "plain/path.go",
	}
	for in, want := range cases {
		if got := normalizeGitDiffPath(in); got != want {
			t.Errorf("normalizeGitDiffPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsReviewableRunDiffPath(t *testing.T) {
	reviewable := []string{"src/app.ts", "docs/plan.md", ".beadsx/keep", "a/internal/x.go"}
	for _, p := range reviewable {
		if !isReviewableRunDiffPath(p) {
			t.Errorf("isReviewableRunDiffPath(%q) = false, want true", p)
		}
	}
	hidden := []string{".beads", ".beads/issues.db", ".gc", ".gc/state.json", "a/.beads/x", "b/.gc/y"}
	for _, p := range hidden {
		if isReviewableRunDiffPath(p) {
			t.Errorf("isReviewableRunDiffPath(%q) = true, want false (control-plane)", p)
		}
	}
}

func TestParseStatusLines(t *testing.T) {
	// Porcelain v1: XY then the path at column 3. The .gc line and a rename
	// touching .beads are filtered; a normal rename and modify are kept.
	in := strings.Join([]string{
		" M src/app.ts",
		"?? newfile.go",
		"R  old.ts -> new.ts",
		" M .gc/state.json",
		"R  src/a.ts -> .beads/b.ts",
		"   ",
		"",
	}, "\n")
	got := parseStatusLines(in)
	want := []string{" M src/app.ts", "?? newfile.go", "R  old.ts -> new.ts"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseStatusLines =\n%#v\nwant\n%#v", got, want)
	}
}

func TestParseNameStatus(t *testing.T) {
	// name-status uses tab separators. R/C carry a similarity score and a
	// src+dst pair; the destination path and collapsed status letter are used.
	// The .gc and .beads lines are dropped as non-reviewable.
	in := strings.Join([]string{
		"M\tsrc/app.ts",
		"A\tdocs/plan.md",
		"D\tcmd/old.go",
		"R100\tsrc/old.ts\tsrc/new.ts",
		"C75\tsrc/a.ts\tsrc/copy.ts",
		"M\t.gc/state.json",
		"A\t.beads/issues.db",
		"",
	}, "\n")
	got := parseNameStatus(in)
	want := []runChangedFile{
		{Path: "src/app.ts", Status: "M", Kind: "code"},
		{Path: "docs/plan.md", Status: "A", Kind: "docs"},
		{Path: "cmd/old.go", Status: "D", Kind: "code"},
		{Path: "src/new.ts", Status: "R", Kind: "code"},
		{Path: "src/copy.ts", Status: "C", Kind: "code"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseNameStatus =\n%#v\nwant\n%#v", got, want)
	}
}

func TestMergeChangedFiles(t *testing.T) {
	in := []runChangedFile{
		{Path: "b.ts", Status: "M", Kind: "code"},
		{Path: "a.ts", Status: "A", Kind: "code"},
		{Path: "b.ts", Status: "D", Kind: "code"}, // last wins
	}
	got := mergeChangedFiles(in)
	want := []runChangedFile{
		{Path: "a.ts", Status: "A", Kind: "code"},
		{Path: "b.ts", Status: "D", Kind: "code"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeChangedFiles =\n%#v\nwant\n%#v", got, want)
	}
}

func TestFilterReviewablePatch(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/src/app.ts b/src/app.ts",
		"@@ -1 +1 @@",
		"-old",
		"+new",
		"diff --git a/.gc/state.json b/.gc/state.json",
		"@@ -1 +1 @@",
		"-secret",
		"+secret2",
		"diff --git a/docs/plan.md b/docs/plan.md",
		"@@ -0,0 +1 @@",
		"+plan",
	}, "\n")
	got := filterReviewablePatch(patch)
	if strings.Contains(got, ".gc/state.json") || strings.Contains(got, "secret") {
		t.Errorf("control-plane block leaked into patch:\n%s", got)
	}
	if !strings.Contains(got, "a/src/app.ts") || !strings.Contains(got, "a/docs/plan.md") {
		t.Errorf("reviewable blocks dropped from patch:\n%s", got)
	}
	// Blocks are separated by a blank line (joinPatch).
	if !strings.Contains(got, "+new\n\ndiff --git a/docs/plan.md") {
		t.Errorf("blocks must be blank-line separated:\n%s", got)
	}
}

func TestFilterReviewablePatchEmpty(t *testing.T) {
	if got := filterReviewablePatch("   \n  "); got != "" {
		t.Errorf("empty patch = %q, want empty", got)
	}
}

func TestEmptyDiffWireShape(t *testing.T) {
	for _, kind := range []string{"not_git", "path_unknown"} {
		d := emptyDiff(kind)
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("marshal %s: %v", kind, err)
		}
		js := string(raw)
		if !strings.Contains(js, `"kind":"`+kind+`"`) {
			t.Errorf("%s: kind missing: %s", kind, js)
		}
		if !strings.Contains(js, `"status":[]`) || !strings.Contains(js, `"changedFiles":[]`) {
			t.Errorf("%s: arrays must be []: %s", kind, js)
		}
		if !strings.Contains(js, `"truncated":false`) {
			t.Errorf("%s: truncated must be false: %s", kind, js)
		}
		if strings.Contains(js, `"error"`) {
			t.Errorf("%s: error field must be absent: %s", kind, js)
		}
	}
}
