package doctor

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func credCtx(t *testing.T) *CheckContext {
	t.Helper()
	city := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_GIT_CREDENTIALS_FILE", "")
	t.Setenv("GC_GIT_CREDENTIAL_COMMAND", "")
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

func TestPackCredentialsCheckUnmatchedRemoteWarns(t *testing.T) {
	ctx := credCtx(t)
	writeCityCred(t, ctx.CityPath, "[[credential]]\nmatch=\"gitlab.com\"\nhelper=\"x\"\n", 0o600)
	imports := map[string]config.Import{
		"tools": {Source: "https://github.com/gascity/tools", Version: "^1.0"},
	}
	r := NewPackCredentialsCheck(imports).Run(ctx)
	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want Warning; msg=%q", r.Status, r.Message)
	}
	if r.FixHint == "" {
		t.Fatalf("expected a FixHint for the unmatched remote")
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
