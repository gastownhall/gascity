package gitcred_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/gitcred"
)

// TestSSHInjectionKeepaliveMatchesGitPackage pins the keepalive flags that
// inject.go hardcodes to the canonical ones in internal/git. gitcred cannot
// import internal/git (clone.go imports gitcred, so the dependency only runs
// one way), which leaves the two copies free to drift silently. This external
// test package can import both without recreating the cycle — nothing imports
// gitcred_test.
func TestSSHInjectionKeepaliveMatchesGitPackage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	t.Setenv(gitcred.EnvCredentialsFile, "")
	t.Setenv(gitcred.EnvCredentialCommand, "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	credFile := filepath.Join(home, "credentials.toml")
	if err := os.WriteFile(credFile, []byte("[[credential]]\nmatch=\"github.com/org\"\nssh_key_file=\"/keys/id\"\n"), 0o600); err != nil {
		t.Fatalf("write credentials.toml: %v", err)
	}

	inj, err := gitcred.CredentialedNetworkArgs("/usr/bin/gc", "", "git@github.com:org/repo.git")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if len(inj.Env) != 1 {
		t.Fatalf("Env = %#v, want one GIT_SSH_COMMAND entry", inj.Env)
	}
	if want := git.SSHKeepaliveOptions(); !strings.Contains(inj.Env[0], want) {
		t.Fatalf("Env[0] = %q\nwant it to contain %q (inject.go drifted from internal/git)", inj.Env[0], want)
	}
}
