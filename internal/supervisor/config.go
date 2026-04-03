package supervisor

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/citylayout"
)

// isTestBinary reports whether the current process is a Go test binary.
// Go test binaries are named *.test (e.g., "supervisor.test").
func isTestBinary() bool {
	if len(os.Args) == 0 {
		return false
	}
	return strings.HasSuffix(os.Args[0], ".test") ||
		strings.Contains(os.Args[0], ".test")
}

// Config holds machine-wide supervisor configuration loaded from
// ~/.gc/supervisor.toml (or $GC_HOME/supervisor.toml).
type Config struct {
	Supervisor  Section           `toml:"supervisor"`
	Publication PublicationConfig `toml:"publication,omitempty"`
}

// Section holds the [supervisor] table fields.
type Section struct {
	Port           int    `toml:"port,omitempty"`
	Bind           string `toml:"bind,omitempty"`
	PatrolInterval string `toml:"patrol_interval,omitempty"`
	AllowMutations bool   `toml:"allow_mutations,omitempty"`
}

// PublicationConfig holds machine-wide publication policy for workspace
// services. Hosted publication is the only supported provider in v0.
type PublicationConfig struct {
	Provider         string                      `toml:"provider,omitempty"`
	TenantSlug       string                      `toml:"tenant_slug,omitempty"`
	PublicBaseDomain string                      `toml:"public_base_domain,omitempty"`
	TenantBaseDomain string                      `toml:"tenant_base_domain,omitempty"`
	TenantAuth       PublicationTenantAuthConfig `toml:"tenant_auth,omitempty"`
}

// PublicationTenantAuthConfig configures tenant-route auth policy.
type PublicationTenantAuthConfig struct {
	PolicyRef string `toml:"policy_ref,omitempty"`
}

// BindOrDefault returns the bind address, defaulting to "127.0.0.1".
func (s Section) BindOrDefault() string {
	if s.Bind == "" {
		return "127.0.0.1"
	}
	return s.Bind
}

// PortOrDefault returns the API port, defaulting to 8372.
func (s Section) PortOrDefault() int {
	if s.Port <= 0 {
		return 8372
	}
	return s.Port
}

// PatrolIntervalDuration returns the patrol interval as a time.Duration.
// Defaults to 10s on empty or unparseable values.
func (s Section) PatrolIntervalDuration() time.Duration {
	if s.PatrolInterval == "" {
		return 10 * time.Second
	}
	d, err := time.ParseDuration(s.PatrolInterval)
	if err != nil || d <= 0 {
		return 10 * time.Second
	}
	return d
}

// ProviderOrDefault returns the normalized publication provider.
func (p PublicationConfig) ProviderOrDefault() string {
	return strings.ToLower(strings.TrimSpace(p.Provider))
}

// Enabled reports whether machine publication is configured.
func (p PublicationConfig) Enabled() bool {
	return p.ProviderOrDefault() != ""
}

// BaseDomainForVisibility returns the base domain for a publication visibility.
func (p PublicationConfig) BaseDomainForVisibility(visibility string) string {
	switch strings.ToLower(strings.TrimSpace(visibility)) {
	case "public":
		return normalizePublicationDomain(p.PublicBaseDomain)
	case "tenant":
		return normalizePublicationDomain(p.TenantBaseDomain)
	default:
		return ""
	}
}

// TenantSlugOrDefault returns the normalized tenant slug.
func (p PublicationConfig) TenantSlugOrDefault() string {
	return normalizePublicationDomain(p.TenantSlug)
}

func normalizePublicationDomain(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimPrefix(value, ".")
	value = strings.TrimSuffix(value, ".")
	return value
}

// LoadConfig loads supervisor config from the given path. Returns a
// zero-value Config (with defaults) if the file doesn't exist.
func LoadConfig(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		seeded, seedErr := seedIsolatedSupervisorConfig(path)
		if seedErr != nil {
			return cfg, seedErr
		}
		if !seeded {
			return cfg, nil
		}
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return cfg, err
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// DefaultHome returns the default GC home directory (~/.gc). Respects
// the GC_HOME environment variable override.
//
// Guard: in test binaries, GC_HOME must be set explicitly to prevent
// silent fallback to the user's real ~/.gc directory.
func DefaultHome() string {
	if v := os.Getenv("GC_HOME"); v != "" {
		return v
	}
	if isTestBinary() {
		panic("supervisor.DefaultHome: GC_HOME must be set during tests to prevent host supervisor interference")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".gc")
	}
	return filepath.Join(home, ".gc")
}

// RuntimeDir returns the directory for ephemeral runtime files (lock,
// socket). Uses $XDG_RUNTIME_DIR/gc if available, falls back to
// DefaultHome().
//
// Guard: in test binaries, XDG_RUNTIME_DIR or GC_HOME must be set to
// prevent connecting to the host supervisor socket.
func RuntimeDir() string {
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return filepath.Join(v, "gc")
	}
	return DefaultHome() // DefaultHome has its own test guard
}

// RegistryPath returns the path to the cities.toml registry file.
func RegistryPath() string {
	return filepath.Join(DefaultHome(), "cities.toml")
}

// ConfigPath returns the path to the supervisor.toml config file.
func ConfigPath() string {
	return filepath.Join(DefaultHome(), "supervisor.toml")
}

// PublicationsPath returns the authoritative publication store path for a city
// runtime when cityPath is set. When cityPath is empty, it falls back to the
// legacy GC_HOME-scoped location.
func PublicationsPath(cityPath string) string {
	if cityPath != "" {
		return citylayout.RuntimePath(cityPath, "supervisor", "publications.json")
	}
	return filepath.Join(DefaultHome(), "supervisor", "publications.json")
}

func seedIsolatedSupervisorConfig(path string) (bool, error) {
	if !shouldSeedIsolatedSupervisorConfig(path) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	port, err := reserveLoopbackPort()
	if err != nil {
		return false, err
	}
	data := []byte(fmt.Sprintf("[supervisor]\nport = %d\n", port))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return true, nil
		}
		return false, err
	}
	defer f.Close() //nolint:errcheck // best-effort cleanup
	if _, err := f.Write(data); err != nil {
		return false, err
	}
	if err := f.Sync(); err != nil {
		return false, err
	}
	return true, nil
}

func shouldSeedIsolatedSupervisorConfig(path string) bool {
	gcHome := os.Getenv("GC_HOME")
	if gcHome == "" {
		return false
	}
	if !samePath(path, ConfigPath()) {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return true
	}
	return !samePath(gcHome, filepath.Join(home, ".gc"))
}

func reserveLoopbackPort() (int, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer lis.Close() //nolint:errcheck // best-effort cleanup
	addr, ok := lis.Addr().(*net.TCPAddr)
	if !ok || addr.Port <= 0 {
		return 0, fmt.Errorf("unexpected supervisor listener address %T", lis.Addr())
	}
	return addr.Port, nil
}

func samePath(a, b string) bool {
	a = canonicalPath(a)
	b = canonicalPath(b)
	return a == b
}

func canonicalPath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Clean(p)
}
