package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// embeddedPinsFixture is a throwaway git repository shaped like the parts of
// gascity that scripts/check-embedded-pins reads.
type embeddedPinsFixture struct {
	t    *testing.T
	root string
	env  []string
}

func newEmbeddedPinsFixture(t *testing.T) *embeddedPinsFixture {
	t.Helper()
	f := &embeddedPinsFixture{
		t:    t,
		root: t.TempDir(),
		env: append(os.Environ(),
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
			"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
			"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com",
		),
	}
	f.git("init", "--quiet", "--initial-branch=main")
	f.write("internal/bootstrap/packs/core/pack.toml", "[pack]\nname = \"core\"\n")
	f.write("examples/bd/pack.toml", "[pack]\nname = \"bd\"\n")
	f.write("examples/bd/dolt/pack.toml", "[pack]\nname = \"dolt\"\n")
	f.write("deps.env", "DOLT_VERSION=2.2.0\nBD_VERSION=v1.3.1\n")
	f.writePublicPacks("sha:" + strings.Repeat("0", 40))
	contentCommit := f.commit("bundled content")
	f.writePublicPacks("sha:" + contentCommit)
	f.commit("pin bundled content")
	return f
}

func (f *embeddedPinsFixture) git(args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", f.root}, args...)...)
	cmd.Env = f.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *embeddedPinsFixture) write(rel, content string) {
	f.t.Helper()
	path := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *embeddedPinsFixture) writePublicPacks(pin string) {
	f.write("internal/config/public_packs.go", `package config

const (
	BundledPackImportVersion = "`+pin+`"
)

var SupersededBundledPackImportVersions = []string{
	"sha:f895c0ff47d6ee9334ed282a416387eb5b084d24",
}

var SupersededPublicGastownPackVersions = []string{
	"sha:fa91a3b4f1fe5cc9d1ba9ffbdd2d26274680adf9",
}

var SupersededPublicGascityPackVersions = []string{
	"sha:99464ed9240b1f6e6b7ab1d351f67016e1a973ff",
}
`)
}

func (f *embeddedPinsFixture) commit(msg string) string {
	f.t.Helper()
	f.git("add", "-A")
	f.git("commit", "--quiet", "-m", msg)
	return f.git("rev-parse", "HEAD")
}

