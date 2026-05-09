package api

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestResolveDoltConnectionUsesCanonicalExternalEndpoint(t *testing.T) {
	clearDoltAuthEnv(t)
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")

	mustWriteCanonicalConfig(t, fs, city, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusUnverified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
		DoltUser:       "agent",
	})
	mustWriteCanonicalMetadata(t, fs, city, "hq")

	targetHost, targetPort, database, user, password, err := resolveDoltConnection(city, city)
	if err != nil {
		t.Fatalf("resolveDoltConnection(city) error = %v", err)
	}
	if targetHost != "db.example.com" || targetPort != 3307 || database != "hq" || user != "agent" || password != "" {
		t.Fatalf("city target = (%q, %d, %q, %q, %q)", targetHost, targetPort, database, user, password)
	}

	mustWriteCanonicalConfig(t, fs, rig, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusUnverified,
		DoltHost:       "rig-db.example.com",
		DoltPort:       "4406",
		DoltUser:       "rig-agent",
	})
	mustWriteCanonicalMetadata(t, fs, rig, "fe")

	targetHost, targetPort, database, user, password, err = resolveDoltConnection(city, rig)
	if err != nil {
		t.Fatalf("resolveDoltConnection(rig) error = %v", err)
	}
	if targetHost != "rig-db.example.com" || targetPort != 4406 || database != "fe" || user != "rig-agent" || password != "" {
		t.Fatalf("rig target = (%q, %d, %q, %q, %q)", targetHost, targetPort, database, user, password)
	}
}

func TestResolveDoltConnectionUsesInheritedCityEndpoint(t *testing.T) {
	clearDoltAuthEnv(t)
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")

	mustWriteCanonicalConfig(t, fs, city, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
		DoltUser:       "agent",
	})
	mustWriteCanonicalMetadata(t, fs, city, "hq")
	mustWriteCanonicalConfig(t, fs, rig, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusUnverified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
		DoltUser:       "agent",
	})
	mustWriteCanonicalMetadata(t, fs, rig, "fe")

	targetHost, targetPort, database, user, password, err := resolveDoltConnection(city, rig)
	if err != nil {
		t.Fatalf("resolveDoltConnection(rig) error = %v", err)
	}
	if targetHost != "db.example.com" || targetPort != 3307 || database != "fe" || user != "agent" || password != "" {
		t.Fatalf("inherited rig target = (%q, %d, %q, %q, %q)", targetHost, targetPort, database, user, password)
	}
}

func TestResolveDoltConnectionInheritedRigUsesCityStorePassword(t *testing.T) {
	clearDoltAuthEnv(t)
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")

	mustWriteCanonicalConfig(t, fs, city, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
		DoltUser:       "agent",
	})
	mustWriteCanonicalMetadata(t, fs, city, "hq")
	mustWriteStorePassword(t, city, "city-secret")

	mustWriteCanonicalConfig(t, fs, rig, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
		DoltUser:       "agent",
	})
	mustWriteCanonicalMetadata(t, fs, rig, "fe")
	mustWriteStorePassword(t, rig, "rig-secret")

	host, port, database, user, password, err := resolveDoltConnection(city, rig)
	if err != nil {
		t.Fatalf("resolveDoltConnection(rig) error = %v", err)
	}
	if host != "db.example.com" || port != 3307 || database != "fe" || user != "agent" || password != "city-secret" {
		t.Fatalf("inherited rig target = (%q, %d, %q, %q, %q)", host, port, database, user, password)
	}
}

func TestResolveDoltConnectionExplicitRigUsesRigStorePassword(t *testing.T) {
	clearDoltAuthEnv(t)
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")

	mustWriteCanonicalConfig(t, fs, city, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
		DoltUser:       "city-agent",
	})
	mustWriteCanonicalMetadata(t, fs, city, "hq")
	mustWriteStorePassword(t, city, "city-secret")

	mustWriteCanonicalConfig(t, fs, rig, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "rig-db.example.com",
		DoltPort:       "4406",
		DoltUser:       "rig-agent",
	})
	mustWriteCanonicalMetadata(t, fs, rig, "fe")
	mustWriteStorePassword(t, rig, "rig-secret")

	host, port, database, user, password, err := resolveDoltConnection(city, rig)
	if err != nil {
		t.Fatalf("resolveDoltConnection(rig) error = %v", err)
	}
	if host != "rig-db.example.com" || port != 4406 || database != "fe" || user != "rig-agent" || password != "rig-secret" {
		t.Fatalf("explicit rig target = (%q, %d, %q, %q, %q)", host, port, database, user, password)
	}
}

