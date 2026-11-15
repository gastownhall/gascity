package doctor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func credCtx(t *testing.T) *CheckContext {
	t.Helper()
	city := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_GIT_CREDENTIALS_FILE", "")
	t.Setenv("GC_GIT_CREDENTIAL_COMMAND", "")
	// Clear the ambient GitHub token so the built-in github.com default rule
	// stays inert and these checks observe only their own configured rules.
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	return &CheckContext{CityPath: city}
}

func writeCityCred(t *testing.T, cityPath, body string, mode os.FileMode) {
	t.Helper()
	dir := filepath.Join(cityPath, ".gc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "credentials.toml")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod: %v", err)
	}
}

func TestPackCredentialsCheckNoRulesOK(t *testing.T) {
	ctx := credCtx(t)
	r := NewPackCredentialsCheck(nil).Run(ctx)
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg=%q", r.Status, r.Message)
	}
}

func TestPackCredentialsCheckInsecurePermsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are POSIX-only")
	}
	ctx := credCtx(t)
	writeCityCred(t, ctx.CityPath, "[[credential]]\nmatch=\"github.com\"\nhelper=\"x\"\n", 0o644)
	r := NewPackCredentialsCheck(nil).Run(ctx)
	if r.Status != StatusError {
		t.Fatalf("status = %v, want Error", r.Status)
	}
	if r.FixHint == "" {
		t.Fatalf("expected a FixHint")
	}
}

// assertPublicImportClean runs the check for a single public import and
// asserts it reports OK with no FixHint and never echoes secret.
func assertPublicImportClean(t *testing.T, ctx *CheckContext, source, secret string) {
	t.Helper()
	imports := map[string]config.Import{
		"public": {Source: source, Version: "^1.0"},
	}
	r := NewPackCredentialsCheck(imports).Run(ctx)
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg=%q fix=%q details=%v", r.Status, r.Message, r.FixHint, r.Details)
	}
	if r.FixHint != "" {
		t.Fatalf("FixHint = %q, want empty (a public import needs no credential)", r.FixHint)
	}
	if secret == "" {
		return
	}
	for _, line := range append([]string{r.Message, r.FixHint}, r.Details...) {
		if strings.Contains(line, secret) {
			t.Fatalf("secret leaked into output: %q", line)
		}
	}
}

// An unmatched networked remote is not evidence that a credential is
// required (issue #5894): offline, a public import is authored exactly like a
// private one, so a rule for some other host or namespace must not change the
// import's status or FixHint.
func TestPackCredentialsCheckUnmatchedPublicRemoteOK(t *testing.T) {
	const source = "https://github.com/gastownhall/gascity-packs"
	for _, tc := range []struct{ name, rule string }{
		{name: "zero rules"},
		{name: "unrelated host rule", rule: "gitlab.com/acme"},
		{name: "same-host unrelated namespace rule", rule: "github.com/acme-private"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := credCtx(t)
			if tc.rule != "" {
				writeCityCred(t, ctx.CityPath, "[[credential]]\nmatch=\""+tc.rule+"\"\nhelper=\"x\"\n", 0o600)
			}
			assertPublicImportClean(t, ctx, source, "")
		})
	}
}

// An ambient credential source — the built-in github.com default loaded from
// GH_TOKEN/GITHUB_TOKEN, or $GC_GIT_CREDENTIAL_COMMAND — must not turn a
// public import no file rule matches into a warning, nor echo the token.
func TestPackCredentialsCheckAmbientCredentialSourceOK(t *testing.T) {
	const tokenSentinel = "gh-token-sentinel-5894"
	for _, tc := range []struct{ name, env, value, source string }{
		{name: "GH_TOKEN default", env: "GH_TOKEN", value: tokenSentinel, source: "https://gitlab.com/gastownhall/tools"},
		{name: "GITHUB_TOKEN default", env: "GITHUB_TOKEN", value: tokenSentinel, source: "https://gitlab.com/gastownhall/tools"},
		{name: "credential command layer", env: "GC_GIT_CREDENTIAL_COMMAND", value: "printf x", source: "https://github.com/gastownhall/gascity-packs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := credCtx(t)
			t.Setenv(tc.env, tc.value)
			assertPublicImportClean(t, ctx, tc.source, tokenSentinel)
		})
	}
}

