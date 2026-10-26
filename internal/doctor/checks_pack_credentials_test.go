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

// TestPackCredentialsCheckUnmatchedPublicRemoteOK pins issue #5894: an
// import no credential rule matches is not evidence that a credential is
// required. A public HTTPS import clones anonymously, so the check must stay
// OK — with zero rules, with rules for unrelated hosts or namespaces, and
// with only ambient credential sources active (the built-in github.com token
// default, or the $GC_GIT_CREDENTIAL_COMMAND fallback layer).
func TestPackCredentialsCheckUnmatchedPublicRemoteOK(t *testing.T) {
	cases := []struct {
		name    string
		imports map[string]config.Import
		cred    string // city credentials.toml body; empty for none
		env     map[string]string
	}{
		{
			name:    "zero rules",
			imports: map[string]config.Import{"public": {Source: "https://github.com/gastownhall/gascity-packs", Version: "^1.0"}},
		},
		{
			name:    "unrelated host rule",
			imports: map[string]config.Import{"public": {Source: "https://github.com/gastownhall/gascity-packs", Version: "^1.0"}},
			cred:    "[[credential]]\nmatch=\"gitlab.com/acme\"\nhelper=\"x\"\n",
		},
		{
			name:    "same host unrelated namespace rule",
			imports: map[string]config.Import{"public": {Source: "https://github.com/gastownhall/gascity-packs", Version: "^1.0"}},
			cred:    "[[credential]]\nmatch=\"github.com/acme-private\"\nhelper=\"x\"\n",
		},
		{
			name:    "ambient github token default",
			imports: map[string]config.Import{"tools": {Source: "https://gitlab.com/gastownhall/tools", Version: "^1.0"}},
			env:     map[string]string{"GH_TOKEN": "gh-sentinel-token"},
		},
		{
			name:    "credential command layer",
			imports: map[string]config.Import{"public": {Source: "https://github.com/gastownhall/gascity-packs", Version: "^1.0"}},
			env:     map[string]string{"GC_GIT_CREDENTIAL_COMMAND": "printf x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := credCtx(t)
			for key, val := range tc.env {
				t.Setenv(key, val)
			}
			if tc.cred != "" {
				writeCityCred(t, ctx.CityPath, tc.cred, 0o600)
			}
			r := NewPackCredentialsCheck(tc.imports).Run(ctx)
			if r.Status != StatusOK {
				t.Fatalf("status = %v, want OK (no matching rule is not evidence a credential is required); msg=%q fix=%q details=%v",
					r.Status, r.Message, r.FixHint, r.Details)
			}
			if r.FixHint != "" {
				t.Fatalf("FixHint = %q, want empty", r.FixHint)
			}
			seen := r.Message + "\n" + strings.Join(r.Details, "\n")
			for _, val := range tc.env {
				if strings.Contains(seen, val) && strings.Contains(val, "sentinel") {
					t.Fatalf("ambient token value leaked into the result: msg=%q details=%v", r.Message, r.Details)
				}
			}
		})
	}
}

// TestPackCredentialsCheckUnmatchedRemoteDetails pins the informational
// rendering of unmatched networked remotes once rules exist: one Details line
// per import, userinfo redacted, never a warning or a FixHint.
func TestPackCredentialsCheckUnmatchedRemoteDetails(t *testing.T) {
	ctx := credCtx(t)
	writeCityCred(t, ctx.CityPath, "[[credential]]\nmatch=\"gitlab.com/acme\"\nhelper=\"x\"\n", 0o600)
	imports := map[string]config.Import{
		"public": {Source: "https://github.com/gastownhall/gascity-packs", Version: "^1.0"},
		"tokend": {Source: "https://ci:s3cret-token@gitlab.com/gastownhall/tools", Version: "^1.0"},
		"local":  {Source: "file:///home/u/shared-packs/review"},
	}
	r := NewPackCredentialsCheck(imports).Run(ctx)
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg=%q fix=%q", r.Status, r.Message, r.FixHint)
	}
	if r.FixHint != "" {
		t.Fatalf("FixHint = %q, want empty", r.FixHint)
	}
	joined := strings.Join(r.Details, "\n")
	if n := strings.Count(joined, " — no matching credential rule"); n != 2 {
		t.Fatalf("unmatched detail lines = %d, want 2; details=%v", n, r.Details)
	}
	if !strings.Contains(joined, "https://github.com/gastownhall/gascity-packs — no matching credential rule") {
		t.Fatalf("details missing the unmatched public import line: %v", r.Details)
	}
	if !strings.Contains(joined, "https://***@gitlab.com/gastownhall/tools — no matching credential rule") {
		t.Fatalf("details missing the redacted userinfo line: %v", r.Details)
	}
	if strings.Contains(joined, "s3cret-token") {
		t.Fatalf("details leak the userinfo secret: %v", r.Details)
	}
	if strings.Contains(joined, "file:///home/u/shared-packs/review") {
		t.Fatalf("details include a file:// import, which authenticates no fetch: %v", r.Details)
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
