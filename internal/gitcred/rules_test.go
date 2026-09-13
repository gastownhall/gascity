package gitcred

import (
	"errors"
	"io/fs"
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
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
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
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
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
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
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
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv(EnvCredentialCommand, "")
	writeCredFile(t, filepath.Join(city, ".gc", "credentials.toml"), "[[credential]]\nmatch=\"a.com\"\nhelper=\"x\"\n", 0o644)

	_, err := Load(city)
	if !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("want ErrInsecurePermissions, got %v", err)
	}
}

func TestSecureMode(t *testing.T) {
	const egid = 1001
	const me = 1001
	tests := []struct {
		name string
		perm fs.FileMode
		uid  uint32
		gid  uint32
		want bool
	}{
		{"owner read only", 0o400, me, egid, true},
		{"owner read write", 0o600, me, egid, true},
		{"kubernetes secret mount", 0o440, 0, egid, true},
		{"world readable", 0o644, 0, egid, false},
		{"world readable owner only otherwise", 0o404, me, egid, false},
		{"group writable", 0o660, 0, egid, false},
		{"group executable", 0o450, 0, egid, false},
		{"group readable foreign gid", 0o440, 0, egid + 1, false},
		{"group readable not root owned", 0o440, me, egid, false},
		{"group readable owner unknown", 0o440, unknownID, unknownID, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := secureMode(tc.perm, tc.uid, tc.gid, egid); got != tc.want {
				t.Fatalf("secureMode(%v, uid=%d, gid=%d, egid=%d) = %v, want %v",
					tc.perm, tc.uid, tc.gid, egid, got, tc.want)
			}
		})
	}
}

func TestLoadRejectsUserOwnedGroupRead(t *testing.T) {
	// The group-read exemption is for root-owned Secret mounts only. A 0640
	// file the user created themselves is still insecure, which is what keeps
	// laptop and CI behavior identical to the pre-exemption check.
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are POSIX-only")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: a file we create is root-owned and would be exempt")
	}
	city := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(EnvCredentialsFile, "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv(EnvCredentialCommand, "")
	writeCredFile(t, filepath.Join(city, ".gc", "credentials.toml"), "[[credential]]\nmatch=\"a.com\"\nhelper=\"x\"\n", 0o640)

	_, err := Load(city)
	if !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("want ErrInsecurePermissions, got %v", err)
	}
}

func TestLoadAcceptsRootOwnedGroupReadable(t *testing.T) {
	// The accept path end to end, on a real file. Only a root test process can
	// produce the root:ourgid 0440 shape kubelet mounts, so this is skipped
	// everywhere else; TestSecureMode covers the predicate unprivileged.
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are POSIX-only")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown the fixture to the Secret-mount shape")
	}
	city := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(EnvCredentialsFile, "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv(EnvCredentialCommand, "")
	path := filepath.Join(city, ".gc", "credentials.toml")
	writeCredFile(t, path, "[[credential]]\nmatch=\"a.com\"\ntoken_file=\"/run/x\"\n", 0o440)
	if err := os.Chown(path, 0, os.Getegid()); err != nil {
		t.Fatalf("chown: %v", err)
	}

	rules, err := Load(city)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if all := rules.All(); len(all) != 1 || all[0].Match != "a.com" {
		t.Fatalf("want the root-owned rule loaded, got %+v", all)
	}
}

func TestLoadRejectsLiteralSecretKeys(t *testing.T) {
	for _, key := range []string{"token", "password", "secret"} {
		t.Run(key, func(t *testing.T) {
			city := t.TempDir()
			t.Setenv("GC_HOME", t.TempDir())
			t.Setenv(EnvCredentialsFile, "")
			t.Setenv("GITHUB_TOKEN", "")
			t.Setenv("GH_TOKEN", "")
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
			t.Setenv("GITHUB_TOKEN", "")
			t.Setenv("GH_TOKEN", "")
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
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv(EnvCredentialCommand, "my-helper get")
	rules, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !rules.HasCommandLayer() {
		t.Fatalf("expected command layer recorded")
	}
}

func TestLoadCommandLayerSkipsGitHubDefault(t *testing.T) {
	// With both an ambient GitHub token and a configured command layer, the
	// built-in github.com default must NOT be created: the command layer is a
	// no-rule fallback consulted only when no rule matches, so a default rule
	// would shadow the deliberately-configured helper. Skipping the default
	// keeps command-layer precedence, which is what the Load comment promises.
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(EnvCredentialsFile, "")
	t.Setenv("GITHUB_TOKEN", "ghp_example")
	t.Setenv("GH_TOKEN", "")
	t.Setenv(EnvCredentialCommand, "my-helper get")
	rules, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := rules.MatchSource("https://github.com/org/repo"); ok {
		t.Fatalf("github.com default must be skipped when a command layer is configured")
	}
	if !rules.HasCommandLayer() {
		t.Fatalf("expected the command layer to remain recorded")
	}
}

func TestLoadSkipsCityLayerWhenRootEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	t.Setenv(EnvCredentialsFile, "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
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