func (f *embeddedPinsFixture) run(args ...string) (string, error) {
	f.t.Helper()
	script := filepath.Join(repoRoot(f.t), "scripts", "check-embedded-pins")
	cmd := exec.Command(script, append([]string{"--repo-root", f.root, "--latest-bd", "v1.3.1", "--bd-qualified-dolt", "2.2.0"}, args...)...)
	cmd.Env = f.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestCheckEmbeddedPins pins the release-time guard that keeps the versions gc
// embeds from silently drifting behind the latest release: the bundled
// core/bd/dolt pin must name a commit with the embedded content, examples must
// not carry a superseded bundled pin, and bd/dolt must match the latest beads
// release and the Dolt version it qualifies.
func TestCheckEmbeddedPins(t *testing.T) {
	t.Run("current pins pass", func(t *testing.T) {
		f := newEmbeddedPinsFixture(t)
		if out, err := f.run(); err != nil {
			t.Fatalf("check-embedded-pins: %v\n%s", err, out)
		}
	})

	t.Run("bundled content moved past the pin", func(t *testing.T) {
		f := newEmbeddedPinsFixture(t)
		f.write("internal/bootstrap/packs/core/formulas/new.toml", "formula = \"new\"\n")
		f.commit("change core")
		out, err := f.run()
		if err == nil || !strings.Contains(out, "bundled core/bd/dolt content changed") {
			t.Fatalf("err = %v, want stale bundled pin failure:\n%s", err, out)
		}
		if out, err := f.run("--skip-bundled"); err != nil {
			t.Fatalf("--skip-bundled still failed: %v\n%s", err, out)
		}
	})

	t.Run("content-equal pin off this branch", func(t *testing.T) {
		f := newEmbeddedPinsFixture(t)
		f.git("checkout", "--quiet", "-b", "side", "HEAD~1")
		f.write("README.md", "side branch\n")
		side := f.commit("side commit with the same pack trees")
		f.git("checkout", "--quiet", "main")
		f.writePublicPacks("sha:" + side)
		f.commit("pin a commit that is not an ancestor")
		out, err := f.run()
		if err == nil || !strings.Contains(out, "is not an ancestor of HEAD") {
			t.Fatalf("err = %v, want non-ancestor pin failure:\n%s", err, out)
		}
	})

	t.Run("newer beads release is advisory with --bd-advisory", func(t *testing.T) {
		f := newEmbeddedPinsFixture(t)
		script := filepath.Join(repoRoot(t), "scripts", "check-embedded-pins")
		cmd := exec.Command(script, "--repo-root", f.root, "--skip-bundled", "--latest-bd", "v1.3.2", "--bd-qualified-dolt", "2.2.0", "--bd-advisory")
		cmd.Env = f.env
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "::warning title=bd pin behind latest beads release::") {
			t.Fatalf("err = %v, want a passing run with a bd warning:\n%s", err, out)
		}
		cmd = exec.Command(script, "--repo-root", f.root, "--skip-bundled", "--latest-bd", "v1.3.2", "--bd-qualified-dolt", "2.2.0")
		cmd.Env = f.env
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("strict mode passed with bd behind the latest release:\n%s", out)
		}
	})

	t.Run("example carries a superseded pin", func(t *testing.T) {
		f := newEmbeddedPinsFixture(t)
		f.write("examples/demo/pack.toml", "[imports.core]\nversion = \"sha:f895c0ff47d6ee9334ed282a416387eb5b084d24\"\n")
		f.commit("stale example")
		out, err := f.run()
		if err == nil || !strings.Contains(out, "examples/demo/pack.toml") {
			t.Fatalf("err = %v, want stale example failure:\n%s", err, out)
		}
	})

	t.Run("doc carries a superseded public pack pin", func(t *testing.T) {
		f := newEmbeddedPinsFixture(t)
		f.write("docs/guide.md", "version = \"sha:fa91a3b4f1fe5cc9d1ba9ffbdd2d26274680adf9\"\n")
		f.commit("stale doc")
		out, err := f.run()
		if err == nil || !strings.Contains(out, "docs/guide.md (fa91a3b4f1fe)") {
			t.Fatalf("err = %v, want stale doc failure:\n%s", err, out)
		}
	})

	t.Run("bd and dolt behind the latest release", func(t *testing.T) {
		f := newEmbeddedPinsFixture(t)
		f.write("deps.env", "DOLT_VERSION=2.1.7\nBD_VERSION=v1.3.0\n")
		f.commit("old deps")
		out, err := f.run("--skip-bundled")
		if err == nil {
			t.Fatalf("stale bd/dolt pins passed:\n%s", out)
		}
		for _, want := range []string{"BD_VERSION=v1.3.0, latest beads release is v1.3.1", "DOLT_VERSION=2.1.7, beads v1.3.1 qualifies Dolt 2.2.0"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
	})
}

// TestRepoWritesNoSupersededPins runs the offline half of check-embedded-pins
// against this checkout: no example or doc may teach a superseded bundled,
// gastown or gascity pin.
func TestRepoWritesNoSupersededPins(t *testing.T) {
	root := repoRoot(t)
	for _, dir := range []string{"docs", "examples"} {
		if _, err := os.Stat(filepath.Join(root, dir)); err != nil {
			t.Skipf("%s not in this test's source tree: %v", dir, err)
		}
	}
	cmd := exec.Command(filepath.Join(root, "scripts", "check-embedded-pins"), "--repo-root", root, "--skip-bundled", "--skip-releases")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("check-embedded-pins: %v\n%s", err, out)
	}
}
