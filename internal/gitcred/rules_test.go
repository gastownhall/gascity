package gitcred

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeCredFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func TestLoadMissingFilesNotError(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(EnvCredentialsFile, "")
	t.Setenv(EnvCredentialCommand, "")
	rules, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rules.All()) != 0 {
		t.Fatalf("expected no rules, got %d", len(rules.All()))
	}
	if rules.HasCommandLayer() {
		t.Fatalf("unexpected command layer")
	}
}

func TestLoadLayeredOrder(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("GC_HOME", home)
	t.Setenv(EnvCredentialsFile, "")
	t.Setenv(EnvCredentialCommand, "")

	writeCredFile(t, filepath.Join(home, "credentials.toml"), `
[[credential]]
match = "github.com"
helper = "gh auth token"
`, 0o600)
	writeCredFile(t, filepath.Join(city, ".gc", "credentials.toml"), `
[[credential]]
match = "github.com/gascity"
token_env = "TOK"
`, 0o600)

	rules, err := Load(city)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	all := rules.All()
	if len(all) != 2 {
		t.Fatalf("want 2 rules, got %d", len(all))
	}
	// City layer is highest precedence → first.
	if all[0].Match != "github.com/gascity" {
		t.Fatalf("want city rule first, got %q", all[0].Match)
	}
	if all[1].Match != "github.com" {
		t.Fatalf("want home rule second, got %q", all[1].Match)
	}
}

func TestLoadEnvFileReplacesFileLayers(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	explicit := filepath.Join(t.TempDir(), "explicit.toml")
	t.Setenv("GC_HOME", home)
	t.Setenv(EnvCredentialCommand, "")

	writeCredFile(t, filepath.Join(home, "credentials.toml"), "[[credential]]\nmatch=\"a.com\"\nhelper=\"x\"\n", 0o600)
	writeCredFile(t, filepath.Join(city, ".gc", "credentials.toml"), "[[credential]]\nmatch=\"b.com\"\nhelper=\"y\"\n", 0o600)
	writeCredFile(t, explicit, "[[credential]]\nmatch=\"c.com\"\nhelper=\"z\"\n", 0o600)
	t.Setenv(EnvCredentialsFile, explicit)

	rules, err := Load(city)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	all := rules.All()
	if len(all) != 1 || all[0].Match != "c.com" {
		t.Fatalf("env file must replace file layers, got %+v", all)
	}
}

func TestLoadInsecurePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are POSIX-only")
	}
	city := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(EnvCredentialsFile, "")
	t.Setenv(EnvCredentialCommand, "")
	writeCredFile(t, filepath.Join(city, ".gc", "credentials.toml"), "[[credential]]\nmatch=\"a.com\"\nhelper=\"x\"\n", 0o644)

	_, err := Load(city)
	if !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("want ErrInsecurePermissions, got %v", err)
	}
}

func TestLoadRejectsLiteralSecretKeys(t *testing.T) {
	for _, key := range []string{"token", "password", "secret"} {
		t.Run(key, func(t *testing.T) {
			city := t.TempDir()
			t.Setenv("GC_HOME", t.TempDir())
			t.Setenv(EnvCredentialsFile, "")
			t.Setenv(EnvCredentialCommand, "")
			writeCredFile(t, filepath.Join(city, ".gc", "credentials.toml"),
				"[[credential]]\nmatch=\"a.com\"\n"+key+"=\"ghp_secretvalue\"\n", 0o600)
			_, err := Load(city)
			if err == nil {
				t.Fatalf("expected hard error for literal %q key", key)
			}
			if strings.Contains(err.Error(), "ghp_secretvalue") {
				t.Fatalf("error leaked the secret value: %v", err)
			}
		})
	}
}

func TestLoadRejectsPointerCardinality(t *testing.T) {
	tests := map[string]string{
		"zero pointers": "[[credential]]\nmatch=\"a.com\"\n",
		"two pointers":  "[[credential]]\nmatch=\"a.com\"\nhelper=\"x\"\ntoken_env=\"Y\"\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			city := t.TempDir()
			t.Setenv("GC_HOME", t.TempDir())
			t.Setenv(EnvCredentialsFile, "")
			t.Setenv(EnvCredentialCommand, "")
			writeCredFile(t, filepath.Join(city, ".gc", "credentials.toml"), body, 0o600)
			if _, err := Load(city); err == nil {
				t.Fatalf("expected hard error for %s", name)
			}
		})
	}
}

func TestLoadRecordsCommandLayer(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(EnvCredentialsFile, "")
	t.Setenv(EnvCredentialCommand, "my-helper get")
	rules, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !rules.HasCommandLayer() {
		t.Fatalf("expected command layer recorded")
	}
}

func TestLoadSkipsCityLayerWhenRootEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	t.Setenv(EnvCredentialsFile, "")
	t.Setenv(EnvCredentialCommand, "")
	writeCredFile(t, filepath.Join(home, "credentials.toml"), "[[credential]]\nmatch=\"a.com\"\nhelper=\"x\"\n", 0o600)
	rules, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rules.All()) != 1 {
		t.Fatalf("want 1 home rule, got %d", len(rules.All()))
	}
}
