package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

const validWaiver = `
[[waiver]]
fingerprint = "dc07e84b7574"
check = "response-optional-property-removed"
target = "GET /v0/widgets/{id}"
reason = "label was never populated"
pr = "https://github.com/gastownhall/gascity/pull/1234"
`

func TestParsePolicyAcceptsCheckedInPolicy(t *testing.T) {
	root := bazeltest.RepoRoot(t)
	p, err := loadPolicy(filepath.Join(root, defaultPolicy))
	if err != nil {
		t.Fatalf("checked-in policy: %v", err)
	}
	levels := p.severityLevelsFile()
	for _, want := range []string{"api-schema-removed err\n", "response-optional-property-removed err\n"} {
		if !strings.Contains(levels, want) {
			t.Errorf("severity levels missing %q:\n%s", want, levels)
		}
	}
}

func TestParsePolicyValidatesWaivers(t *testing.T) {
	if _, err := parsePolicy([]byte(validWaiver)); err != nil {
		t.Fatalf("valid waiver rejected: %v", err)
	}
	if _, err := parsePolicy([]byte(strings.Replace(validWaiver, "https://github.com/gastownhall/gascity/pull/1234", "#1234", 1))); err != nil {
		t.Fatalf("#N pr rejected: %v", err)
	}
	cases := map[string]struct {
		policy  string
		wantErr string
	}{
		"missing reason":     {strings.Replace(validWaiver, `reason = "label was never populated"`, "", 1), "missing required fields: reason"},
		"missing pr":         {strings.Replace(validWaiver, `pr = "https://github.com/gastownhall/gascity/pull/1234"`, "", 1), "missing required fields: pr"},
		"blank target":       {strings.Replace(validWaiver, `target = "GET /v0/widgets/{id}"`, `target = " "`, 1), "missing required fields: target"},
		"placeholder pr":     {strings.Replace(validWaiver, "https://github.com/gastownhall/gascity/pull/1234", "#TODO", 1), "pr \"#TODO\""},
		"bad fingerprint":    {strings.Replace(validWaiver, "dc07e84b7574", "DC07", 1), "12 lowercase hex"},
		"duplicate waiver":   {validWaiver + validWaiver, "duplicate waiver"},
		"unknown key":        {validWaiver + "approved_by = \"me\"\n", "unknown keys"},
		"bad severity level": {"[[severity]]\ncheck = \"x\"\nlevel = \"fatal\"\nreason = \"r\"\n", "must be one of"},
		"severity no reason": {"[[severity]]\ncheck = \"x\"\nlevel = \"err\"\n", "check and reason are required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parsePolicy([]byte(tc.policy))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestProblemTypeRemovalsFlagsOnlyRemovedURNs(t *testing.T) {
	base := readFixture(t, "base.json")

	removed, err := problemTypeRemovals(base, readFixture(t, "removed-problem-type.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || !strings.Contains(removed[0].Text, "urn:gascity:error:widget-locked") {
		t.Fatalf("removals = %+v, want exactly widget-locked", removed)
	}
	f := removed[0]
	if f.ID != checkProblemTypeRemoved || f.target() != componentsSection || f.Level != levelErr {
		t.Fatalf("finding = %+v", f)
	}
	if f.Fingerprint != fingerprintFor(f.ID, "", componentsSection, "urn:gascity:error:widget-locked") {
		t.Fatalf("fingerprint %s does not follow the oasdiff formula", f.Fingerprint)
	}

	added, err := problemTypeRemovals(base, readFixture(t, "additive.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Fatalf("adding a problem type flagged: %+v", added)
	}
}

func TestProblemTypeRemovalsRejectsMalformedExtension(t *testing.T) {
	bad := []byte(`{"components":{"schemas":{"ErrorModel":{"properties":{"type":{"x-gascity-problem-types":"urn:x"}}}}}}`)
	if _, err := problemTypeRemovals(readFixture(t, "base.json"), bad); err == nil {
		t.Fatal("malformed extension accepted")
	}
}

func TestFingerprintMatchesOasdiff(t *testing.T) {
	// Fingerprint oasdiff v1.33.0 reports for the removed-response-property fixture.
	got := fingerprintFor("response-optional-property-removed", "GET", "/v0/widgets/{id}", "label", "200")
	if got != "dc07e84b7574" {
		t.Fatalf("fingerprint = %s, want dc07e84b7574", got)
	}
}

func TestParseOasdiffChangelogKeepsOnlyErrors(t *testing.T) {
	data := []byte(`[
	 {"id":"response-optional-property-added","text":"a","level":1,"operation":"GET","path":"/x","fingerprint":"000000000001"},
	 {"id":"request-property-removed","text":"w","level":2,"operation":"GET","path":"/x","fingerprint":"000000000002"},
	 {"id":"api-schema-removed","text":"e","level":3,"section":"components","fingerprint":"000000000003"}]`)
	got, err := parseOasdiffChangelog(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "api-schema-removed" || got[0].target() != "components" {
		t.Fatalf("got %+v", got)
	}
	if got, err := parseOasdiffChangelog([]byte("null")); err != nil || len(got) != 0 {
		t.Fatalf("null changelog: %+v, %v", got, err)
	}
	if _, err := parseOasdiffChangelog([]byte("Error: boom")); err == nil {
		t.Fatal("non-JSON output accepted")
	}
}

func TestEvaluateAppliesWaivers(t *testing.T) {
	p, err := parsePolicy([]byte(validWaiver))
	if err != nil {
		t.Fatal(err)
	}
	removal := finding{
		ID: "response-optional-property-removed", Text: "removed label", Level: levelErr,
		Operation: "GET", Path: "/v0/widgets/{id}", Fingerprint: "dc07e84b7574",
	}
	other := finding{ID: "api-schema-removed", Text: "removed Widget", Level: levelErr, Section: "components", Fingerprint: "aaaaaaaaaaaa"}

	v, err := evaluate([]finding{removal, other}, p.Waivers)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Waived) != 1 || len(v.Unwaived) != 1 || v.Unwaived[0].ID != "api-schema-removed" || len(v.Unused) != 0 {
		t.Fatalf("verdict = %+v", v)
	}
	var out bytes.Buffer
	if pass, err := report(&out, v, defaultPolicy); err != nil || pass {
		t.Fatalf("report pass=%v err=%v with an unwaived change", pass, err)
	}
	for _, want := range []string{"[[waiver]]", `fingerprint = "aaaaaaaaaaaa"`, `target = "components"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report missing %q:\n%s", want, out.String())
		}
	}

	v, err = evaluate(nil, p.Waivers)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if pass, err := report(&out, v, defaultPolicy); err != nil || !pass ||
		!strings.Contains(out.String(), "matches no change") || !strings.Contains(out.String(), "later identical change") {
		t.Fatalf("unused waiver should pass with a prune note:\n%s", out.String())
	}

	mismatched := removal
	mismatched.Operation = "POST"
	if _, err := evaluate([]finding{mismatched}, p.Waivers); err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("mismatched waiver target err = %v", err)
	}
}

// TestLoadBaseReadsSpecAtGitRef covers the path CI takes: the base spec is
// read from a commit (-base / $OPENAPI_BREAKING_BASE, else the merge base of
// HEAD and origin/main), never from the working tree.
func TestLoadBaseReadsSpecAtGitRef(t *testing.T) {
	isolateGit(t)
	t.Chdir(t.TempDir())
	runGit(t, "init", "--quiet")
	runGit(t, "config", "user.name", "openapi-breaking test")
	runGit(t, "config", "user.email", "test@example.com")
	commitSpec := func(content, msg string) string {
		t.Helper()
		path := filepath.FromSlash(defaultRevision)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, "add", defaultRevision)
		runGit(t, "commit", "--quiet", "-m", msg)
		return strings.TrimSpace(runGit(t, "rev-parse", "HEAD"))
	}
	const baseSpec = `{"openapi":"3.1.0","info":{"title":"base","version":"1"}}`
	base := commitSpec(baseSpec, "base")
	runGit(t, "update-ref", "refs/remotes/"+defaultUpstream, base)
	commitSpec(`{"openapi":"3.1.0","info":{"title":"head","version":"2"}}`, "head")

	for name, ref := range map[string]string{"explicit ref": base, "merge-base fallback": ""} {
		got, err := loadBase(context.Background(), options{BaseRef: ref, Revision: defaultRevision})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != baseSpec {
			t.Errorf("%s: base spec = %s, want the spec committed at %s", name, got, base)
		}
	}

	const missing = "no-such-ref"
	_, err := loadBase(context.Background(), options{BaseRef: missing, Revision: defaultRevision})
	if err == nil || !strings.Contains(err.Error(), "reading base spec at "+missing) {
		t.Fatalf("missing ref: err = %v, want an error naming %s", err, missing)
	}
}

// isolateGit points every git subprocess in the test, loadBase's included, at
// the repository in the working directory. A test run from a git hook
// inherits GIT_DIR, GIT_INDEX_FILE, and friends for the outer repository.
// System and global git config are ignored too.
func isolateGit(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, "GIT_") {
			continue
		}
		t.Setenv(key, "") // restores the inherited value after the test
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
}

// runGit builds fixtures with gitOutput, the git runner loadBase uses,
// instead of a second exec.Command call site in test source.
func runGit(t *testing.T, args ...string) string {
	t.Helper()
	out, err := gitOutput(context.Background(), args...)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
