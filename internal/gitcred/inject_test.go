package gitcred

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// clearCredEnv resets the credential env so a test starts from a clean slate.
func clearCredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(EnvCredentialsFile, "")
	t.Setenv(EnvCredentialCommand, "")
}

func stubExe(t *testing.T, path string) {
	t.Helper()
	prev := osExecutable
	osExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { osExecutable = prev })
}

func TestInjectionZeroWhenNoFilesNoMatch(t *testing.T) {
	clearCredEnv(t)
	inj, err := CredentialedNetworkArgs("/usr/bin/gc", "", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if len(inj.CfgArgs) != 0 || len(inj.Env) != 0 || inj.Matched {
		t.Fatalf("expected zero injection, got %+v", inj)
	}
}

func TestInjectionZeroWhenTransportIncompatibleOnly(t *testing.T) {
	home := t.TempDir()
	clearCredEnv(t)
	t.Setenv("GC_HOME", home)
	writeCredFile(t, filepath.Join(home, "credentials.toml"),
		"[[credential]]\nmatch=\"github.com/org\"\nssh_key_file=\"~/.ssh/id\"\n", 0o600)
	// https URL with only an ssh rule → no match → zero injection.
	inj, err := CredentialedNetworkArgs("/usr/bin/gc", "", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if len(inj.CfgArgs) != 0 || len(inj.Env) != 0 || inj.Matched {
		t.Fatalf("expected zero injection, got %+v", inj)
	}
}

func TestInjectionHTTPSMatch(t *testing.T) {
	home := t.TempDir()
	clearCredEnv(t)
	t.Setenv("GC_HOME", home)
	writeCredFile(t, filepath.Join(home, "credentials.toml"),
		"[[credential]]\nmatch=\"github.com/org\"\nhelper=\"gh auth token\"\n", 0o600)

	gcExe := "/opt/my gc/bin/gc" // path with a space to exercise sq-quoting.
	inj, err := CredentialedNetworkArgs(gcExe, "/city", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if !inj.Matched {
		t.Fatalf("expected matched injection")
	}
	wantCfg := []string{
		"-c", "credential.helper=",
		"-c", "credential.helper=!'/opt/my gc/bin/gc' git-credential",
		"-c", "credential.useHttpPath=true",
	}
	if !reflect.DeepEqual(inj.CfgArgs, wantCfg) {
		t.Fatalf("CfgArgs = %#v\nwant %#v", inj.CfgArgs, wantCfg)
	}
	wantEnv := []string{"GIT_TERMINAL_PROMPT=0", "GC_CREDENTIAL_CITY=/city"}
	if !reflect.DeepEqual(inj.Env, wantEnv) {
		t.Fatalf("Env = %#v\nwant %#v", inj.Env, wantEnv)
	}
}

func TestInjectionOmitsCityWhenEmpty(t *testing.T) {
	home := t.TempDir()
	clearCredEnv(t)
	t.Setenv("GC_HOME", home)
	writeCredFile(t, filepath.Join(home, "credentials.toml"),
		"[[credential]]\nmatch=\"github.com\"\nhelper=\"gh auth token\"\n", 0o600)
	inj, err := CredentialedNetworkArgs("/usr/bin/gc", "", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	for _, e := range inj.Env {
		if strings.HasPrefix(e, EnvCredentialCity+"=") {
			t.Fatalf("GC_CREDENTIAL_CITY must be omitted when cityRoot is empty, got %v", inj.Env)
		}
	}
}

func TestInjectionSSHMatch(t *testing.T) {
	home := t.TempDir()
	clearCredEnv(t)
	t.Setenv("GC_HOME", home)
	writeCredFile(t, filepath.Join(home, "credentials.toml"),
		"[[credential]]\nmatch=\"github.com/org\"\nssh_key_file=\"/keys/id ed\"\n", 0o600)
	inj, err := CredentialedNetworkArgs("/usr/bin/gc", "", "git@github.com:org/repo.git")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if inj.CfgArgs != nil {
		t.Fatalf("ssh match must have nil CfgArgs, got %#v", inj.CfgArgs)
	}
	want := "GIT_SSH_COMMAND=ssh -i '/keys/id ed' -o IdentitiesOnly=yes -o BatchMode=yes"
	if len(inj.Env) != 1 || inj.Env[0] != want {
		t.Fatalf("Env = %#v\nwant [%q]", inj.Env, want)
	}
}

func TestInjectionResolvesExeViaSeamWhenEmpty(t *testing.T) {
	home := t.TempDir()
	clearCredEnv(t)
	t.Setenv("GC_HOME", home)
	writeCredFile(t, filepath.Join(home, "credentials.toml"),
		"[[credential]]\nmatch=\"github.com\"\nhelper=\"gh auth token\"\n", 0o600)
	stubExe(t, "/seam/gc")
	inj, err := CredentialedNetworkArgs("", "", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if !strings.Contains(strings.Join(inj.CfgArgs, " "), "'/seam/gc' git-credential") {
		t.Fatalf("expected seam exe in helper, got %#v", inj.CfgArgs)
	}
}

func TestInjectionFailsClosedOnBadPerms(t *testing.T) {
	city := t.TempDir()
	clearCredEnv(t)
	writeCredFile(t, filepath.Join(city, ".gc", "credentials.toml"),
		"[[credential]]\nmatch=\"github.com\"\nhelper=\"x\"\n", 0o644)
	if _, err := CredentialedNetworkArgs("/usr/bin/gc", city, "https://github.com/org/repo"); err == nil {
		t.Fatalf("expected fail-closed error on bad perms")
	}
}

func TestInjectionMatchedUnresolvableExe(t *testing.T) {
	home := t.TempDir()
	clearCredEnv(t)
	t.Setenv("GC_HOME", home)
	writeCredFile(t, filepath.Join(home, "credentials.toml"),
		"[[credential]]\nmatch=\"github.com\"\nhelper=\"x\"\n", 0o600)
	prev := osExecutable
	osExecutable = func() (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { osExecutable = prev })
	if _, err := CredentialedNetworkArgs("", "", "https://github.com/org/repo"); err == nil {
		t.Fatalf("expected error when matched rule has unresolvable gcExe")
	}
}

func TestInjectionCommandLayerWiresHelper(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvCredentialCommand, "my-helper get")
	inj, err := CredentialedNetworkArgs("/usr/bin/gc", "", "https://github.com/org/repo")
	if err != nil {
		t.Fatalf("CredentialedNetworkArgs: %v", err)
	}
	if len(inj.CfgArgs) == 0 {
		t.Fatalf("command layer must wire gc as the helper for https URLs")
	}
	if inj.Matched {
		t.Fatalf("command-layer fallback is not a rule match")
	}
}
