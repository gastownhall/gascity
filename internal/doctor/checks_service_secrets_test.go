package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func writeSecretFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	// WriteFile mode is umask-filtered; chmod to the exact fixture mode.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func serviceSecretsTestConfig() *config.City {
	return &config.City{
		Services: []config.Service{
			{Name: "bridge", Kind: "proxy_process", StateRoot: ".gc/services/bridge"},
			// Same state root as bridge: the shared dir must be checked once.
			{Name: "bridge-admin", Kind: "proxy_process", StateRoot: ".gc/services/bridge"},
			{Name: "intake", Kind: "proxy_process", StateRoot: ".gc/services/intake"},
		},
	}
}

func TestServiceSecretsPermsCheckOKWhenTight(t *testing.T) {
	cityPath := t.TempDir()
	writeSecretFile(t, filepath.Join(cityPath, ".gc", "services", "bridge", "secrets", "bot-token.txt"), 0o600)

	check := NewServiceSecretsPermsCheck(serviceSecretsTestConfig(), cityPath)
	r := check.Run(&CheckContext{CityPath: cityPath})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want OK (message %q)", r.Status, r.Message)
	}
}

func TestServiceSecretsPermsCheckOKWhenNoSecretsDir(t *testing.T) {
	cityPath := t.TempDir()
	check := NewServiceSecretsPermsCheck(serviceSecretsTestConfig(), cityPath)
	r := check.Run(&CheckContext{CityPath: cityPath})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want OK (message %q)", r.Status, r.Message)
	}
}

func TestServiceSecretsPermsCheckFlagsLooseFilesAndDirs(t *testing.T) {
	cityPath := t.TempDir()
	loose := filepath.Join(cityPath, ".gc", "services", "bridge", "secrets", "bot-token.txt")
	writeSecretFile(t, loose, 0o644)
	nested := filepath.Join(cityPath, ".gc", "services", "intake", "secrets", "nested")
	writeSecretFile(t, filepath.Join(nested, "key.pem"), 0o600)
	if err := os.Chmod(nested, 0o750); err != nil {
		t.Fatalf("chmod nested: %v", err)
	}

	check := NewServiceSecretsPermsCheck(serviceSecretsTestConfig(), cityPath)
	r := check.Run(&CheckContext{CityPath: cityPath})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning (message %q)", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want advisory", r.Severity)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, loose) {
		t.Fatalf("details missing loose file %s:\n%s", loose, joined)
	}
	if !strings.Contains(joined, nested) {
		t.Fatalf("details missing loose dir %s:\n%s", nested, joined)
	}
}

func TestServiceSecretsPermsCheckFixTightensPerms(t *testing.T) {
	cityPath := t.TempDir()
	loose := filepath.Join(cityPath, ".gc", "services", "bridge", "secrets", "bot-token.txt")
	writeSecretFile(t, loose, 0o644)
	nested := filepath.Join(cityPath, ".gc", "services", "bridge", "secrets", "nested")
	writeSecretFile(t, filepath.Join(nested, "key.pem"), 0o640)
	if err := os.Chmod(nested, 0o755); err != nil {
		t.Fatalf("chmod nested: %v", err)
	}

	check := NewServiceSecretsPermsCheck(serviceSecretsTestConfig(), cityPath)
	if !check.CanFix() {
		t.Fatal("CanFix = false, want true")
	}
	if r := check.Run(&CheckContext{CityPath: cityPath}); r.Status != StatusWarning {
		t.Fatalf("pre-fix Status = %v, want Warning", r.Status)
	}
	if err := check.Fix(&CheckContext{CityPath: cityPath}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	for path, want := range map[string]os.FileMode{
		loose:                            0o600,
		filepath.Join(nested, "key.pem"): 0o600,
		nested:                           0o700,
	} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if st.Mode().Perm() != want {
			t.Fatalf("%s mode = %o, want %o", path, st.Mode().Perm(), want)
		}
	}
	if r := check.Run(&CheckContext{CityPath: cityPath}); r.Status != StatusOK {
		t.Fatalf("post-fix Status = %v, want OK (message %q)", r.Status, r.Message)
	}
}

func TestServiceSecretsPermsCheckNilConfig(t *testing.T) {
	check := NewServiceSecretsPermsCheck(nil, t.TempDir())
	if r := check.Run(&CheckContext{}); r.Status != StatusOK {
		t.Fatalf("Status = %v, want OK", r.Status)
	}
}