// Unmatched networked remotes stay visible as redacted Details lines so an
// operator can still see which imports no rule covers.
func TestPackCredentialsCheckUnmatchedRemoteDetails(t *testing.T) {
	ctx := credCtx(t)
	writeCityCred(t, ctx.CityPath, "[[credential]]\nmatch=\"gitlab.com/acme\"\nhelper=\"x\"\n", 0o600)
	imports := map[string]config.Import{
		"public":    {Source: "https://user:s3cret-token@github.com/gastownhall/gascity-packs", Version: "^1.0"},
		"tools":     {Source: "https://github.com/gascity/tools", Version: "^1.0"},
		"covered":   {Source: "https://gitlab.com/acme/private", Version: "^1.0"},
		"local-dir": {Source: "./packs/review"},
	}
	r := NewPackCredentialsCheck(imports).Run(ctx)
	if r.Status != StatusOK || r.FixHint != "" {
		t.Fatalf("status = %v fix=%q, want OK with no FixHint; details=%v", r.Status, r.FixHint, r.Details)
	}
	for _, want := range []string{
		"https://***@github.com/gastownhall/gascity-packs — no matching credential rule",
		"https://github.com/gascity/tools — no matching credential rule",
	} {
		n := 0
		for _, d := range r.Details {
			if d == want {
				n++
			}
		}
		if n != 1 {
			t.Errorf("Details contains %q %d time(s), want 1; details=%v", want, n, r.Details)
		}
	}
	for _, line := range append([]string{r.Message, r.FixHint}, r.Details...) {
		if strings.Contains(line, "s3cret-token") || strings.Contains(line, "user:") {
			t.Fatalf("userinfo leaked into output: %q", line)
		}
		if strings.Contains(line, "gitlab.com/acme/private") || strings.Contains(line, "packs/review") {
			t.Fatalf("matched or local import listed as unmatched: %q", line)
		}
	}
}

// A public HTTPS import that clones anonymously must stay healthy when an
// unrelated private-host rule is added (issue #5894): absence of a matching
// rule is not evidence that a credential is required.
func TestPackCredentialsCheckPublicImportWithUnrelatedRuleOK(t *testing.T) {
	ctx := credCtx(t)
	imports := map[string]config.Import{
		"public": {Source: "https://github.com/gastownhall/gascity-packs", Version: "^1.0"},
	}
	before := NewPackCredentialsCheck(imports).Run(ctx)
	if before.Status != StatusOK {
		t.Fatalf("zero rules: status = %v, want OK; msg=%q", before.Status, before.Message)
	}
	writeCityCred(t, ctx.CityPath, "[[credential]]\nmatch=\"git.private.example.com\"\nhelper=\"x\"\n", 0o600)
	after := NewPackCredentialsCheck(imports).Run(ctx)
	if after.Status != StatusOK {
		t.Fatalf("unrelated rule: status = %v, want OK (public import needs no credential); msg=%q fix=%q details=%v",
			after.Status, after.Message, after.FixHint, after.Details)
	}
}

func TestPackCredentialsCheckMatchedRemoteOK(t *testing.T) {
	ctx := credCtx(t)
	writeCityCred(t, ctx.CityPath, "[[credential]]\nmatch=\"github.com/gascity\"\nhelper=\"x\"\n", 0o600)
	imports := map[string]config.Import{
		"tools": {Source: "https://github.com/gascity/tools", Version: "^1.0"},
	}
	r := NewPackCredentialsCheck(imports).Run(ctx)
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg=%q details=%v", r.Status, r.Message, r.Details)
	}
}

func TestPackCredentialsCheckSkipsFileAndLocalImports(t *testing.T) {
	ctx := credCtx(t)
	writeCityCred(t, ctx.CityPath, "[[credential]]\nmatch=\"github.com/gascity\"\nhelper=\"x\"\n", 0o600)
	imports := map[string]config.Import{
		"local-file":  {Source: "file:///home/u/shared-packs/review"},
		"local-path":  {Source: "/Users/you/shared-packs/packs/review"},
		"local-rel":   {Source: "./packs/review"},
		"remote-http": {Source: "https://github.com/gascity/tools", Version: "^1.0"},
	}
	r := NewPackCredentialsCheck(imports).Run(ctx)
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want OK (file:// and local imports need no credential rule); msg=%q details=%v", r.Status, r.Message, r.Details)
	}
}

func TestPackCredentialsCheckNoFixNoWarmup(t *testing.T) {
	c := NewPackCredentialsCheck(nil)
	if c.CanFix() {
		t.Fatalf("CanFix must be false")
	}
	if c.WarmupEligible() {
		t.Fatalf("WarmupEligible must be false")
	}
}
