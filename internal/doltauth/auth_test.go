package doltauth

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

func TestAuthScopeRoot(t *testing.T) {
	tests := []struct {
		name      string
		cityRoot  string
		scopeRoot string
		target    contract.DoltConnectionTarget
		want      string
	}{
		{name: "city scope", cityRoot: "/city", scopeRoot: "/city", target: contract.DoltConnectionTarget{}, want: "/city"},
		{name: "explicit rig", cityRoot: "/city", scopeRoot: "/city/rig", target: contract.DoltConnectionTarget{EndpointOrigin: contract.EndpointOriginExplicit}, want: "/city/rig"},
		{name: "inherited rig", cityRoot: "/city", scopeRoot: "/city/rig", target: contract.DoltConnectionTarget{EndpointOrigin: contract.EndpointOriginInheritedCity}, want: "/city"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AuthScopeRoot(tt.cityRoot, tt.scopeRoot, tt.target); got != tt.want {
				t.Fatalf("AuthScopeRoot() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolvePrefersProcessOverrides(t *testing.T) {
	scopeRoot := t.TempDir()
	writeStorePassword(t, scopeRoot, "store-secret")
	credentialsPath := writeCredentialsFile(t, "db.example.com", 3307, "credentials-secret")
	t.Setenv("GC_DOLT_USER", "override-user")
	t.Setenv("GC_DOLT_PASSWORD", "override-secret")
	t.Setenv("BEADS_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	resolved := Resolve(scopeRoot, "fallback-user", "db.example.com", 3307)
	if resolved.User != "override-user" {
		t.Fatalf("User = %q, want override-user", resolved.User)
	}
	if resolved.Password != "override-secret" {
		t.Fatalf("Password = %q, want override-secret", resolved.Password)
	}
	if resolved.CredentialsFileOverride != credentialsPath {
		t.Fatalf("CredentialsFileOverride = %q, want %q", resolved.CredentialsFileOverride, credentialsPath)
	}
}

func TestResolveUsesStoreLocalPasswordBeforeCredentialsFile(t *testing.T) {
	scopeRoot := t.TempDir()
	writeStorePassword(t, scopeRoot, "store-secret")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", writeCredentialsFile(t, "db.example.com", 3307, "credentials-secret"))

	resolved := Resolve(scopeRoot, "fallback-user", "db.example.com", 3307)
	if resolved.User != "fallback-user" {
		t.Fatalf("User = %q, want fallback-user", resolved.User)
	}
	if resolved.Password != "store-secret" {
		t.Fatalf("Password = %q, want store-secret", resolved.Password)
	}
}

func TestResolveUsesCredentialsFileFallback(t *testing.T) {
	scopeRoot := t.TempDir()
	credentialsPath := writeCredentialsFile(t, "db.example.com", 3307, "credentials-secret")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	resolved := Resolve(scopeRoot, "fallback-user", "db.example.com", 3307)
	if resolved.Password != "credentials-secret" {
		t.Fatalf("Password = %q, want credentials-secret", resolved.Password)
	}
}

func TestResolveReturnsNoCredentialsPasswordWithoutHostOrPort(t *testing.T) {
	scopeRoot := t.TempDir()
	credentialsPath := writeCredentialsFile(t, "db.example.com", 3307, "credentials-secret")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	resolved := Resolve(scopeRoot, "fallback-user", "", 0)
	if resolved.Password != "" {
		t.Fatalf("Password = %q, want empty without host/port", resolved.Password)
	}
}

func TestResolveFromEnvDefaultsLoopbackHostWhenOnlyPortIsPresent(t *testing.T) {
	scopeRoot := t.TempDir()
	credentialsPath := writeCredentialsFile(t, "127.0.0.1", 3307, "loopback-secret")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	resolved := ResolveFromEnv(scopeRoot, "fallback-user", map[string]string{"GC_DOLT_PORT": "3307"})
	if resolved.Password != "loopback-secret" {
		t.Fatalf("Password = %q, want loopback-secret", resolved.Password)
	}
}

func TestResolveFromEnvUsesAmbientBeadsDoltPassword(t *testing.T) {
	scopeRoot := t.TempDir()
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "operator-secret")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	resolved := ResolveFromEnv(scopeRoot, "fallback-user", map[string]string{})
	if resolved.Password != "operator-secret" {
		t.Fatalf("Password = %q, want operator-secret", resolved.Password)
	}
}

func TestResolveFromEnvUsesProjectedBeadsDoltPassword(t *testing.T) {
	scopeRoot := t.TempDir()
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	resolved := ResolveFromEnv(scopeRoot, "fallback-user", map[string]string{
		"BEADS_DOLT_PASSWORD": "projected-secret",
	})
	if resolved.Password != "projected-secret" {
		t.Fatalf("Password = %q, want projected-secret", resolved.Password)
	}
}

func TestResolveFromEnvPrefersStoreLocalPasswordOverProjectedPassword(t *testing.T) {
	scopeRoot := t.TempDir()
	writeStorePassword(t, scopeRoot, "rig-secret")
	t.Setenv("GC_DOLT_USER", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	resolved := ResolveFromEnv(scopeRoot, "fallback-user", map[string]string{
		"BEADS_DOLT_PASSWORD": "projected-city-secret",
	})
	if resolved.Password != "rig-secret" {
		t.Fatalf("Password = %q, want rig-secret", resolved.Password)
	}
}

func writeStorePassword(t *testing.T, scopeRoot, password string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.beads): %v", err)
	}
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD="+password+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(.env): %v", err)
	}
}

//nolint:unparam // test helper keeps explicit host/port shape
func writeCredentialsFile(t *testing.T, host string, port int, password string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials")
	contents := "[" + host + ":" + strconv.Itoa(port) + "]\npassword=" + password + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile(credentials): %v", err)
	}
	return path
}