func TestResolveDoltConnectionUsesCredentialsFileFallback(t *testing.T) {
	clearDoltAuthEnv(t)
	fs := fsys.OSFS{}
	city := t.TempDir()
	rig := filepath.Join(t.TempDir(), "frontend")

	mustWriteCanonicalConfig(t, fs, city, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
		DoltUser:       "agent",
	})
	mustWriteCanonicalMetadata(t, fs, city, "hq")
	mustWriteCanonicalConfig(t, fs, rig, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
		DoltUser:       "agent",
	})
	mustWriteCanonicalMetadata(t, fs, rig, "fe")
	credentialsPath := mustWriteCredentialsFile(t, "db.example.com", 3307, "credentials-secret")
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	host, port, database, user, password, err := resolveDoltConnection(city, rig)
	if err != nil {
		t.Fatalf("resolveDoltConnection(rig) error = %v", err)
	}
	if host != "db.example.com" || port != 3307 || database != "fe" || user != "agent" || password != "credentials-secret" {
		t.Fatalf("credentials target = (%q, %d, %q, %q, %q)", host, port, database, user, password)
	}
}

func TestResolveDoltConnectionUsesManagedRuntimePort(t *testing.T) {
	clearDoltAuthEnv(t)
	fs := fsys.OSFS{}
	city := t.TempDir()
	mustWriteCanonicalConfig(t, fs, city, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	mustWriteCanonicalMetadata(t, fs, city, "hq")
	managedPort := mustWriteManagedRuntimeState(t, fs, city, 0)

	host, port, database, user, password, err := resolveDoltConnection(city, city)
	if err != nil {
		t.Fatalf("resolveDoltConnection() error = %v", err)
	}
	if host != "127.0.0.1" || port != managedPort || database != "hq" || user != "" || password != "" {
		t.Fatalf("managed target = (%q, %d, %q, %q, %q)", host, port, database, user, password)
	}
}

func TestResolveDoltConnectionRejectsInvalidCityExplicitOrigin(t *testing.T) {
	clearDoltAuthEnv(t)
	fs := fsys.OSFS{}
	city := t.TempDir()
	mustWriteCanonicalConfig(t, fs, city, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
	})
	mustWriteCanonicalMetadata(t, fs, city, "hq")

	if _, _, _, _, _, err := resolveDoltConnection(city, city); err == nil || !strings.Contains(err.Error(), "invalid for city scope") {
		t.Fatalf("resolveDoltConnection() error = %v, want city-scope origin rejection", err)
	}
}

func TestBuildDoltDSNUsesResolvedUserAndPassword(t *testing.T) {
	tests := []struct {
		name     string
		user     string
		password string
		want     string
	}{
		{name: "explicit user", user: "agent", want: "agent@tcp(db.example.com:3307)/hq?allowNativePasswords=false&checkConnLiveness=false&parseTime=true&timeout=10s&maxAllowedPacket=0"},
		{name: "defaults to root", user: "", want: "root@tcp(db.example.com:3307)/hq?allowNativePasswords=false&checkConnLiveness=false&parseTime=true&timeout=10s&maxAllowedPacket=0"},
		{name: "escapes password", user: "agent", password: "p@ss:word", want: "agent:p@ss:word@tcp(db.example.com:3307)/hq?allowNativePasswords=false&checkConnLiveness=false&parseTime=true&timeout=10s&maxAllowedPacket=0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildDoltDSN(tt.user, tt.password, "db.example.com", 3307, "hq"); got != tt.want {
				t.Fatalf("buildDoltDSN() = %q, want %q", got, tt.want)
			}
		})
	}
}

func clearDoltAuthEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"GC_DOLT_USER", "GC_DOLT_PASSWORD", "BEADS_DOLT_PASSWORD", "BEADS_CREDENTIALS_FILE"} {
		t.Setenv(key, "")
	}
}

//nolint:unparam // helper keeps FS explicit in tests
func mustWriteCanonicalConfig(t *testing.T, fs fsys.FS, dir string, state contract.ConfigState) {
	t.Helper()
	if err := fs.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalConfig(fs, filepath.Join(dir, ".beads", "config.yaml"), state); err != nil {
		t.Fatal(err)
	}
}

//nolint:unparam // helper keeps FS explicit in tests
func mustWriteCanonicalMetadata(t *testing.T, fs fsys.FS, dir, db string) {
	t.Helper()
	if _, err := contract.EnsureCanonicalMetadata(fs, filepath.Join(dir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: db,
	}); err != nil {
		t.Fatal(err)
	}
}

func mustWriteStorePassword(t *testing.T, dir, password string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD="+password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustWriteCredentialsFile(t *testing.T, host string, port int, password string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials")
	contents := "[" + host + ":" + strconv.Itoa(port) + "]\npassword=" + password + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustWriteManagedRuntimeState(t *testing.T, fs fsys.FS, city string, port int) int {
	t.Helper()
	stateDir := filepath.Join(city, ".gc", "runtime", "packs", "dolt")
	if err := fs.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	addr := "127.0.0.1:0"
	if port > 0 {
		addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port = ln.Addr().(*net.TCPAddr).Port
	payload, err := json.Marshal(struct {
		Running bool   `json:"running"`
		PID     int    `json:"pid"`
		Port    int    `json:"port"`
		DataDir string `json:"data_dir"`
	}{
		Running: true,
		PID:     os.Getpid(),
		Port:    port,
		DataDir: filepath.Join(city, ".beads", "dolt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	return port
}
